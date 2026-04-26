# Security Policy

`go-github-kit` is a solo-maintained open-source Go library. This document describes how to report a vulnerability, what is in scope, which versions receive security fixes, and what reporters should expect from the maintainer.

## Reporting a Vulnerability

Please report suspected vulnerabilities through **GitHub Private Vulnerability Reporting**:

- Open a private report at <https://github.com/pcanilho/go-github-kit/security/advisories/new>
- Or, from the repository, use the **Security** tab → **Report a vulnerability**

**Please do not open a public issue, pull request, or discussion for security reports.** Public reports expose users before a fix is available.

PVR is the only supported reporting channel. It keeps the report private, links the disclosure to a GitHub Security Advisory (GHSA) and CVE, and lets the maintainer collaborate with the reporter on a fix without a public trail.

## What to Include

A useful report typically contains:

- The affected version or commit hash
- Reproduction steps or a proof-of-concept
- An impact assessment (what an attacker could achieve)
- Any known mitigations or workarounds

## Supported Versions

| Version                | Supported          |
| ---------------------- | ------------------ |
| `1.x` (latest minor)   | :white_check_mark: |
| `< 1.x` (older minors) | :x:                |

Older minor releases reach end-of-life when the next minor ships. Consumers should track the latest minor of `1.x` to receive security fixes.

### Supported Go Versions

Security fixes target the two most recent stable Go releases, per [Go's own release policy](https://go.dev/doc/devel/release).

## Scope

### In scope

Code in this repository:

- The top-level `ghkit` package
- The `etag/`, `ratelimit/`, and `throttle/` subpackages

### Out of scope: `google/go-github`

`ghkit.New` is generic over the returned client type, and **no part of this kit's main module imports `github.com/google/go-github`**. It is absent from the kit's `go.mod`, source tree, and compiled binary. Consumers wire whichever `go-github` major they choose via the generic factory (see the [README's "Using a different go-github version"](https://github.com/pcanilho/go-github-kit#using-a-different-go-github-version) section, and runnable starters in [`examples/`](https://github.com/pcanilho/go-github-kit/tree/main/examples)).

`go-github` vulnerabilities are therefore not in scope for this kit's advisories. Please report and track them via the [`google/go-github` advisory channel](https://github.com/google/go-github/security) directly. The same applies to any other client library you wire into the generic factory.

### Out of scope: runtime transitive dependencies

The kit's runtime transitive dependencies (those that *do* link into consumers' binaries when they import the kit or its subpackages) are:

| Dependency                                  | Used by                  |
| ------------------------------------------- | ------------------------ |
| `github.com/gofri/go-github-ratelimit/v2`   | `ratelimit/`             |
| `github.com/hashicorp/golang-lru/v2`        | `etag/`                  |
| `golang.org/x/oauth2`                       | root pkg (`WithToken`)   |
| `golang.org/x/time`                         | `etag/`, `throttle/`     |

Vulnerabilities in these are not in scope for this kit's own advisories. Please report them upstream via the dependency's own channel. When an upstream vulnerability in one of these materially affects users of this kit, a **dependent GitHub Security Advisory may be published** at the maintainer's discretion, referencing the upstream advisory so consumers' SCA tools surface the issue. This is best-effort; no patch, fork, or backport obligation is implied if upstream is unfixed.

## Response Process

Reports are handled on a best-effort basis. The general flow is:

1. Triage the report and confirm the vulnerability
2. Coordinate a fix or workaround with the reporter
3. Publish a GHSA (and request a CVE where appropriate) once a fix or mitigation is available
4. Credit the reporter in the advisory unless they request otherwise

**No specific response, fix, or disclosure timelines are committed.** This is a solo-maintained project; promising SLAs that vacations or busy weeks would silently break would be worse than promising none.

## Safe Harbor

Good-faith security research conducted under this policy is welcomed. The maintainer will not pursue legal action against researchers who:

- Report vulnerabilities through the channel described above
- Make a good-faith effort to avoid privacy violations, service disruption, and destruction of data
- Do not exploit a vulnerability beyond what is necessary to demonstrate the issue
- Give the maintainer a reasonable opportunity to address the issue before any public disclosure

## No Bug Bounty

`go-github-kit` is a solo-maintained open-source project. There is no bug bounty program. Reports are accepted, taken seriously, and credited, but unpaid.
