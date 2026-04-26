package etag

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// tripMismatch / tripSuccess drive the drift state machine and fire the
// resulting transition event, matching how RoundTrip wires the two helpers.
func tripMismatch(tr *Transport) {
	if evt, fire := tr.recordMismatch(); fire {
		tr.fireDriftEvent(context.Background(), evt)
	}
}

func tripSuccess(tr *Transport) {
	if evt, fire := tr.recordSuccess(); fire {
		tr.fireDriftEvent(context.Background(), evt)
	}
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", raw, err)
	}
	return u
}

// newDriftTransport returns the concrete *Transport so unit tests can call
// unexported drift helpers and inspect Stats().
func newDriftTransport(t *testing.T, opts ...Option) *Transport {
	t.Helper()
	rt, err := NewTransport(nil, opts...)
	if err != nil {
		t.Fatalf("NewTransport: %v", err)
	}
	tr, ok := rt.(*Transport)
	if !ok {
		t.Fatalf("NewTransport returned %T, want *Transport", rt)
	}
	return tr
}

func TestDrift_BelowThresholdDoesNotTrip(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tr := newDriftTransport(t)
		for range driftThreshold - 1 {
			tripMismatch(tr)
		}
		if tr.Stats().Degraded {
			t.Fatalf("degraded after %d mismatches; threshold is %d", driftThreshold-1, driftThreshold)
		}
	})
}

func TestDrift_ThresholdTripsAndFiresCallbackOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls int
		var lastEvt DriftEvent
		tr := newDriftTransport(t, WithDriftDetected(func(e DriftEvent) {
			calls++
			lastEvt = e
		}))

		for range driftThreshold {
			tripMismatch(tr)
		}
		if !tr.Stats().Degraded {
			t.Fatalf("not degraded after %d mismatches", driftThreshold)
		}
		if calls != 1 {
			t.Fatalf("callback fired %d times, want 1", calls)
		}
		if lastEvt.Recovered {
			t.Fatalf("first event should be Recovered=false")
		}

		// Further mismatches must not re-fire the callback while degraded.
		for range 100 {
			tripMismatch(tr)
		}
		if calls != 1 {
			t.Fatalf("callback fired %d times after 100 extra mismatches; want 1", calls)
		}
	})
}

func TestDrift_WindowRolloverResetsCount(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tr := newDriftTransport(t)
		for range driftThreshold - 1 {
			tripMismatch(tr)
		}
		// Past-window: counter resets, so a single fresh mismatch must not
		// trip the latch.
		time.Sleep(driftWindow + time.Second)
		tripMismatch(tr)
		if tr.Stats().Degraded {
			t.Fatalf("degraded after window rollover; counter did not reset")
		}
	})
}

func TestDrift_StatsTotalMismatchesMonotonic(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tr := newDriftTransport(t)
		const n = 25
		for range n {
			tripMismatch(tr)
		}
		if got := tr.Stats().TotalMismatches; got != n {
			t.Fatalf("TotalMismatches = %d, want %d", got, n)
		}
	})
}

func TestDrift_RecoversAfterProbesPostCooldown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var events []DriftEvent
		tr := newDriftTransport(t, WithDriftDetected(func(e DriftEvent) {
			events = append(events, e)
		}))

		for range driftThreshold {
			tripMismatch(tr)
		}
		if !tr.Stats().Degraded {
			t.Fatalf("expected degraded after threshold")
		}

		// recordSuccess advances the recovery counter directly. Drive enough
		// successes to clear the latch.
		time.Sleep(driftCooldown + time.Second)
		for range driftRecoverAfterN {
			tripSuccess(tr)
		}
		if tr.Stats().Degraded {
			t.Fatalf("expected recovery after %d probe successes", driftRecoverAfterN)
		}
		if len(events) != 2 {
			t.Fatalf("expected 2 events (degraded + recovered); got %d", len(events))
		}
		if !events[1].Recovered {
			t.Fatalf("second event should be Recovered=true; got %+v", events[1])
		}
	})
}

