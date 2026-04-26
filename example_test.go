package ghkit_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"

	ghkit "github.com/pcanilho/go-github-kit"
	"github.com/pcanilho/go-github-kit/etag"
	"github.com/pcanilho/go-github-kit/throttle"
)

// ExampleHTTPClient is the library-agnostic entry point: a configured
// *http.Client you can hand to any client library that takes one.
func ExampleHTTPClient() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	}))
	defer srv.Close()

	hc, err := ghkit.HTTPClient(
		ghkit.WithToken("fake-token"),
		ghkit.WithETagCache(),
	)
	if err != nil {
		fmt.Println("construct:", err)
		return
	}

	resp, err := hc.Get(srv.URL)
	if err != nil {
		fmt.Println("get:", err)
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	fmt.Print(string(body))
	// Output: ok
}

// Example_etagOnly uses only the etag sub-package inside a hand-built
// transport chain.
func Example_etagOnly() {
	rt, err := etag.NewTransport(nil,
		etag.WithCache(etag.NewLRUCache(1024)),
		etag.WithKeyScope("tenant-42"),
	)
	if err != nil {
		fmt.Println("construct:", err)
		return
	}
	hc := &http.Client{Transport: rt}
	resp, err := hc.Get("https://api.github.com/meta")
	if err != nil {
		fmt.Println("get:", err)
		return
	}
	if err := resp.Body.Close(); err != nil {
		fmt.Println("close:", err)
	}
}

// Example_throttle wraps any http.RoundTripper in a token-bucket cap.
func Example_throttle() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	}))
	defer srv.Close()

	rt, err := throttle.NewTransport(http.DefaultTransport, 10.0, throttle.WithBurst(1))
	if err != nil {
		fmt.Println("construct:", err)
		return
	}
	hc := &http.Client{Transport: rt}

	resp, err := hc.Get(srv.URL)
	if err != nil {
		fmt.Println("get:", err)
		return
	}
	defer resp.Body.Close()
	fmt.Println(resp.StatusCode)
	// Output: 200
}
