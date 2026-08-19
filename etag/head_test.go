package etag

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// doHead issues a HEAD and drains the body.
func doHead(t *testing.T, c *http.Client, url string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodHead, url, nil)
	if err != nil {
		t.Fatalf("NewRequest HEAD: %v", err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("HEAD %s: %v", url, err)
	}
	body, readErr := io.ReadAll(resp.Body)
	closeErr := resp.Body.Close()
	if readErr != nil {
		t.Fatalf("HEAD %s read body: %v", url, readErr)
	}
	if closeErr != nil {
		t.Fatalf("HEAD %s Body.Close: %v", url, closeErr)
	}
	return resp, body
}

// A HEAD response has no body, so hashing it always mismatched and fed
// the drift detector.
func TestETag_HEADRecordsNoMismatch(t *testing.T) {
	body := []byte(`{"hello":"world"}`)
	s, _ := ghServer(t, body)
	rt := newTestTransport(t)
	c := &http.Client{Transport: rt}
	tr := rt.(*Transport)

	for range 3 {
		doHead(t, c, s.URL+"/users/a")
	}

	got := tr.Stats()
	if got.TotalMismatches != 0 {
		t.Fatalf("HEAD recorded %d mismatch(es); want 0 (%+v)", got.TotalMismatches, got)
	}
	if got.TotalStores != 0 {
		t.Fatalf("HEAD stored %d entr(ies); want 0 (%+v)", got.TotalStores, got)
	}
}

// driftThreshold mismatches in driftWindow degrade the transport to
// passive mode. HEAD traffic must not trip it.
func TestETag_HEADDoesNotDegradeDrift(t *testing.T) {
	body := []byte(`{"hello":"world"}`)
	s, _ := ghServer(t, body)
	rt := newTestTransport(t)
	c := &http.Client{Transport: rt}
	tr := rt.(*Transport)

	for range driftThreshold + 2 {
		doHead(t, c, s.URL+"/users/a")
	}

	if got := tr.Stats(); got.Degraded {
		t.Fatalf("HEAD traffic degraded the drift detector: %+v", got)
	}
}

// Mirror case: a HEAD hitting a GET-populated entry used to be handed
// that entry's body.
func TestETag_HEADAfterGETReturnsNoBody(t *testing.T) {
	body := []byte(`{"hello":"world"}`)
	s, _ := ghServer(t, body)
	c := newTestClient(t)

	if got := doGet(t, c, s.URL+"/users/a"); got.StatusCode != 200 {
		t.Fatalf("seed GET status = %d", got.StatusCode)
	}

	resp, headBody := doHead(t, c, s.URL+"/users/a")
	if len(headBody) != 0 {
		t.Fatalf("HEAD returned %d body bytes; want 0", len(headBody))
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD status = %d; want 200", resp.StatusCode)
	}
}

// readBounded used to overwrite ContentLength with the empty body's
// length.
func TestETag_HEADPreservesContentLength(t *testing.T) {
	body := []byte(`{"hello":"world"}`)
	s, _ := ghServer(t, body)
	c := newTestClient(t)

	resp, _ := doHead(t, c, s.URL+"/users/a")
	if resp.ContentLength != int64(len(body)) {
		t.Fatalf("HEAD ContentLength = %d; want %d", resp.ContentLength, len(body))
	}
}

// Once degraded, a HEAD-stored entry made a later GET receive a
// synthesised 200 with an empty body.
func TestETag_DegradedGETAfterHEADReturnsRealBody(t *testing.T) {
	body := []byte(`{"hello":"world"}`)
	s, _ := ghServer(t, body)
	rt := newTestTransport(t)
	c := &http.Client{Transport: rt}
	tr := rt.(*Transport)

	// Pre-fix, the HEAD stored an empty-bodied entry and the GET replayed it
	// as an empty 200. DegradedAt must be recent: a zero value reads as past
	// the cooldown and sends a recomputed probe, not the stored ETag.
	doHead(t, c, s.URL+"/users/a")
	tr.driftDegraded.Store(true)
	tr.driftDegradedAt.Store(time.Now().UnixNano())

	got := doGet(t, c, s.URL+"/users/a")
	if got.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d; want 200", got.StatusCode)
	}
	if !bytes.Equal(got.Body, body) {
		t.Fatalf("GET body = %q; want %q", got.Body, body)
	}
}

// doGetWith issues a GET with the given headers.
func doGetWith(t *testing.T, c *http.Client, url string, hdr http.Header) getResult {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	body, readErr := io.ReadAll(resp.Body)
	closeErr := resp.Body.Close()
	if readErr != nil {
		t.Fatalf("read body: %v", readErr)
	}
	if closeErr != nil {
		t.Fatalf("Body.Close: %v", closeErr)
	}
	return getResult{StatusCode: resp.StatusCode, Header: resp.Header, Body: body}
}

// Two Accept values are different representations; they used to share a
// key and evict each other.
func TestETag_AcceptVariantsDoNotShareAnEntry(t *testing.T) {
	body := []byte(`{"hello":"world"}`)
	s, _ := ghServer(t, body)
	rt := newTestTransport(t)
	c := &http.Client{Transport: rt}
	tr := rt.(*Transport)

	raw := http.Header{headerAccept: {"application/vnd.github.raw"}}
	js := http.Header{headerAccept: {"application/vnd.github+json"}}

	doGetWith(t, c, s.URL+"/users/a", raw)
	doGetWith(t, c, s.URL+"/users/a", js)
	// Both are now cached under their own key, so each repeat is a hit.
	doGetWith(t, c, s.URL+"/users/a", raw)
	doGetWith(t, c, s.URL+"/users/a", js)

	got := tr.Stats()
	if got.TotalMisses != 2 {
		t.Fatalf("TotalMisses = %d; want 2 (one per Accept variant): %+v", got.TotalMisses, got)
	}
	if got.TotalHits != 2 {
		t.Fatalf("TotalHits = %d; want 2: %+v", got.TotalHits, got)
	}
}

