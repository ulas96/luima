# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```sh
make test        # go test ./...  — TestCRUD SKIPS, see below
make test-db     # sources .env, go test -v -count=1 ./...  — TestCRUD runs
make lint        # golangci-lint run
make check       # gofmt + vet + lint + test-db + example — run before any PR
make example     # build examples/quickstart, grep for unfilled resolver stubs
make example-generate  # re-run gqlgen in the example, then `git add examples/quickstart`
```

Single test: `go test -v -count=1 -run TestCRUD ./tests/` (with `DATABASE_URL` set for that one).

**A green `go test ./...` proves less than it looks.** `TestCRUD` is the only test that touches a
real driver, and it `t.Skip`s without `DATABASE_URL` — a skip still reports `ok`. Confirm
`--- PASS: TestCRUD`, not `--- SKIP`. CI pins this with a `postgres:16` service container and greps
the `-v` output. `TestCRUD` creates and drops its own `luima_test_users` table, so `DATABASE_URL`
(copy `.env.example` → `.env`, unquoted) is the whole setup.

## Architecture

luima is the boilerplate between gqlgen and Fiber v3, over go-pg. It **cannot** replace gqlgen
codegen — `generated.NewExecutableSchema` is a symbol only the consumer's own `go tool gqlgen
generate` produces. Consumers still own `gqlgen.yml`, `schema.graphqls`, `graph/resolver.go`.
`docs/gqlgen-contract.md` is that contract; `docs/gotchas.md` is the failure catalogue.

| package | what belongs there |
|---|---|
| `server/` | mounting the handler, Fiber wiring, and portable net/http middleware (`middleware.go`) |
| `crud/` | query helpers, **only if they classify an error** |
| `luimaerr/` | the error contract. Imports nothing else in luima — keep it that way |
| `db/` | connection |
| `luima.go` | re-export shim; see below |
| `tests/` | every test and runnable example for the five library packages |
| `luimagen/` | the CRUD generator's library — `Field`, `Options`, `Generate`; never imported by `luima.go`, and the one package that keeps its own tests. See `docs/luimagen.md` |

**`luima.go` is a hand-maintained shim.** Types are aliases (`luima.Config` *is* `server.Config`,
so field changes carry for free); the five generic CRUD functions are wrappers, because Go has no
alias for a generic function. A genuinely new exported symbol is invisible from the root package
until it is added here by hand. `tests/luima_test.go` asserts signature identity for the existing
fifteen — and one of those assertions does double duty: `runFn` spells `Run`'s signature out in
full, naming no Fiber type, which is the whole Fiber-seam guarantee checked by the compiler rather
than by review.

**Tests live in one `package tests` outside the packages they exercise**, so they can only reach
the exported surface — the same view a consumer has. Nothing there can touch an unexported symbol.
Trade-off: `Example` functions don't render on pkg.go.dev under their symbols (godoc binds by
directory), but they still compile and their `Output` blocks still run.

`luimagen/internal_test.go` is the one exception, and `package luimagen`. The rule buys a
consumer's-eye view of an API, and `Generate` has almost none — one call that shells out to gqlgen
and rewrites files. Everything worth pinning is in the unexported steps between: `snakeCase`'s
go-pg boundary, `lowerFirst`, the declare-vs-extend probe, `patchSource`'s splice and its import
fix. `tests/luimagen_test.go` still covers the exported surface from outside. `docs/luimagen.md` §5.

### The two load-bearing invariants

1. **Error redaction is the design.** `luimaerr.PresentError` passes through `*CustomError` (the
   resolver declared it safe) and `*gqlerror.Error` (gqlgen's own text about the client's query —
   drop that branch and every schema typo reads as "internal server error"); everything else is
   logged server-side and redacted. luima ships no auth, so this is the only thing between a caller
   and your constraint and column names. This is *why* `crud/` exists — the helpers do the
   classification so a resolver can't forget it.

2. **`server.Mount` registers `r.All(endpoint, ...)`, never `r.Post`.** gqlgen's transports dispatch
   on method themselves, so GET, POST and the OPTIONS preflight must all reach the handler.
   `Post` → every browser client 405s on preflight with nothing in the log. `TestMountRoutes` pins it.

`Mount` panics on a nil `Config.Schema`, and it is the only place luima chooses a panic over an
error. The rule is whether the caller could ever recover: an unreachable database is an environment
failure worth retrying, so `db.Connect` returns an error; a nil schema cannot become valid later.
Measured before the guard: `Mount(app, Config{})` mounted cleanly and every request then panicked
inside gqlgen's executor, answered as gqlgen's `internal system error` with no `extensions.code` —
so it was not even luima's error contract that fired. `TestMountRequiresSchema` pins it.

In `Config`, zero means *unset*, not *off* — for `RequestTimeout`, `ReadTimeout`, `WriteTimeout`,
`QueryCache`, `ComplexityLimit` and `MaxDepth`. A zero-valued `Config{}` has to be the good
configuration. Negative disables.

That invariant leaked at the Fiber seam until `ReadTimeout`/`WriteTimeout` landed: `New` passed
`cfg.Fiber` through verbatim, and Fiber assigns the three timeouts to fasthttp with no default of
its own, which reads zero as *no deadline*. `New` now fills them over `cfg.Fiber`, and a field set
in `Fiber` wins — `resolveTimeout` is where that precedence lives, and `TestTimeoutPrecedence` pins
it. `Mount` sets nothing on a router it did not create, so this reaches `New` consumers only.
`WriteTimeout` does not bound a resolver at any value: fasthttp sets that deadline after the
handler returns, unlike net/http.

## examples/quickstart

A nested module with `replace … => ../..`, so it builds against the working tree. **`go build` is
not the schema check** — gqlgen writes a stub for every unresolved schema field, and a stub
*compiles*, then panics when called. `grep -rn 'not implemented' graph/*.resolvers.go` is the check
(`make example`). CI also re-runs codegen and fails on any diff.

**`main.go` deliberately does not use the zero `Config`.** It is an application, not a library
call: development conveniences are opened by `LUIMA_DEV` rather than closed by its absence. Every
other doc shows the zero `Config` as idiomatic — it is, for a library call.

**`main.go` importing no `github.com/gofiber` package is the acceptance test for the whole
Fiber-seam effort**, and `! grep -rn gofiber examples/quickstart/main.go` is how to check it. It
used to need `fiber.New` + `luima.Mount` because `luima.New` mounts before it returns, so a later
`app.Use(limiter.New())` would land behind `/graphql` and never run; `luima.Run` plus
`RateLimit`/`CORS` in `HTTPMiddleware` retires that by construction rather than by documentation —
there is no app to register on. If a future change puts a Fiber type back in this file, the
question to answer first is which `Config` field or free function is missing.

## Conventions

- Doc comments use NatSpec tags inside ordinary godoc comments: open with the symbol name, then
  `@notice` (what), `@dev` (why it's written that way), `@param`/`@return`.
  `examples/quickstart/graph/schema.resolvers.go` is exempt — gqlgen rewrites it; `luimagen/gen.go`
  and `luimagen/patch.go` are exempt — they are one internal pipeline, and their reasoning is
  carried as prose here and in `docs/luimagen.md` §2 rather than split across thirty tag blocks.
  `luimagen/luimagen.go` and `cmd/luimagen/main.go` are not exempt: they are the exported surface.
- Comments explain what breaks if the line is removed (`All` not `Post`, the two absence signals in
  `Update`, `RETURNING *`). If code moves, the comment moves with it.
- A behaviour change needs a test that fails without it; new public API needs a godoc example in
  `tests/`; update `CHANGELOG.md` under `## [Unreleased]`.

## Out of scope, deliberately

Auth, pagination, filtering, dataloaders, subscriptions, file upload, migrations, a scaffolding CLI.

The one exception is `cmd/luimagen`, a separate binary that scaffolds a table's CRUD layer —
`github.com/ulas96/luima` itself gains no new surface from it, since nothing in the library imports
it. Full reasoning: `docs/luimagen.md` §1.

Also out, and each for a reason that survives being asked again: **a routing facade** (wrapping
`Group`/`Get`/`Static` re-implements Fiber behind a worse API — `New` returning the app is the
honest exit); **a `Use []fiber.Handler` field** (the field's *type* is the import, for exactly the
consumers avoiding it); **a `Server` type** (`New` already returns the app, and `Run` covers
listen-and-drain — add one the day a consumer arrives holding a built server it starts later);
**`Access-Control-Allow-Credentials`** (luima ships no auth, so it would only ever be set with the
origin wildcard that makes it a vulnerability — leaving the field out makes that unrepresentable,
which is stronger than validating it); and **anything that makes `Mount` less than the whole
library** — every addition is a `Config` field or a free function.

`CORS` and `RateLimit` are in, and the line they do not cross is worth stating: both are stdlib-only
`func(http.Handler) http.Handler`, so they add no dependency and no opinion that a consumer cannot
replace by not calling them. Rate limiting is not "real" rate limiting — it is per process, and
`SECURITY.md` still says put the shared-store version in the layer that does auth.

Subscriptions are not implemented, and the reason recorded here until 0.3.0 — that the adaptor
buffers the whole response — is false. Measured: a `Flush`ing handler streams correctly through
`adaptor.HTTPHandlerWithContext`, eight chunked frames 400 ms apart. What actually blocked them was
`Mount` registering `transport.POST` before `Configure` ran, so a `Configure`-registered SSE
transport was never selected — fixed in 0.3.0. What still blocks them is fasthttp: it does not
cancel the request context when the client disconnects (see `withFiberContext`), so an abandoned
stream has no upper bound once `RequestTimeout` is disabled — which a subscription requires. That
one is upstream. `transport.Websocket` was not measured; the adaptor implements `http.Hijacker` as
of fasthttp v1.72 (`fasthttpadaptor/adaptor.go`), so it is no longer structurally blocked, but this
file does not claim it works.
