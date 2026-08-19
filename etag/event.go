package etag

import (
	"net/url"
	"time"
)

// Kind discriminates Event values. Bare names match the slog "kind"
// attribute on etag_event records 1:1; drift kinds drop the "etag_"
// prefix that the etag_drift_* slog kind attributes carry.
type Kind string

const (
	KindGetError        Kind = "get_error"
	KindHit             Kind = "hit"
	KindMiss            Kind = "miss"
	KindBypassOversize  Kind = "bypass_oversize"
	KindBypassNoncache  Kind = "bypass_noncacheable"
	KindBypassEmptyBody Kind = "bypass_empty_body"
	KindNoEtagHeader    Kind = "no_etag_header"
	KindValidatedOK     Kind = "validated_ok"
	KindMismatch        Kind = "mismatch"
	KindStoreError      Kind = "store_error"
	KindStore           Kind = "store"
	KindRemoveError     Kind = "remove_error"
	KindInvalidatedGone Kind = "invalidated_gone"
	KindDriftDetected   Kind = "drift_detected"
	KindDriftRecovered  Kind = "drift_recovered"
)

// Event is the per-call payload delivered to a WithEventCallback handler.
// Fields are populated when meaningful for the Kind; zero values are valid
// otherwise. URL is the caller's pointer (captured before the transport's
// internal request clone); the callback must not mutate it.
//
// KindHit fires at cache lookup, before the wire 304 round-trip; the
// served-from-cache outcome is signalled to the caller via
// cond.HeaderCacheStatus on the synth response. A single tripping
// mismatch fires KindMismatch then KindDriftDetected. With the pages
// package, the callback fires once per page in a paginated traversal.
//
// Panics in the callback are not recovered; they propagate through
// RoundTrip.
type Event struct {
	Kind            Kind
	URL             *url.URL      // nil on KindDriftDetected, KindDriftRecovered
	PathTemplate    string        // normalised path; see algo.go
	Status          int           // 0 when no resp
	BodyLen         int           // KindStore only
	Age             time.Duration // KindHit only
	Err             error         // KindGetError, KindStoreError, KindRemoveError
	GitHubRequestID string        // X-GitHub-Request-Id; empty when no resp
	DriftEvent      DriftEvent    // KindDriftDetected, KindDriftRecovered
}