func TestDrift_MismatchResetsRecoveryCounter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tr := newDriftTransport(t)
		for range driftThreshold {
			tripMismatch(tr)
		}

		time.Sleep(driftCooldown + time.Second)
		// One short of recovery.
		for range driftRecoverAfterN - 1 {
			tripSuccess(tr)
		}
		// A fresh mismatch must reset the recovery counter so the next
		// success does not clear the latch.
		tripMismatch(tr)
		tripSuccess(tr)
		if !tr.Stats().Degraded {
			t.Fatalf("recovery cleared after mismatch reset; expected still degraded")
		}
	})
}

func TestDrift_BuildIfNoneMatch_PassiveWhileDegraded(t *testing.T) {
	tr := newDriftTransport(t)
	// Force the degraded state directly; synctest unnecessary because we are
	// not measuring time.
	tr.driftDegraded.Store(true)
	tr.driftDegradedAt.Store(time.Now().UnixNano())

	hdrs := http.Header{}
	entry := Entry{ETag: `"server-stored-tag"`, Body: []byte(`{}`), Headers: http.Header{}}
	got, probe := tr.buildIfNoneMatch(hdrs, entry)
	if got != entry.ETag {
		t.Fatalf("passive mode should send entry.ETag verbatim; got %q want %q", got, entry.ETag)
	}
	if probe {
		t.Fatalf("passive (in-cooldown) call must not be marked as a probe")
	}
}

func TestDrift_BuildIfNoneMatch_PrecomputeWhenNotDegraded(t *testing.T) {
	tr := newDriftTransport(t)
	body := []byte(`{"hello":"world"}`)
	entry := Entry{ETag: `"server-stored-tag"`, Body: body, Headers: http.Header{}}
	got, probe := tr.buildIfNoneMatch(http.Header{}, entry)
	if got == entry.ETag {
		t.Fatalf("precompute mode should NOT echo entry.ETag verbatim")
	}
	if got[0] != '"' || got[len(got)-1] != '"' {
		t.Fatalf("precompute value must be strong-quoted; got %q", got)
	}
	if probe {
		t.Fatalf("precompute in non-degraded mode must not be marked as a probe")
	}
}

func TestDrift_BuildIfNoneMatch_ProbeAfterCooldown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tr := newDriftTransport(t)
		tr.driftDegraded.Store(true)
		tr.driftDegradedAt.Store(time.Now().UnixNano())

		entry := Entry{ETag: `"server-stored-tag"`, Body: []byte(`{}`), Headers: http.Header{}}

		// Within cooldown: every call returns passive ETag, never precomputed.
		for range driftProbeEveryN * 2 {
			got, probe := tr.buildIfNoneMatch(http.Header{}, entry)
			if got != entry.ETag {
				t.Fatalf("within cooldown, expected passive %q, got %q", entry.ETag, got)
			}
			if probe {
				t.Fatalf("within cooldown, no call should be marked as a probe")
			}
		}

		// After cooldown: every Nth call returns precomputed AND is marked.
		time.Sleep(driftCooldown + time.Second)
		var precomputeCount, passiveCount, probeCount int
		for range driftProbeEveryN * 4 {
			got, probe := tr.buildIfNoneMatch(http.Header{}, entry)
			if got == entry.ETag {
				passiveCount++
				if probe {
					t.Fatalf("passive value must not be marked as a probe")
				}
			} else {
				precomputeCount++
				if !probe {
					t.Fatalf("precomputed probe-back call must be marked as a probe")
				}
				probeCount++
			}
		}
		if precomputeCount == 0 {
			t.Fatalf("expected probe-back precomputes after cooldown; got none in %d calls", driftProbeEveryN*4)
		}
		if passiveCount == 0 {
			t.Fatalf("probe-back should be sampled, not unconditional; got 0 passives")
		}
		if probeCount != precomputeCount {
			t.Fatalf("probe-marker count (%d) must equal precompute count (%d)", probeCount, precomputeCount)
		}
	})
}

