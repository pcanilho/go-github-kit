package etag

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type recorder struct {
	mu     sync.Mutex
	events []Event
}

func (r *recorder) record(_ context.Context, evt Event) {
	r.mu.Lock()
	r.events = append(r.events, evt)
	r.mu.Unlock()
}

func (r *recorder) snapshot() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Event, len(r.events))
	copy(out, r.events)
	return out
}

func (r *recorder) byKind(k Kind) []Event {
	out := []Event{}
	for _, e := range r.snapshot() {
		if e.Kind == k {
			out = append(out, e)
		}
	}
	return out
}

func TestEventCallback_HitMissStoreSequence(t *testing.T) {
	body := []byte(`{"hello":"world"}`)
	s, _ := ghServer(t, body)
	r := &recorder{}
	c := newTestClient(t, WithEventCallback(r.record))

	doGet(t, c, s.URL+"/users/a")
	doGet(t, c, s.URL+"/users/a")

	snap := r.snapshot()
	kinds := make([]Kind, 0, len(snap))
	for _, e := range snap {
		kinds = append(kinds, e.Kind)
	}
	want := []Kind{KindMiss, KindValidatedOK, KindStore, KindHit}
	if len(kinds) != len(want) {
		t.Fatalf("kinds=%v, want %v", kinds, want)
	}
	for i, k := range want {
		if kinds[i] != k {
			t.Fatalf("kind[%d]=%v, want %v", i, kinds[i], k)
		}
	}
}

func TestEventCallback_LookupHitFiresKindHitWithAge(t *testing.T) {
	body := []byte(`{"x":1}`)
	s, _ := ghServer(t, body)
	r := &recorder{}
	c := newTestClient(t, WithEventCallback(r.record))

	doGet(t, c, s.URL+"/users/a")
	doGet(t, c, s.URL+"/users/a")

	hits := r.byKind(KindHit)
	if len(hits) != 1 {
		t.Fatalf("hits=%d, want 1", len(hits))
	}
	if hits[0].Age <= 0 {
		t.Fatalf("KindHit Age must be positive; got %v", hits[0].Age)
	}
}

func TestEventCallback_URLPopulated(t *testing.T) {
	body := []byte(`{"x":1}`)
	s, _ := ghServer(t, body)
	r := &recorder{}
	c := newTestClient(t, WithEventCallback(r.record))

	doGet(t, c, s.URL+testPathOctocat)

	for _, e := range r.snapshot() {
		if e.Kind == KindDriftDetected || e.Kind == KindDriftRecovered {
			continue
		}
		if e.URL == nil {
			t.Fatalf("URL nil on Kind=%v", e.Kind)
		}
		if !strings.Contains(e.URL.Host, "127.0.0.1") && !strings.Contains(e.URL.Host, "localhost") {
			t.Fatalf("unexpected host: %s", e.URL.Host)
		}
		if e.URL.Path != testPathOctocat {
			t.Fatalf("Path=%s, want /users/octocat", e.URL.Path)
		}
	}
}

func TestEventCallback_PathTemplateNormalised(t *testing.T) {
	body := []byte(`{"x":1}`)
	s, _ := ghServer(t, body)
	r := &recorder{}
	c := newTestClient(t, WithEventCallback(r.record))

	doGet(t, c, s.URL+testPathOctocat)

	stores := r.byKind(KindStore)
	if len(stores) != 1 {
		t.Fatalf("stores=%d", len(stores))
	}
	if got := stores[0].PathTemplate; got != "/users/{u}" {
		t.Fatalf("PathTemplate=%q, want /users/{u}", got)
	}
}

