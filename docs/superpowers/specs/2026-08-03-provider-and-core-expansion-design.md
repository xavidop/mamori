# Provider and core expansion

**Goal:** add seven REST-backed providers and four core features on top of one shared
HTTP resolve core, then migrate the sixteen existing `net/http` providers onto that
core. Sixteen independent pull requests.

**Status:** design approved, pending spec review.

## Scope: this is a program, not a feature

Sixteen deliverables. `providers/cloudflare-kv`, the most recent REST provider, is
2,689 lines across nine files plus a 296-line README, a docs-site page, two coverage
table rows, and a skill entry. Eight providers at that bar is roughly 20,000 lines
before the four core features are written.

So this document is a **program spec**. It fixes the decomposition, the shared
contracts every deliverable inherits, and the full design of PR1, which is the only
deliverable that blocks others. Deliverables 2 through 12 are scoped here (what each
one is, what done means, what it depends on) and each gets its own design spec and
implementation plan before it is built. That boundary is deliberate, not deferred
work: PRs 2 through 8 depend on vendor API research that has not happened yet, and
writing their wire shapes now would be inventing them.

## Delivery

One branch per deliverable, all off `main`. PR1 merges first; nothing else depends on
anything else.

| # | Branch | Deliverable | Blocked by |
| --- | --- | --- | --- |
| 1 | `xavier/httpcore` | `providers/httpcore` + `providers/https` | none |
| 2 | `xavier/infisical` | `providers/infisical` | PR1 |
| 3 | `xavier/hcp-vault-secrets` | `providers/hcp-vault-secrets` | PR1 |
| 4 | `xavier/bitwarden` | `providers/bitwarden` | PR1 |
| 5 | `xavier/nacos` | `providers/nacos` | PR1 |
| 6 | `xavier/supabase` | `providers/supabase` | PR1 |
| 7 | `xavier/heroku` | `providers/heroku` | PR1 |
| 8 | `xavier/posthog` | `providers/posthog` | PR1 |
| 9 | `xavier/bootstrap-cache` | `WithBootstrapCache` | none |
| 10 | `xavier/rotation-window` | `?stage=` dual-credential window | none |
| 11 | `xavier/secret-generic` | `secret.Value[T]` | none |
| 12 | `xavier/examples` | runnable examples | 9 and 11, for the demo |
| 13 | `xavier/migrate-poll-1` | migrate azblob, azure, cosmos, doppler, firebase-rc | PR2-8 |
| 14 | `xavier/migrate-poll-2` | migrate flipt, growthbook, onepassword, scaleway-sm, unleash, vault, vercel-gc, cloudflare-kv | PR2-8 |
| 15 | `xavier/httpcore-sse` | `httpcore` SSE, migrate firebase-rtdb and mamori | PR2-8 |
| 16 | `xavier/migrate-consul` | migrate consul onto `LongPoll` | PR2-8, needs `LongPoll` from PR5 |

A stack was considered and rejected. Only PR1 is a genuine dependency, so an
eight-deep chain would buy nothing and would restack seven branches every time a
review comment landed on the bottom one.

## Contracts every deliverable inherits

**Documentation ships with the feature.** Each PR carries, as applicable: the module
`README.md`, the `site/src/pages/docs/**` page, the provider coverage table in the
root `README.md`, the coverage table in `site/src/pages/docs/providers/index.md`, and
`skills/mamori/references/providers.md`. This is the standing rule in this repo, not
a per-PR judgment call.

**Testing.** Each provider PR carries `providertest.Run` with `Fail` and `Clear`
wired so the `ErrorClassification` case runs, a table test mapping real backend error
values to `mamori.ErrorKind`, `PointerRef` supplied wherever `#key` selects into a
JSON payload, and a `//go:build integration` test that skips when its environment
variables are unset. CI vets the integration build tag but never runs it, matching
the existing 25 providers.

