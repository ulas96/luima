# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

While the major version is `0`, the public API may change in a minor release. Every such change
will be listed here under **Changed** with the migration in one line.

## [Unreleased]

## [0.4.0] — 2026-08-12

A consumer can now write a complete, production-shaped luima server without importing Fiber.
`examples/quickstart/main.go` is the proof: it imports `github.com/ulas96/luima` and nothing from
`github.com/gofiber`, where before it named a Fiber type for timeouts, for a rate limiter and for
graceful shutdown. Everything below is additive except the two entries under **Changed**.

### Added

- **`Run(ctx, addr, cfg)`** — build, listen, and block until `ctx` is done, then drain in-flight
  requests and return. The whole server in one call, naming no Fiber type; a signature assertion in
  `tests/luima_test.go` makes the compiler enforce that rather than review.

  It replaces `New` + `app.Listen(addr, fiber.ListenConfig{GracefulContext: ctx})`, and not only for
  the import. Fiber's own graceful path throws the shutdown error away — it hands it to the
  `OnPostShutdown` hook while `Listen` returns nil regardless, so a server that force-closed live
  connections because the drain timed out is indistinguishable from a clean exit and the process
  exits 0. `Run` returns it. It also removes the ordering trap that kept the quickstart off `New`:
  `New` mounts before it returns, so a later `app.Use` lands behind `/graphql` and never runs. With
  `Run` there is no app to register on and `HTTPMiddleware` is the only seam — which is where
  middleware belonged anyway, because it is the only layer that sees the resolvers' context.

  The drain window is 10s, matching Fiber's own `ListenConfig.ShutdownTimeout` default. `Run` binds
  dual-stack `tcp`, where `app.Listen` defaults to `tcp4`.

- **`CORS(CORSConfig{...})`** — cross-origin access as `func(http.Handler) http.Handler`, for
  `Config.HTTPMiddleware`. luima already answers the OPTIONS preflight — `transport.Options` is why
  it is a 200 and not a 405 — but it set no `Access-Control-Allow-Origin`, so the browser refused
  the response anyway and its error named neither field. The misconfiguration was the default.

  Sets `Vary: Origin` on every response it touches, including refusals, which is the reason to
  prefer it over four hand-written headers: the grant is echoed from a request header, and without
  `Vary` a shared cache serves the first caller's grant to the second. Methods are fixed at GET,
  POST and OPTIONS. There is deliberately no `Credentials` knob — the combination that makes a
  wildcard origin dangerous is not representable. Use `rs/cors` in `HTTPMiddleware` if you need it;
  that needs no Fiber import either.

  One wart, documented at the symbol: `HTTPMiddleware` runs inside the adaptor and Fiber's timeout
  middleware wraps outside it, so a request that hits `RequestTimeout` is answered without CORS
  headers and the browser reports a CORS error rather than a timeout.

- **`RateLimit(n, per, key)`** — fixed-window limiter as `func(http.Handler) http.Handler`, 429 with
  `Retry-After` over the limit. This is the bound `ComplexityLimit` cannot supply: row count is not
  an input to the complexity calculation, so an unbounded `{ users { id } }` costs the same as one
  field. Fixed window, not sliding, so the real ceiling is 2n across a boundary. Counters are
  dropped wholesale at each rollover — a per-key map with no eviction would be a memory-exhaustion
  bug inside the feature that exists to prevent one. Per process: two replicas enforce 2n. `key` is
  nil for `r.RemoteAddr`; read a header instead when you are behind a proxy you control, because
  otherwise every caller shares the proxy's single bucket.

- **`Config.Health` and `Config.HealthCheck`** — a liveness path, e.g. `/healthz`. Empty disables
  it. A nil check answers 200 whenever the process is up; a non-nil error is 503 and the error text
  stays server-side. `db.Ping` already has the signature, so the common case is `HealthCheck:
  db.Ping`.

  Registered by `Mount`, so it works on a group and on an app you built yourself, and it is *not*
  wrapped by `HTTPMiddleware` — a rate limiter must not 429 the probe. The check gets a 2s deadline
  of its own and runs on its own goroutine, so a check that never reads its context still answers
  503 rather than hanging; a probe that hangs for 15s reads to a load balancer as a slow server
  rather than a broken one.

