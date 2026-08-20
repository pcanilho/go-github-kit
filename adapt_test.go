package ghkit_test

import (
	"errors"
	"net/http"
	"testing"

	ghkit "github.com/pcanilho/go-github-kit"
	"golang.org/x/oauth2"
)

// --- Fake SDK mirroring the go-github v87+ constructor shape ---

// fakeOpt is the stand-in for github.ClientOptionsFunc.
type fakeOpt func(*fakeOpts) error

type fakeOpts struct {
	hc      *http.Client
	applied []string
}

// fakeWithHTTPClient stands in for github.WithHTTPClient: the one option
// constructor that accepts an *http.Client.
func fakeWithHTTPClient(hc *http.Client) fakeOpt {
	return func(o *fakeOpts) error {
		o.hc = hc
		o.applied = append(o.applied, "httpClient")
		return nil
	}
}

var errFakeOpt = errors.New("option rejected")

// newFakeVariadic stands in for github.NewClient.
func newFakeVariadic(opts ...fakeOpt) (*fakeClient, error) {
	var o fakeOpts
	for _, opt := range opts {
		if err := opt(&o); err != nil {
			return nil, err
		}
	}
	return &fakeClient{hc: o.hc}, nil
}

func TestGHKit_AdaptHappyPath(t *testing.T) {
	fc, err := ghkit.NewE(
		ghkit.Adapt(newFakeVariadic, fakeWithHTTPClient),
		ghkit.WithToken("abc"),
		ghkit.WithRateLimitDisabled(),
	)
	if err != nil {
		t.Fatalf("NewE(Adapt(...)): %v", err)
	}
	if fc == nil || fc.hc == nil {
		t.Fatal("adapted factory did not receive the ghkit-built *http.Client")
	}
	// WithToken means the stack is more than a bare *http.Transport.
	if _, bare := fc.hc.Transport.(*http.Transport); bare {
		t.Fatal("transport stack missing: got a bare *http.Transport")
	}
}

// Compile-time proof of the property Adapt exists for: both type parameters
// are inferred at a call site with no explicit type arguments. Writing
// Adapt[*fakeClient, fakeOpt](...) here would silently void it.
var _ func(*http.Client) (*fakeClient, error) = ghkit.Adapt(newFakeVariadic, fakeWithHTTPClient)

func TestGHKit_AdaptInfersTypeArguments(t *testing.T) {
	if ghkit.Adapt(newFakeVariadic, fakeWithHTTPClient) == nil {
		t.Fatal("Adapt returned nil for two non-nil arguments")
	}
}

// Adapt builds a closure; it must not call the constructor itself.
func TestGHKit_AdaptDoesNotCallFactoryEagerly(t *testing.T) {
	called := false
	factory := func(opts ...fakeOpt) (*fakeClient, error) {
		called = true
		return &fakeClient{}, nil
	}
	_ = ghkit.Adapt(factory, fakeWithHTTPClient)
	if called {
		t.Fatal("Adapt invoked the factory instead of deferring it")
	}
}

// A constructor error must stay distinguishable from ghkit's own sentinels.
func TestGHKit_AdaptWrapsFactoryError(t *testing.T) {
	factory := func(opts ...fakeOpt) (*fakeClient, error) {
		return nil, errFactory
	}
	fc, err := ghkit.NewE(ghkit.Adapt(factory, fakeWithHTTPClient), ghkit.WithToken("abc"))
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

// An option that fails surfaces through the same wrapping path.
func TestGHKit_AdaptWrapsOptionError(t *testing.T) {
	bad := func(hc *http.Client) fakeOpt {
		return func(*fakeOpts) error { return errFakeOpt }
	}
	_, err := ghkit.NewE(ghkit.Adapt(newFakeVariadic, bad), ghkit.WithToken("abc"))
	if !errors.Is(err, errFakeOpt) {
		t.Fatalf("option error not wrapped: %v", err)
	}
}

// The nil-factory and nil-httpOption branches need separate tests: Adapt's
// guard uses ||, which short-circuits, so passing both as nil would leave the
// httpOption branch uncovered.
//
// The typed nil below is required. A bare nil literal here does not compile:
// T has no other source, so inference fails with "cannot infer T".
func TestGHKit_AdaptNilFactory(t *testing.T) {
	var f func(...fakeOpt) (*fakeClient, error)
	if got := ghkit.Adapt(f, fakeWithHTTPClient); got != nil {
		t.Fatal("Adapt must return nil for a nil factory")
	}
	fc, err := ghkit.NewE(ghkit.Adapt(f, fakeWithHTTPClient), ghkit.WithToken("abc"))
	if !errors.Is(err, ghkit.ErrNilFactory) {
		t.Fatalf("want ErrNilFactory; got %v", err)
	}
	if fc != nil {
		t.Fatalf("expected nil client on error; got %+v", fc)
	}
}

func TestGHKit_AdaptNilHTTPOption(t *testing.T) {
	if got := ghkit.Adapt(newFakeVariadic, nil); got != nil {
		t.Fatal("Adapt must return nil for a nil httpOption")
	}
	fc, err := ghkit.NewE(ghkit.Adapt(newFakeVariadic, nil), ghkit.WithToken("abc"))
	if !errors.Is(err, ghkit.ErrNilFactory) {
		t.Fatalf("want ErrNilFactory; got %v", err)
	}
	if fc != nil {
		t.Fatalf("expected nil client on error; got %+v", fc)
	}
}

// A config error must short-circuit before the adapted factory runs.
func TestGHKit_AdaptConfigErrorShortCircuits(t *testing.T) {
	called := false
	factory := func(opts ...fakeOpt) (*fakeClient, error) {
		called = true
		return &fakeClient{}, nil
	}
	_, err := ghkit.NewE(ghkit.Adapt(factory, fakeWithHTTPClient),
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