func TestDrift_ProbeInduced304CountsTowardRecovery(t *testing.T) {
	// Server: always returns 304 to any If-None-Match (simulating a fully
	// stable cached body). Probe-induced 304s must advance the recovery
	// counter so stable-traffic operators can recover.
	body := []byte(`{}`)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != "" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"`+ComputeExpectedETag(r.Header, nil, body)+`"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	defer s.Close()

	c := newTestClient(t)
	tr := c.Transport.(*Transport)

	// Seed the cache with a successful 200.
	_ = doGet(t, c, s.URL+"/repos/a/b").StatusCode
	if got := tr.Stats().Degraded; got {
		t.Fatalf("transport unexpectedly degraded after seed")
	}

	// Force degraded with the cooldown already past so every call is a
	// candidate for a probe.
	tr.driftDegraded.Store(true)
	tr.driftDegradedAt.Store(time.Now().Add(-2 * driftCooldown).UnixNano())

	// Drive enough requests to land driftRecoverAfterN probes (each is 1 in
	// driftProbeEveryN). Allow generous headroom.
	for range driftProbeEveryN*driftRecoverAfterN + driftProbeEveryN {
		_ = doGet(t, c, s.URL+"/repos/a/b").StatusCode
		if !tr.Stats().Degraded {
			break // recovered
		}
	}
	if tr.Stats().Degraded {
		t.Fatalf("expected recovery via probe-induced 304s; still degraded after driving requests")
	}
}

func TestDrift_MismatchUnderDegradedStillCachesEntry(t *testing.T) {
	// Drift is sustained: server's ETag never matches our precompute. A
	// freshly issued server ETag must still land in the cache so the next
	// passive-mode If-None-Match has a chance to 304.
	body := []byte(`{"x":1}`)
	var seq atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Each call invents a fresh garbage ETag.
		etag := `"garbage-` + strconv.FormatInt(seq.Add(1), 10) + `"`
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	defer s.Close()

	c := newTestClient(t)
	tr := c.Transport.(*Transport)

	// First request: cache cold, server returns garbage ETag, validation
	// fails. Cache must still take the entry.
	_ = doGet(t, c, s.URL+"/repos/a/b").StatusCode
	cached, ok, err := tr.cache.Get(t.Context(), cacheKey(mustParseURL(t, s.URL+"/repos/a/b"), tr.scopeDigest))
	if err != nil {
		t.Fatalf("cache.Get: %v", err)
	}
	if !ok {
		t.Fatalf("cache miss after first 200 with mismatched ETag; expected entry to be stored regardless of validation")
	}
	if cached.ETag == "" {
		t.Fatalf("cached entry has empty ETag")
	}
}

func TestDrift_CallbackPanicDoesNotKillTransport(t *testing.T) {
	tr := newDriftTransport(t, WithDriftDetected(func(e DriftEvent) {
		panic("boom")
	}))
	for range driftThreshold {
		tripMismatch(tr)
	}
	// If recover guard works, this point is reached. Stats remain consistent.
	if !tr.Stats().Degraded {
		t.Fatalf("expected degraded after threshold")
	}
	// Subsequent calls must still work.
	tripMismatch(tr)
	if got := tr.Stats().TotalMismatches; got <= driftThreshold {
		t.Fatalf("TotalMismatches did not advance after panic: %d", got)
	}
}