// The API version is not in the hash domain, so the key names it
// explicitly. Both spellings must land on one entry.
func TestETag_APIVersionVariants(t *testing.T) {
	body := []byte(`{"hello":"world"}`)
	s, _ := ghServer(t, body)
	rt := newTestTransport(t)
	c := &http.Client{Transport: rt}
	tr := rt.(*Transport)

	doGetWith(t, c, s.URL+"/users/a", http.Header{"X-GitHub-Api-Version": {"2022-11-28"}})
	doGetWith(t, c, s.URL+"/users/a", http.Header{"X-GitHub-Api-Version": {"2026-03-10"}})
	if got := tr.Stats(); got.TotalMisses != 2 {
		t.Fatalf("distinct API versions shared an entry: %+v", got)
	}

	// go-github spells it X-Github-Api-Version; net/http canonicalises
	// both to the same key, so this must be a hit, not a third miss.
	doGetWith(t, c, s.URL+"/users/a", http.Header{"X-Github-Api-Version": {"2022-11-28"}})
	if got := tr.Stats(); got.TotalMisses != 2 || got.TotalHits != 1 {
		t.Fatalf("header spelling split the entry: %+v", got)
	}
}

// No variant header keeps the pre-1.7.0 key, so external caches skip a
// cold pass in the common case.
func TestETag_NoVariantHeadersKeepsBaseKey(t *testing.T) {
	rt := newTestTransport(t)
	tr := rt.(*Transport)
	req := mustGetRequest(t, "https://api.github.com/users/a")

	got := cacheKey(req, tr.scopeDigest)
	want := "https://api.github.com/users/a|" + tr.scopeDigest
	if got != want {
		t.Fatalf("cacheKey = %q; want %q", got, want)
	}
}

// Pre-1.7.0 stored HEAD responses under the key a GET uses. A persistent
// Cache survives the upgrade; such an entry must not be replayed.
func TestETag_PoisonedEmptyBodyEntryIsTreatedAsMiss(t *testing.T) {
	body := []byte(`{"hello":"world"}`)
	s, _ := ghServer(t, body)
	rt := newTestTransport(t)
	c := &http.Client{Transport: rt}
	tr := rt.(*Transport)

	// Hand-plant the entry a pre-1.7.0 HEAD would have left behind.
	req := mustGetRequest(t, s.URL+"/users/a")
	key := cacheKey(req, tr.scopeDigest)
	// Must be the ETag the server really computes, or the 200 comes back
	// for the wrong reason.
	realETag := `"` + ComputeExpectedETag(req.Header, nil, body) + `"`
	if err := tr.cache.Add(t.Context(), key, Entry{
		ETag:    realETag,
		Body:    []byte{},
		Headers: http.Header{},
	}); err != nil {
		t.Fatalf("cache.Add: %v", err)
	}
	tr.driftDegraded.Store(true)
	tr.driftDegradedAt.Store(time.Now().UnixNano())

	got := doGet(t, c, s.URL+"/users/a")
	if got.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200", got.StatusCode)
	}
	if !bytes.Equal(got.Body, body) {
		t.Fatalf("body = %q; want %q", got.Body, body)
	}
}

// Replaying an empty body gives the caller nothing, and validating it
// records a false mismatch.
func TestETag_EmptyBodyResponseBypassesCache(t *testing.T) {
	var hits int
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.Header().Set("ETag", `"an-etag-over-nothing"`)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(s.Close)

	rt := newTestTransport(t)
	c := &http.Client{Transport: rt}
	tr := rt.(*Transport)

	for range 3 {
		if got := doGet(t, c, s.URL+"/empty"); got.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", got.StatusCode)
		}
	}

	got := tr.Stats()
	if got.TotalMismatches != 0 {
		t.Fatalf("empty body recorded %d mismatch(es); want 0 (%+v)", got.TotalMismatches, got)
	}
	if got.TotalStores != 0 {
		t.Fatalf("empty body stored %d entr(ies); want 0 (%+v)", got.TotalStores, got)
	}
	if got.TotalBypasses != 3 {
		t.Fatalf("TotalBypasses = %d; want 3 (%+v)", got.TotalBypasses, got)
	}
}

// Skipping is not enough: if the wire response is also empty, nothing
// overwrites the entry and it lives forever.
func TestETag_PoisonedEntryIsEvictedNotJustSkipped(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("ETag", `"still-empty"`)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(s.Close)

	rt := newTestTransport(t)
	c := &http.Client{Transport: rt}
	tr := rt.(*Transport)

	req := mustGetRequest(t, s.URL+"/empty")
	key := cacheKey(req, tr.scopeDigest)
	if err := tr.cache.Add(t.Context(), key, Entry{
		ETag: `"planted"`, Body: []byte{}, Headers: http.Header{},
	}); err != nil {
		t.Fatalf("cache.Add: %v", err)
	}

	doGet(t, c, s.URL+"/empty")

	if _, ok, err := tr.cache.Get(t.Context(), key); err != nil {
		t.Fatalf("cache.Get: %v", err)
	} else if ok {
		t.Fatal("poisoned entry survived; it must be evicted, not just skipped")
	}
}
