package ghkit_test

import (
	"errors"
	"net/http"
	"testing"

	ghkit "github.com/pcanilho/go-github-kit"
	"github.com/pcanilho/go-github-kit/etag"
	"golang.org/x/oauth2"
)

var errFactory = errors.New("constructor blew up")

func TestGHKit_NewEHappyPath(t *testing.T) {
	fc, err := ghkit.NewE(func(hc *http.Client) (*fakeClient, error) {
		return &fakeClient{hc: hc}, nil
	}, ghkit.WithToken("abc"), ghkit.WithRateLimitDisabled())
	if err != nil {
		t.Fatalf("NewE: %v", err)
	}
	if fc == nil || fc.hc == nil {
		t.Fatal("NewE returned a client with no *http.Client")
	}
}

func TestGHKit_NewENilFactory(t *testing.T) {
	fc, err := ghkit.NewE[*fakeClient](nil, ghkit.WithToken("abc"))
	if !errors.Is(err, ghkit.ErrNilFactory) {
		t.Fatalf("want ErrNilFactory; got %v", err)
	}
	if fc != nil {
		t.Fatalf("expected nil client on error; got %+v", fc)
	}
}

// A factory error must stay distinguishable from ghkit's own sentinels.
func TestGHKit_NewEWrapsFactoryError(t *testing.T) {
	fc, err := ghkit.NewE(func(*http.Client) (*fakeClient, error) {
		return nil, errFactory
	}, ghkit.WithToken("abc"))
	if !errors.Is(err, errFactory) {
		t.Fatalf("factory error not wrapped: %v", err)
	}
	if errors.Is(err, ghkit.ErrConflictingAuth) {
		t.Fatal("factory error must not read as a ghkit config error")
	}
	if fc != nil {
		t.Fatalf("expected nil client on error; got %+v", fc)
	}
}

func TestGHKit_NewEConfigErrorShortCircuits(t *testing.T) {
	called := false
	_, err := ghkit.NewE(func(*http.Client) (*fakeClient, error) {
		called = true
		return &fakeClient{}, nil
	},
		ghkit.WithToken("abc"),
		ghkit.WithTokenSource(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "x"})),
	)
	if !errors.Is(err, ghkit.ErrConflictingAuth) {
		t.Fatalf("want ErrConflictingAuth; got %v", err)
	}
	if called {
		t.Fatal("factory ran despite an invalid option combination")
	}
}

// Must enable the ETag layer on its own, or the callback never fires.
func TestGHKit_WithETagTransportWithoutETagCache(t *testing.T) {
	var got *etag.Transport
	hc, err := ghkit.HTTPClient(
		ghkit.WithToken("abc"),
		ghkit.WithETagTransport(func(tr *etag.Transport) { got = tr }),
	)
	if err != nil {
		t.Fatalf("HTTPClient: %v", err)
	}
	if hc == nil {
		t.Fatal("nil client")
	}
	if got == nil {
		t.Fatal("WithETagTransport callback never fired without WithETagCache")
	}
	if s := got.Stats(); s.Degraded {
		t.Fatalf("fresh transport reports degraded: %+v", s)
	}
}

func TestGHKit_WithETagTransportAlongsideETagCache(t *testing.T) {
	var got *etag.Transport
	if _, err := ghkit.HTTPClient(
		ghkit.WithToken("abc"),
		ghkit.WithETagCache(),
		ghkit.WithETagTransport(func(tr *etag.Transport) { got = tr }),
	); err != nil {
		t.Fatalf("HTTPClient: %v", err)
	}
	if got == nil {
		t.Fatal("callback never fired")
	}
}

func TestGHKit_WithETagTransportNilFunc(t *testing.T) {
	if _, err := ghkit.HTTPClient(
		ghkit.WithToken("abc"),
		ghkit.WithETagTransport(nil),
	); err != nil {
		t.Fatalf("HTTPClient: %v", err)
	}
}

// A shared helper and a call site can each want the handle.
func TestGHKit_WithETagTransportAccumulates(t *testing.T) {
	var first, second *etag.Transport
	if _, err := ghkit.HTTPClient(
		ghkit.WithToken("abc"),
		ghkit.WithETagTransport(func(tr *etag.Transport) { first = tr }),
		ghkit.WithETagTransport(func(tr *etag.Transport) { second = tr }),
	); err != nil {
		t.Fatalf("HTTPClient: %v", err)
	}
	if first == nil || second == nil {
		t.Fatalf("callbacks dropped: first=%v second=%v", first != nil, second != nil)
	}
	if first != second {
		t.Fatal("callbacks received different transports")
	}
}
