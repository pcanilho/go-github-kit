// Package etag [drift.go] implements transparent ETag drift detection and
// fallback. Precompute mode is the default; if the client-side hash diverges
// from the server-issued ETag on driftThreshold validations inside
// driftWindow, the Transport silently switches to passive mode (sends the
// server's stored ETag verbatim). After driftCooldown, sampled probe-back
// requests retry precompute; consecutive successes restore the precompute
// path. State transitions are observable via WithDriftDetected and the
// read-only Stats() snapshot.

package etag

import (
	"context"
	"time"
)

// ctxKeyProbe marks a request whose If-None-Match was a probe-back
// precomputed value. The 304 branch reads it to credit recovery: a 304 in
// reply to a probe means the algorithm matches again. Without this marker,
// stable-traffic operators (mostly 304s) could never recover.
type ctxKeyProbe struct{}

func markProbe(ctx context.Context) context.Context {
	return context.WithValue(ctx, ctxKeyProbe{}, true)
}

func isProbe(ctx context.Context) bool {
	v, _ := ctx.Value(ctxKeyProbe{}).(bool)
	return v
}

// Detector thresholds are intentionally unexported: behaviour-degradation
// knobs are not API surface and tend to leak into compatibility commitments.
const (
	driftWindow        = 60 * time.Second
	driftThreshold     = 10        // mismatches inside driftWindow trips degraded
	driftCooldown      = time.Hour // wait after trip before probing
	driftProbeEveryN   = 50        // 1-in-N sampled probes while degraded
	driftRecoverAfterN = 3         // consecutive probe successes before clearing
)

// DriftEvent fires on each drift state transition. Recovered=false on
// detection; Recovered=true on probe-back recovery. Drift state is
// per-Transport: if you build multiple *Transport instances in one process,
// the callback fires per Transport. Read Stats() for the current truth at
// any time.
//
// DetectedAt is the time of the transition. For Recovered=false events this
// is when drift was first observed; for Recovered=true events this is when
// recovery was confirmed.
type DriftEvent struct {
	DetectedAt time.Time
	Recovered  bool
}

// Stats is the read-only snapshot returned by (*Transport).Stats. Suitable
// for /healthz probes, Prometheus gauges, or polling dashboards.
type Stats struct {
	Degraded        bool
	DegradedAt      time.Time // zero when not degraded
	TotalMismatches int64     // monotonic over Transport lifetime
}

// Stats returns a snapshot of the Transport's drift detector state. Safe to
// call from any goroutine.
//
// Holds driftMu for the {Degraded, DegradedAt} pair so the snapshot is
// internally consistent under concurrent transitions. The hot path
// (buildIfNoneMatch) intentionally stays lock-free; it can briefly see a
// transient state and at worst sends one extra probe.
func (t *Transport) Stats() Stats {
	t.driftMu.Lock()
	degraded := t.driftDegraded.Load()
	at := t.driftDegradedAt.Load()
	t.driftMu.Unlock()
	s := Stats{
		Degraded:        degraded,
		TotalMismatches: t.driftTotalMismatch.Load(),
	}
	if degraded && at != 0 {
		s.DegradedAt = time.Unix(0, at)
	}
	return s
}

// recordMismatch advances the window counter and may trip the degraded
// latch. The {degraded, degradedAt} pair is updated under driftMu so a
// concurrent Stats reader cannot observe a {Degraded=true, DegradedAt=0}
// transient.
func (t *Transport) recordMismatch() {
	now := time.Now()
	t.driftTotalMismatch.Add(1)

	t.driftMu.Lock()
	if now.Sub(t.driftWindowStart) >= driftWindow {
		t.driftWindowStart = now
		t.driftMismatches = 0
	}
	t.driftMismatches++
	t.driftRecoverSuccesses = 0
	shouldTrip := t.driftMismatches >= driftThreshold && !t.driftDegraded.Load()
	if shouldTrip {
		t.driftDegradedAt.Store(now.UnixNano())
		t.driftProbeCounter.Store(0)
		t.driftDegraded.Store(true)
	}
	t.driftMu.Unlock()

	if shouldTrip {
		t.fireDriftEvent(DriftEvent{DetectedAt: now, Recovered: false})
	}
}

// recordSuccess credits a validated 200 or a probe-induced 304 toward
// recovery. No-op while not degraded; one atomic load on the hot path.
func (t *Transport) recordSuccess() {
	if !t.driftDegraded.Load() {
		return
	}
	now := time.Now()
	t.driftMu.Lock()
	t.driftRecoverSuccesses++
	shouldRecover := t.driftRecoverSuccesses >= driftRecoverAfterN && t.driftDegraded.Load()
	if shouldRecover {
		// Reset the window so a stale mismatch tail cannot re-trip.
		t.driftMismatches = 0
		t.driftWindowStart = now
		t.driftRecoverSuccesses = 0
		t.driftDegraded.Store(false)
		t.driftDegradedAt.Store(0)
	}
	t.driftMu.Unlock()

	if shouldRecover {
		t.fireDriftEvent(DriftEvent{DetectedAt: now, Recovered: true})
	}
}

// fireDriftEvent emits the slog signal and invokes the user callback under
// a recover guard. Detection is Warn; recovery is Info.
func (t *Transport) fireDriftEvent(evt DriftEvent) {
	if evt.Recovered {
		t.logger.Info("etag_drift_recovered",
			"kind", "etag_drift_recovered",
			"recovered", true,
		)
	} else {
		t.logger.Warn("etag_drift_detected",
			"kind", "etag_drift_detected",
			"recovered", false,
		)
	}
	if t.driftCallback == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			t.logger.Error("drift_callback_panic",
				"kind", "drift_callback_panic",
			)
		}
	}()
	t.driftCallback(evt)
}