func TestEventCallback_BypassOversize(t *testing.T) {
	big := bytes.Repeat([]byte("x"), 4096)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("ETag", `"a"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(big)
	}))
	t.Cleanup(s.Close)

	r := &recorder{}
	c := newTestClient(t, WithMaxBodyBytes(64), WithEventCallback(r.record))
	doGet(t, c, s.URL+"/users/a")

	got := r.byKind(KindBypassOversize)
	if len(got) != 1 {
		t.Fatalf("KindBypassOversize=%d, want 1", len(got))
	}
	if got[0].Status != http.StatusOK {
		t.Fatalf("Status=%d", got[0].Status)
	}
	if got[0].URL == nil || got[0].URL.Path != "/users/a" {
		t.Fatalf("URL not populated: %+v", got[0].URL)
	}
}

func TestEventCallback_BypassNoncacheable(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("ETag", `"a"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(s.Close)

	r := &recorder{}
	c := newTestClient(t, WithEventCallback(r.record))
	doGet(t, c, s.URL+"/users/a")

	if got := r.byKind(KindBypassNoncache); len(got) != 1 {
		t.Fatalf("KindBypassNoncache=%d, want 1", len(got))
	}
}

func TestEventCallback_NoEtagHeader(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(s.Close)

	r := &recorder{}
	c := newTestClient(t, WithEventCallback(r.record))
	doGet(t, c, s.URL+"/users/a")

	if got := r.byKind(KindNoEtagHeader); len(got) != 1 {
		t.Fatalf("KindNoEtagHeader=%d, want 1", len(got))
	}
}

func TestEventCallback_ValidatedOK(t *testing.T) {
	body := []byte(`{"x":1}`)
	s, _ := ghServer(t, body)
	r := &recorder{}
	c := newTestClient(t, WithEventCallback(r.record))

	doGet(t, c, s.URL+"/users/a")

	if got := r.byKind(KindValidatedOK); len(got) != 1 {
		t.Fatalf("KindValidatedOK=%d, want 1", len(got))
	}
}

func TestEventCallback_MismatchNotThrottled(t *testing.T) {
	body := []byte(`{"x":1}`)
	mismatchServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("ETag", `"server-fixed"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	t.Cleanup(mismatchServer.Close)

	r := &recorder{}
	c := newTestClient(t, WithEventCallback(r.record))
	for range 5 {
		doGet(t, c, mismatchServer.URL+"/users/a")
	}

	if got := r.byKind(KindMismatch); len(got) != 5 {
		t.Fatalf("KindMismatch=%d, want 5 (callback must not be throttled like warnMismatch slog)", len(got))
	}
}

func TestEventCallback_StoreErrorIncludesErr(t *testing.T) {
	body := []byte(`{"x":1}`)
	s, _ := ghServer(t, body)
	fc := failingAddCache{inner: NewLRUCache(4)}
	r := &recorder{}
	rt, err := NewTransport(nil,
		WithCache(fc), WithKeyScope("t"),
		WithEventCallback(r.record),
	)
	if err != nil {
		t.Fatal(err)
	}
	c := &http.Client{Transport: rt}
	doGet(t, c, s.URL+"/users/a")

	got := r.byKind(KindStoreError)
	if len(got) != 1 {
		t.Fatalf("KindStoreError=%d, want 1", len(got))
	}
	if got[0].Err == nil {
		t.Fatalf("KindStoreError must carry Err")
	}
}

func TestEventCallback_GetErrorIncludesErr(t *testing.T) {
	body := []byte(`{"x":1}`)
	s, _ := ghServer(t, body)
	fc := failingGetCache{inner: NewLRUCache(4)}
	r := &recorder{}
	rt, err := NewTransport(nil,
		WithCache(fc), WithKeyScope("t"),
		WithEventCallback(r.record),
	)
	if err != nil {
		t.Fatal(err)
	}
	c := &http.Client{Transport: rt}
	doGet(t, c, s.URL+"/users/a")

	got := r.byKind(KindGetError)
	if len(got) != 1 {
		t.Fatalf("KindGetError=%d, want 1", len(got))
	}
	if got[0].Err == nil {
		t.Fatalf("KindGetError must carry Err")
	}
}

type failingRemoveCache struct{ inner Cache }

func (f failingRemoveCache) Get(ctx context.Context, k string) (Entry, bool, error) {
	return f.inner.Get(ctx, k)
}
func (f failingRemoveCache) Add(ctx context.Context, k string, e Entry) error {
	return f.inner.Add(ctx, k, e)
}
func (failingRemoveCache) Remove(context.Context, string) error {
	return errors.New("backend down")
}

func TestEventCallback_RemoveErrorIncludesErr(t *testing.T) {
	body := []byte(`{"x":1}`)
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("ETag", `"`+ComputeExpectedETag(r.Header, nil, body)+`"`)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(s.Close)

	fc := failingRemoveCache{inner: NewLRUCache(4)}
	r := &recorder{}
	rt, err := NewTransport(nil,
		WithCache(fc), WithKeyScope("t"),
		WithEventCallback(r.record),
	)
	if err != nil {
		t.Fatal(err)
	}
	c := &http.Client{Transport: rt}

	doGet(t, c, s.URL+"/users/a")
	doGet(t, c, s.URL+"/users/a")

	got := r.byKind(KindRemoveError)
	if len(got) != 1 {
		t.Fatalf("KindRemoveError=%d, want 1", len(got))
	}
	if got[0].Err == nil {
		t.Fatalf("KindRemoveError must carry Err")
	}
}

func TestEventCallback_InvalidatedGone(t *testing.T) {
	body := []byte(`{"x":1}`)
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("ETag", `"`+ComputeExpectedETag(r.Header, nil, body)+`"`)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(s.Close)

	r := &recorder{}
	c := newTestClient(t, WithEventCallback(r.record))
	doGet(t, c, s.URL+"/users/a")
	doGet(t, c, s.URL+"/users/a")

	if got := r.byKind(KindInvalidatedGone); len(got) != 1 {
		t.Fatalf("KindInvalidatedGone=%d, want 1", len(got))
	}
}

func TestEventCallback_DriftTransitionsAreEmitted(t *testing.T) {
	r := &recorder{}
	rt, err := NewTransport(nil, WithEventCallback(r.record))
	if err != nil {
		t.Fatal(err)
	}
	tr := rt.(*Transport)

	for range driftThreshold {
		if evt, fire := tr.recordMismatch(); fire {
			tr.fireDriftEvent(context.Background(), evt)
		}
	}
	tr.driftDegradedAt.Store(0)
	for range driftRecoverAfterN {
		if evt, fire := tr.recordSuccess(); fire {
			tr.fireDriftEvent(context.Background(), evt)
		}
	}

	if got := r.byKind(KindDriftDetected); len(got) != 1 {
		t.Fatalf("KindDriftDetected=%d, want 1", len(got))
	}
	if got := r.byKind(KindDriftRecovered); len(got) != 1 {
		t.Fatalf("KindDriftRecovered=%d, want 1", len(got))
	}
}

func TestEventCallback_NilCallbackIsNoOp(t *testing.T) {
	body := []byte(`{"x":1}`)
	s, _ := ghServer(t, body)
	c := newTestClient(t, WithEventCallback(nil))
	doGet(t, c, s.URL+"/users/a")
}

func TestEventCallback_NonCacheableRequestSilent(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(s.Close)

	r := &recorder{}
	c := newTestClient(t, WithEventCallback(r.record))

	resp, err := c.Post(s.URL+"/users/a", "application/json", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if got := r.snapshot(); len(got) != 0 {
		t.Fatalf("non-cacheable request must not fire events; got %d", len(got))
	}
}

func TestEventCallback_ContextPropagation(t *testing.T) {
	type ctxKey struct{}
	body := []byte(`{"x":1}`)
	s, _ := ghServer(t, body)

	var seen string
	c := newTestClient(t, WithEventCallback(func(ctx context.Context, evt Event) {
		if evt.Kind != KindStore {
			return
		}
		v, _ := ctx.Value(ctxKey{}).(string)
		seen = v
	}))

	req, err := http.NewRequest(http.MethodGet, s.URL+"/users/a", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(req.Context(), ctxKey{}, "push")
	req = req.WithContext(ctx)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if seen != "push" {
		t.Fatalf("ctx value not propagated; got %q", seen)
	}
}

func TestEventCallback_KindStringsMatchSlogKindAttribute(t *testing.T) {
	cases := []struct {
		name   string
		kind   Kind
		source string
	}{
		{"Hit", KindHit, "hit"},
		{"Miss", KindMiss, "miss"},
		{"Store", KindStore, "store"},
		{"GetError", KindGetError, "get_error"},
		{"StoreError", KindStoreError, "store_error"},
		{"RemoveError", KindRemoveError, "remove_error"},
		{"BypassOversize", KindBypassOversize, "bypass_oversize"},
		{"BypassNoncache", KindBypassNoncache, "bypass_noncacheable"},
		{"BypassEmptyBody", KindBypassEmptyBody, "bypass_empty_body"},
		{"NoEtagHeader", KindNoEtagHeader, "no_etag_header"},
		{"ValidatedOK", KindValidatedOK, "validated_ok"},
		{"InvalidatedGone", KindInvalidatedGone, "invalidated_gone"},
		{"Mismatch", KindMismatch, "mismatch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if string(tc.kind) != tc.source {
				t.Fatalf("Kind=%q, source slog kind=%q", tc.kind, tc.source)
			}
		})
	}
	if string(KindDriftDetected)+"_x" == "etag_drift_detected_x" {
		t.Fatalf("KindDriftDetected unexpectedly carries the etag_ prefix")
	}
	if string(KindDriftRecovered)+"_x" == "etag_drift_recovered_x" {
		t.Fatalf("KindDriftRecovered unexpectedly carries the etag_ prefix")
	}
}

func TestEventCallback_ConcurrentRoundTripsRaceFree(t *testing.T) {
	t.Parallel()
	body := []byte(`{"x":1}`)
	s, _ := ghServer(t, body)
	r := &recorder{}
	c := newTestClient(t, WithEventCallback(r.record))

	const n = 100
	var wg sync.WaitGroup
	wg.Add(n)
	for range n {
		go func() {
			defer wg.Done()
			resp, err := c.Get(s.URL + "/users/a")
			if err != nil {
				return
			}
			_, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
		}()
	}
	wg.Wait()

	hits := len(r.byKind(KindHit))
	misses := len(r.byKind(KindMiss))
	if hits+misses != n {
		t.Fatalf("hits+misses=%d, want %d", hits+misses, n)
	}
}
