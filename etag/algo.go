package etag

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"net/http"
	"regexp"
	"slices"
	"strings"
)

// varyHeaders is the canonical list of response-Vary header names GitHub
// participates in. Iteration order is part of the algorithm contract; do not
// alphabetize or reorder. Unexported to prevent mutation of in-process state;
// callers who need the list call VaryHeaders().
var varyHeaders = []string{"Accept", "Authorization", "Cookie"}

// VaryHeaders returns an immutable copy of the canonical Vary header list.
// Mutating the returned slice does not affect internal state.
func VaryHeaders() []string {
	return slices.Clone(varyHeaders)
}

// Hash returns a SHA256 hasher pre-loaded with the VaryHeaders values in
// declaration order, each suffixed with ':'. Callers Write the raw
// (pre-compression) response body and then Sum to obtain the ETag bytes.
//
// The vary argument is a FILTER: when non-nil, a header is only mixed into
// the hash when vary contains its name. When vary is nil, all three canonical
// headers are used. This matches GitHub's server-side behaviour: the server
// hashes over a fixed header set regardless of what it advertises in Vary.
func Hash(reqHeaders http.Header, vary []string) hash.Hash {
	h := sha256.New()
	for _, name := range varyHeaders {
		if vary != nil && !slices.Contains(vary, name) {
			continue
		}
		for _, v := range reqHeaders.Values(name) {
			_, _ = h.Write([]byte(v))
			_, _ = h.Write([]byte{':'})
		}
	}
	return h
}

// ComputeExpectedETag is the single source of truth for the client-side
// hash computation.
func ComputeExpectedETag(reqHeaders http.Header, respVary []string, body []byte) string {
	h := Hash(reqHeaders, respVary)
	_, _ = h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// NormaliseETag strips the W/ weak-marker prefix and surrounding quotes so
// two ETag strings can be byte-compared. Callers building If-None-Match
// values construct the strong-quoted form from the raw hex hash; this
// function is for comparisons only.
func NormaliseETag(e string) string {
	e = strings.TrimSpace(e)
	e = strings.TrimPrefix(e, "W/")
	e = strings.Trim(e, `"`)
	return e
}

// ParseVary MUST only be called on 200-response headers. RFC 7232 allows
// servers to omit Vary on 304 responses (GitHub does), so calling ParseVary
// on a 304 would silently fall back to the canonical list and lose any
// endpoint-specific Vary the original 200 carried. The transport's 304
// branch does NOT call this function.
//
// Server order is preserved; we do not sort. The server hashes in its
// iteration order, and any client-side reordering diverges.
func ParseVary(h http.Header) []string {
	raw := h.Values("Vary")
	if len(raw) == 0 {
		return nil
	}
	var out []string
	for _, val := range raw {
		for f := range strings.SplitSeq(val, ",") {
			f = strings.TrimSpace(f)
			if f == "" || f == "*" {
				continue
			}
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// cacheable returns true for requests whose responses carry an ETag we can
// revalidate against. The request-side checks run before we issue any call;
// response-side checks (cacheableResponse) run after we read the response.
func cacheable(req *http.Request) bool {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		return false
	}
	if req.Header.Get("Range") != "" {
		return false
	}
	if req.URL.Path == "/rate_limit" || req.URL.Path == "/api/v3/rate_limit" {
		return false
	}
	return true
}

// cacheableResponse applies RFC 9111 alignment: responses with
// Cache-Control: no-store or Vary: * are never cached.
func cacheableResponse(resp *http.Response) bool {
	if cc := resp.Header.Get("Cache-Control"); cc != "" {
		for part := range strings.SplitSeq(cc, ",") {
			if strings.EqualFold(strings.TrimSpace(part), "no-store") {
				return false
			}
		}
	}
	for _, v := range resp.Header.Values("Vary") {
		for f := range strings.SplitSeq(v, ",") {
			if strings.TrimSpace(f) == "*" {
				return false
			}
		}
	}
	return true
}

// pathTemplates normalises request paths for low-cardinality slog fields.
// Longest-match-first; unmatched paths fall through normalisePath's
// generic collapser.
var pathTemplates = []struct {
	re   *regexp.Regexp
	tmpl string
}{
	{regexp.MustCompile(`^/repos/[^/]+/[^/]+/commits/[0-9a-f]{7,40}/check-runs$`), "/repos/{o}/{r}/commits/{sha}/check-runs"},
	{regexp.MustCompile(`^/repos/[^/]+/[^/]+/commits/[0-9a-f]{7,40}/statuses$`), "/repos/{o}/{r}/commits/{sha}/statuses"},
	{regexp.MustCompile(`^/repos/[^/]+/[^/]+/commits/[0-9a-f]{7,40}$`), "/repos/{o}/{r}/commits/{sha}"},
	{regexp.MustCompile(`^/repos/[^/]+/[^/]+/compare/[^/]+\.\.\.[^/]+$`), "/repos/{o}/{r}/compare/{base...head}"},
	{regexp.MustCompile(`^/repos/[^/]+/[^/]+/branches$`), "/repos/{o}/{r}/branches"},
	{regexp.MustCompile(`^/repos/[^/]+/[^/]+/pulls/\d+$`), "/repos/{o}/{r}/pulls/{n}"},
	{regexp.MustCompile(`^/repos/[^/]+/[^/]+/issues/\d+$`), "/repos/{o}/{r}/issues/{n}"},
	{regexp.MustCompile(`^/repos/[^/]+/[^/]+/check-runs/\d+$`), "/repos/{o}/{r}/check-runs/{id}"},
	{regexp.MustCompile(`^/repos/[^/]+/[^/]+/deployments/\d+/statuses$`), "/repos/{o}/{r}/deployments/{id}/statuses"},
	{regexp.MustCompile(`^/repos/[^/]+/[^/]+/deployments/\d+$`), "/repos/{o}/{r}/deployments/{id}"},
	{regexp.MustCompile(`^/repos/[^/]+/[^/]+/deployments$`), "/repos/{o}/{r}/deployments"},
	{regexp.MustCompile(`^/repos/[^/]+/[^/]+$`), "/repos/{o}/{r}"},
	{regexp.MustCompile(`^/users/[^/]+$`), "/users/{u}"},
	{regexp.MustCompile(`^/orgs/[^/]+$`), "/orgs/{o}"},
	{regexp.MustCompile(`^/app/installations/\d+$`), "/app/installations/{id}"},
	{regexp.MustCompile(`^/meta$`), "/meta"},
}

// normalisePath returns a bounded-cardinality label for the URL path.
// Unmapped paths collapse to /<first-segment>/_, then "unknown".
func normalisePath(p string) string {
	for _, r := range pathTemplates {
		if r.re.MatchString(p) {
			return r.tmpl
		}
	}
	if len(p) > 1 && p[0] == '/' {
		if rest, _, ok := strings.Cut(p[1:], "/"); ok {
			return "/" + rest + "/_"
		}
	}
	return "unknown"
}