**Wire shapes come from vendor documentation.** Before writing each adapter, fetch
the vendor's current REST reference and pin the endpoint, auth header, JSON shape,
and status codes. Cite the documentation URL in the module README's `Testing status`
table. A fake test written from an assumed shape only confirms the assumption, so the
README must state plainly which rows are verified against documentation and which
require a live backend.

**Commits.** Conventional Commits, one logical change per PR. `feat(<module>):` for a
new provider, `feat(core):` for a core feature, `docs:` for documentation-only work.

## PR1: `httpcore` and `https`

### Why this exists

Sixteen of the thirty-seven providers use `net/http` with no shared helper. Each
hand-rolls request building, auth injection, status mapping, response body bounding
and draining, and JSON decoding. Closed issue #107, "HTTP response-body hygiene is
inconsistent across the HTTP-based providers", was that duplication surfacing as a
bug. Five providers additionally hand-roll conditional-GET polling: `s3` on ETag,
`gcs` on generation, `azblob` on ETag, `cosmos` on ETag, and `vercel-gc` on a digest.

Seven of the eight providers requested here are REST APIs. Building them the current
way writes that layer seven more times.

### `providers/httpcore`

A public module, `github.com/xavidop/mamori/providers/httpcore`, standard library
only apart from its `mamori` dependency for the error sentinels. It is public rather
than internal because `CONTRIBUTING.md` actively recruits third-party provider
authors, and Go's internal rule would block anyone outside
`github.com/xavidop/mamori/`. It is its own module rather than a core package so that
core's public API and compatibility promise are untouched.

Three bounded units.

**`Client`** owns one round trip against one backend.

```go
type Config struct {
    BaseURL    string       // required
    HTTPClient *http.Client // nil -> a client with DefaultTimeout
    Auth       Authenticator
    MaxBody    int64        // <= 0 -> DefaultMaxBody (1 MiB)
    UserAgent  string
}

func New(cfg Config) (*Client, error)

type Request struct {
    Method          string // "" -> GET
    Path            string // joined onto BaseURL
    Query           url.Values
    Header          http.Header
    Body            []byte
    IfNoneMatch     string
    IfModifiedSince string
}

type Response struct {
    Status       int
    Body         []byte // nil when NotModified
    ETag         string
    LastModified string
    NotModified  bool
}

func (c *Client) Do(ctx context.Context, r Request) (*Response, error)
```

`Do` never retries. Core's `backoff.go` already owns retry for a failed resolve, and
a second retry layer inside the provider would multiply against it. `Do` always
drains and closes the body so connections are reused, and reads at most `MaxBody`
bytes so a hostile or broken backend cannot exhaust memory. `Client` is safe for
concurrent use.

**`Authenticator`** injects credentials, never logs them.

```go
type Authenticator interface {
    Apply(ctx context.Context, req *http.Request) error
}

func Bearer(token string) Authenticator
func HeaderAuth(name, value string) Authenticator
func BasicAuth(user, pass string) Authenticator
func QueryAuth(name, value string) Authenticator
func OAuth2ClientCredentials(cfg OAuth2Config) Authenticator
```

`OAuth2ClientCredentials` caches the access token and refreshes it before expiry,
which is what PR3 (HCP Vault Secrets) needs and what several vendors require. Its
token exchange is itself a `Client` call, so it inherits the same body bounding and
status classification.

**`ClassifyStatus`** is the single status-to-sentinel table.

```go
func ClassifyStatus(status int, detail string) error
```

Returns nil for 2xx and 304. Otherwise a wrapped `mamori` sentinel: 400 to
`ErrInvalid`, 401 to `ErrUnauthenticated`, 403 to `ErrPermissionDenied`, 404 to
`ErrNotFound`, 408 and 429 to `ErrRateLimited`, 5xx to `ErrUnavailable`, everything
else to `ErrUnavailable`. Wrapping uses `%w` so `errors.Is` keeps working through the
chain.

`detail` is supplied by the caller, not extracted from the body by `httpcore`. A
response body may contain the value itself, and only the provider knows which field
of its vendor's error envelope is safe to surface. Making the caller pass the string
puts that decision where the knowledge is.

