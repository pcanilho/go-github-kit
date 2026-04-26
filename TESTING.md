# Testing code that uses ghkit

The `ghtest` sub-package ships two helpers for the GitHub-specific traps in
testing ghkit-using code: secondary-rate-limit classification
(`WriteSecondaryLimit`) and the bored-engineer ETag hash domain
(`Write304IfMatch`). Everything else is plain stdlib code, shown inline as
recipes you can copy.

`ghtest` is shape-correct, not behaviour-correct. It does not enforce
rate-limit budgets or run a real ETag database. For behaviour fidelity,
run integration tests against `api.github.com` with a throwaway token.

## Routing a ghkit-built client at a test server

go-github's `*Client` has a `BaseURL` field. Point it at the test server
URL and every request the SDK builds is sent there instead of
api.github.com.

```go
package myservice_test

import (
    "net/http"
    "net/http/httptest"
    "net/url"
    "testing"

    "github.com/google/go-github/v85/github"
    ghkit "github.com/pcanilho/go-github-kit"
)

func TestRouting(t *testing.T) {
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        _, _ = w.Write([]byte(`{"login":"octocat"}`))
    }))
    defer srv.Close()

    hc, err := ghkit.HTTPClient(ghkit.WithToken("test"))
    if err != nil { t.Fatal(err) }
    gh := github.NewClient(hc)
    base, _ := url.Parse(srv.URL + "/")
    gh.BaseURL = base // trailing slash required; go-github appends API paths
    _ = gh
}
```

## Recipe: handling ETag 304 replays

```go
package myservice_test

import (
    "net/http"
    "net/http/httptest"
    "net/url"
    "testing"

    "github.com/google/go-github/v85/github"
    ghkit "github.com/pcanilho/go-github-kit"
    "github.com/pcanilho/go-github-kit/etag"
    "github.com/pcanilho/go-github-kit/ghtest"
)

func TestETag304(t *testing.T) {
    body := []byte(`{"login":"octocat"}`)
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if ghtest.Write304IfMatch(w, r, body) {
            return
        }
        // ETag-on-200 path: compute the same hash the request will use on replay.
        // The bored-engineer algorithm hashes Authorization + Accept + Cookie + body.
        w.Header().Set("ETag", `"`+etag.ComputeExpectedETag(r.Header, nil, body)+`"`)
        w.Header().Set("Content-Type", "application/json")
        _, _ = w.Write(body)
    }))
    defer srv.Close()

    hc, _ := ghkit.HTTPClient(ghkit.WithToken("test"), ghkit.WithETagCache())
    gh := github.NewClient(hc)
    base, _ := url.Parse(srv.URL + "/")
    gh.BaseURL = base
    _ = gh
    // drive your service: first call primes the cache, second call sees 304
}
```

## Recipe: rate-limit headers

Five header sets, no helper. Adapt the values per scenario:

```go
import (
    "net/http"
    "strconv"
    "time"
)

func writeRateLimit(w http.ResponseWriter, remaining int, reset time.Time) {
    h := w.Header()
    h.Set("X-RateLimit-Limit", "5000")
    h.Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
    h.Set("X-RateLimit-Used", strconv.Itoa(5000-remaining))
    h.Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
    h.Set("X-RateLimit-Resource", "core")
}
```

## Recipe: Link-header pagination

First page omits `prev` and `first`. Last page omits `next` and `last`.
Single-page responses (`lastPage <= 1`) omit the Link header entirely.

```go
import (
    "fmt"
    "net/http"
    "strings"
)

func writeLinkPage(w http.ResponseWriter, baseURL string, page, perPage, lastPage int) {
    if lastPage <= 1 {
        return
    }
    var parts []string
    add := func(p int, rel string) {
        parts = append(parts, fmt.Sprintf(`<%s?page=%d&per_page=%d>; rel="%s"`, baseURL, p, perPage, rel))
    }
    if page > 1 {
        add(page-1, "prev")
    }
    if page < lastPage {
        add(page+1, "next")
        add(lastPage, "last")
    }
    if page > 1 {
        add(1, "first")
    }
    w.Header().Set("Link", strings.Join(parts, ", "))
}
```

## Recipe: secondary rate limits

```go
package myservice_test

import (
    "net/http"
    "net/http/httptest"
    "testing"
    "time"

    "github.com/pcanilho/go-github-kit/ghtest"
)

func TestSecondaryLimit(t *testing.T) {
    var hits int
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        hits++
        if hits == 1 {
            ghtest.WriteSecondaryLimit(w, 1*time.Second)
            return
        }
        _, _ = w.Write([]byte(`[]`))
    }))
    defer srv.Close()
    // drive your service through srv.URL; assert it waited and retried
}
```

The helper sets `documentation_url` with the suffix `#secondary-rate-limits`
so go-github classifies the error as an `AbuseRateLimitError`. Without that
exact suffix the consumer's retry logic never triggers and the test passes
for the wrong reason.

## See also

[`migueleliasweb/go-github-mock`](https://github.com/migueleliasweb/go-github-mock)
mocks go-github's typed methods (`Repositories.Get`, `Issues.List`, etc.)
with pre-canned responses, never sending requests through an HTTP
transport. Use it when you want to assert what your code does given a
specific go-github response body. Use `ghtest` when you want to exercise
the actual ghkit transport stack (ETag, retry, rate-limit) with
GitHub-shape headers. The two are complementary; a service test suite can
use both.
