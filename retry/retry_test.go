package retry

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// statusServer returns an httptest.Server that emits the given status codes
// in order, looping. The hits counter is shared by all requests.
func statusServer(t *testing.T, codes ...int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		i := hits.Add(1) - 1
		w.WriteHeader(codes[int(i)%len(codes)])
		_, _ = w.Write([]byte("body"))
	}))
	t.Cleanup(s.Close)
	return s, &hits
}

func mustNewTransport(t *testing.T, opts ...Option) http.RoundTripper {
	t.Helper()
	rt, err := NewTransport(http.DefaultTransport, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return rt
}

func TestRetry_HappyPath_NoRetry(t *testing.T) {
	s, hits := statusServer(t, 200)
	rt := mustNewTransport(t)
	c := &http.Client{Transport: rt}

	resp, err := c.Get(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("hits=%d, want 1", got)
	}
}

func TestRetry_5xxThen200(t *testing.T) {
	s, hits := statusServer(t, 503, 200)
	rt := mustNewTransport(t,
		WithBackoff(time.Millisecond, 5*time.Millisecond),
	)
	c := &http.Client{Transport: rt}

	resp, err := c.Get(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("hits=%d, want 2", got)
	}
}

func TestRetry_Exhaustion(t *testing.T) {
	s, hits := statusServer(t, 503, 503, 503)
	rt := mustNewTransport(t,
		WithMaxAttempts(3),
		WithBackoff(time.Millisecond, 5*time.Millisecond),
	)
	c := &http.Client{Transport: rt}

	resp, err := c.Get(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Fatalf("status=%d, want 503", resp.StatusCode)
	}
	if got := hits.Load(); got != 3 {
		t.Fatalf("hits=%d, want 3", got)
	}
}

func TestRetry_429Handoff(t *testing.T) {
	s, hits := statusServer(t, 429, 200)
	rt := mustNewTransport(t)
	c := &http.Client{Transport: rt}

	resp, err := c.Get(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 429 {
		t.Fatalf("status=%d, want 429 (no retry)", resp.StatusCode)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("hits=%d, want 1 (429 should not be retried)", got)
	}
}

func TestRetry_NonIdempotentNotRetried(t *testing.T) {
	s, hits := statusServer(t, 503)
	rt := mustNewTransport(t,
		WithBackoff(time.Millisecond, 5*time.Millisecond),
	)
	c := &http.Client{Transport: rt}

	resp, err := c.Post(s.URL, "text/plain", strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("hits=%d, want 1 (POST is non-idempotent by default)", got)
	}
}

func TestRetry_UserPredicateOptInForPOST(t *testing.T) {
	s, hits := statusServer(t, 503, 200)
	rt := mustNewTransport(t,
		WithBackoff(time.Millisecond, 5*time.Millisecond),
		WithRetryOn(func(req *http.Request, resp *http.Response, err error) bool {
			if req.Method == http.MethodPost {
				if err != nil {
					return IsTransientNetErr(err)
				}
				return resp != nil && IsRetryable5xx(resp.StatusCode)
			}
			return false
		}),
	)
	c := &http.Client{Transport: rt}

	resp, err := c.Post(s.URL, "text/plain", strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("hits=%d, want 2", got)
	}
}

func TestRetry_UserPredicatePanicsRecovered(t *testing.T) {
	s, hits := statusServer(t, 503)
	var buf syncBuf
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	rt := mustNewTransport(t,
		WithBackoff(time.Millisecond, 5*time.Millisecond),
		WithLogger(logger),
		WithRetryOn(func(*http.Request, *http.Response, error) bool {
			panic("boom")
		}),
	)
	c := &http.Client{Transport: rt}

	resp, err := c.Get(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("hits=%d, want 1 (panicking predicate stops retries)", got)
	}
	if !strings.Contains(buf.String(), "retry_predicate_panic") {
		t.Fatalf("expected retry_predicate_panic event in log; got: %s", buf.String())
	}
}

func TestRetry_ContextCancelDuringSleep(t *testing.T) {
	var hits atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(503)
	}))
	t.Cleanup(s.Close)

	// maxDelay >= 5s so the Retry-After does NOT exceed the cap; we want to
	// actually enter the sleep so context-cancellation can fire.
	rt := mustNewTransport(t,
		WithMaxAttempts(3),
		WithBackoff(time.Millisecond, 10*time.Second),
	)
	c := &http.Client{Transport: rt}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", s.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	resp, err := c.Do(req)
	elapsed := time.Since(start)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("expected ctx error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v, want DeadlineExceeded", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("ctx cancel should abort within ~100ms; elapsed %v", elapsed)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("hits=%d, want 1 (the second attempt should not have fired)", got)
	}
}

