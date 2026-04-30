package ghtest

import (
	"fmt"
	"net/http"
	"strconv"
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
