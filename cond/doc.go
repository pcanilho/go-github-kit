// Package cond surfaces the change-vs-unchanged signal that the etag
// transport already computes but currently erases before the response
// reaches the consumer. The etag layer sets HeaderCacheStatus on
// synth-200 (cache hit) and wire-200 (cache store); StatusOf maps it
// to the typed Status enum.
//
// Use Fetch[T] for one-shot conditional GETs that want both the
// decoded value and the cache status. Use polling.WithChangeOnly to
// silently skip iterations when the resource hasn't changed.
//
// When the etag layer is not in the transport chain (or a non-cache
// response is received), the header is absent and StatusOf returns
// Updated; consumers see "not from cache" semantics correctly.
//
// Body lifecycle: Fetch[T] owns the response body and closes it via
// defer regardless of decode success. Decode errors are wrapped as
// "cond: decode: %w" for consistency with pages.As, polling.As, and
// search.iterate.
package cond
