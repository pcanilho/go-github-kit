package ghtest

import (
	"net/http"
	"strings"

	"github.com/pcanilho/go-github-kit/etag"
)

// Write304IfMatch computes the expected ETag for body using the
// bored-engineer algorithm (which hashes the request's Authorization,
// Accept, and Cookie headers along with the body). If any tag in
// If-None-Match (split on commas, trimmed, and normalised to strip the
// W/ weak prefix and surrounding quotes) matches, it sets a quoted ETag
// response header per RFC 7232, writes 304 Not Modified with empty body,
// and returns true. Otherwise it writes nothing and returns false.
func Write304IfMatch(w http.ResponseWriter, r *http.Request, body []byte) bool {
	expected := etag.ComputeExpectedETag(r.Header, nil, body)
	inm := r.Header.Get("If-None-Match")
	if inm == "" {
		return false
	}
	expNorm := etag.NormaliseETag(expected)
	for _, tok := range strings.Split(inm, ",") {
		if etag.NormaliseETag(strings.TrimSpace(tok)) == expNorm {
			w.Header().Set("ETag", `"`+expected+`"`)
			w.WriteHeader(http.StatusNotModified)
			return true
		}
	}
	return false
}
