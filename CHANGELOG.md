# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

While the major version is `0`, the public API may change in a minor release. Every such change
will be listed here under **Changed** with the migration in one line.

## [Unreleased]

## [0.2.1] — 2026-08-05

Documentation only. No code changed, so there is nothing to upgrade for — `v0.2.0` and `v0.2.1`
are the same library.

### Added

- **[docs/security-review.md](docs/security-review.md)** — the component-by-component review of
  0.1.0 that produced `v0.2.0`, and the reasoning behind each fix. It was written but never
  tracked, which `v0.2.0` shipped a dangling reference to: `tests/context_test.go` cites
  "D-02 in docs/security-review.md" by finding ID. Left as written, against 0.1.0 — the
  "what breaks if you remove this line" arguments are what stop the fixes being undone — with a
  status banner and per-finding markers so it cannot be misread as a report on the current
  release. What is still open (S-06, S-07, C-05, D-04, and half of E-03) is named in the fix list.
- **`AUTH_HANDOUT.md`** and **`AUTH_INTEGRATION.md`** — the build specification for
  [kal](https://github.com/ulas96/kal), the auth library built against the seams this release
  line froze. They live here because this is the repository that owns those seams:
  `HTTPMiddleware`, `Configure` and the `crud` query options each exist for a reason recorded
  only in these two documents, and the next person to touch `Mount` needs to know what is
  pressing on them. luima itself still ships no auth, and none is planned — see `SECURITY.md`.

## [0.2.0] — 2026-08-05

A security remediation pass, plus the two seams it showed were missing. Everything below is
source-compatible: the CRUD signature changes are variadic additions, and the four new `Config`
fields have working zero values. No migration required.

The one behaviour change to know about: requests now carry a 15s deadline by default. If you have
resolvers that legitimately run longer, set `RequestTimeout` before upgrading.

### Security

- **`luimaerr.PresentError` no longer unwraps to find a `*gqlerror.Error`.** It matched with
  `errors.As`, which walks the chain, so any error *wrapping* a `gqlerror` was returned whole —
  `fmt.Errorf("insert into %s failed for tenant %d: %w", table, tenantID, gqlErr)` reached the
  client verbatim. Redaction was effectively opt-out. It is now a type assertion on the top-level
  error; gqlgen delivers its own parse and validation errors unwrapped, so nothing legitimate
  changes.
- **The resolver error log escapes its input.** `%v` → `%q`. `err` routinely carries
  attacker-controlled text, and `%v` writes newlines literally, so a caller sending
  `"x\nresolver error: all clear"` could forge a log line.
- **`db.Connect` no longer puts the connection string in its error.** A malformed DSN produced a
  `*url.Error`, which embeds the raw URL — password included — and the quickstart calls
  `log.Fatal` on it.
- **`crud.Get`, `crud.Update` and `crud.Delete` can express an ownership predicate.** They were
  hard-wired to `WherePK()`, so `WHERE personal_id = $1 AND owner_id = $2` was inexpressible
  without dropping to raw go-pg and hand-rolling the SQLSTATE classification the package exists to
  provide. luima still ships no auth; it no longer prevents you from writing it.

### Added

- **`Config.RequestTimeout`** — a deadline on the whole request, propagated into the resolver
  context. Default 15s, negative disables. Previously nothing bounded a query at any layer: go-pg
  sets no read or write timeout, `pg.ParseURL` rejects `statement_timeout` in the DSN, and the
  resolver context had no deadline to inherit.
- **`Config.DisableIntrospection`** — the sibling `DisablePlayground` never had. Turning
  introspection off previously meant abandoning `Mount`.
- **The resolver receives a real `context.Context`.** `Mount` now uses
  `adaptor.HTTPHandlerWithContext` and re-attaches Fiber's request context. Before, it was the raw
  `*fasthttp.RequestCtx`: no deadline, cancelled only at server shutdown, and everything a
  middleware put in `c.SetContext` was silently discarded. `c.Locals` still reaches
  `ctx.Value` — the re-attached context falls back to fasthttp's user values, so consumers on that
  path are unaffected.
- **`Config.HTTPMiddleware`** — `[]func(http.Handler) http.Handler`, wrapped around the gqlgen
  handler, outermost first, inside the context re-attach: a middleware receives the real request
  context — `RequestTimeout` deadline included — whatever it adds with `r.WithContext` reaches
  every resolver typed, and a `Set-Cookie` it writes survives the adaptor. The mount point for
  anything written against `net/http`: logging, tracing, tenancy, rate limiting, a session layer.
- **`Config.Configure`** — `func(*handler.Server)`, run after luima's defaults and immediately
  before mounting, so it can override them. The escape hatch for `Use`, `AroundOperations`,
  `SetRecoverFunc`, `SetParserTokenLimit` and `SetDisableSuggestion`, none of which were reachable
  while `srv` stayed a local inside `Mount`.
- **`opts ...func(*orm.Query) *orm.Query`** on `Get`, `Update` and `Delete`, matching `List`. On
  `Update`, `q.Column(...)` is also the partial update the full replace otherwise rules out.
- **`make audit`** — `govulncheck` over both modules, wired into `make check` and CI.

### Fixed

- **`?sslmode=verify-full` can connect.** `pg.ParseURL` builds the `tls.Config` for `verify-ca`
  and `verify-full` but never sets `ServerName`, and crypto/tls refuses a handshake without it. The
  one mode that verifies anything failed at boot, and four documents said otherwise. `Connect` now
  fills it in from the address.
- **`db.Connect` cannot hang forever.** The boot-time `select 1` ran with no context and no
  deadline — `DialTimeout` bounds the dial only, and go-pg leaves the read and write timeouts at
  zero — so a host that completed the TCP handshake and then stalled blocked startup indefinitely.
  It is now bounded by `?connect_timeout=N`, defaulting to 5s.

### Changed

- `examples/quickstart` is the production shape, with the playground and introspection opened by
  `LUIMA_DEV` rather than closed by its absence. It also sets the HTTP timeouts, adds a rate
  limiter, bounds its `users` query at 100 rows, and shuts down gracefully on SIGTERM. The library
  default is unchanged: a zero `Config` is still the good *development* configuration.
- `errorlint` added to `.golangci.yml` — the linter that would have caught the redaction bug above.
  CI actions are SHA-pinned and `golangci-lint` is version-pinned.
- `Connect` keeps its single-argument signature. A `Connect(url, opts ...func(*pg.Options))` was
  considered and rejected: a context deadline already bounds the pool wait, the socket and the
  running backend, so `RequestTimeout` covers what the variadic was for. Tuning `pg.Options`
  genuinely needs is a matter of calling `pg.ParseURL` and `pg.Connect` yourself.

## [0.1.0] — 2026-08-03

Initial release.

### Added

- **`luima`** — root package re-exporting the four below, so the common case is one import.
  `luima.Config` is a type alias for `server.Config`, not a copy, so the two spellings are
  interchangeable.
- **`luima/server`** — `Config`, `New`, `Mount`. Mounts a gqlgen handler on Fiber v3 with the
  query cache, introspection extension, complexity limit and error presenter all configured;
  `New` builds an app, `Mount` takes any `fiber.Router` including a group.
- **`luima/crud`** — `Get`, `List`, `Create`, `Update`, `Delete`. Generic helpers over go-pg that
  classify driver errors: `pg.ErrNoRows` becomes `(nil, nil)` so a missing row renders as GraphQL
  `null`, `23505` becomes a client-visible conflict, and a `List` seeds a non-nil slice so an empty
  table marshals as `[]`. All take `orm.DB`, so they work inside a transaction unchanged.
- **`luima/luimaerr`** — `CustomError`, `PresentError`, `SQLState`. `PresentError` passes
  `*CustomError` and `*gqlerror.Error` through and redacts everything else, so raw driver text
  never reaches an unauthenticated caller. `SQLState` wraps the `errors.As` dance whose target type
  (`pg.Error`, an interface) is easy to get wrong.
- **`luima/db`** — `Connect`. Returns its error rather than calling `log.Fatal`, and proves the
  connection with an eager `select 1` because `pg.Connect` dials nothing.
- **`examples/quickstart`** — a complete server as a nested module, built by CI.
- Documentation for the gqlgen contract, the Fiber integration, deployment, and a 26-entry gotcha
  register.

### Notes

- `Create` and `Update` issue `RETURNING *`, which the server this was extracted from deliberately
  did not. A library serves tables it has never seen: without it, a table with a `DEFAULT now()`,
  a trigger, an identity column or a generated column makes a mutation answer with the value the
  client *sent* rather than the value Postgres *stored*. Same statement, same round trip.
- `Update` is a full replace. `UpdateNotZero` would skip zero values, which means an empty slice
  could not clear an array column — a silent data-retention bug rather than a partial-update
  feature. Partial updates need a real design and are not in this release.
- `Update`'s absence signal is checked two ways. A plain `UPDATE` succeeds with
  `RowsAffected() == 0`, but with `RETURNING *` go-pg reports zero rows as `pg.ErrNoRows` instead.
  Found by running the round-trip test against a real Postgres.

### Not included, deliberately

Auth, pagination, filtering, dataloaders, subscriptions, file upload, migrations, a scaffolding
CLI. Subscriptions are blocked by architecture rather than effort: `adaptor.HTTPHandler` buffers
the whole response, so a streaming transport cannot work through it.

[Unreleased]: https://github.com/ulas96/luima/compare/v0.2.1...HEAD
[0.2.1]: https://github.com/ulas96/luima/releases/tag/v0.2.1
[0.2.0]: https://github.com/ulas96/luima/releases/tag/v0.2.0
[0.1.0]: https://github.com/ulas96/luima/releases/tag/v0.1.0
