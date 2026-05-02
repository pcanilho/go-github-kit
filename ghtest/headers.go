package ghtest

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// WriteSecondaryLimit writes a 403 with a Retry-After header (whole
// seconds) and a JSON body whose documentation_url ends in
// #secondary-rate-limits. That suffix is what go-github pattern-matches on
// to classify the error as an AbuseRateLimitError, so the consumer's retry
// path actually triggers in tests. Negative durations are clamped to zero.
func WriteSecondaryLimit(w http.ResponseWriter, retryAfter time.Duration) {
	secs := max(int(retryAfter.Round(time.Second).Seconds()), 0)
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_, _ = fmt.Fprint(w, `{"message":"You have exceeded a secondary rate limit.","documentation_url":"https://docs.github.com/rest/overview/resources-in-the-rest-api#secondary-rate-limits"}`)
}

// LinkHeader builds an RFC 8288 Link header value for pagination
// fixtures. baseURL is the request URL without the page or per_page
// query string (e.g. "https://api.example/items"); page and perPage
// are 1-indexed; lastPage is the final page number.
//
// First page omits prev and first; last page omits next and last.
// Returns "" when lastPage <= 1, the convention go-github sees on a
// single-page response.
func LinkHeader(baseURL string, page, perPage, lastPage int) string {
	if lastPage <= 1 {
		return ""
	}
	parts := make([]string, 0, 4)
	add := func(p int, rel string) {
		parts = append(parts, fmt.Sprintf(`<%s?page=%d&per_page=%d>; rel=%q`, baseURL, p, perPage, rel))
	}
	if page > 1 {
		add(page-1, "prev")
	}
	if page < lastPage {
		add(page+1, "next")
		add(lastPage, "last")
	}
	if page > 1 {
		add(1, "first")
	}
	return strings.Join(parts, ", ")
}
