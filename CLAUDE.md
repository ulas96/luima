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
| `server/` | mounting the handler, Fiber wiring |
| `crud/` | query helpers, **only if they classify an error** |
| `luimaerr/` | the error contract. Imports nothing else in luima — keep it that way |
| `db/` | connection |
| `luima.go` | re-export shim; see below |
| `tests/` | every test and runnable example, for all five packages |

**`luima.go` is a hand-maintained shim.** Types are aliases (`luima.Config` *is* `server.Config`,
so field changes carry for free); the five generic CRUD functions are wrappers, because Go has no
alias for a generic function. A genuinely new exported symbol is invisible from the root package
until it is added here by hand. `tests/luima_test.go` asserts signature identity for the existing
nine.

**Tests live in one `package tests` outside the packages they exercise**, so they can only reach
the exported surface — the same view a consumer has. Nothing there can touch an unexported symbol.
Trade-off: `Example` functions don't render on pkg.go.dev under their symbols (godoc binds by
directory), but they still compile and their `Output` blocks still run.

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

In `Config`, zero means *unset*, not *off*, for `QueryCache` and `ComplexityLimit` — a zero-valued
`Config{}` has to be the good configuration. Negative disables.

## examples/quickstart

A nested module with `replace … => ../..`, so it builds against the working tree. **`go build` is
not the schema check** — gqlgen writes a stub for every unresolved schema field, and a stub
*compiles*, then panics when called. `grep -rn 'not implemented' graph/*.resolvers.go` is the check
(`make example`). CI also re-runs codegen and fails on any diff.

**`main.go` deliberately does not use the zero `Config`, and does not use `luima.New`.** It is an
application, not a library call: development conveniences are opened by `LUIMA_DEV` rather than
closed by its absence, and `fiber.New` + `luima.Mount` is required because `luima.New` mounts
before it returns, so a later `app.Use(limiter.New())` would land behind `/graphql` and never run.
Every other doc shows the zero `Config` as idiomatic — it is, for a library call. Do not
"simplify" this file back to it.

## Conventions

- Doc comments use NatSpec tags inside ordinary godoc comments: open with the symbol name, then
  `@notice` (what), `@dev` (why it's written that way), `@param`/`@return`.
  `examples/quickstart/graph/schema.resolvers.go` is exempt — gqlgen rewrites it.
- Comments explain what breaks if the line is removed (`All` not `Post`, the two absence signals in
  `Update`, `RETURNING *`). If code moves, the comment moves with it.
- A behaviour change needs a test that fails without it; new public API needs a godoc example in
  `tests/`; update `CHANGELOG.md` under `## [Unreleased]`.

## Out of scope, deliberately

Auth, pagination, filtering, dataloaders, subscriptions, file upload, migrations, a scaffolding CLI.
Subscriptions are blocked by architecture, not effort: `adaptor.HTTPHandler` buffers the whole
response, so no streaming transport works through it.
