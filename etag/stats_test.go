package etag

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestStats_HitMissStoreCounters(t *testing.T) {
	body := []byte(`{"hello":"world"}`)
	s, _ := ghServer(t, body)
	rt := newTestTransport(t)
	c := &http.Client{Transport: rt}
	tr := rt.(*Transport)

	doGet(t, c, s.URL+"/users/a")
	if got := tr.Stats(); got.TotalMisses != 1 || got.TotalStores != 1 || got.TotalHits != 0 {
		t.Fatalf("after cold call: %+v", got)
	}

	doGet(t, c, s.URL+"/users/a")
	got := tr.Stats()
	if got.TotalHits != 1 {
		t.Fatalf("expected 1 hit; got %d", got.TotalHits)
	}
	if got.TotalMisses != 1 {
		t.Fatalf("expected miss count unchanged on warm hit; got %d", got.TotalMisses)
	}
	if got.TotalStores != 1 {
		t.Fatalf("expected store count unchanged on synth-200; got %d", got.TotalStores)
	}
}

func TestStats_BypassOversize(t *testing.T) {
	big := bytes.Repeat([]byte("x"), 4096)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("ETag", `"abc"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(big)
	}))
	t.Cleanup(s.Close)

	rt := newTestTransport(t, WithMaxBodyBytes(64))
	c := &http.Client{Transport: rt}
	tr := rt.(*Transport)
	doGet(t, c, s.URL+"/users/a")

	got := tr.Stats()
	if got.TotalBypasses != 1 {
		t.Fatalf("TotalBypasses=%d, want 1", got.TotalBypasses)
	}
	if got.TotalStores != 0 {
		t.Fatalf("TotalStores should not advance on oversize bypass; got %d", got.TotalStores)
	}
}

func TestStats_BypassNoncacheable(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("ETag", `"abc"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(s.Close)

	rt := newTestTransport(t)
	c := &http.Client{Transport: rt}
	tr := rt.(*Transport)
	doGet(t, c, s.URL+"/users/a")

	if got := tr.Stats(); got.TotalBypasses != 1 {
		t.Fatalf("TotalBypasses=%d, want 1", got.TotalBypasses)
	}
}

func TestStats_BypassNoEtagHeader(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(s.Close)

	rt := newTestTransport(t)
	c := &http.Client{Transport: rt}
	tr := rt.(*Transport)
	doGet(t, c, s.URL+"/users/a")

	if got := tr.Stats(); got.TotalBypasses != 1 {
		t.Fatalf("TotalBypasses=%d, want 1", got.TotalBypasses)
	}
}

func TestStats_StoreCounterIncludesValidatedOK(t *testing.T) {
	body := []byte(`{"a":1}`)
	s, _ := ghServer(t, body)
	rt := newTestTransport(t)
	c := &http.Client{Transport: rt}
	tr := rt.(*Transport)

	doGet(t, c, s.URL+"/users/a")
	doGet(t, c, s.URL+"/users/a")

	if got := tr.Stats(); got.TotalStores != 1 {
		t.Fatalf("TotalStores=%d, want 1 (304 path does not re-store)", got.TotalStores)
	}
}

func TestStats_RareEventsDoNotIncrementCounters(t *testing.T) {
	body := []byte(`{"a":1}`)
	s, _ := ghServer(t, body)
	fc := failingAddCache{inner: NewLRUCache(4)}
	rt, err := NewTransport(nil, WithCache(fc), WithKeyScope("t"))
	if err != nil {
		t.Fatal(err)
	}
	c := &http.Client{Transport: rt}
	tr := rt.(*Transport)

	doGet(t, c, s.URL+"/users/a")

	got := tr.Stats()
	if got.TotalStores != 0 {
		t.Fatalf("TotalStores must not advance on store_error: %d", got.TotalStores)
	}
	if got.TotalBypasses != 0 {
		t.Fatalf("store_error is not a bypass: %d", got.TotalBypasses)
	}
}

type failingGetCache struct{ inner Cache }

func (failingGetCache) Get(context.Context, string) (Entry, bool, error) {
	return Entry{}, false, errors.New("backend down")
}
func (f failingGetCache) Add(ctx context.Context, k string, e Entry) error {
	return f.inner.Add(ctx, k, e)
}
func (f failingGetCache) Remove(ctx context.Context, k string) error {
	return f.inner.Remove(ctx, k)
}

func TestStats_GetErrorIsNotABypass(t *testing.T) {
	body := []byte(`{"a":1}`)
	s, _ := ghServer(t, body)
	fc := failingGetCache{inner: NewLRUCache(4)}
	rt, err := NewTransport(nil, WithCache(fc), WithKeyScope("t"))
	if err != nil {
		t.Fatal(err)
	}
	c := &http.Client{Transport: rt}
	tr := rt.(*Transport)

	doGet(t, c, s.URL+"/users/a")

	got := tr.Stats()
	if got.TotalMisses != 1 {
		t.Fatalf("get_error treated as miss; TotalMisses=%d", got.TotalMisses)
	}
	if got.TotalBypasses != 0 {
		t.Fatalf("get_error must not increment TotalBypasses: %d", got.TotalBypasses)
	}
	if got.TotalStores != 1 {
		t.Fatalf("response stored after get_error miss: TotalStores=%d", got.TotalStores)
	}
}

func TestStats_ConcurrentRoundTripsRaceFree(t *testing.T) {
	t.Parallel()
	body := []byte(`{"hello":"world"}`)
	s, _ := ghServer(t, body)
	rt := newTestTransport(t)
	c := &http.Client{Transport: rt}
	tr := rt.(*Transport)

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

	got := tr.Stats()
	if got.TotalHits+got.TotalMisses != n {
		t.Fatalf("hit+miss=%d, want %d", got.TotalHits+got.TotalMisses, n)
	}
}

func TestStats_SnapshotInternallyConsistent(t *testing.T) {
	body := []byte(`{"hello":"world"}`)
	s, _ := ghServer(t, body)
	rt := newTestTransport(t)
	c := &http.Client{Transport: rt}
	tr := rt.(*Transport)

	doGet(t, c, s.URL+"/users/a")
	doGet(t, c, s.URL+"/users/a")
	doGet(t, c, s.URL+"/users/a")

	prev := tr.Stats()
	for range 5 {
		now := tr.Stats()
		if now.TotalHits < prev.TotalHits || now.TotalMisses < prev.TotalMisses ||
			now.TotalStores < prev.TotalStores || now.TotalBypasses < prev.TotalBypasses ||
			now.TotalMismatches < prev.TotalMismatches {
			t.Fatalf("counter went backwards: prev=%+v now=%+v", prev, now)
		}
		prev = now
	}
}

func TestStats_TotalBypassesAggregatesAllSources(t *testing.T) {
	rt := newTestTransport(t, WithMaxBodyBytes(8))
	c := &http.Client{Transport: rt}
	tr := rt.(*Transport)

	oversize := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("ETag", `"a"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(bytes.Repeat([]byte("x"), 64))
	}))
	t.Cleanup(oversize.Close)
	doGet(t, c, oversize.URL+"/users/a")

	noStore := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("ETag", `"a"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(noStore.Close)
	doGet(t, c, noStore.URL+"/users/b")

	noETag := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(noETag.Close)
	doGet(t, c, noETag.URL+"/users/c")

	if got := tr.Stats().TotalBypasses; got != 3 {
		t.Fatalf("expected 3 bypasses across oversize+noncacheable+no_etag; got %d", got)
	}
}
