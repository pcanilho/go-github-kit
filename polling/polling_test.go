package polling

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pcanilho/go-github-kit/cond"
)

// captureSleeps returns a WithSleepFunc that records each requested
// duration into the returned slice.
func captureSleeps() (*[]time.Duration, Option) {
	var (
		mu     sync.Mutex
		sleeps []time.Duration
	)
	return &sleeps, WithSleepFunc(func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		sleeps = append(sleeps, d)
	})
}

// sequencedJSONServer returns a server that emits the given payloads
// in order; after the last one is exhausted it repeats the last.
func sequencedJSONServer(t *testing.T, payloads ...string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		i := int(hits.Add(1) - 1)
		if i >= len(payloads) {
			i = len(payloads) - 1
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, payloads[i])
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestPoll_HappyPath_DonePredicateMatches(t *testing.T) {
	// Poll's WithDone predicate is contractually header/status-only;
	// the caller owns the body. Body-aware predicates belong on
	// As[T] via WithDoneT (see TestAs_DonePredicateOnDecodedValue).
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := hits.Add(1)
		if n >= 3 {
			w.Header().Set("X-Run-Status", "completed")
		}
		_, _ = io.WriteString(w, `{"status":"in_progress"}`)
	}))
	t.Cleanup(srv.Close)

	_, sleepOpt := captureSleeps()
	done := func(r *http.Response) bool {
		return r.Header.Get("X-Run-Status") == "completed"
	}

	var n int
	for resp, err := range Poll(t.Context(), srv.Client(), http.MethodGet, srv.URL, nil, nil,
		10*time.Millisecond, WithDone(done), sleepOpt) {
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		n++
	}
	if n != 3 {
		t.Fatalf("yielded=%d want 3", n)
	}
	if got := hits.Load(); got != 3 {
		t.Fatalf("hits=%d want 3", got)
	}
}

func TestPoll_MaxAttemptsExceeded_YieldsLastResp(t *testing.T) {
	srv, _ := sequencedJSONServer(t, `{"status":"in_progress"}`)
	_, sleepOpt := captureSleeps()

	var n int
	var lastErr error
	var lastResp *http.Response
	var lastStatus int
	for resp, err := range Poll(t.Context(), srv.Client(), http.MethodGet, srv.URL, nil, nil,
		time.Millisecond, WithMaxAttempts(3), sleepOpt) {
		n++
		if err != nil {
			lastErr = err
			// Body has already been drained+closed by the prior
			// normal-yield iteration (per Poll's body-ownership
			// contract). Inspect headers/status only.
			if resp != nil {
				lastResp = resp
				lastStatus = resp.StatusCode
			}
			continue
		}
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}
	if !errors.Is(lastErr, ErrMaxAttemptsExceeded) {
		t.Fatalf("err=%v want ErrMaxAttemptsExceeded", lastErr)
	}
	if lastResp == nil {
		t.Fatal("lastResp must be non-nil on boundary yield")
	}
	if lastStatus != http.StatusOK {
		t.Fatalf("lastResp.StatusCode=%d want 200 (boundary yield exposes status/headers)", lastStatus)
	}
	if n != 4 {
		t.Fatalf("yields=%d want 4 (3 normal + 1 boundary)", n)
	}
}

func TestPoll_MaxWallClockExceeded_WrapsDeadlineExceeded(t *testing.T) {
	srv, _ := sequencedJSONServer(t, `{"status":"in_progress"}`)

	var lastErr error
	for resp, err := range Poll(t.Context(), srv.Client(), http.MethodGet, srv.URL, nil, nil,
		50*time.Millisecond, WithMaxWallClock(60*time.Millisecond)) {
		if err != nil {
			lastErr = err
		}
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}
	if !errors.Is(lastErr, ErrMaxWallClockExceeded) {
		t.Fatalf("err=%v want ErrMaxWallClockExceeded", lastErr)
	}
	if !errors.Is(lastErr, context.DeadlineExceeded) {
		t.Fatalf("err=%v should also wrap context.DeadlineExceeded", lastErr)
	}
}

func TestPoll_CtxCancelDuringSleep(t *testing.T) {
	srv, _ := sequencedJSONServer(t, `{"status":"in_progress"}`)

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	var n int
	for resp, err := range Poll(ctx, srv.Client(), http.MethodGet, srv.URL, nil, nil,
		5*time.Second) {
		n++
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		_ = err
	}
	elapsed := time.Since(start)
	if elapsed > 500*time.Millisecond {
		t.Fatalf("ctx cancel should abort within ~50ms; elapsed %v", elapsed)
	}
	if n == 0 {
		t.Fatal("expected at least one yield before cancel")
	}
}