- **`db.ConnectWith(url, tune)` and `db.StatementTimeout(d)`** — the `pg.Options` tuning a DSN
  cannot express. `pg.ParseURL` accepts only `sslmode`, `application_name` and `connect_timeout`, so
  reaching anything else meant re-implementing the two things `Connect` does that are easy to lose:
  the TLS `ServerName` fill without which `?sslmode=verify-full` cannot complete a handshake, and
  the bounded boot round trip. `tune` runs after both and before `pg.Connect`.

  `StatementTimeout` is the case worth pre-writing, and the gap `SECURITY.md` already named:
  `RequestTimeout` reaches Postgres as a `CancelRequest`, which is best-effort — go-pg dials a
  second connection to send it and only logs a failure — so a query can outlive its own
  cancellation while holding a pooled connection. `statement_timeout` is enforced by the server
  whether or not the client is still there. A query that exceeds it comes back as SQLSTATE `57014`,
  readable with `luimaerr.SQLState`.

  **`Connect(url)` is unchanged** and is now `ConnectWith(url, nil)` internally, so there is one
  implementation of the TLS fix and the boot ping rather than two.

- **`Config.ReadTimeout` and `Config.WriteTimeout`** — transport deadlines, defaulting to 10s and
  30s. Zero means unset and negative disables, as with `RequestTimeout`. Setting one no longer
  requires naming a Fiber type. `Config.Fiber` still takes precedence where both are set, so an
  existing `Fiber: fiber.Config{ReadTimeout: …}` keeps working unchanged. There is no
  `IdleTimeout`: fasthttp falls back to `ReadTimeout` when it is zero, so the keep-alive wait is
  bounded by the same field.

### Changed

- **`Mount` now panics when `Config.Schema` is nil**, where before it mounted cleanly. This was
  measured, not reasoned about: `Mount(app, Config{})` returned normally, the process booted, a
  readiness check that only proved the database passed — and then every request panicked inside
  gqlgen's executor, was recovered by gqlgen's own handler, and came back as
  `{"errors":[{"message":"internal system error"}]}` with **no `extensions.code`**. That is gqlgen's
  message, not luima's, so an alert keyed on `INTERNAL_SERVER_ERROR` never fired and the first
  symptom was the volume of stack traces. A panic is right here where `db.Connect` returning an
  error is right there: an unreachable database is an environment failure worth retrying, a nil
  schema is a programmer error that cannot become valid later.

  **Migration:** none. A build this affects was already broken; it now says so at boot instead of
  once per request.

- **A zero `Config` now ships a 10s read deadline and a 30s write deadline**, where before it
  shipped neither. Fiber fills in its own defaults for `BodyLimit` and `Concurrency` but passes
  the timeouts through verbatim, and fasthttp reads zero as *no deadline* — so one client could
  hold a connection slot indefinitely by dribbling a request body a byte at a time, against a
  default `Concurrency` of 262144, and `Shutdown` could not reclaim that slot either because it
  does not close keep-alive connections. This contradicted the invariant that a zero `Config` is
  the good configuration; it no longer does.

  Only `New` — and therefore `Run` — is affected. `Mount` is handed a router that already exists
  and never set its configuration, so an app you built with `fiber.New` yourself is unchanged.

  **Migration:** if you were relying on an unbounded write deadline for a long-running query, set
  `WriteTimeout: -1`. Realistically nobody is: `RequestTimeout` already bounds the resolver at 15s,
  a full fifteen seconds before the new default, so this can only bite a consumer who disabled
  `RequestTimeout` with a negative value too.

## [0.3.0] — 2026-08-06

Two bug fixes that were filed as features, one new bound, and a documentation pass that removes a
claim this project had been repeating for three releases without measuring it.