func TestRetry_RetryAfterExceedsMaxAborts(t *testing.T) {
	var hits atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(503)
	}))
	t.Cleanup(s.Close)

	rt := mustNewTransport(t,
		WithMaxAttempts(3),
		WithBackoff(time.Millisecond, 50*time.Millisecond),
	)

	req, err := http.NewRequest("GET", s.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := rt.RoundTrip(req) //nolint:bodyclose // resp is nil on abort path
	if !errors.Is(err, ErrRetryAfterExceedsMax) {
		t.Fatalf("err=%v, want ErrRetryAfterExceedsMax", err)
	}
	if resp != nil {
		t.Fatalf("resp must be nil on abort path; got %+v", resp)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("hits=%d, want 1 (abort on first cap-collision check)", got)
	}
}

// TestRetry_RetryAfterExceedsMax_ConnReuse pins the no-leak invariant: when
// the abort path drains and closes resp.Body, the conn returns to the idle
// pool and the follow-up GET reuses it.
func TestRetry_RetryAfterExceedsMax_ConnReuse(t *testing.T) {
	var hits atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := hits.Add(1)
		if n == 1 {
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, "retry-after server-busy")
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(s.Close)

	rt := mustNewTransport(t,
		WithMaxAttempts(3),
		WithBackoff(time.Millisecond, 50*time.Millisecond),
	)
	client := &http.Client{Transport: rt}

	resp1, err := client.Get(s.URL) //nolint:bodyclose // resp is nil on abort path
	if !errors.Is(err, ErrRetryAfterExceedsMax) {
		t.Fatalf("err=%v, want ErrRetryAfterExceedsMax", err)
	}
	if resp1 != nil {
		t.Fatalf("resp must be nil on abort path; got %+v", resp1)
	}

	var reused bool
	trace := &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused },
	}
	req2, err := http.NewRequestWithContext(
		httptrace.WithClientTrace(context.Background(), trace),
		http.MethodGet, s.URL, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	resp2, err := client.Do(req2)
	if err != nil {
		t.Fatalf("follow-up: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp2.Body)
	_ = resp2.Body.Close()

	if !reused {
		t.Fatal("expected connection reuse; abort path leaked the conn")
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("hits=%d, want 2", got)
	}
}

func TestRetry_BodyNotRewindable(t *testing.T) {
	s, _ := statusServer(t, 503, 200)
	rt := mustNewTransport(t,
		WithBackoff(time.Millisecond, 5*time.Millisecond),
		WithRetryOn(func(req *http.Request, resp *http.Response, err error) bool {
			// allow POST retries so we hit the body-rewind path
			return resp != nil && IsRetryable5xx(resp.StatusCode)
		}),
	)
	c := &http.Client{Transport: rt}

	// http.NewRequest with a *bytes.Reader sets GetBody. Stripping it
	// simulates a caller that built a body-bearing request without GetBody.
	req, err := http.NewRequest("POST", s.URL, bytes.NewReader([]byte("payload")))
	if err != nil {
		t.Fatal(err)
	}
	req.GetBody = nil

	resp, err := c.Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, ErrBodyNotRewindable) {
		t.Fatalf("err=%v, want ErrBodyNotRewindable in chain", err)
	}
}

func TestRetry_GetBodyErrorPropagates(t *testing.T) {
	s, _ := statusServer(t, 503, 200)
	rt := mustNewTransport(t,
		WithBackoff(time.Millisecond, 5*time.Millisecond),
		WithRetryOn(func(req *http.Request, resp *http.Response, err error) bool {
			return resp != nil && IsRetryable5xx(resp.StatusCode)
		}),
	)
	c := &http.Client{Transport: rt}

	getBodyErr := errors.New("synthetic GetBody failure")
	req, err := http.NewRequest("POST", s.URL, bytes.NewReader([]byte("payload")))
	if err != nil {
		t.Fatal(err)
	}
	// Fail on the first GetBody call (which only happens on the first
	// retry attempt; the original Body covers attempt 0).
	req.GetBody = func() (io.ReadCloser, error) { return nil, getBodyErr }

	resp, err := c.Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, getBodyErr) {
		t.Fatalf("err=%v, want chain containing %v", err, getBodyErr)
	}
}

