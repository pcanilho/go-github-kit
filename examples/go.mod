module github.com/pcanilho/go-github-kit/examples

go 1.26.2

require (
	github.com/google/go-github/v85 v85.0.0
	github.com/pcanilho/go-github-kit v1.0.0
	golang.org/x/oauth2 v0.36.0
)

require (
	github.com/gofri/go-github-ratelimit/v2 v2.0.2 // indirect
	github.com/google/go-querystring v1.2.0 // indirect
	github.com/hashicorp/golang-lru/v2 v2.0.7 // indirect
	golang.org/x/time v0.15.0 // indirect
)

replace github.com/pcanilho/go-github-kit => ../
