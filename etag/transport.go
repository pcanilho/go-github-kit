// Package etag [transport.go]
//
// SECURITY INVARIANT: no log line emitted from this file may include
// req.Header or resp.Header as a structured field. The Authorization header
// value may be a live installation token. Leaking it via %+v or a careless
// logger.Info(..., "req", req) would be a credential disclosure. Log only
// specific scalar fields from the allowlist below. TestETag_LogHygiene
// enforces this.
//
// slog event allowlist (strict):
//   - "etag_event" (debug):           {kind, path_template, age_ms?, github_request_id?}
//   - "etag_mismatch" (warn):         {kind, path_template, status, body_len, vary_names, github_request_id}
//   - "etag_drift_detected" (warn):   {kind, recovered=false}
//   - "etag_drift_recovered" (info):  {kind, recovered=true}
//   - "drift_callback_panic" (error): {kind}
//
// github_request_id is the upstream X-GitHub-Request-Id response header: a
// public, opaque, GitHub-issued correlator. Not a credential. Safe to log.

package etag

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"
)

// Sentinel errors exported from NewTransport so callers can use errors.Is.
var (
	// ErrKeyScopeRequired is returned by NewTransport when a caller-supplied
	// Cache is used without a WithKeyScope option. Required to prevent
	// cross-tenant body leaks when multiple identities share one cache.
	ErrKeyScopeRequired = errors.New("etag: WithKeyScope is required when WithCache supplies a Cache; see package docs")

	// ErrDoubleWrap is returned when NewTransport is called with a base that
	// is already an *etag.Transport. Nesting the transport is never intended.
	ErrDoubleWrap = errors.New("etag: base is already an *etag.Transport; do not double-wrap")

	// ErrBaseTransportType is returned when NewTransport is called with a
	// base that is not nil and not an *http.Transport. The gzip invariant
	// requires DisableCompression=true on an *http.Transport, which we
	// cannot set on an arbitrary wrapper.
	ErrBaseTransportType = errors.New("etag: base transport must be *http.Transport so DisableCompression can be set")

	// ErrNilCache is returned when WithCache is called with a nil Cache. If
	// you want the default LRU, omit WithCache entirely.
	ErrNilCache = errors.New("etag: WithCache was called with nil; omit the option to use the default LRU")
)

// Transport is an http.RoundTripper that adds If-None-Match on cacheable
// GET/HEAD requests and replays the cached body as a synthesised 200 when
// the server answers with 304 Not Modified.
//
// Transport runs precompute-mode by default: the If-None-Match value is
// computed from the cached body and the CURRENT request headers, so cached
// entries stay useful across Authorization rotation (GitHub App installation
// tokens, rotating fine-grained PATs). If algorithm drift is detected (10
// precompute/server-ETag mismatches inside a 60-second window), the
// transport transparently switches to passive mode and sends the server's
// stored ETag verbatim. After a 1-hour cooldown, sampled probe-back
// requests retry precompute; consecutive successes restore precompute mode
// automatically. Passive mode never replays caller credentials in
// If-None-Match: only the server's previously issued opaque ETag is sent.
// Use WithDriftDetected for state-transition callbacks; use Stats() to
// read live state.
type Transport struct {
	base        http.RoundTripper
	cache       Cache
	scopeDigest string // hex(sha256(keyScope)), precomputed once
	maxBody     int
	logger      *slog.Logger

	// throttle holds one rate.Limiter per normalised path template for
	// mismatch-log rate limiting.
	throttle sync.Map // key: path_template (string) -> *rate.Limiter

	// Drift detector state; see drift.go. Mutex-guarded counter; atomics
	// for the lock-free hot-path read in buildIfNoneMatch.
	driftMu               sync.Mutex
	driftWindowStart      time.Time
	driftMismatches       int
	driftRecoverSuccesses int

	driftDegraded      atomic.Bool
	driftDegradedAt    atomic.Int64 // unix nanos; 0 when not degraded
	driftTotalMismatch atomic.Int64
	driftProbeCounter  atomic.Int64

	driftCallback func(DriftEvent)
}

