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

func TestLinkHeader_First(t *testing.T) {
	got := ghtest.LinkHeader("https://api.example/items", 1, 30, 3)
	want := `<https://api.example/items?page=2&per_page=30>; rel="next", <https://api.example/items?page=3&per_page=30>; rel="last"`
	if got != want {
		t.Errorf("LinkHeader(first) =\n  %q\nwant\n  %q", got, want)
	}
	if strings.Contains(got, `rel="prev"`) {
		t.Errorf("first page must not include prev: %q", got)
	}
	if strings.Contains(got, `rel="first"`) {
		t.Errorf("first page must not include first: %q", got)
	}
}

func TestLinkHeader_Middle(t *testing.T) {
	got := ghtest.LinkHeader("https://api.example/items", 2, 30, 3)
	wantParts := []string{
		`<https://api.example/items?page=1&per_page=30>; rel="prev"`,
		`<https://api.example/items?page=3&per_page=30>; rel="next"`,
		`<https://api.example/items?page=3&per_page=30>; rel="last"`,
		`<https://api.example/items?page=1&per_page=30>; rel="first"`,
	}
	for _, p := range wantParts {
		if !strings.Contains(got, p) {
			t.Errorf("middle page header missing %q in %q", p, got)
		}
	}
}

func TestLinkHeader_Last(t *testing.T) {
	got := ghtest.LinkHeader("https://api.example/items", 3, 30, 3)
	want := `<https://api.example/items?page=2&per_page=30>; rel="prev", <https://api.example/items?page=1&per_page=30>; rel="first"`
	if got != want {
		t.Errorf("LinkHeader(last) =\n  %q\nwant\n  %q", got, want)
	}
	if strings.Contains(got, `rel="next"`) {
		t.Errorf("last page must not include next: %q", got)
	}
	if strings.Contains(got, `rel="last"`) {
		t.Errorf("last page must not include last: %q", got)
	}
}

func TestLinkHeader_SinglePage(t *testing.T) {
	if got := ghtest.LinkHeader("https://api.example/items", 1, 30, 1); got != "" {
		t.Errorf("single-page LinkHeader = %q, want empty", got)
	}
	if got := ghtest.LinkHeader("https://api.example/items", 1, 30, 0); got != "" {
		t.Errorf("zero-lastPage LinkHeader = %q, want empty", got)
	}
}

func TestLinkHeader_PerPagePropagates(t *testing.T) {
	got := ghtest.LinkHeader("https://api.example/items", 1, 100, 2)
	if !strings.Contains(got, "per_page=100") {
		t.Errorf("LinkHeader did not propagate per_page=100: %q", got)
	}
}
