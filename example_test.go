package ghkit_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"

	ghkit "github.com/pcanilho/go-github-kit"
	"github.com/pcanilho/go-github-kit/etag"
	"github.com/pcanilho/go-github-kit/ghtest"
	"github.com/pcanilho/go-github-kit/pages"
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

// Example_paginated walks a Link-paginated endpoint with the pages
// sub-package. The fixture serves three pages of two items each; the
// iterator yields one element at a time without the caller writing the
// Link-walking loop.
func Example_paginated() {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := 1
		if p := r.URL.Query().Get("page"); p == "2" {
			page = 2
		} else if p == "3" {
			page = 3
		}
		base := srv.URL + r.URL.Path
		if link := ghtest.LinkHeader(base, page, 2, 3); link != "" {
			w.Header().Set("Link", link)
		}
		w.Header().Set("Content-Type", "application/json")
		switch page {
		case 1:
			fmt.Fprint(w, `[{"id":1},{"id":2}]`)
		case 2:
			fmt.Fprint(w, `[{"id":3},{"id":4}]`)
		case 3:
			fmt.Fprint(w, `[{"id":5},{"id":6}]`)
		}
	}))
	defer srv.Close()

	type item struct {
		ID int `json:"id"`
	}
	var ids []int
	for it, err := range pages.As[item](context.Background(), srv.Client(), "GET", srv.URL+"/items", nil) {
		if err != nil {
			fmt.Println("err:", err)
			return
		}
		ids = append(ids, it.ID)
	}
	fmt.Println(ids)
	// Output: [1 2 3 4 5 6]
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