// NewTransport returns a Transport wrapping base. When base is nil, a cloned
// http.DefaultTransport with DisableCompression=true is used. Returns an
// error when:
//
//   - base is a non-nil http.RoundTripper that is not an *http.Transport
//     (the library cannot set DisableCompression on an arbitrary wrapper).
//   - base is already an *Transport (double-wrap).
//   - a caller-supplied Cache was provided via WithCache without a
//     non-empty WithKeyScope (cross-tenant safety requirement).
func NewTransport(base http.RoundTripper, opts ...Option) (http.RoundTripper, error) {
	cfg := newConfig(opts)

	if cfg.callerCache && cfg.cache == nil {
		return nil, ErrNilCache
	}
	if cfg.callerCache && cfg.keyScope == "" {
		return nil, ErrKeyScopeRequired
	}
	if _, ok := base.(*Transport); ok {
		return nil, ErrDoubleWrap
	}

	resolvedBase, err := resolveBase(base)
	if err != nil {
		return nil, err
	}

	cache := cfg.cache
	if cache == nil {
		cache = NewLRUCache(defaultLRUSize)
	}
	if lru, ok := cache.(*lruCache); ok {
		lru.setByteCap(cfg.maxCacheCap)
	}

	// Hash so caller-supplied keyScope cannot inject the cache-key
	// delimiter and alias another tenant's entries.
	sum := sha256.Sum256([]byte(cfg.keyScope))
	scopeDigest := hex.EncodeToString(sum[:])

	return &Transport{
		base:             resolvedBase,
		cache:            cache,
		scopeDigest:      scopeDigest,
		maxBody:          cfg.maxBodyBytes,
		logger:           cfg.logger,
		driftWindowStart: time.Now(),
		driftCallback:    cfg.driftCallback,
	}, nil
}

// resolveBase clones the supplied *http.Transport (or http.DefaultTransport
// when nil) and forces DisableCompression=true so the hash domain matches
// the server's pre-compression body. Other concrete types are rejected.
func resolveBase(base http.RoundTripper) (http.RoundTripper, error) {
	if base == nil {
		t, ok := http.DefaultTransport.(*http.Transport)
		if !ok {
			return nil, fmt.Errorf("%w: http.DefaultTransport is %T", ErrBaseTransportType, http.DefaultTransport)
		}
		cloned := t.Clone()
		cloned.DisableCompression = true
		return cloned, nil
	}
	if t, ok := base.(*http.Transport); ok {
		cloned := t.Clone()
		cloned.DisableCompression = true
		return cloned, nil
	}
	return nil, fmt.Errorf("%w: got %T", ErrBaseTransportType, base)
}

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !cacheable(req) {
		return t.base.RoundTrip(req)
	}

	ctx := req.Context()
	key := cacheKey(req.URL, t.scopeDigest)
	entry, haveEntry, getErr := t.cache.Get(ctx, key)
	if getErr != nil {
		// Backend-side error (e.g. Redis network blip). Treat as a miss
		// and log; never fail the request on a cache-read failure.
		t.logEvent(ctx, "get_error", req.URL.Path, nil, nil)
		haveEntry = false
	}
	if haveEntry {
		t.logEvent(ctx, "hit", req.URL.Path, &entry, nil)
		// Clone the request before mutating headers: the http.RoundTripper
		// contract forbids modifying the caller's request, and wrappers above
		// (rate-limit, retry) may reuse the original.
		ifNoneMatch, probe := t.buildIfNoneMatch(req.Header, entry)
		reqCtx := ctx
		if probe {
			reqCtx = markProbe(reqCtx)
		}
		reqCopy := req.Clone(reqCtx)
		reqCopy.Header.Set("If-None-Match", ifNoneMatch)
		req = reqCopy
	} else {
		t.logEvent(ctx, "miss", req.URL.Path, nil, nil)
	}

	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}

	// 304 Not Modified: synthesise a 200 from the cached entry.
	if resp.StatusCode == http.StatusNotModified && haveEntry {
		// Probe-back recovery: a 304 in reply to a precomputed probe means
		// the algorithm matches again. Without this, stable-traffic
		// operators (mostly 304s) could never recover.
		if isProbe(req.Context()) {
			if evt, fire := t.recordSuccess(); fire {
				t.fireDriftEvent(ctx, evt)
			}
		}
		if resp.Body != nil {
			buf := make([]byte, 1<<8)
			_, _ = io.CopyBuffer(io.Discard, resp.Body, buf)
			_ = resp.Body.Close()
		}
		// Start from the LIVE 304 headers (fresh X-RateLimit-* values that
		// wrappers above depend on). Fill in missing headers from the cached
		// ones (Content-Type, Link, etc.). The 304 branch must NOT call
		// ParseVary: GitHub omits Vary on 304 responses.
		mergedHeaders := resp.Header.Clone()
		for k, v := range entry.Headers {
			if _, present := mergedHeaders[k]; !present {
				// slices.Clone: a caller appending to this header key with
				// spare cap could otherwise mutate the cached entry's array.
				mergedHeaders[k] = slices.Clone(v)
			}
		}
		synth := &http.Response{
			Status:        "200 OK",
			StatusCode:    http.StatusOK,
			Proto:         resp.Proto,
			ProtoMajor:    resp.ProtoMajor,
			ProtoMinor:    resp.ProtoMinor,
			Header:        mergedHeaders,
			Body:          io.NopCloser(bytes.NewReader(entry.Body)),
			ContentLength: int64(len(entry.Body)),
			Request:       resp.Request,
			TLS:           resp.TLS,
		}
		synth.Header.Set("Content-Length", strconv.Itoa(len(entry.Body)))
		return synth, nil
	}

	// 200 OK: bounded read, validate, maybe cache.
	if resp.StatusCode == http.StatusOK {
		body, oversize, rerr := readBounded(resp, t.maxBody)
		if rerr != nil {
			return nil, rerr
		}
		if oversize {
			t.logEvent(ctx, "bypass_oversize", req.URL.Path, nil, resp)
			return resp, nil
		}
		if !cacheableResponse(resp) {
			t.logEvent(ctx, "bypass_noncacheable", req.URL.Path, nil, resp)
			return resp, nil
		}

		serverETag := resp.Header.Get("ETag")
		if serverETag == "" {
			t.logEvent(ctx, "no_etag_header", req.URL.Path, nil, resp)
			return resp, nil
		}

		// Validation feeds the drift detector and the warn log. Storage
		// proceeds in both cases: passive mode needs the latest server
		// ETag to send, and precompute mode never reads entry.ETag.
		ok, _, _, skipped := t.validate(req, resp, body)
		if skipped {
			t.logEvent(ctx, "no_etag_header", req.URL.Path, nil, resp)
			return resp, nil
		}
		if ok {
			t.logEvent(ctx, "validated_ok", req.URL.Path, nil, resp)
			if evt, fire := t.recordSuccess(); fire {
				t.fireDriftEvent(ctx, evt)
			}
		} else {
			tmpl := normalisePath(req.URL.Path)
			t.warnMismatch(ctx, resp, body, tmpl)
			if evt, fire := t.recordMismatch(); fire {
				t.fireDriftEvent(ctx, evt)
			}
		}

		addErr := t.cache.Add(ctx, key, Entry{
			ETag:    serverETag,
			Body:    body,
			Headers: resp.Header.Clone(),
		})
		if addErr != nil {
			// Cache-store errors are observability-only: the HTTP request
			// already succeeded, the caller gets the 200 unchanged.
			t.logEvent(ctx, "store_error", req.URL.Path, nil, resp)
			//nolint:nilerr // intentional: Add failure does not fail the HTTP request
			return resp, nil
		}
		t.logEvent(ctx, "store", req.URL.Path, nil, resp)
		return resp, nil
	}

	// 404/410: the resource is gone. Evict the cached entry so we stop
	// computing If-None-Match values the server will ignore.
	if haveEntry && (resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone) {
		if rmErr := t.cache.Remove(ctx, key); rmErr != nil {
			t.logEvent(ctx, "remove_error", req.URL.Path, nil, resp)
		} else {
			t.logEvent(ctx, "invalidated_gone", req.URL.Path, nil, resp)
		}
	}

	return resp, nil
}