**`Revalidator`** turns a repeated poll into a conditional GET.

```go
func NewRevalidator(c *Client, maxEntries int) *Revalidator
func (rv *Revalidator) Get(ctx context.Context, key string, r Request) (*Response, error)
```

Keyed by the ref's raw string, it remembers the last ETag, Last-Modified, and body.
The next poll sends the validators; a 304 returns the cached body with `NotModified`
set, so an unchanged value costs one small round trip instead of a full payload. The
entry map is bounded by `maxEntries` with LRU eviction so a large config cannot grow
it without limit. Guarded by a mutex; safe for concurrent use.

**`Version`** resolves the value version consistently.

```go
func Version(resp *Response, body []byte) string
```

ETag if present, else Last-Modified, else `mamori.VersionHash(body)`.

### `providers/https`

A public module, `github.com/xavidop/mamori/providers/https`, built on `httpcore` and
serving as its proof.

**Refs name an operator-declared endpoint, not a raw URL.**

```
https://<endpoint>/<path>[#<key>][?<opts>]
```

```go
p, err := https.New(
    https.Endpoint{
        Name:    "billing",
        BaseURL: "https://api.acme.com/v1",
        Auth:    httpcore.Bearer(os.Getenv("ACME_TOKEN")),
        Query:   url.Values{"env": {"prod"}},
    },
)

type Config struct {
    DBPass secret.String `source:"https://billing/cfg#/db/pass"`
    Region string        `source:"https://billing/cfg#/region"`
}
```

Three problems forced this and it solves all three at once.

*Query parameters have nowhere to go.* mamori's grammar is
`scheme://path[#key][?opts]` with the fragment before the query, and `?opts` is
mamori's own namespace for `decode` and `debounce` (`ref.go:90-101`). A raw-URL ref
like `https://api.acme.com/cfg?env=prod#/db/pass` does not fail loudly; `ParseRef`
splits on the first `?`, so `Key` comes out empty and `Opts` comes out as `env` =
`prod#/db/pass`. Silently wrong is worse than unsupported. Registering fixed query
parameters on the endpoint removes the collision entirely.

*Credentials cannot live in a struct tag,* because a struct tag is source code. Auth
belongs to the endpoint registration, where it can be read from the environment or a
secret store at startup.

*A raw-URL provider makes every struct tag a potential SSRF.* Restricting refs to
declared endpoints matches the posture the rest of the project already takes: the
config server serves "a fixed, operator-declared table of bindings, never a
client-supplied ref", and `exec:` is opt-in for the same class of reason.

An unknown endpoint name resolves to `ErrInvalid`, so `mamori doctor` catches a typo
before deployment rather than at the first poll.

**Endpoint.**

```go
type Endpoint struct {
    Name          string // ref authority
    BaseURL       string // required
    Auth          httpcore.Authenticator
    Query         url.Values
    Header        http.Header
    Client        *http.Client
    MaxBody       int64
    Sensitive     bool // mark resolved Values sensitive
    AllowInsecure bool // permit an http:// BaseURL
}
```

`New` rejects a `BaseURL` with an `http://` scheme unless `AllowInsecure` is set,
since fetching configuration in cleartext is a footgun that should be opted into
explicitly. `Sensitive` is per-endpoint because a generic HTTP endpoint may serve
either secrets or plain configuration, and mamori cannot infer which.

**Selection, watching, options.** `#key` is handed to `mamori.SelectKey`, so both a
plain key and an RFC 6901 pointer work. There is no native watch: the provider does
not implement `WatchableProvider`, mamori wraps it in the polling adapter, and each
poll goes through `httpcore.Revalidator` so it is a conditional GET. Unrecognized
`?opts` are passed through untouched, which is what the `providertest` `DecodeOption`
case asserts.

**Testing.** `providertest.Run` against an `httptest.Server` fake. `Fail` makes the
fake return a chosen status so `ErrorClassification` runs; `Clear` makes it return
404. `PointerRef` is supplied, since `#key` selects into a JSON payload.