func TestPoll_DonePredicatePanics_RecoveredAndStops(t *testing.T) {
	srv, _ := sequencedJSONServer(t, `{"status":"in_progress"}`, `{"status":"in_progress"}`, `{"status":"completed"}`)
	_, sleepOpt := captureSleeps()

	var buf syncBuf
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	var attempts atomic.Int32
	done := func(*http.Response) bool {
		if attempts.Add(1) == 2 {
			panic("boom")
		}
		return false
	}

	var lastErr error
	for resp, err := range Poll(t.Context(), srv.Client(), http.MethodGet, srv.URL, nil, nil,
		time.Millisecond,
		WithDone(done), WithLogger(logger), sleepOpt) {
		if err != nil {
			lastErr = err
		}
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}
	if !errors.Is(lastErr, ErrPredicatePanic) {
		t.Fatalf("err=%v want ErrPredicatePanic", lastErr)
	}
	if !strings.Contains(buf.String(), "polling_predicate_panic") {
		t.Fatalf("expected polling_predicate_panic event; got: %s", buf.String())
	}
}

func TestPoll_RetryAfterHonored(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := hits.Add(1)
		if n == 1 {
			w.Header().Set("Retry-After", "5")
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(srv.Close)

	sleeps, sleepOpt := captureSleeps()

	for resp, err := range Poll(t.Context(), srv.Client(), http.MethodGet, srv.URL, nil, nil,
		100*time.Millisecond,
		WithMaxAttempts(2),
		WithMaxWallClock(time.Hour),
		sleepOpt) {
		_ = err
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}
	if len(*sleeps) == 0 {
		t.Fatal("expected at least one captured sleep")
	}
	if (*sleeps)[0] != 5*time.Second {
		t.Fatalf("first sleep=%v want 5s", (*sleeps)[0])
	}
}

func TestPoll_RetryAfterNotApplied_WhenJitterSet(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := hits.Add(1)
		if n == 1 {
			w.Header().Set("Retry-After", "5")
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(srv.Close)

	sleeps, sleepOpt := captureSleeps()

	for resp := range Poll(t.Context(), srv.Client(), http.MethodGet, srv.URL, nil, nil,
		100*time.Millisecond,
		WithMaxAttempts(2),
		WithMaxWallClock(time.Hour),
		WithJitter(0.5),
		sleepOpt) {
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}
	if (*sleeps)[0] != 5*time.Second {
		t.Fatalf("first sleep=%v want exactly 5s (no jitter when honoring Retry-After)", (*sleeps)[0])
	}
}

func TestPoll_RetryAfterSaturated_ClampedToInterval(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := hits.Add(1)
		if n == 1 {
			w.Header().Set("Retry-After", "10000000000")
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(srv.Close)

	sleeps, sleepOpt := captureSleeps()

	for resp := range Poll(t.Context(), srv.Client(), http.MethodGet, srv.URL, nil, nil,
		100*time.Millisecond, WithMaxAttempts(2), sleepOpt) {
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}
	if (*sleeps)[0] != 100*time.Millisecond {
		t.Fatalf("first sleep=%v want 100ms (saturated upstream clamped)", (*sleeps)[0])
	}
}

func TestPoll_RetryAfterNegative_TreatedAsAbsent(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := hits.Add(1)
		if n == 1 {
			w.Header().Set("Retry-After", "-1")
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(srv.Close)

	sleeps, sleepOpt := captureSleeps()

	for resp := range Poll(t.Context(), srv.Client(), http.MethodGet, srv.URL, nil, nil,
		100*time.Millisecond, WithMaxAttempts(2), sleepOpt) {
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}
	if (*sleeps)[0] != 100*time.Millisecond {
		t.Fatalf("first sleep=%v want 100ms", (*sleeps)[0])
	}
}

func TestPoll_HeadersClonedPerRequest(t *testing.T) {
	var (
		mu   sync.Mutex
		seen []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("X-Probe"))
		mu.Unlock()
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(srv.Close)

	headers := http.Header{"X-Probe": []string{"orig"}}
	_, sleepOpt := captureSleeps()

	for resp := range Poll(t.Context(), srv.Client(), http.MethodGet, srv.URL, headers, nil,
		time.Millisecond, WithMaxAttempts(3), sleepOpt) {
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}

	// Mutate caller's slice; the iterator's already-issued requests
	// were independent. This is the parity-with-pages assertion.
	headers["X-Probe"][0] = "MUTATED"

	mu.Lock()
	defer mu.Unlock()
	for i, v := range seen {
		if v != "orig" {
			t.Fatalf("attempt %d saw %q; clone failed", i, v)
		}
	}
}

func TestPoll_BodyConstructedPerAttempt(t *testing.T) {
	var (
		mu     sync.Mutex
		bodies []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(srv.Close)

	_, sleepOpt := captureSleeps()
	for resp := range Poll(t.Context(), srv.Client(), http.MethodPost, srv.URL, nil, []byte("payload"),
		time.Millisecond, WithMaxAttempts(3), sleepOpt) {
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 3 {
		t.Fatalf("len(bodies)=%d want 3", len(bodies))
	}
	for i, b := range bodies {
		if b != "payload" {
			t.Fatalf("attempt %d body=%q want payload", i, b)
		}
	}
}

type myStruct struct {
	Status string `json:"status"`
}

func TestAs_DecodeJSON_DefaultPath(t *testing.T) {
	srv, _ := sequencedJSONServer(t,
		`{"status":"a"}`, `{"status":"b"}`, `{"status":"c"}`)
	_, sleepOpt := captureSleeps()

	var got []string
	for v, err := range As[*myStruct](t.Context(), srv.Client(), http.MethodGet, srv.URL, nil, nil,
		time.Millisecond, WithMaxAttempts(3), sleepOpt) {
		if err != nil && !errors.Is(err, ErrMaxAttemptsExceeded) {
			t.Fatalf("err=%v", err)
		}
		if v != nil {
			got = append(got, v.Status)
		}
	}
	want := []string{"a", "b", "c"}
	if len(got) < 3 {
		t.Fatalf("got=%v want at least %v", got, want)
	}
	for i := range 3 {
		if got[i] != want[i] {
			t.Fatalf("got[%d]=%q want %q", i, got[i], want[i])
		}
	}
}

func TestAs_DonePredicateOnDecodedValue(t *testing.T) {
	srv, hits := sequencedJSONServer(t,
		`{"status":"in_progress"}`, `{"status":"in_progress"}`, `{"status":"completed"}`)
	_, sleepOpt := captureSleeps()

	var n int
	var last *myStruct
	for v, err := range As[*myStruct](t.Context(), srv.Client(), http.MethodGet, srv.URL, nil, nil,
		time.Millisecond,
		WithDoneT(func(s *myStruct) bool { return s.Status == "completed" }),
		sleepOpt) {
		if err != nil {
			t.Fatal(err)
		}
		n++
		last = v
	}
	if n != 3 || last == nil || last.Status != "completed" {
		t.Fatalf("n=%d last=%+v", n, last)
	}
	if got := hits.Load(); got != 3 {
		t.Fatalf("hits=%d want 3", got)
	}
}

func TestAs_DecodeError_YieldsOnceAndStops(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{not json`)
	}))
	t.Cleanup(srv.Close)

	_, sleepOpt := captureSleeps()
	var n int
	var lastErr error
	for _, err := range As[*myStruct](t.Context(), srv.Client(), http.MethodGet, srv.URL, nil, nil,
		time.Millisecond, WithMaxAttempts(3), sleepOpt) {
		n++
		lastErr = err
	}
	if n != 1 {
		t.Fatalf("yields=%d want 1", n)
	}
	if lastErr == nil {
		t.Fatal("expected decode error")
	}
}

func TestPoll_InvalidInterval_ReturnsErrorImmediately(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(srv.Close)

	var n int
	var lastErr error
	for resp, err := range Poll(t.Context(), srv.Client(), http.MethodGet, srv.URL, nil, nil, 0) {
		n++
		lastErr = err
		_ = resp
	}
	if n != 1 || !errors.Is(lastErr, ErrInvalidInterval) {
		t.Fatalf("n=%d err=%v want 1 yield with ErrInvalidInterval", n, lastErr)
	}
	if hits.Load() != 0 {
		t.Fatalf("server should not have been hit; got %d", hits.Load())
	}
}

func TestPoll_WithMaxAttempts_ZeroOrNegative_Errors(t *testing.T) {
	srv, _ := sequencedJSONServer(t, `{}`)
	cases := []int{0, -1, -100}
	for _, n := range cases {
		var lastErr error
		var yields int
		for _, err := range Poll(t.Context(), srv.Client(), http.MethodGet, srv.URL, nil, nil,
			time.Millisecond, WithMaxAttempts(n)) {
			yields++
			lastErr = err
		}
		if yields != 1 || !errors.Is(lastErr, ErrInvalidOption) {
			t.Fatalf("n=%d yields=%d err=%v want 1/ErrInvalidOption", n, yields, lastErr)
		}
	}
}

func TestPoll_NilSleepFunc_ErrInvalidOption(t *testing.T) {
	srv, _ := sequencedJSONServer(t, `{}`)
	var n int
	var lastErr error
	for _, err := range Poll(t.Context(), srv.Client(), http.MethodGet, srv.URL, nil, nil,
		time.Millisecond, WithSleepFunc(nil)) {
		n++
		lastErr = err
	}
	if n != 1 || !errors.Is(lastErr, ErrInvalidOption) {
		t.Fatalf("n=%d err=%v", n, lastErr)
	}
}

func TestPoll_TransportErrorYieldsOnceAndStops(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	url := srv.URL
	srv.Close()

	_, sleepOpt := captureSleeps()
	var n int
	var lastErr error
	for resp, err := range Poll(t.Context(), &http.Client{}, http.MethodGet, url, nil, nil,
		time.Millisecond, WithMaxAttempts(3), sleepOpt) {
		n++
		if err != nil {
			lastErr = err
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
	}
	if n != 1 {
		t.Fatalf("yields=%d want 1", n)
	}
	if lastErr == nil {
		t.Fatal("expected transport error")
	}
}

func TestPoll_DoubleIteration_FreshEachCall(t *testing.T) {
	srv, hits := sequencedJSONServer(t, `{}`)
	_, sleepOpt := captureSleeps()

	for run := 1; run <= 2; run++ {
		_ = run
		for resp := range Poll(t.Context(), srv.Client(), http.MethodGet, srv.URL, nil, nil,
			time.Millisecond, WithMaxAttempts(2), sleepOpt) {
			if resp != nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}
		}
	}
	if got := hits.Load(); got != 4 {
		t.Fatalf("hits=%d want 4 (2 attempts x 2 calls)", got)
	}
}

func TestPoll_WithDoneT_OnPoll_Errors(t *testing.T) {
	srv, _ := sequencedJSONServer(t, `{}`)
	var lastErr error
	for _, err := range Poll(t.Context(), srv.Client(), http.MethodGet, srv.URL, nil, nil,
		time.Millisecond, WithDoneT(func(*myStruct) bool { return true })) {
		lastErr = err
	}
	if !errors.Is(lastErr, ErrInvalidOption) {
		t.Fatalf("err=%v want ErrInvalidOption", lastErr)
	}
}

func TestAs_WithDone_Errors(t *testing.T) {
	srv, _ := sequencedJSONServer(t, `{}`)
	var lastErr error
	for _, err := range As[*myStruct](t.Context(), srv.Client(), http.MethodGet, srv.URL, nil, nil,
		time.Millisecond, WithDone(func(*http.Response) bool { return true })) {
		lastErr = err
	}
	if !errors.Is(lastErr, ErrInvalidOption) {
		t.Fatalf("err=%v want ErrInvalidOption", lastErr)
	}
}

func TestAs_DoneTPredicatePanics_RecoveredAndStops(t *testing.T) {
	srv, _ := sequencedJSONServer(t,
		`{"status":"a"}`, `{"status":"b"}`, `{"status":"c"}`)
	_, sleepOpt := captureSleeps()

	var buf syncBuf
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	var attempts atomic.Int32
	doneT := func(*myStruct) bool {
		if attempts.Add(1) == 2 {
			panic("typed boom")
		}
		return false
	}

	var lastErr error
	for _, err := range As[*myStruct](t.Context(), srv.Client(), http.MethodGet, srv.URL, nil, nil,
		time.Millisecond, WithDoneT(doneT), WithLogger(logger), sleepOpt) {
		if err != nil {
			lastErr = err
		}
	}
	if !errors.Is(lastErr, ErrPredicatePanic) {
		t.Fatalf("err=%v want ErrPredicatePanic", lastErr)
	}
	out := buf.String()
	if !strings.Contains(out, "polling_predicate_panic") {
		t.Fatalf("expected polling_predicate_panic event; got: %s", out)
	}
	if !strings.Contains(out, "panic_type=") {
		t.Fatalf("expected panic_type attribute on event; got: %s", out)
	}
}

// TestPoll_ChangeOnly_MaxWallClockExpiresDuringSkipSleep exercises the
// cache-hit-skip path: every server response is a cache hit, polling
// silently skips yields, and the wall-clock budget eventually fires
// during a skip-sleep (or at the next loop-top ctx check). Either path
// surfaces ErrMaxWallClockExceeded. Real-time bounded at 200ms,
// mirroring retry/retry_test.go:201-242 for parity.
// Pins yieldDoErr's wall-clock branch when the deadline fires inside
// c.Do rather than between requests.
func TestPoll_MaxWallClockExpiresDuringDo_RemappedToErrMaxWallClockExceeded(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			_, _ = io.WriteString(w, `{}`)
			return
		}
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	var lastErr error
	var lastResp *http.Response
	var n int
	for resp, err := range Poll(t.Context(), srv.Client(), http.MethodGet, srv.URL, nil, nil,
		10*time.Millisecond,
		WithMaxWallClock(150*time.Millisecond),
	) {
		n++
		if err != nil {
			lastErr = err
			lastResp = resp
			continue
		}
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}
	if !errors.Is(lastErr, ErrMaxWallClockExceeded) {
		t.Fatalf("err=%v want ErrMaxWallClockExceeded", lastErr)
	}
	if !errors.Is(lastErr, context.DeadlineExceeded) {
		t.Fatalf("err=%v should also wrap context.DeadlineExceeded", lastErr)
	}
	if lastResp == nil {
		t.Fatal("lastResp must be non-nil on the in-flight remap (carries the prior successful attempt)")
	}
}

func TestPoll_ChangeOnly_MaxWallClockExpiresDuringSkipSleep(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Ghkit-Cache", "hit")
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(srv.Close)

	var lastErr error
	for resp, err := range Poll(t.Context(), srv.Client(), http.MethodGet, srv.URL, nil, nil,
		20*time.Millisecond,
		WithChangeOnly(),
		WithMaxWallClock(50*time.Millisecond),
	) {
		if err != nil {
			lastErr = err
		}
		_ = resp
	}
	if !errors.Is(lastErr, ErrMaxWallClockExceeded) {
		t.Fatalf("err=%v want ErrMaxWallClockExceeded", lastErr)
	}
}

func TestAs_TypeMismatch_ErrorsAtFirstIteration(t *testing.T) {
	srv, _ := sequencedJSONServer(t, `{}`)
	type other struct {
		X int
	}
	var lastErr error
	for _, err := range As[*myStruct](t.Context(), srv.Client(), http.MethodGet, srv.URL, nil, nil,
		time.Millisecond, WithDoneT(func(*other) bool { return true })) {
		lastErr = err
	}
	if !errors.Is(lastErr, ErrInvalidOption) {
		t.Fatalf("err=%v want ErrInvalidOption", lastErr)
	}
}

func TestPoll_ChangeOnly_SkipsCacheHits(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := hits.Add(1)
		if n > 1 {
			w.Header().Set(cond.HeaderCacheStatus, "hit")
		}
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	}))
	t.Cleanup(srv.Close)

	_, sleepOpt := captureSleeps()
	var n int
	for resp, err := range Poll(t.Context(), srv.Client(), http.MethodGet, srv.URL, nil, nil,
		time.Millisecond, WithMaxAttempts(5), WithChangeOnly(), sleepOpt) {
		n++
		_ = err
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}
	// Attempt 1 yields normally; attempts 2-5 are cache hits and
	// silently skipped; attempt 5 also returns a cache-hit response so
	// the boundary is reached on the cache-hit path which yields
	// (nil, ErrMaxAttemptsExceeded). So we expect 2 yields total.
	if n != 2 {
		t.Fatalf("yields=%d want 2 (1 normal + 1 boundary)", n)
	}
	if got := hits.Load(); got != 5 {
		t.Fatalf("hits=%d want 5", got)
	}
}

// syncBuf is a goroutine-safe slog buffer.
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