// buildIfNoneMatch returns the If-None-Match value to send on a cache-hit
// request along with a probe marker. In precompute mode the value is the
// recomputed hash; in degraded mode it's entry.ETag verbatim, except every
// driftProbeEveryN-th call after cooldown sends the recomputed value as a
// probe-back. The bool is true exactly when this call produced a probe so
// the 304 branch can credit recovery via recordSuccess.
func (t *Transport) buildIfNoneMatch(reqHeaders http.Header, entry Entry) (string, bool) {
	if t.driftDegraded.Load() {
		degradedAt := time.Unix(0, t.driftDegradedAt.Load())
		if time.Since(degradedAt) >= driftCooldown {
			if n := t.driftProbeCounter.Add(1); n%int64(driftProbeEveryN) == 0 {
				return `"` + ComputeExpectedETag(reqHeaders, ParseVary(entry.Headers), entry.Body) + `"`, true
			}
		}
		return entry.ETag, false
	}
	return `"` + ComputeExpectedETag(reqHeaders, ParseVary(entry.Headers), entry.Body) + `"`, false
}

// validate compares the precomputed hash against the server's ETag.
// skipped=true means the response lacked an ETag header (unvalidatable),
// not a mismatch.
func (t *Transport) validate(req *http.Request, resp *http.Response, body []byte) (ok bool, ours, theirs string, skipped bool) {
	serverETag := resp.Header.Get("ETag")
	if serverETag == "" {
		return false, "", "", true
	}
	ours = ComputeExpectedETag(req.Header, ParseVary(resp.Header), body)
	theirs = NormaliseETag(serverETag)
	return ours == theirs, ours, theirs, false
}