### Deferred out of PR1, but in the program

Retro-fitting the existing sixteen `net/http` providers is PR13 through PR16, not
PR1. PR1 must stay a reviewable module.

Server-sent events are PR15. `httpcore` grows an SSE unit there and `firebase-rtdb`
and `mamori` migrate onto it in the same PR, so the abstraction lands with both of
its consumers rather than ahead of them.

Long polling is PR5. Nacos needs it, `consul` already hand-rolls it, so `httpcore`
gains `LongPoll` when the new provider forces the question and `consul` harvests it
in PR16.

## Deliverables 2 through 8: the REST providers

Each is a `providers/<name>` module built on `httpcore`, inheriting the contracts
above. The design of each, including its ref grammar and its error table, is settled
in its own spec after its vendor documentation is fetched. What is fixed now is scope
and the reason each one is not merely an `https://` recipe.

| # | Module | Scheme | Why it needs code, not a recipe |
| --- | --- | --- | --- |
| 2 | `providers/infisical` | `infisical://` | Project, environment, and secret path form a structured ref; error envelope needs mapping |
| 3 | `providers/hcp-vault-secrets` | `hcp-vs://` | OAuth2 client-credentials exchange with token caching; distinct product and API from `vault://` |
| 4 | `providers/bitwarden` | `bws://` | Client-side cryptography; the value is decrypted locally, not served in the clear |
| 5 | `providers/nacos` | `nacos://` | Long-poll listener protocol gives it a native watch, so it implements `WatchableProvider`. **Also adds `httpcore.LongPoll`**, which PR16 harvests for `consul` |
| 6 | `providers/supabase` | `supabase://` | Reads `vault.decrypted_secrets` through PostgREST; needs its own row and column shaping |
| 7 | `providers/heroku` | `heroku://` | Requires the Heroku API `Accept` version header and returns all config vars in one document, so it is a `BatchProvider` |
| 8 | `providers/posthog` | `posthog://` | Flag evaluation is a POST with a distinct id, not a GET; result is a facet on an evaluated payload |

PR5 is the only one expected to implement `WatchableProvider`. PR7 is the only one
expected to implement `BatchProvider`. Both claims are provisional until the vendor
documentation confirms them, and either may change in that deliverable's own spec.

## Deliverables 9 through 12: the core features

Each gets its own design spec before implementation. Scope and the problem each
solves are fixed here.

**PR9, `WithBootstrapCache`.** `middleware.Cache` is memory-only and nothing in the
project persists a resolved snapshot. A process that is already running survives a
backend outage, because `Get()` keeps serving the last valid config, but a process
that restarts during one cannot boot at all, even when the configuration has not
changed in weeks. The feature writes an encrypted snapshot on every applied update
and, on a cold start where the backend is unreachable, boots from it. `Status()` must
report that it is serving from the cache and how stale that snapshot is, because a
silent fallback to old configuration is worse than a failure to start. Open questions
for its spec: the encryption key source, whether a stale bootstrap satisfies
`Health()`, and how `Doctor` reports it.

**PR10, the dual-credential rotation window.** `providers/aws/sm.go:79` calls
`GetSecretValue` without `VersionStage`, so `AWSPREVIOUS` and `AWSPENDING` are
unreachable. During a Secrets Manager rotation both the old and the new credential
are briefly valid, which is exactly the window a service needs in order to rotate
without dropping connections. `PreApply` can prove a new credential works but cannot
hold both. A `?stage=` ref option exposes the staging label. Open question for its
spec: whether this stays AWS-specific or becomes a general ref option that other
versioned backends can honour.

