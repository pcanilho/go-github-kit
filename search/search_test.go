package search_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pcanilho/go-github-kit/ghtest"
	"github.com/pcanilho/go-github-kit/search"
)

type issue struct {
	ID    int    `json:"id"`
	Title string `json:"title"`
}

// envelopeServer emits a paginated /search/issues envelope. Returns
// total pages of `perPage` items each. Sets Link rel="next" until the
// last page. The first response carries `incomplete_results=true` if
// `incomplete` is set on attempt 1.
func envelopeServer(t *testing.T, totalPages, perPage int, incomplete bool) *httptest.Server {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := pageOf(r.URL.Query().Get("page"))
		_ = hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		base := "http://" + r.Host + r.URL.Path
		if link := ghtest.LinkHeader(base, page, perPage, totalPages); link != "" {
			w.Header().Set("Link", link)
		}
		startID := (page - 1) * perPage
		var items []string
		for i := 0; i < perPage; i++ {
			items = append(items, fmt.Sprintf(`{"id":%d,"title":"i-%d"}`, startID+i, startID+i))
		}
		incFlag := "false"
		if incomplete && page == 1 {
			incFlag = "true"
		}
		fmt.Fprintf(w, `{"total_count":%d,"incomplete_results":%s,"items":[%s]}`,
			totalPages*perPage, incFlag, strings.Join(items, ","))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func pageOf(s string) int {
	if s == "" {
		return 1
	}
	var n int
	for _, r := range s {
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int(r-'0')
	}
	if n == 0 {
		return 1
	}
	return n
}

func TestSearch_Issues_HappyPath(t *testing.T) {
	srv := envelopeServer(t, 3, 2, false)

	hc := srv.Client()
	ctx := t.Context()

	var got []int
	var seenTotal int
	var seenIncomplete bool
	for r, err := range search.Issues[issue](ctx, hc, "is:open", search.WithPerPage(2), search.WithBaseURL(srv.URL)) {
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, r.Item.ID)
		seenTotal = r.TotalCount
		if r.IncompleteResults {
			seenIncomplete = true
		}
	}
	if seenTotal != 6 {
		t.Fatalf("total=%d want 6", seenTotal)
	}
	if seenIncomplete {
		t.Fatalf("expected incomplete_results=false; got true")
	}
	wantIDs := []int{0, 1, 2, 3, 4, 5}
	if len(got) != len(wantIDs) {
		t.Fatalf("ids=%v want %v", got, wantIDs)
	}
	for i := range wantIDs {
		if got[i] != wantIDs[i] {
			t.Fatalf("ids[%d]=%d want %d", i, got[i], wantIDs[i])
		}
	}
}

func TestSearch_IncompleteResults_Propagated(t *testing.T) {
	srv := envelopeServer(t, 2, 1, true)

	hc := srv.Client()
	var seen []bool
	for r, err := range search.Issues[issue](t.Context(), hc, "x", search.WithBaseURL(srv.URL)) {
		if err != nil {
			t.Fatal(err)
		}
		seen = append(seen, r.IncompleteResults)
	}
	if len(seen) != 2 {
		t.Fatalf("yields=%d want 2", len(seen))
	}
	// Page 1 carries incomplete_results=true; page 2 carries false.
	if !seen[0] || seen[1] {
		t.Fatalf("incomplete flags=%v want [true false]", seen)
	}
}

func TestSearch_CapHit_YieldsErrResultCapHit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		// The exact substring is undocumented in GitHub's status
		// table; pinning it here surfaces drift.
		_, _ = io.WriteString(w, `{"message":"Validation Failed","errors":[{"code":"invalid","message":"Only the first 1000 search results are available"}]}`)
	}))
	t.Cleanup(srv.Close)

	var lastErr error
	var n int
	for _, err := range search.Issues[issue](t.Context(), srv.Client(), "x", search.WithBaseURL(srv.URL)) {
		n++
		lastErr = err
	}
	if !errors.Is(lastErr, search.ErrResultCapHit) {
		t.Fatalf("err=%v want ErrResultCapHit", lastErr)
	}
	if n != 1 {
		t.Fatalf("yields=%d want 1", n)
	}
}