func TestDrift_ConcurrentRecordMismatchRaceFree(t *testing.T) {
	// -race exercises the mutex/atomic boundaries; without -race this test
	// still validates that TotalMismatches matches the concurrent input
	// exactly (no lost updates).
	tr := newDriftTransport(t)
	const goroutines, perG = 32, 200
	var wg sync.WaitGroup
	for range goroutines {
		wg.Go(func() {
			for range perG {
				tripMismatch(tr)
			}
		})
	}
	wg.Wait()
	if got, want := tr.Stats().TotalMismatches, int64(goroutines*perG); got != want {
		t.Fatalf("TotalMismatches = %d, want %d", got, want)
	}
}

// TestDrift_RecoveryClearsMismatchCounter ensures a stale tail of mismatches
// from the pre-recovery window cannot re-trip immediately after recovery.
func TestDrift_RecoveryClearsMismatchCounter(t *testing.T) {
	tr := newDriftTransport(t)

	for range driftThreshold - 1 {
		tripMismatch(tr)
	}
	tr.driftDegraded.Store(true)
	tr.driftDegradedAt.Store(time.Now().Add(-2 * driftCooldown).UnixNano())
	for range driftRecoverAfterN {
		tripSuccess(tr)
	}
	if tr.Stats().Degraded {
		t.Fatalf("expected recovery after %d successes", driftRecoverAfterN)
	}

	// Single fresh mismatch must not re-trip: the pre-recovery 9 mismatches
	// must have been cleared by recovery.
	tripMismatch(tr)
	if tr.Stats().Degraded {
		t.Fatalf("re-tripped after one mismatch post-recovery; pre-recovery counter not cleared")
	}
}

// TestDrift_MultiCycleFiresEachTransition verifies the callback fires once
// per *each* drift -> recovered -> drift cycle, not once per process
// lifetime.
func TestDrift_MultiCycleFiresEachTransition(t *testing.T) {
	var detected, recovered int
	tr := newDriftTransport(t, WithDriftDetected(func(e DriftEvent) {
		if e.Recovered {
			recovered++
		} else {
			detected++
		}
	}))

	cycle := func() {
		for range driftThreshold {
			tripMismatch(tr)
		}
		// Force cooldown past so successes count immediately.
		tr.driftDegradedAt.Store(time.Now().Add(-2 * driftCooldown).UnixNano())
		for range driftRecoverAfterN {
			tripSuccess(tr)
		}
	}

	cycle()
	cycle()

	if detected != 2 {
		t.Fatalf("detected fired %d times across 2 cycles, want 2", detected)
	}
	if recovered != 2 {
		t.Fatalf("recovered fired %d times across 2 cycles, want 2", recovered)
	}
}

// TestDrift_StatsConsistentDuringTransitions drives recordMismatch /
// recordSuccess transitions on one goroutine while another polls Stats(),
// asserting no caller observes an inconsistent {Degraded, DegradedAt}
// snapshot.
func TestDrift_StatsConsistentDuringTransitions(t *testing.T) {
	tr := newDriftTransport(t)

	const cycles = 100
	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Mutator: trip and recover repeatedly.
	wg.Go(func() {
		for range cycles {
			for range driftThreshold {
				tripMismatch(tr)
			}
			// Backdate to bypass the cooldown gate immediately.
			tr.driftDegradedAt.Store(time.Now().Add(-2 * driftCooldown).UnixNano())
			for range driftRecoverAfterN {
				tripSuccess(tr)
			}
		}
		close(stop)
	})

	// Reader: hammer Stats() and assert self-consistency on every snapshot.
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			s := tr.Stats()
			if s.Degraded && s.DegradedAt.IsZero() {
				t.Errorf("inconsistent stats: Degraded=true with zero DegradedAt")
				return
			}
			if !s.Degraded && !s.DegradedAt.IsZero() {
				t.Errorf("inconsistent stats: Degraded=false with non-zero DegradedAt")
				return
			}
		}
	})

	wg.Wait()
}