Everything is source-compatible except one thing: `luimaerr.CustomError` gains a third field, so
an **unkeyed** composite literal — `&CustomError{"msg", err}` — no longer compiles. Use keyed
fields: `&CustomError{UserMessage: "msg", InternalError: err}`. Keyed literals, which `go vet`'s
`composites` check has always pushed you toward for a struct from another module, are unaffected.

The one behaviour change to know about: operations nested deeper than 15 levels are now rejected. If
your schema legitimately nests deeper than that, set `Config.MaxDepth` before upgrading.

### Added

- **`Config.MaxDepth`** — caps operation nesting depth. Default 15, negative disables, zero means
  unset, as with `QueryCache` and `ComplexityLimit`. `SECURITY.md` had already named the gap:
  complexity does not bound depth, because a 40-level query costs about 40 against a limit of
  1000, so a cyclic schema — `User.friends: [User!]!` — passes it and multiplies into a resolver
  call per node per level. gqlgen ships no depth limiter. The walk resolves fragment spreads out of
  `doc.Fragments`, and that is not an optimisation: a spread node carries no selection set, so a
  limiter that walks only the operation reads every named fragment as a leaf, and a 40-deep document
  behind `...F` measures 1 and executes. An inline fragment is a type condition, not a level. The
  default is chosen against the deepest document a default install serves — the playground's own
  introspection query, which measures 13.
- **`crud.Create` takes query modifiers**, like the other four. The clause it needed was
  `ON CONFLICT`, which is the only way to attempt an insert inside a transaction without a real
  23505 aborting the whole thing — a suppressed conflict does not abort it.
- **`luimaerr.CustomError.Code`** — becomes `extensions.code` on the wire. `crud.Create` sends
  `CONFLICT`, `crud.Update` sends `NOT_FOUND`, and every redacted error sends
  `INTERNAL_SERVER_ERROR`. Clients had to string-match a message that `crud.Create` builds from
  caller-supplied text, which `docs/gotchas.md` #30 already documents as attacker-influenced. An
  empty `Code` emits no extensions object, so responses that do not opt in are byte-identical to
  `0.2.1`. Nothing here is auth-shaped: `UNAUTHENTICATED` and `FORBIDDEN` are not a library that
  ships no auth's to define.
- **`docs/deployment.md` § "Serving over TLS"** — the HTTP side of deployment, which the document
  did not previously cover at all. Both shapes: `fiber.ListenConfig` for terminating TLS in the
  process (and why that is HTTP/1.1 only — fasthttp ships no h2), and running behind a
  TLS-terminating proxy, with a table of what a resolver actually sees on such a request. Three
  silent failures are named: `Config.Fiber.TrustProxy` changes `fiber.Ctx` accessors and so is
  invisible to resolvers, which hold an `*http.Request`; `r.TLS` is `nil`, so middleware that
  infers HTTPS from it drops `Secure` cookies or redirect-loops; and a prefix-stripping proxy
  breaks the playground's fetch URL, whose scheme is otherwise handled automatically.
- **`docs/gotchas.md` rows 34–37.** 34–36 are the three TLS failures above. 37 is new: adding
  `transport.MultipartForm` through `Configure` makes your endpoint reachable by a cross-site HTML
  form, because `multipart/form-data` is a "simple" request that no preflight protects — a
  mutation submitted from any origin executes with the caller's cookies. Measured. The docs
  recommended that transport in three places with no warning attached. luima grows no CSRF field,
  because it registers no transport that needs one, and `SECURITY.md` now records that the default
  is closed.
- **A request-ID `HTTPMiddleware` and a `Configure` block in the quickstart.** Both seams shipped in
  `0.2.0` with no worked example outside the test suite.

### Fixed

