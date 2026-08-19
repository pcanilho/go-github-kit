package ghkit_test

import (
	"errors"
	"iter"
	"net/http"
	"testing"

	"github.com/pcanilho/go-github-kit/pages"
	"github.com/pcanilho/go-github-kit/polling"
	"github.com/pcanilho/go-github-kit/search"
)

// These used to be inline errors.New, unmatchable by errors.Is.
func TestSentinels_NilClientIsMatchable(t *testing.T) {
	t.Run("pages", func(t *testing.T) {
		for _, err := range pages.Pages(t.Context(), nil, http.MethodGet, "https://x/y", nil) {
			if !errors.Is(err, pages.ErrNilClient) {
				t.Fatalf("got %v; want pages.ErrNilClient", err)
			}
			return
		}
		t.Fatal("iterator yielded nothing")
	})

	t.Run("polling", func(t *testing.T) {
		for _, err := range polling.Poll(t.Context(), nil, http.MethodGet, "https://x/y", nil, nil, 0) {
			if !errors.Is(err, polling.ErrNilClient) {
				t.Fatalf("got %v; want polling.ErrNilClient", err)
			}
			return
		}
		t.Fatal("iterator yielded nothing")
	})

	t.Run("search", func(t *testing.T) {
		for _, err := range search.Issues[map[string]any](t.Context(), nil, "q") {
			if !errors.Is(err, search.ErrNilClient) {
				t.Fatalf("got %v; want search.ErrNilClient", err)
			}
			return
		}
		t.Fatal("iterator yielded nothing")
	})
}

// The As wrappers delegate to the same guards.
func TestSentinels_NilClientViaWrappers(t *testing.T) {
	t.Run("pages.As", func(t *testing.T) {
		for _, err := range pages.As[map[string]any](t.Context(), nil, http.MethodGet, "https://x/y", nil) {
			if !errors.Is(err, pages.ErrNilClient) {
				t.Fatalf("got %v; want pages.ErrNilClient", err)
			}
			return
		}
		t.Fatal("iterator yielded nothing")
	})

	t.Run("polling.As", func(t *testing.T) {
		for _, err := range polling.As[map[string]any](t.Context(), nil, http.MethodGet, "https://x/y", nil, nil, 0) {
			if !errors.Is(err, polling.ErrNilClient) {
				t.Fatalf("got %v; want polling.ErrNilClient", err)
			}
			return
		}
		t.Fatal("iterator yielded nothing")
	})

	for name, fn := range map[string]func() error{
		"search.Code":  func() error { return firstErr(search.Code[map[string]any](t.Context(), nil, "q")) },
		"search.Repos": func() error { return firstErr(search.Repos[map[string]any](t.Context(), nil, "q")) },
		"search.Users": func() error { return firstErr(search.Users[map[string]any](t.Context(), nil, "q")) },
	} {
		t.Run(name, func(t *testing.T) {
			if err := fn(); !errors.Is(err, search.ErrNilClient) {
				t.Fatalf("got %v; want search.ErrNilClient", err)
			}
		})
	}
}

func firstErr[T any](seq iter.Seq2[search.Result[T], error]) error {
	for _, err := range seq {
		return err
	}
	return nil
}

func TestSentinels_MessagesUnchanged(t *testing.T) {
	cases := map[error]string{
		pages.ErrNilClient:   "pages: nil *http.Client",
		polling.ErrNilClient: "polling: nil *http.Client",
		search.ErrNilClient:  "search: nil *http.Client",
		search.ErrEmptyQuery: "search: q is required",
	}
	for err, want := range cases {
		if err.Error() != want {
			t.Errorf("message drift: got %q, want %q", err.Error(), want)
		}
	}
}

func TestSentinels_EmptyQueryIsMatchable(t *testing.T) {
	for _, err := range search.Issues[map[string]any](t.Context(), &http.Client{}, "") {
		if !errors.Is(err, search.ErrEmptyQuery) {
			t.Fatalf("got %v; want search.ErrEmptyQuery", err)
		}
		return
	}
	t.Fatal("iterator yielded nothing")
}