func TestRetry_BackoffValidation(t *testing.T) {
	cases := []struct {
		name string
		min  time.Duration
		max  time.Duration
	}{
		{"both-zero", 0, 0},
		{"min-zero-max-positive", 0, 5 * time.Second},
		{"swapped", 2 * time.Second, 200 * time.Millisecond},
		{"negative-min", -1, time.Second},
		{"min-below-floor", 100 * time.Microsecond, time.Second},
		{"max-above-ceiling", time.Second, 2 * time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt, err := NewTransport(http.DefaultTransport, WithBackoff(tc.min, tc.max))
			if !errors.Is(err, ErrInvalidBackoff) {
				t.Fatalf("err=%v want ErrInvalidBackoff", err)
			}
			if rt != nil {
				t.Fatalf("transport=%v, want nil", rt)
			}
		})
	}
}

func TestRetry_MaxAttemptsValidation(t *testing.T) {
	cases := []int{0, -1, 101, 1_000_000}
	for _, n := range cases {
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			rt, err := NewTransport(http.DefaultTransport, WithMaxAttempts(n))
			if !errors.Is(err, ErrInvalidMaxAttempts) {
				t.Fatalf("err=%v want ErrInvalidMaxAttempts", err)
			}
			if rt != nil {
				t.Fatalf("transport=%v, want nil", rt)
			}
		})
	}
}

func TestRetry_NilBaseUsesDefault(t *testing.T) {
	rt, err := NewTransport(nil)
	if err != nil {
		t.Fatal(err)
	}
	if rt == nil {
		t.Fatal("transport is nil")
	}
}

func TestRetry_MaxAttempts1DisablesRetries(t *testing.T) {
	s, hits := statusServer(t, 503)
	rt := mustNewTransport(t, WithMaxAttempts(1))
	c := &http.Client{Transport: rt}
	resp, err := c.Get(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("hits=%d, want 1", got)
	}
}

// drainProbeBody tracks whether Close was called and whether at least one
// Read happened (proxy for "drained").
type drainProbeBody struct {
	r         io.Reader
	closed    atomic.Bool
	readBytes atomic.Int64
}

func (b *drainProbeBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	b.readBytes.Add(int64(n))
	return n, err
}
func (b *drainProbeBody) Close() error {
	b.closed.Store(true)
	return nil
}

// drainProbeTransport returns 503 with a body that records drain, then 200.
type drainProbeTransport struct {
	first *drainProbeBody
	hits  atomic.Int32
}

func (t *drainProbeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	hit := t.hits.Add(1)
	if hit == 1 {
		return &http.Response{
			StatusCode: 503,
			Body:       t.first,
			Header:     make(http.Header),
			Request:    req,
		}, nil
	}
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader("ok")),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

func TestRetry_DrainsPriorBody(t *testing.T) {
	bigPayload := strings.NewReader(strings.Repeat("x", 8<<10)) // 8 KiB
	probe := &drainProbeBody{r: bigPayload}
	inner := &drainProbeTransport{first: probe}

	rt, err := NewTransport(inner,
		WithBackoff(time.Millisecond, 5*time.Millisecond),
	)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest("GET", "http://example/", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if !probe.closed.Load() {
		t.Fatal("prior response body was not closed before retry")
	}
	if probe.readBytes.Load() == 0 {
		t.Fatal("prior response body was not drained before retry")
	}
}

func TestRetry_TypeOfChainWalking(t *testing.T) {
	netErr := &errStruct{name: "neterr"}
	wrapped := fmt.Errorf("wrap: %w", netErr)
	joined := errors.Join(netErr, errors.New("plain"))
	joinedWrap := errors.Join(wrapped, errors.New("plain"))

	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"plain", netErr, "*retry.errStruct"},
		{"wrapped", wrapped, "*fmt.wrapError/*retry.errStruct"},
		{"joined", joined, "*retry.errStruct|*errors.errorString"},
		{"joined-wrap", joinedWrap, "*fmt.wrapError/*retry.errStruct|*errors.errorString"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := typeOf(tc.err)
			if got != tc.want {
				t.Fatalf("typeOf=%q want %q", got, tc.want)
			}
		})
	}
}

