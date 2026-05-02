package retry

import (
	"net/http"
	"testing"
)

// FuzzParseRetryAfter pins the parser as total: never panics, every input
// classifies, durations stay non-negative. Seeds mix spec-compliant forms
// with adversarial inputs (overflow, ISO 8601, control bytes).
func FuzzParseRetryAfter(f *testing.F) {
	seeds := []string{
		"", "5", "0", "-3", "3600", "10000000000", "9223372036", "9223372037",
		" 5", "5 ", " 5 ", "5.0", "abc", "not a date",
		"2026-01-02T15:04:05Z", "2026-01-02T15:04:05+00:00",
		"Mon, 02 Jan 2026 15:04:05 GMT", "Mon, 02 Jan 2026 15:04:05 UTC",
		"Monday, 02-Jan-26 15:04:05 GMT", "Mon Jan  2 15:04:05 2026",
		"\x00\x01\x02", "ten seconds", "  ",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, header string) {
		resp := &http.Response{Header: make(http.Header), Body: http.NoBody}
		if header != "" {
			resp.Header.Set("Retry-After", header)
		}
		defer func() { _ = resp.Body.Close() }()

		dur, outcome := parseRetryAfter(resp)
		if dur < 0 {
			t.Fatalf("negative duration for %q: %v", header, dur)
		}
		switch outcome {
		case outcomeAbsent, outcomeUnparseable:
			if dur != 0 {
				t.Fatalf("outcome=%v should yield 0 for %q; got %v", outcome, header, dur)
			}
		case outcomeNumeric, outcomeDate:
		default:
			t.Fatalf("unknown outcome %v for %q", outcome, header)
		}

		switch sourceLabel(dur, outcome) {
		case "retry_after", "jitter", "malformed":
		default:
			t.Fatalf("unknown source label for (%v, %v) input %q", dur, outcome, header)
		}

		_ = previewRawHeader(header)
	})
}
