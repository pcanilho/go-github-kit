// Package etag implements GitHub's reverse-engineered ETag algorithm and a
// conditional-request HTTP transport that uses it.
//
// The algorithm was originally reverse-engineered by bored-engineer:
//
//	https://github.com/bored-engineer/github-conditional-http-transport
//	https://www.bored-engineer.com/posts/github-etag-algorithm/
//
// GitHub's server-side ETag hash includes the Authorization header, so a plain
// store-and-forward ETag cache falls over under rotating auth (GitHub App
// installation tokens refresh hourly; fine-grained PATs rotate on a schedule).
// The precompute trick reproduces that hash client-side at request time using
// the current Authorization header, so the cached body stays valid across
// rotations and 304 hit rates stay high.
//
// This package ships:
//
//   - ComputeExpectedETag, NormaliseETag, ParseVary: low-level helpers for
//     callers composing their own transport.
//   - Cache: a three-method interface (Get/Add/Remove) any backend can
//     implement. The default NewLRUCache is memory-bounded and in-process.
//   - NewTransport: an http.RoundTripper that does the hit/miss/304/write-
//     invalidation dance around any Cache implementation.
//
// Drift-detector tuning. The detector is calibrated for steady GitHub-App
// traffic. Internal thresholds (how often the transport probes after the
// cooldown elapses, and how many consecutive successful probes are needed
// to recover) are private and may change. With the current values, a
// transport handling fewer than roughly 100 cacheable requests after the
// cooldown window elapses will not complete recovery to precompute mode.
// This is a deliberate trade-off against probe-induced load on high-traffic
// deployments.
//
// Security invariant: no log line emitted from this package may include
// req.Header or resp.Header as a structured field. The Authorization header
// value is a live credential. Only specific scalar fields (lengths, status
// codes, path templates, event kinds) are ever logged.
package etag
