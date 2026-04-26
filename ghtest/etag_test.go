package ghtest_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pcanilho/go-github-kit/etag"
	"github.com/pcanilho/go-github-kit/ghtest"
)

func TestWrite304IfMatch_quotedMatch(t *testing.T) {
	body := []byte(`{"login":"octocat"}`)
	req := httptest.NewRequest("GET", "/users/octocat", nil)
	req.Header.Set("Authorization", "token ghp_test")
	req.Header.Set("Accept", "application/vnd.github+json")
	expected := etag.ComputeExpectedETag(req.Header, nil, body)
	req.Header.Set("If-None-Match", `"`+expected+`"`)

	rec := httptest.NewRecorder()
	if !ghtest.Write304IfMatch(rec, req, body) {
		t.Fatal("Write304IfMatch returned false on matching quoted ETag")
	}
	if rec.Code != http.StatusNotModified {
		t.Errorf("status = %d, want 304", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("body = %q, want empty on 304", rec.Body.String())
	}
	if got := rec.Header().Get("ETag"); got != `"`+expected+`"` {
		t.Errorf("ETag on 304 = %q, want %q", got, `"`+expected+`"`)
	}
}

func TestWrite304IfMatch_unquotedMatchNormalises(t *testing.T) {
	body := []byte(`{"login":"octocat"}`)
	req := httptest.NewRequest("GET", "/users/octocat", nil)
	req.Header.Set("Authorization", "token ghp_test")
	req.Header.Set("Accept", "application/vnd.github+json")
	expected := etag.ComputeExpectedETag(req.Header, nil, body)
	req.Header.Set("If-None-Match", expected)

	rec := httptest.NewRecorder()
	if !ghtest.Write304IfMatch(rec, req, body) {
		t.Fatal("Write304IfMatch did not normalise unquoted If-None-Match")
	}
	if rec.Code != http.StatusNotModified {
		t.Errorf("status = %d, want 304", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("body = %q, want empty on 304", rec.Body.String())
	}
}

func TestWrite304IfMatch_weakOnlyMatch(t *testing.T) {
	body := []byte(`{}`)
	req := httptest.NewRequest("GET", "/", nil)
	expected := etag.ComputeExpectedETag(req.Header, nil, body)
	req.Header.Set("If-None-Match", `W/"`+expected+`"`)

	rec := httptest.NewRecorder()
	if !ghtest.Write304IfMatch(rec, req, body) {
		t.Fatal("Write304IfMatch did not match a single weak tag")
	}
	if rec.Code != http.StatusNotModified {
		t.Errorf("status = %d, want 304", rec.Code)
	}
}

func TestWrite304IfMatch_multiTagComma(t *testing.T) {
	body := []byte(`{"login":"octocat"}`)
	req := httptest.NewRequest("GET", "/users/octocat", nil)
	req.Header.Set("Authorization", "token ghp_test")
	req.Header.Set("Accept", "application/vnd.github+json")
	expected := etag.ComputeExpectedETag(req.Header, nil, body)
	req.Header.Set("If-None-Match", strings.Join([]string{`W/"deadbeef"`, `"` + expected + `"`}, ", "))

	rec := httptest.NewRecorder()
	if !ghtest.Write304IfMatch(rec, req, body) {
		t.Fatal("Write304IfMatch returned false when match was second of two tags")
	}
}

func TestWrite304IfMatch_noMatchLeavesResponseUntouched(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("If-None-Match", `"deadbeef"`)
	if ghtest.Write304IfMatch(rec, req, []byte(`{}`)) {
		t.Fatal("Write304IfMatch returned true on non-matching ETag")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status mutated to %d on no-match", rec.Code)
	}
	if rec.Header().Get("ETag") != "" {
		t.Errorf("ETag leaked on no-match: %q", rec.Header().Get("ETag"))
	}
}

func TestWrite304IfMatch_noHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	if ghtest.Write304IfMatch(rec, req, []byte(`{}`)) {
		t.Fatal("Write304IfMatch returned true with no If-None-Match")
	}
}

func TestWrite304IfMatch_differentBodyMisses(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	expected := etag.ComputeExpectedETag(req.Header, nil, []byte(`{"a":1}`))
	req.Header.Set("If-None-Match", `"`+expected+`"`)
	rec := httptest.NewRecorder()
	if ghtest.Write304IfMatch(rec, req, []byte(`{"a":2}`)) {
		t.Fatal("Write304IfMatch matched against the wrong body")
	}
}
