package retry

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

// A hand-rolled base transport can return a bare &http.Response with no
// Body; WithBaseTransport allows that.
type nilBodyTransport struct{ calls int }

func (t *nilBodyTransport) RoundTrip(*http.Request) (*http.Response, error) {
	t.calls++
	if t.calls == 1 {
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{}}, nil
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}}, nil
}

// Draining a nil Body used to panic.
func TestRetry_NilResponseBodyDoesNotPanic(t *testing.T) {
	base := &nilBodyTransport{}
	rt, err := NewTransport(base,
		WithMaxAttempts(3),
		WithBackoff(time.Millisecond, 2*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("NewTransport: %v", err)
	}

	req, reqErr := http.NewRequest(http.MethodGet, "https://example.invalid/x", nil)
	if reqErr != nil {
		t.Fatalf("NewRequest: %v", reqErr)
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200 after one retry", resp.StatusCode)
	}
	if base.calls != 2 {
		t.Fatalf("base called %d times; want 2", base.calls)
	}
}

// The Retry-After abort path drains too.
func TestRetry_NilResponseBodyOnRetryAfterAbort(t *testing.T) {
	base := retryAfterNilBody{}
	rt, err := NewTransport(base,
		WithMaxAttempts(3),
		WithBackoff(time.Millisecond, time.Second),
	)
	if err != nil {
		t.Fatalf("NewTransport: %v", err)
	}

	req, reqErr := http.NewRequest(http.MethodGet, "https://example.invalid/x", nil)
	if reqErr != nil {
		t.Fatalf("NewRequest: %v", reqErr)
	}
	if _, err := rt.RoundTrip(req); !errors.Is(err, ErrRetryAfterExceedsMax) {
		t.Fatalf("got %v; want ErrRetryAfterExceedsMax", err)
	}
}

type retryAfterNilBody struct{}

func (retryAfterNilBody) RoundTrip(*http.Request) (*http.Response, error) {
	h := http.Header{}
	h.Set("Retry-After", "3600")
	return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: h}, nil
}