// TestDrift_EndToEndFallback drives a real httptest server that returns a
// garbage ETag for the first burst of requests (simulating algorithm drift),
// then serves correct precompute ETags. Asserts the transport degrades, then
// recovers via probe-back.
func TestDrift_EndToEndFallback(t *testing.T) {
	body := []byte(`{"k":"v"}`)
	var requests atomic.Int64
	var sendGarbage atomic.Bool
	sendGarbage.Store(true)

	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		var etag string
		if sendGarbage.Load() {
			etag = `"garbage-not-our-hash"`
		} else {
			etag = `"` + ComputeExpectedETag(r.Header, nil, body) + `"`
		}
		w.Header().Set("ETag", etag)
		w.Header().Set("Content-Type", "application/json")
		// 304 path: server compares If-None-Match against the same value it
		// would issue. Under garbage-mode the stored client value never
		// matches; under good-mode the precompute matches and we 304.
		if got := r.Header.Get("If-None-Match"); got != "" && NormaliseETag(got) == NormaliseETag(etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	defer s.Close()

	var events []DriftEvent
	var mu sync.Mutex
	c := newTestClient(t, WithDriftDetected(func(e DriftEvent) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
	}))

	// Phase 1: drive enough garbage responses to trip the latch.
	for range driftThreshold + 5 {
		_ = doGet(t, c, s.URL+"/repos/a/b").StatusCode
	}

	tr := c.Transport.(*Transport)
	mu.Lock()
	got := len(events)
	firstEvt := events[0]
	mu.Unlock()
	if got < 1 || !firstEvt.DetectedAt.Before(time.Now().Add(time.Second)) || firstEvt.Recovered {
		t.Fatalf("expected one Recovered=false event; got %+v", events)
	}
	if !tr.Stats().Degraded {
		t.Fatalf("transport not degraded after %d garbage responses", driftThreshold+5)
	}

	// Phase 2: server is fixed, but cooldown is still in effect. Drive
	// requests; transport must stay degraded (no probes yet).
	sendGarbage.Store(false)
	for range driftRecoverAfterN * 2 {
		_ = doGet(t, c, s.URL+"/repos/a/b").StatusCode
	}
	if !tr.Stats().Degraded {
		t.Fatalf("transport recovered before cooldown elapsed")
	}

	// Phase 3: backdate degradedAt to bypass the 1-hour real-time cooldown.
	tr.driftDegradedAt.Store(time.Now().Add(-2 * driftCooldown).UnixNano())

	// Drive enough requests for at least driftRecoverAfterN probe successes
	// (1 in driftProbeEveryN). Cap iterations to avoid runaway on bug.
	for range driftProbeEveryN * (driftRecoverAfterN + 2) {
		_ = doGet(t, c, s.URL+"/repos/a/b").StatusCode
		if !tr.Stats().Degraded {
			break
		}
	}
	if tr.Stats().Degraded {
		t.Fatalf("transport did not recover after probes post-cooldown")
	}

	// Final state: at least one Recovered=true event, total >= 2 events.
	mu.Lock()
	defer mu.Unlock()
	if len(events) < 2 {
		t.Fatalf("expected >=2 events (degraded + recovered); got %d: %+v", len(events), events)
	}
	if !events[len(events)-1].Recovered {
		t.Fatalf("last event should be Recovered=true; got %+v", events[len(events)-1])
	}
}

// TestDrift_NonProbe304DoesNotCreditRecovery pins the isProbe gate in the
// 304 branch. If the gate ever drops, every steady-state 304 would credit
// recovery and degraded mode would clear in driftRecoverAfterN hits.
func TestDrift_NonProbe304DoesNotCreditRecovery(t *testing.T) {
	body := []byte(`{}`)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != "" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"`+ComputeExpectedETag(r.Header, nil, body)+`"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	defer s.Close()

	c := newTestClient(t)
	tr := c.Transport.(*Transport)

	// Seed the cache.
	_ = doGet(t, c, s.URL+"/repos/a/b").StatusCode

	// Force degraded with cooldown still in effect, so no probes fire.
	tr.driftDegraded.Store(true)
	tr.driftDegradedAt.Store(time.Now().UnixNano())

	// Drive enough non-probe 304s to clear recovery if the gate were broken.
	for range driftRecoverAfterN * 5 {
		_ = doGet(t, c, s.URL+"/repos/a/b").StatusCode
	}
	if !tr.Stats().Degraded {
		t.Fatalf("transport recovered from non-probe 304s; isProbe gate is broken")
	}
}

