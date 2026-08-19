# Testing code that uses ghkit

The `ghtest` sub-package ships four helpers for the GitHub-specific traps in
testing ghkit-using code:

| Helper | What it covers |
|---|---|
| `WriteSecondaryLimit` | 403 plus `Retry-After` and the `documentation_url` suffix go-github classifies on |
| `Write304IfMatch` | The bored-engineer ETag hash domain, comma-splitting `If-None-Match` correctly |
| `ETagServer` | A whole `httptest.Server` that computes ETags and synthesises 304s, so you do not hand-roll the handler |
| `LinkHeader` | RFC 8288 `Link` headers for pagination fixtures |

Everything else is plain stdlib code, shown inline as recipes you can copy.

`ghtest` is shape-correct, not behaviour-correct. It does not enforce
rate-limit budgets or run a real ETag database. For behaviour fidelity,
run integration tests against `api.github.com` with a throwaway token.

## Routing a ghkit-built client at a test server

Pass the test server URL to `github.WithURLs` at construction and every
request the SDK builds is sent there instead of
api.github.com.

```go
package myservice_test

import (
    "net/http"
    "net/http/httptest"
    "testing"

    "github.com/google/go-github/v90/github"
    ghkit "github.com/pcanilho/go-github-kit"
)

func TestRouting(t *testing.T) {
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        _, _ = w.Write([]byte(`{"login":"octocat"}`))
    }))
    defer srv.Close()

    hc, err := ghkit.HTTPClient(ghkit.WithToken("test"))
    if err != nil { t.Fatal(err) }
    base := srv.URL + "/"
    gh, err := github.NewClient(github.WithHTTPClient(hc), github.WithURLs(&base, nil))
    if err != nil { t.Fatal(err) }
    _ = gh
}
```

## Recipe: handling ETag 304 replays

```go
package myservice_test

import (
    "net/http"
    "net/http/httptest"
    "testing"

    "github.com/google/go-github/v90/github"
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
    base := srv.URL + "/"
    gh, err := github.NewClient(github.WithHTTPClient(hc), github.WithURLs(&base, nil))
    if err != nil { t.Fatal(err) }
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

`ghtest.LinkHeader(baseURL, page, perPage, lastPage)` builds the RFC
8288 Link header value for a given fixture page. First page omits
`prev` and `first`; last page omits `next` and `last`; `lastPage <= 1`
returns an empty string so the handler can skip setting the header on
single-page responses.

```go
import (
    "net/http"
    "net/http/httptest"

    "github.com/pcanilho/go-github-kit/ghtest"
)

func paginatedFixture(t *testing.T, perPage, lastPage int) *httptest.Server {
    var srv *httptest.Server
    srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        page := 1
        if p := r.URL.Query().Get("page"); p != "" {
            page, _ = strconv.Atoi(p)
        }
        base := srv.URL + r.URL.Path
        if link := ghtest.LinkHeader(base, page, perPage, lastPage); link != "" {
            w.Header().Set("Link", link)
        }
        // ... write the page body
    }))
    return srv
}
```

Pair this with the `pages` sub-package on the call site to walk the
fixture without writing the loop yourself; see the
"Iterating over paginated results" recipe in the README.

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