func TestSearch_422_NotCap_SurfacesGenericError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, `{"message":"Validation Failed","errors":[{"code":"invalid","message":"some other validation error"}]}`)
	}))
	t.Cleanup(srv.Close)

	var lastErr error
	for _, err := range search.Issues[issue](t.Context(), srv.Client(), "x", search.WithBaseURL(srv.URL)) {
		lastErr = err
	}
	if errors.Is(lastErr, search.ErrResultCapHit) {
		t.Fatalf("err=%v should NOT be ErrResultCapHit", lastErr)
	}
	if lastErr == nil || !strings.Contains(lastErr.Error(), "422") {
		t.Fatalf("err=%v want generic 422 error", lastErr)
	}
}

func TestSearch_DecodeError_YieldsOnce(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{not json`)
	}))
	t.Cleanup(srv.Close)

	var n int
	var lastErr error
	for _, err := range search.Issues[issue](t.Context(), srv.Client(), "x", search.WithBaseURL(srv.URL)) {
		n++
		lastErr = err
	}
	if n != 1 || lastErr == nil {
		t.Fatalf("n=%d err=%v want 1 yield with decode error", n, lastErr)
	}
}

func TestSearch_NilClient_Errors(t *testing.T) {
	var n int
	var lastErr error
	for _, err := range search.Issues[issue](context.Background(), nil, "x") {
		n++
		lastErr = err
	}
	if n != 1 || lastErr == nil {
		t.Fatalf("n=%d err=%v want nil-client error", n, lastErr)
	}
}

func TestSearch_EmptyQuery_Errors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(srv.Close)

	var n int
	var lastErr error
	for _, err := range search.Issues[issue](t.Context(), srv.Client(), "", search.WithBaseURL(srv.URL)) {
		n++
		lastErr = err
	}
	if n != 1 || lastErr == nil {
		t.Fatalf("n=%d err=%v want empty-q error", n, lastErr)
	}
}

func TestSearch_BreakStopsIterator(t *testing.T) {
	srv := envelopeServer(t, 5, 10, false)
	var n int
	for r, err := range search.Issues[issue](t.Context(), srv.Client(), "x", search.WithBaseURL(srv.URL)) {
		if err != nil {
			t.Fatal(err)
		}
		_ = r
		n++
		if n == 3 {
			break
		}
	}
	if n != 3 {
		t.Fatalf("yields=%d want 3", n)
	}
}

func TestSearch_WithSort_AndWithOrder_EncodedAsQueryParams(t *testing.T) {
	var seenSort, seenOrder string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenSort = r.URL.Query().Get("sort")
		seenOrder = r.URL.Query().Get("order")
		_, _ = io.WriteString(w, `{"total_count":0,"incomplete_results":false,"items":[]}`)
	}))
	t.Cleanup(srv.Close)
	for _, err := range search.Issues[issue](t.Context(), srv.Client(), "x",
		search.WithBaseURL(srv.URL),
		search.WithSort("updated"),
		search.WithOrder("desc"),
	) {
		if err != nil {
			t.Fatal(err)
		}
	}
	if seenSort != "updated" {
		t.Fatalf("sort=%q want updated", seenSort)
	}
	if seenOrder != "desc" {
		t.Fatalf("order=%q want desc", seenOrder)
	}
}

func TestSearch_WithHeaders_AppliedToRequest(t *testing.T) {
	var seenAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAccept = r.Header.Get("Accept")
		_, _ = io.WriteString(w, `{"total_count":0,"incomplete_results":false,"items":[]}`)
	}))
	t.Cleanup(srv.Close)
	headers := http.Header{"Accept": []string{"application/vnd.github+json"}}
	for _, err := range search.Issues[issue](t.Context(), srv.Client(), "x",
		search.WithBaseURL(srv.URL),
		search.WithHeaders(headers),
	) {
		if err != nil {
			t.Fatal(err)
		}
	}
	if seenAccept != "application/vnd.github+json" {
		t.Fatalf("Accept=%q want application/vnd.github+json", seenAccept)
	}
}

func TestSearch_PerPageClampedToMax(t *testing.T) {
	var seenPerPage string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPerPage = r.URL.Query().Get("per_page")
		_, _ = io.WriteString(w, `{"total_count":0,"incomplete_results":false,"items":[]}`)
	}))
	t.Cleanup(srv.Close)
	for _, err := range search.Issues[issue](t.Context(), srv.Client(), "x", search.WithPerPage(500), search.WithBaseURL(srv.URL)) {
		if err != nil {
			t.Fatal(err)
		}
	}
	if seenPerPage != "100" {
		t.Fatalf("per_page=%s want 100 (clamped)", seenPerPage)
	}
}
