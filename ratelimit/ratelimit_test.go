package ratelimit

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofri/go-github-ratelimit/v2/github_ratelimit/github_primary_ratelimit"
)

// syncBuf is a concurrent-safe bytes.Buffer (the rate-limiter logs from its
// handler goroutine while the test goroutine reads).
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}
func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// rlServer serves N 403 primary-rate-limit responses, then 200. This lets us
// observe the primary-limit callback firing.
func rlPrimaryServer(t *testing.T, limitResponses int) *httptest.Server {
	t.Helper()
	var seen atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if int(seen.Add(1)) <= limitResponses {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", "1")
			w.Header().Set("X-RateLimit-Resource", "core")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"message":"API rate limit exceeded"}`)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(s.Close)
	return s
}

func TestRL_DefaultLogFieldsSafe(t *testing.T) {
	s := rlPrimaryServer(t, 1)

	buf := &syncBuf{}
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	rt := NewTransport(http.DefaultTransport,
		WithLogger(logger),
		// Cap sleep so the request returns quickly in the test even if the
		// limiter would otherwise wait for the simulated reset time.
		WithTotalSleepLimit(100*time.Millisecond),
	)
	c := &http.Client{Transport: rt, Timeout: 5 * time.Second}

	r, err := c.Get(s.URL)
	if err == nil {
		_ = r.Body.Close()
	}

	out := buf.String()
	if !strings.Contains(out, "ratelimit_primary_detected") {
		t.Fatalf("expected default handler to emit ratelimit_primary_detected; got %q", out)
	}
	if !strings.Contains(out, "kind=primary") {
		t.Errorf("expected kind=primary scalar field; got %q", out)
	}
	// Banned fields: anything that could reveal token-adjacent material or
	// carry a raw CallbackContext field into the log.
	banned := []string{
		"Request=", "Response=", "ResetTime=", "RoundTripper=",
		"Authorization", "token",
	}
	for _, b := range banned {
		if strings.Contains(out, b) {
			t.Errorf("banned field %q appeared in default log: %q", b, out)
		}
	}
}

func TestRL_PrimaryCallbackFires(t *testing.T) {
	s := rlPrimaryServer(t, 1)

	var fired atomic.Int32
	rt := NewTransport(http.DefaultTransport,
		WithPrimaryLimitDetected(func(ev *PrimaryEvent) {
			fired.Add(1)
		}),
		WithTotalSleepLimit(100*time.Millisecond),
	)
	c := &http.Client{Transport: rt, Timeout: 5 * time.Second}

	// First call hits the 403; the limiter may or may not retry. We assert
	// only that the callback fired at least once.
	r, err := c.Get(s.URL)
	if err != nil {
		t.Logf("get returned err (expected when sleep-limit is tight): %v", err)
	} else {
		_ = r.Body.Close()
	}
	if fired.Load() == 0 {
		t.Fatal("primary limit callback did not fire on 403 response")
	}
}

func TestRL_CustomLoggerReceivesEvents(t *testing.T) {
	s := rlPrimaryServer(t, 1)

	buf := &syncBuf{}
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	rt := NewTransport(http.DefaultTransport,
		WithLogger(logger),
		WithTotalSleepLimit(100*time.Millisecond),
	)
	c := &http.Client{Transport: rt, Timeout: 5 * time.Second}

	r, err := c.Get(s.URL)
	if err == nil {
		_ = r.Body.Close()
	}

	if !strings.Contains(buf.String(), "ratelimit_primary_detected") {
		t.Fatalf("expected default-logger primary event; got %q", buf.String())
	}
}

func TestRL_NewTransportWithoutBase(t *testing.T) {
	rt := NewTransport(nil)
	if rt == nil {
		t.Fatal("NewTransport(nil) must not panic; upstream accepts nil base")
	}
}

func TestRL_WithUpstreamOptionsPassesThrough(t *testing.T) {
	s := rlPrimaryServer(t, 1)

	var fired atomic.Int32
	rt := NewTransport(http.DefaultTransport,
		WithTotalSleepLimit(100*time.Millisecond),
		WithUpstreamOptions(
			github_primary_ratelimit.WithLimitDetectedCallback(func(*PrimaryEvent) {
				fired.Add(1)
			}),
		),
	)
	c := &http.Client{Transport: rt, Timeout: 5 * time.Second}
	if r, err := c.Get(s.URL); err == nil {
		_ = r.Body.Close()
	}
	if fired.Load() == 0 {
		t.Fatal("upstream callback installed via WithUpstreamOptions did not fire")
	}
}

func TestRL_WithUpstreamOptionsAppendsAcrossCalls(t *testing.T) {
	cfg := &config{}
	WithUpstreamOptions(1, 2).apply(cfg)
	WithUpstreamOptions(3).apply(cfg)
	if got := cfg.upstream; len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 3 {
		t.Fatalf("expected [1 2 3]; got %v", got)
	}
}