// warnMismatch emits a throttled warn line on algorithm mismatch. Strict
// field allowlist: no hash prefixes, no request-header values. github_request_id
// is always emitted (empty string distinguishes "GitHub did not return one"
// from "proxied response stripped it").
func (t *Transport) warnMismatch(ctx context.Context, resp *http.Response, body []byte, tmpl string) {
	if !t.allowLog(tmpl) {
		return
	}
	t.logger.WarnContext(ctx, "etag_mismatch",
		"kind", "mismatch",
		"path_template", tmpl,
		"status", resp.StatusCode,
		"body_len", len(body),
		"vary_names", ParseVary(resp.Header),
		"github_request_id", resp.Header.Get("X-GitHub-Request-Id"),
	)
}

// logEvent emits a debug event. path is the raw URL path, which we normalise
// before putting in the log. entry is optional (only "hit" reads it for
// age_ms). resp is optional (nil for pre-RoundTrip events: get_error, hit,
// miss); when present, github_request_id is included when non-empty.
func (t *Transport) logEvent(ctx context.Context, kind, path string, entry *Entry, resp *http.Response) {
	attrs := []any{
		"kind", kind,
		"path_template", normalisePath(path),
	}
	if kind == "hit" && entry != nil && !entry.StoredAt.IsZero() {
		attrs = append(attrs, "age_ms", time.Since(entry.StoredAt).Milliseconds())
	}
	if resp != nil {
		if id := resp.Header.Get("X-GitHub-Request-Id"); id != "" {
			attrs = append(attrs, "github_request_id", id)
		}
	}
	t.logger.DebugContext(ctx, "etag_event", attrs...)
}

// allowLog consults the per-template rate limiter (1/min, burst 3). The
// keyspace is bounded by normalisePath, so the throttle map is naturally
// capped at the path-template count.
func (t *Transport) allowLog(key string) bool {
	lim, ok := t.throttle.Load(key)
	if !ok {
		newLim := rate.NewLimiter(rate.Every(time.Minute), 3)
		lim, _ = t.throttle.LoadOrStore(key, newLim)
	}
	return lim.(*rate.Limiter).Allow()
}

// cacheKey is the URL plus the per-Transport scope digest. Fragments are
// stripped because they are never sent over the wire.
func cacheKey(u *url.URL, scopeDigest string) string {
	stripped := *u
	stripped.Fragment = ""
	return stripped.String() + "|" + scopeDigest
}

// readBounded reads up to maxBody+1 bytes. Returns (body, false, nil) on
// fit (resp.Body reassigned to a bytes.Reader); (_, true, nil) on
// oversize (resp.Body wrapped in oversizeBody). Initial allocation uses
// the Content-Length hint when credible, else bodyBufferFloor, so a
// missing or pathological hint can't force a 4 MiB allocation.
func readBounded(resp *http.Response, maxBody int) ([]byte, bool, error) {
	if resp.Body == nil {
		return nil, false, nil
	}
	if maxBody <= 0 {
		maxBody = defaultPerEntryCap
	}
	initialAlloc := int64(bodyBufferFloor)
	if hint := resp.ContentLength; hint > 0 && hint <= int64(maxBody) {
		initialAlloc = hint + 1
		if initialAlloc < bodyBufferFloor {
			initialAlloc = bodyBufferFloor
		}
	}
	buf := bytes.NewBuffer(make([]byte, 0, initialAlloc))
	_, err := io.CopyN(buf, resp.Body, int64(maxBody)+1)
	switch {
	case errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF):
		body := buf.Bytes()
		_ = resp.Body.Close()
		if int64(len(body)) > int64(maxBody) {
			resp.Body = io.NopCloser(bytes.NewReader(body))
			return nil, true, nil
		}
		resp.Body = io.NopCloser(bytes.NewReader(body))
		resp.ContentLength = int64(len(body))
		return body, false, nil
	case err == nil:
		// Oversize path: body exceeds our cap. Expose the full body via a
		// chained reader (buffered prefix + remaining wire bytes); see
		// oversizeBody for Close semantics.
		rest := resp.Body
		resp.Body = &oversizeBody{
			Reader: io.MultiReader(bytes.NewReader(buf.Bytes()), rest),
			inner:  rest,
		}
		return nil, true, nil
	default:
		_ = resp.Body.Close()
		return nil, false, err
	}
}

// oversizeBody wraps the response body for oversize-bypass: the caller
// reads past our buffered prefix; Close releases without draining (a
// slow upstream would otherwise pin Close for minutes).
type oversizeBody struct {
	io.Reader
	inner io.ReadCloser
}

func (o *oversizeBody) Close() error { return o.inner.Close() }
