package throttle

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func countingServer(t *testing.T) (*httptest.Server, *int64) {
	t.Helper()
	var n int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&n, 1)
		w.WriteHeader(200)
	}))
	t.Cleanup(s.Close)
	return s, &n
}

func TestThrottle_RPSCapObserved(t *testing.T) {
	s, _ := countingServer(t)
	rt, err := NewTransport(http.DefaultTransport, 5) // rps=5, burst=1
	if err != nil {
		t.Fatal(err)
	}
	c := &http.Client{Transport: rt}

	start := time.Now()
	for range 6 {
		r, err := c.Get(s.URL)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if err := r.Body.Close(); err != nil {
			t.Fatalf("Body.Close: %v", err)
		}
	}
	// 6 requests at 5 rps with burst=1 should take at least ~1s (the first
	// request uses the initial token; the remaining 5 are paced at 200ms).
	elapsed := time.Since(start)
	if elapsed < 900*time.Millisecond {
		t.Fatalf("expected rps cap to throttle the burst; elapsed %v", elapsed)
	}
}

func TestThrottle_ContextCancelAborts(t *testing.T) {
	s, count := countingServer(t)
	// Very slow rate: 1 req per 10s. After the first burst token is used,
	// the second request should block for 10s.
	rt, err := NewTransport(http.DefaultTransport, 0.1)
	if err != nil {
		t.Fatal(err)
	}
	c := &http.Client{Transport: rt}

	// Exhaust the burst token.
	r, err := c.Get(s.URL)
	if err != nil {
		t.Fatalf("first get: %v", err)
	}
	if err := r.Body.Close(); err != nil {
		t.Fatalf("Body.Close: %v", err)
	}

	// Second request: cancel fast.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", s.URL, nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}
	start := time.Now()
	resp, err := c.Do(req)
	elapsed := time.Since(start)
	if err == nil {
		// Defensive: Do returned nil err only if the request somehow got
		// through. Close the body in that case to avoid leaks.
		if resp != nil {
			_ = resp.Body.Close()
		}
		t.Fatal("expected ctx cancellation error")
	}
	// When Do returns an error, resp is typically nil. If it isn't,
	// we're obligated to close the body anyway.
	if resp != nil {
		_ = resp.Body.Close()
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("ctx cancel should abort quickly; took %v", elapsed)
	}
	// The cancelled request must NOT have reached the server.
	if atomic.LoadInt64(count) != 1 {
		t.Fatalf("cancelled request hit the server; count=%d", atomic.LoadInt64(count))
	}
}

func TestThrottle_BurstHonoured(t *testing.T) {
	s, _ := countingServer(t)
	rt, err := NewTransport(http.DefaultTransport, 1, WithBurst(5)) // 1 rps, burst 5
	if err != nil {
		t.Fatal(err)
	}
	c := &http.Client{Transport: rt}

	start := time.Now()
	for range 5 {
		r, err := c.Get(s.URL)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if err := r.Body.Close(); err != nil {
			t.Fatalf("Body.Close: %v", err)
		}
	}
	// 5 requests within the initial burst should complete fast.
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("burst=5 should admit 5 immediate requests; took %v", elapsed)
	}
}

func TestThrottle_NilBaseUsesDefault(t *testing.T) {
	rt, err := NewTransport(nil, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if rt == nil {
		t.Fatal("NewTransport returned nil")
	}
}

func TestThrottle_InvalidRateReturnsError(t *testing.T) {
	cases := []struct {
		name  string
		rps   float64
		burst int
	}{
		{"zero-rps", 0, 1},
		{"negative-rps", -1, 1},
		{"zero-burst", 1, 0},
		{"negative-burst", 1, -1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var opts []Option
			if c.burst != 1 {
				opts = append(opts, WithBurst(c.burst))
			}
			rt, err := NewTransport(nil, c.rps, opts...)
			if !errors.Is(err, ErrInvalidRate) {
				t.Fatalf("want ErrInvalidRate for rps=%v burst=%d; got rt=%v err=%v", c.rps, c.burst, rt, err)
			}
			if rt != nil {
				t.Fatalf("expected nil transport on invalid rate; got %v", rt)
			}
		})
	}
}