func TestRetry_TypeOfDepthCap(t *testing.T) {
	// Build a 7-deep linear wrap chain: depth-cap should fire.
	err := error(&errStruct{name: "leaf"})
	for i := range 7 {
		err = fmt.Errorf("layer%d: %w", i, err)
	}
	got := typeOf(err)
	if !strings.Contains(got, "...") {
		t.Fatalf("expected truncation marker; got %q", got)
	}
}

func TestRetry_ParseRetryAfter(t *testing.T) {
	// Cases store the header value (or nilResp) so the *http.Response is
	// constructed inside the loop body. bodyclose can't track responses
	// through table literals; constructing in-loop sidesteps that.
	farFuture := time.Now().Add(45 * time.Minute)
	farPast := time.Now().Add(-45 * time.Minute)
	cases := []struct {
		name        string
		header      string // Retry-After value; ignored when nilResp is true
		nilResp     bool
		wantOutcome parseOutcome
		// wantDur is checked exactly when wantOutcome is Absent, Unparseable,
		// or Numeric. For date outcomes the test asserts a tolerance window.
		wantDur time.Duration
	}{
		{name: "nil-resp", nilResp: true, wantOutcome: outcomeAbsent, wantDur: 0},
		{name: "missing", header: "", wantOutcome: outcomeAbsent, wantDur: 0},
		{name: "delta-positive", header: "5", wantOutcome: outcomeNumeric, wantDur: 5 * time.Second},
		{name: "delta-zero", header: "0", wantOutcome: outcomeNumeric, wantDur: time.Nanosecond},
		{name: "delta-negative", header: "-3", wantOutcome: outcomeNumeric, wantDur: 0},
		{name: "delta-leading-space", header: " 5", wantOutcome: outcomeNumeric, wantDur: 5 * time.Second},
		{name: "delta-trailing-space", header: "5 ", wantOutcome: outcomeNumeric, wantDur: 5 * time.Second},
		{name: "delta-both-spaces", header: " 5 ", wantOutcome: outcomeNumeric, wantDur: 5 * time.Second},
		{name: "delta-fractional", header: "5.0", wantOutcome: outcomeUnparseable, wantDur: 0},
		{name: "delta-overflows-int64-nanos", header: "10000000000", wantOutcome: outcomeNumeric, wantDur: maxDelayCeiling + time.Hour},
		{name: "delta-just-below-threshold", header: "9223372036", wantOutcome: outcomeNumeric, wantDur: 9223372036 * time.Second},
		{name: "delta-just-above-threshold", header: "9223372037", wantOutcome: outcomeNumeric, wantDur: maxDelayCeiling + time.Hour},
		{name: "delta-maxint64", header: strconv.FormatInt(math.MaxInt64, 10), wantOutcome: outcomeNumeric, wantDur: maxDelayCeiling + time.Hour},
		{name: "garbage", header: "not a date", wantOutcome: outcomeUnparseable, wantDur: 0},
		{name: "iso-8601", header: "2026-01-02T15:04:05Z", wantOutcome: outcomeUnparseable, wantDur: 0},
		{name: "rfc1123-utc-literal", header: farFuture.UTC().Format("Mon, 02 Jan 2006 15:04:05 UTC"), wantOutcome: outcomeUnparseable, wantDur: 0},
		{name: "http-date-past", header: farPast.UTC().Format(http.TimeFormat), wantOutcome: outcomeDate},
		{name: "http-date-future", header: farFuture.UTC().Format(http.TimeFormat), wantOutcome: outcomeDate},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var resp *http.Response
			if !tc.nilResp {
				resp = &http.Response{Header: make(http.Header), Body: http.NoBody}
				if tc.header != "" {
					resp.Header.Set("Retry-After", tc.header)
				}
				defer func() { _ = resp.Body.Close() }()
			}
			gotDur, gotOutcome := parseRetryAfter(resp)
			if gotOutcome != tc.wantOutcome {
				t.Fatalf("outcome=%v want %v", gotOutcome, tc.wantOutcome)
			}
			switch tc.wantOutcome {
			case outcomeAbsent, outcomeUnparseable, outcomeNumeric:
				if gotDur != tc.wantDur {
					t.Fatalf("duration=%v want %v", gotDur, tc.wantDur)
				}
			case outcomeDate:
				if tc.name == "http-date-past" {
					if gotDur != 0 {
						t.Fatalf("past date duration=%v want 0", gotDur)
					}
				} else {
					if gotDur < 44*time.Minute || gotDur > 46*time.Minute {
						t.Fatalf("future date duration=%v outside [44m,46m]", gotDur)
					}
				}
			}
		})
	}
}

