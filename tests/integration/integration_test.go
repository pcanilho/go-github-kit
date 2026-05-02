package integration_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ghkit "github.com/pcanilho/go-github-kit"
	"github.com/pcanilho/go-github-kit/cond"
	"github.com/pcanilho/go-github-kit/ghtest"
	"github.com/pcanilho/go-github-kit/polling"
)

// TestIntegration_PollingPlusEtag exercises polling.As[T] against an
// httptest.Server that emits stable ETags. The first attempt is a
// wire 200 (cond.HeaderCacheStatus=miss); subsequent attempts are
// synth-200 cache hits (cond.HeaderCacheStatus=hit) and the decoded
// value is identical across attempts.
func TestIntegration_PollingPlusEtag(t *testing.T) {
	body := []byte(`{"v":1}`)
	srv, _ := ghtest.ETagServer(t, body)

	hc, err := ghkit.HTTPClient(ghkit.WithETagCache())
	if err != nil {
		t.Fatal(err)
	}

	type payload struct {
		V int `json:"v"`
	}

	captured := []cond.Status{}
	var mu sync.Mutex
	probe := http.Header{"X-Probe": []string{"yes"}}

	_, sleepOpt := captureSleeps()

	var seen int
	for v, err := range polling.As[*payload](
		t.Context(), hc, http.MethodGet, srv.URL, probe, nil,
		time.Millisecond,
		polling.WithMaxAttempts(3),
		sleepOpt,
		// Inspect the underlying *http.Response status via WithDecode
		// chain: we capture the cache header by wrapping the default
		// JSON decode and reading the response header before close.
		polling.WithDecode(func(resp *http.Response) (*payload, error) {
			mu.Lock()
			captured = append(captured, cond.StatusOf(resp))
			mu.Unlock()
			var p payload
			err := json.NewDecoder(resp.Body).Decode(&p)
			return &p, err
		}),
	) {
		_ = v
		if err != nil && !errors.Is(err, polling.ErrMaxAttemptsExceeded) {
			t.Fatal(err)
		}
		if v != nil {
			seen++
		}
	}
	if seen != 3 {
		t.Fatalf("yielded=%d want 3", seen)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(captured) != 3 {
		t.Fatalf("captured=%v want 3 entries", captured)
	}
	if captured[0] != cond.Updated {
		t.Fatalf("attempt 1 status=%v want Updated", captured[0])
	}
	for i := 1; i < len(captured); i++ {
		if captured[i] != cond.Unchanged {
			t.Fatalf("attempt %d status=%v want Unchanged (cache hit)", i+1, captured[i])
		}
	}
}

// TestIntegration_PollingPlusCondPlusEtag pins polling.WithChangeOnly:
// the iterator silently skips ticks where the etag layer signals a
// cache hit. Five attempts, only one yields (the first wire 200).
func TestIntegration_PollingPlusCondPlusEtag(t *testing.T) {
	body := []byte(`{"v":1}`)
	srv, reqs := ghtest.ETagServer(t, body)

	hc, err := ghkit.HTTPClient(ghkit.WithETagCache())
	if err != nil {
		t.Fatal(err)
	}
	_, sleepOpt := captureSleeps()

	type payload struct {
		V int `json:"v"`
	}

	var n int
	for v, err := range polling.As[*payload](
		t.Context(), hc, http.MethodGet, srv.URL, nil, nil,
		time.Millisecond,
		polling.WithMaxAttempts(5),
		polling.WithChangeOnly(),
		sleepOpt,
	) {
		_ = v
		if err != nil && !errors.Is(err, polling.ErrMaxAttemptsExceeded) {
			t.Fatal(err)
		}
		if v != nil {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("yielded=%d want 1 (first wire 200; rest are cache hits skipped)", n)
	}
	if got := atomic.LoadInt64(reqs); got != 5 {
		t.Fatalf("server reqs=%d want 5 (each tick still hits wire for the 304 round-trip)", got)
	}
}

// TestIntegration_ConcurrentPollingSameResource runs two polling
// iterators concurrently against the same etag-cached resource.
// Verifies the etag layer's cache + the cond header signal are
// race-safe under contention.
func TestIntegration_ConcurrentPollingSameResource(t *testing.T) {
	body := []byte(`{"v":1}`)
	srv, _ := ghtest.ETagServer(t, body)

	hc, err := ghkit.HTTPClient(ghkit.WithETagCache())
	if err != nil {
		t.Fatal(err)
	}

	type payload struct {
		V int `json:"v"`
	}
	_, sleepOpt := captureSleeps()

	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for v, err := range polling.As[*payload](
				t.Context(), hc, http.MethodGet, srv.URL, nil, nil,
				time.Millisecond,
				polling.WithMaxAttempts(5),
				sleepOpt,
			) {
				_ = v
				_ = err
			}
		}()
	}
	wg.Wait()
}

// TestIntegration_ConcurrentCondFetch runs cond.Fetch from many
// goroutines against an etag-cached resource. Pins concurrency
// safety of the cond.StatusOf header read.
func TestIntegration_ConcurrentCondFetch(t *testing.T) {
	body := []byte(`{"v":1}`)
	srv, _ := ghtest.ETagServer(t, body)

	hc, err := ghkit.HTTPClient(ghkit.WithETagCache())
	if err != nil {
		t.Fatal(err)
	}
	type payload struct {
		V int `json:"v"`
	}
	decode := func(r io.Reader) (payload, error) {
		var p payload
		err := json.NewDecoder(r).Decode(&p)
		return p, err
	}

	var wg sync.WaitGroup
	for w := 0; w < 16; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
			_, _, err := cond.Fetch(t.Context(), hc, req, decode)
			if err != nil {
				t.Errorf("Fetch: %v", err)
			}
		}()
	}
	wg.Wait()
}

// captureSleeps returns a WithSleepFunc that captures durations.
// Same shape as the polling-private helper; duplicated here because
// the polling test helpers are unexported.
func captureSleeps() (*[]time.Duration, polling.Option) {
	var (
		mu     sync.Mutex
		sleeps []time.Duration
	)
	return &sleeps, polling.WithSleepFunc(func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		sleeps = append(sleeps, d)
	})
}
