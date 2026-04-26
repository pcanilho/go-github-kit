package ghtest_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pcanilho/go-github-kit/ghtest"
)

func TestWriteSecondaryLimit_basic(t *testing.T) {
	rec := httptest.NewRecorder()
	ghtest.WriteSecondaryLimit(rec, 60*time.Second)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if got := rec.Header().Get("Retry-After"); got != "60" {
		t.Errorf("Retry-After = %q, want %q", got, "60")
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want %q", got, "application/json")
	}
	body := rec.Body.String()
	if !strings.Contains(body, "secondary rate limit") {
		t.Errorf("body missing 'secondary rate limit': %q", body)
	}
	if !strings.Contains(body, "#secondary-rate-limits") {
		t.Errorf("body missing #secondary-rate-limits docs URL: %q", body)
	}
}

func TestWriteSecondaryLimit_zeroDuration(t *testing.T) {
	rec := httptest.NewRecorder()
	ghtest.WriteSecondaryLimit(rec, 0)
	if got := rec.Header().Get("Retry-After"); got != "0" {
		t.Errorf("Retry-After = %q, want %q", got, "0")
	}
}

func TestWriteSecondaryLimit_negativeClampsToZero(t *testing.T) {
	rec := httptest.NewRecorder()
	ghtest.WriteSecondaryLimit(rec, -30*time.Second)
	if got := rec.Header().Get("Retry-After"); got != "0" {
		t.Errorf("Retry-After = %q, want %q", got, "0")
	}
}