func TestRetry_RetryAfter(t *testing.T) {
	farFuture := time.Now().Add(45 * time.Minute)
	cases := []struct {
		name     string
		header   string
		nilResp  bool
		wantDur  time.Duration
		wantOk   bool
		wantSkew time.Duration
	}{
		{name: "nil-resp", nilResp: true},
		{name: "absent"},
		{name: "delta-positive", header: "5", wantDur: 5 * time.Second, wantOk: true},
		{name: "delta-zero", header: "0", wantDur: time.Nanosecond, wantOk: true},
		{name: "delta-negative", header: "-3"},
		{name: "delta-leading-space", header: " 5", wantDur: 5 * time.Second, wantOk: true},
		{name: "garbage", header: "not a date"},
		{name: "iso-8601", header: "2026-01-02T15:04:05Z"},
		{name: "rfc1123-utc-literal", header: farFuture.UTC().Format("Mon, 02 Jan 2006 15:04:05 UTC")},
		{name: "http-date-future", header: farFuture.UTC().Format(http.TimeFormat), wantDur: 45 * time.Minute, wantOk: true, wantSkew: time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var resp *http.Response
			if !tc.nilResp {
				resp = &http.Response{Header: make(http.Header), Body: http.NoBody}
				if tc.header != "" {
					resp.Header.Set("Retry-After", tc.header)
				}
				defer func() { _ = resp.Body.Close() }()
			}
			gotDur, gotOk := RetryAfter(resp)
			if gotOk != tc.wantOk {
				t.Fatalf("ok=%v want %v (dur=%v)", gotOk, tc.wantOk, gotDur)
			}
			if !tc.wantOk {
				if gotDur != 0 {
					t.Fatalf("expected zero duration when !ok, got %v", gotDur)
				}
				return
			}
			if tc.wantSkew == 0 {
				if gotDur != tc.wantDur {
					t.Fatalf("dur=%v want %v", gotDur, tc.wantDur)
				}
				return
			}
			diff := gotDur - tc.wantDur
			if diff < -tc.wantSkew || diff > tc.wantSkew {
				t.Fatalf("dur=%v diverges from %v by more than %v", gotDur, tc.wantDur, tc.wantSkew)
			}
		})
	}
}

func TestRetry_SourceLabel(t *testing.T) {
	cases := []struct {
		name    string
		ra      time.Duration
		outcome parseOutcome
		wantSrc string
	}{
		{"absent", 0, outcomeAbsent, "jitter"},
		{"numeric-positive", 5 * time.Second, outcomeNumeric, "retry_after"},
		{"numeric-clamped-zero-stays-jitter", 0, outcomeNumeric, "jitter"},
		{"date-future", 30 * time.Minute, outcomeDate, "retry_after"},
		{"date-past-clamped", 0, outcomeDate, "jitter"},
		{"unparseable", 0, outcomeUnparseable, "malformed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sourceLabel(tc.ra, tc.outcome)
			if got != tc.wantSrc {
				t.Fatalf("sourceLabel=%q want %q", got, tc.wantSrc)
			}
		})
	}
}

func TestRetry_PreviewRawHeader(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"short-ascii", "abc", "abc"},
		{"non-printable", "a\x00b\x7fc\nd", "a?b?c?d"},
		{"truncates-at-32", strings.Repeat("x", 64), strings.Repeat("x", 32)},
		{"high-byte", "\xffhello", "?hello"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := previewRawHeader(tc.in)
			if got != tc.want {
				t.Fatalf("previewRawHeader=%q want %q", got, tc.want)
			}
		})
	}
}