// TestDrift_404DoesNotAffectDriftCounters pins the 404/410 eviction
// branch's non-interaction with the drift detector.
func TestDrift_404DoesNotAffectDriftCounters(t *testing.T) {
	body := []byte(`{}`)
	var phase atomic.Int32 // 0 = serve 200; 1 = serve 404
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if phase.Load() == 1 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("ETag", `"`+ComputeExpectedETag(r.Header, nil, body)+`"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	defer s.Close()

	c := newTestClient(t)
	tr := c.Transport.(*Transport)

	// Seed cache, then flip to 404 mode.
	_ = doGet(t, c, s.URL+"/repos/a/b").StatusCode
	phase.Store(1)
	for range 10 {
		_ = doGet(t, c, s.URL+"/repos/a/b").StatusCode
	}
	if got := tr.Stats().TotalMismatches; got != 0 {
		t.Fatalf("404 responses recorded %d mismatches; want 0", got)
	}
}

// TestDrift_BypassPathsDoNotRecordMismatch pins that the early-return paths
// (oversize, !cacheableResponse, no-ETag header) do not feed the detector.
func TestDrift_BypassPathsDoNotRecordMismatch(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{
			name: "no_etag_header",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{}`))
			},
		},
		{
			name: "noncacheable_no_store",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("ETag", `"garbage"`)
				w.Header().Set("Cache-Control", "no-store")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{}`))
			},
		},
		{
			name: "noncacheable_vary_star",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("ETag", `"garbage"`)
				w.Header().Set("Vary", "*")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{}`))
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(tc.handler)
			defer s.Close()
			c := newTestClient(t)
			tr := c.Transport.(*Transport)
			for range driftThreshold * 2 {
				_ = doGet(t, c, s.URL+"/repos/a/b").StatusCode
			}
			if got := tr.Stats().TotalMismatches; got != 0 {
				t.Fatalf("%s: bypass path recorded %d mismatches; want 0", tc.name, got)
			}
		})
	}
}

// TestDrift_NaturalValidated200CreditsRecoveryPreCooldown pins the design
// intent: cooldown gates probe issuance, not the processing of natural
// validation evidence. A natural 200 in degraded mode whose precompute
// validates SHOULD credit recovery, even before driftCooldown elapses.
func TestDrift_NaturalValidated200CreditsRecoveryPreCooldown(t *testing.T) {
	body := []byte(`{"k":"v"}`)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		etag := `"` + ComputeExpectedETag(r.Header, nil, body) + `"`
		w.Header().Set("ETag", etag)
		if got := r.Header.Get("If-None-Match"); got != "" && NormaliseETag(got) == NormaliseETag(etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	defer s.Close()

	c := newTestClient(t)
	tr := c.Transport.(*Transport)

	// Force degraded with cooldown still in effect, no cached entry yet.
	tr.driftDegraded.Store(true)
	tr.driftDegradedAt.Store(time.Now().UnixNano())

	// Drive driftRecoverAfterN distinct URLs (each is a fresh natural 200
	// because there's no cached entry for that URL yet). Each validates
	// successfully; together they should recover the transport.
	for i := range driftRecoverAfterN {
		_ = doGet(t, c, s.URL+"/repos/a/b/"+strconv.Itoa(i)).StatusCode
	}
	if tr.Stats().Degraded {
		t.Fatal("natural validated 200s did not recover pre-cooldown; cooldown is over-gating")
	}
}