- **`crud.Create` no longer redacts an insert the database deliberately suppressed.** `Create`
  issues `RETURNING *`, so go-pg scans a result set — and an insert that produced no row arrives
  as `pg.ErrNoRows`, which `Create` returned bare and `PresentError` redacted. A `BEFORE INSERT`
  trigger returning `NULL` is the ordinary way to write a soft-ignore, needs no cooperation from the
  caller, and made every such mutation answer `"internal server error"` while having done exactly
  what the schema told it to. It now returns `(nil, nil)`, like `Get`. **Check the result** — a
  caller assuming non-nil nil-dereferences the second time the same key is inserted.
- **The default transports are registered after `Configure` runs.** gqlgen selects the first
  transport whose `Supports` matches and `AddTransport` appends, so registration order is
  precedence. `transport.POST` matches `POST` + `application/json` and never reads `Accept`, which
  makes it a strict superset of what SSE matches — so a transport registered through `Configure`
  could never be selected. It compiled, it mounted, it returned 200, and a subscription silently
  answered with one buffered response. Nothing can have depended on the old order, since nothing
  registered that way ever ran.
- **The reason given for subscriptions being out of scope was false**, in six files including the
  `0.1.0` entry below, which is left as written because it records what was believed then. The
  adaptor does not buffer: a `Flush`ing handler streams eight chunked frames 400 ms apart through
  `adaptor.HTTPHandlerWithContext`, and through plain `adaptor.HTTPHandler` too. What blocked
  subscriptions was the transport order above, now fixed, and fasthttp not cancelling the request
  context when a client disconnects — measured at twenty frames produced after hangup with
  `ctx.Err() == nil`. That second one is the real blocker and it is upstream: a subscription must
  disable `RequestTimeout` to outlive 15s, and disabling it removes the only bound there is.
  `transport.Websocket` was not measured and nothing claims it now.
- **`ErrorPresenter` is not the only path to the wire**, contrary to three files. gqlgen's
  transports write their own errors before an executor exists, so a malformed JSON body comes back
  as HTTP 400 with the body echoed into the message, unpresented and unredacted. Only the caller's
  own bytes are exposed — but the claim is what someone leans on when deciding they need not
  sanitize something. Measured alongside: errors gqlgen *does* hand to the presenter keep their
  codes, so parse, validation and complexity rejections are byte-identical to gqlgen's own
  `DefaultErrorPresenter`.
- **Panics are fully answered by `Configure`, and nothing further is owed.** A panic in
  `HTTPMiddleware` returns HTTP 500 and the process survives, with `RequestTimeout` enabled or not;
  a `SetRecoverFunc` installed through `Configure` routes the recovered value through
  `PresentError`, which redacts it — a DSN embedded in a panic came out as `"internal server
  error"` with the detail on stderr only. This closes the last item `docs/security-review.md` left
  half-open.
- **Two anchors in `docs/gotchas.md` never resolved** — `#14-returning-` and `#30-create-s-label`.

### Changed

- **`luimaerr.CustomError` gains a `Code` field.** Unkeyed composite literals stop compiling;
  migrate with `&CustomError{UserMessage: msg, InternalError: err}`.

### Removed

- **`docs/security-review.md`, `AUTH_HANDOUT.md` and `AUTH_INTEGRATION.md`** — the three documents
  `v0.2.1` added are no longer tracked here. No code changed; the fixes they describe are still in
  the library and still described in the `0.2.0` entry below.

## [0.2.1] — 2026-08-05

Documentation only. No code changed, so there is nothing to upgrade for — `v0.2.0` and `v0.2.1`
are the same library.

### Added

- **`docs/security-review.md`** — the component-by-component review of
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

[Unreleased]: https://github.com/ulas96/luima/compare/v0.4.0...HEAD
[0.4.0]: https://github.com/ulas96/luima/releases/tag/v0.4.0
[0.3.0]: https://github.com/ulas96/luima/releases/tag/v0.3.0
[0.2.1]: https://github.com/ulas96/luima/releases/tag/v0.2.1
[0.2.0]: https://github.com/ulas96/luima/releases/tag/v0.2.0
[0.1.0]: https://github.com/ulas96/luima/releases/tag/v0.1.0