// TestRetry_MalformedRetryAfter_LogsWarn drives a server that emits an
// unparseable Retry-After header twice, then 200. The transport must:
//   - emit retry_retry_after_unparseable at Warn,
//   - label retry_sleep source as "malformed",
//   - still complete the retry via the jitter sleep (not abort).
func TestRetry_MalformedRetryAfter_LogsWarn(t *testing.T) {
	var hits atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := hits.Add(1)
		if n < 3 {
			w.Header().Set("Retry-After", "definitely-not-a-date")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(s.Close)

	var buf syncBuf
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	rt := mustNewTransport(t,
		WithMaxAttempts(3),
		WithBackoff(time.Millisecond, 5*time.Millisecond),
		WithLogger(logger),
	)
	c := &http.Client{Transport: rt}

	resp, err := c.Get(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200 after retries", resp.StatusCode)
	}
	if got := hits.Load(); got != 3 {
		t.Fatalf("hits=%d, want 3", got)
	}

	out := buf.String()
	if !strings.Contains(out, "retry_retry_after_unparseable") {
		t.Fatalf("expected retry_retry_after_unparseable event; got: %s", out)
	}
	if !strings.Contains(out, "raw=definitely-not-a-date") {
		t.Fatalf("expected raw=definitely-not-a-date attribute; got: %s", out)
	}
	if !strings.Contains(out, "source=malformed") {
		t.Fatalf("expected source=malformed on retry_sleep; got: %s", out)
	}
}

func TestRetry_LoggerEmitsExhausted(t *testing.T) {
	s, _ := statusServer(t, 503, 503)
	var buf syncBuf
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	rt := mustNewTransport(t,
		WithMaxAttempts(2),
		WithBackoff(time.Millisecond, 5*time.Millisecond),
		WithLogger(logger),
	)
	c := &http.Client{Transport: rt}
	resp, err := c.Get(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	out := buf.String()
	if !strings.Contains(out, "retry_exhausted") {
		t.Fatalf("expected retry_exhausted event; got: %s", out)
	}
	if !strings.Contains(out, "last_status=503") {
		t.Fatalf("expected last_status=503; got: %s", out)
	}
}

// syncBuf is a goroutine-safe bytes.Buffer for capturing slog output.
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// errStruct is a typed error so test assertions can name the type.
type errStruct struct{ name string }

func (e *errStruct) Error() string { return e.name }

// TestRetry_SilentByDefault_NoSlogDefaultOutput verifies that without an
// explicit WithLogger, retry emits zero records into slog.Default. We swap
// slog.Default for a buffer-backed handler that accepts every level
// (including Debug), drive the transport through every event branch we can
// reach (sleep, decision-retry, decision-stop, exhausted), then assert the
// buffer is empty.
func TestRetry_SilentByDefault_NoSlogDefaultOutput(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	var buf syncBuf
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	s, _ := statusServer(t, 503, 503, 200)
	rt := mustNewTransport(t,
		WithMaxAttempts(3),
		WithBackoff(time.Millisecond, 5*time.Millisecond),
		// No WithLogger; this is the contract under test.
	)
	c := &http.Client{Transport: rt}
	resp, err := c.Get(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	if got := buf.String(); got != "" {
		t.Fatalf("retry emitted to slog.Default without WithLogger; got: %q", got)
	}
}

func TestRetry_IsTransientNetErr_PermanentExclusions(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"io.EOF", io.EOF, true},
		{"io.ErrUnexpectedEOF", io.ErrUnexpectedEOF, true},
		{"context.DeadlineExceeded", context.DeadlineExceeded, true},
		{"net.OpError generic", &net.OpError{Op: "read", Err: errors.New("transient")}, true},
		{"DNS NXDOMAIN (IsNotFound)", &net.DNSError{Name: "nope.invalid", IsNotFound: true}, false},
		{"DNS server failure (not NotFound)", &net.DNSError{Name: "x", IsTemporary: true}, true},
		{"syscall.ECONNREFUSED bare", syscall.ECONNREFUSED, false},
		{"ECONNREFUSED wrapped in OpError", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, false},
		{"x509.UnknownAuthorityError", x509.UnknownAuthorityError{}, false},
		{"x509.HostnameError", &x509.HostnameError{Host: "example.com"}, false},
		{"x509.CertificateInvalidError", x509.CertificateInvalidError{Reason: x509.Expired}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := IsTransientNetErr(tc.err)
			if got != tc.want {
				t.Fatalf("IsTransientNetErr(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