**PR11, `secret.Value[T]`.** `secret/secret.go` has only `String` and `Bytes`, so the
redaction guarantee ends at the first non-string secret: a numeric key or a
structured credential blob has to be held in the clear. A generic `secret.Value[T]`
carrying the same `Reveal`, `String`, `GoString`, `MarshalJSON`, `LogValue`,
`Sensitive`, `IsZero`, `Clone`, and `Zero` surface closes that. Open questions for its
spec: whether `String` and `Bytes` become aliases or stay as they are for
compatibility, and how `mamori vet` recognises the generic form.

**PR12, examples.** `examples/basic` is the only runnable example for a surface that
spans source chains, `PreApply`, `WithDerive`, pin and history, the admin endpoint,
and the config server. This PR adds runnable examples covering the paths a new user
is most likely to get wrong. It lands after PR9 and PR11 so it can demonstrate them.

## Deliverables 13 through 16: migrating the existing HTTP providers

Sixteen providers use `net/http` with no shared helper today. Migrating them onto
`httpcore` is the reason `httpcore` is worth building: it is what turns issue #107
from a recurring class of bug into a single place to fix.

**Sequencing.** These land after PR2 through PR8, not before. Eight new consumers
(`https` plus seven adapters) exercise the API first, so that if they reveal a gap it
is fixed once, in `httpcore`, rather than after sixteen modules have already committed
to the wrong shape.

**What makes this verifiable.** Every one of the sixteen already passes
`providertest.Run` and carries its own unit tests. A migration that preserves
behaviour keeps them green, and a migration that does not fails loudly. No new test
strategy is needed; the safety net exists. Each PR must show its modules' conformance
runs passing before and after, and must not change any provider's public API,
`Scheme()`, ref grammar, or error mapping. This is a refactor, and any behaviour
change found along the way belongs in a separate PR.

| PR | Modules | Shape |
| --- | --- | --- |
| 13 | azblob, azure, cosmos, doppler, firebase-rc | Poll-only. First migration batch, kept small so the pattern is established under real review before it is applied at scale |
| 14 | flipt, growthbook, onepassword, scaleway-sm, unleash, vault, vercel-gc, cloudflare-kv | Poll-only. Applies the pattern PR13 established |
| 15 | firebase-rtdb, mamori | Adds `httpcore` SSE and migrates both streaming consumers in the same PR |
| 16 | consul | Migrates onto `httpcore.LongPoll`, which PR5 built for Nacos |

**PR15 is the one to watch.** `providers/mamori` is 3,485 lines, the largest of the
sixteen, and it is the client half of the config server's wire protocol. Its SSE
handling is load-bearing for `server/`, so PR15 carries more risk than PR13 and PR14
combined. Splitting it further is an acceptable outcome if its own spec finds the
SSE unit does not fit both consumers cleanly.

**`vault` migrates its resolve path only.** Its lease-aware refresh reads `NotAfter`
from the lease and is not an HTTP concern, so it stays where it is.

## Risks

**Vendor API drift.** Seven adapters are written against documentation rather than
live calls. Mitigation: cite the documentation URL per module, keep the `Testing
status` table honest about what is unverified, and ship a runnable integration test
so the first person with credentials can confirm the shape in one command.

**`httpcore` is load-bearing before it has users.** Its API is fixed by PR1 but only
exercised by one provider until PR2. Mitigation: PR1 ships `https` on top of it as a
real consumer rather than a demonstration; `httpcore` is a separate module on its own
tag, so a v0 API change does not force a core release; and the sixteen migrations are
sequenced after eight consumers have exercised it.

**The migrations touch working code.** PR13 through PR16 change sixteen providers
that work today, for an internal benefit rather than a user-visible one. Mitigation:
every one of them already passes the conformance kit, so the refactor is verifiable
rather than hopeful, and the batches are ordered so the smallest establishes the
pattern first. `providers/mamori` in PR15 carries the most risk, as noted above.

**Program size.** Sixteen PRs is months of review, not one sitting. Mitigation: the
decomposition above means every PR is independently reviewable and mergeable, and
stopping after any one of them leaves the project in a coherent state. PR1 through
PR12 deliver all of the user-visible value; PR13 through PR16 are internal cleanup
that can be dropped without losing any of it.
