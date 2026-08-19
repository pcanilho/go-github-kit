package polling

import (
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func jsonServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func collectSleeps(t *testing.T, attempts int, opts ...Option) []time.Duration {
	t.Helper()
	srv := jsonServer(t)
	sleeps, sleepOpt := captureSleeps()
	for resp := range Poll(t.Context(), srv.Client(), http.MethodGet, srv.URL, nil, nil,
		100*time.Millisecond,
		append(append([]Option{WithMaxAttempts(attempts)}, opts...), sleepOpt)...) {
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}
	return *sleeps
}

// WithJitter stays deterministic.
func TestPoll_WithJitterStaysDeterministic(t *testing.T) {
	got := collectSleeps(t, 6, WithJitter(0.5))
	if len(got) == 0 {
		t.Fatal("no sleeps recorded")
	}
	const want = 125 * time.Millisecond // 100ms + (100ms * 0.5)/2
	for i, d := range got {
		if d != want {
			t.Fatalf("sleep[%d] = %v; want %v", i, d, want)
		}
	}
}

func TestPoll_WithFullJitterStaysInBounds(t *testing.T) {
	const interval = 100 * time.Millisecond
	got := collectSleeps(t, 12, WithFullJitter(0.5))
	if len(got) == 0 {
		t.Fatal("no sleeps recorded")
	}
	lo := interval - (interval/2)/2 // interval - span/2, span = 50ms
	hi := interval + (interval/2)/2
	for i, d := range got {
		if d < lo || d > hi {
			t.Fatalf("sleep[%d] = %v; outside [%v, %v]", i, d, lo, hi)
		}
	}
}

// Successive intervals must differ, or pollers stay in lockstep.
func TestPoll_WithFullJitterVaries(t *testing.T) {
	got := collectSleeps(t, 12, WithFullJitter(1))
	if len(got) < 2 {
		t.Fatalf("need at least 2 sleeps, got %d", len(got))
	}
	allSame := true
	for _, d := range got[1:] {
		if d != got[0] {
			allSame = false
			break
		}
	}
	if allSame {
		t.Fatalf("all %d sleeps identical (%v); full jitter is not varying", len(got), got[0])
	}
}

// frac=1 spans the documented range and must not escalate.
func TestPoll_WithFullJitterDoesNotEscalate(t *testing.T) {
	const interval = 100 * time.Millisecond
	got := collectSleeps(t, 20, WithFullJitter(1))
	for i, d := range got {
		if d < interval/2 || d > 3*interval/2 {
			t.Fatalf("sleep[%d] = %v; outside [%v, %v]", i, d, interval/2, 3*interval/2)
		}
	}
}

// Last-one-wins has to hold in both directions.
func TestPoll_JitterOptionsLastOneWins(t *testing.T) {
	t.Run("WithJitter after WithFullJitter is deterministic", func(t *testing.T) {
		got := collectSleeps(t, 8, WithFullJitter(0.5), WithJitter(0.5))
		if len(got) == 0 {
			t.Fatal("no sleeps recorded")
		}
		const want = 125 * time.Millisecond
		for i, d := range got {
			if d != want {
				t.Fatalf("sleep[%d] = %v; want deterministic %v", i, d, want)
			}
		}
	})

	t.Run("WithFullJitter after WithJitter varies", func(t *testing.T) {
		got := collectSleeps(t, 12, WithJitter(1), WithFullJitter(1))
		if len(got) < 2 {
			t.Fatalf("need at least 2 sleeps, got %d", len(got))
		}
		allSame := true
		for _, d := range got[1:] {
			if d != got[0] {
				allSame = false
				break
			}
		}
		if allSame {
			t.Fatalf("all sleeps identical (%v); WithFullJitter did not win", got[0])
		}
	})
}

// float64(interval)*frac can round past MaxInt64. Must not panic.
func TestPoll_FullJitterPathologicalInterval(t *testing.T) {
	cfg := &config{jitter: 1, fullJitter: true}
	got := nextSleep(cfg, nil, time.Duration(math.MaxInt64))
	if got <= 0 {
		t.Fatalf("nextSleep = %v; want a positive duration", got)
	}
}
