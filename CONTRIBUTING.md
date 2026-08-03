# Contributing

Thanks for looking. luima is small on purpose, so the most useful contributions are usually a bug
with a failing test attached rather than a new feature.

## Setup

```sh
git clone https://github.com/ulas96/luima
cd luima
go build ./...
```

You need Go ≥ 1.25 and, for the tests that matter, a Postgres you can throw away.

```sh
cp .env.example .env    # or write it yourself
# DATABASE_URL=postgres://user:pass@localhost:5432/postgres?sslmode=disable
```

**Unquoted.** `docker --env-file` passes quotes through literally, so a quoted value fails to
parse inside a container while working fine outside it.

## Running the tests

```sh
make test       # no database. TestCRUD SKIPS.
make test-db    # sources .env. TestCRUD runs.
```

> ### A green `go test ./...` proves less than it looks
>
> `TestCRUD` calls `t.Skip` without `DATABASE_URL`, and **a skipped test still reports `ok`**.
> It is also the only test that exercises the driver behaviours the library exists to handle. Run
> `make test-db`, read the `-v` output, and confirm you see `--- PASS: TestCRUD` rather than
> `--- SKIP`.
>
> CI runs it against a `postgres:16` service container and fails the build if it skips, so this
> cannot rot — but it can still waste your afternoon locally.

`TestCRUD` creates and drops its own `luima_test_users` table, so `DATABASE_URL` is the only setup.

## Before you open a PR

```sh
make check    # gofmt + vet + lint + test-db + example
```

## Where code goes

| package | what belongs there |
|---|---|
| `server/` | anything about mounting the handler or Fiber wiring |
| `crud/` | query helpers, and only if they classify an error |
| `luimaerr/` | the error contract. Imports nothing else in luima — keep it that way |
| `db/` | connection |
| `luima.go` | **a re-export shim for every new exported symbol, or it is invisible from the root package** |
| `tests/` | every test and runnable example, for all five packages |

That second-to-last row is the cost of the layout: types are aliases and carry field changes for
free, but a genuinely new exported function has to be added to `luima.go` by hand.
`tests/luima_test.go` asserts signature identity for the ones that exist.

Tests live in one `package tests` outside the packages they exercise, so they reach only the
exported surface — the same view a consumer has. Nothing there can touch an unexported symbol,
which is the point. The trade is that the `Example` functions no longer render on pkg.go.dev
under the symbols they demonstrate, since godoc binds an example to a symbol by directory; they
still compile and their `Output` blocks still run.

## Comment style

Doc comments use NatSpec tags inside ordinary `//` godoc comments — the comment opens with the
symbol name so godoc and `go vet` stay happy, then `@notice` for what it does, `@dev` for why it
is written that way, and `@param`/`@return` for the signature:

```go
// Delete @notice Removes the row with key's primary key, reporting whether one was there.
//
// @dev Nothing to classify: absence is false, not an error.
//
// @param ctx    the resolver context
// @param key    a model with only its primary key populated
// @return bool  true when a row was deleted, false when none matched
// @return error any driver error
```

`examples/quickstart/graph/schema.resolvers.go` is exempt: gqlgen rewrites its comments on every
codegen run.

## The example

`examples/quickstart` is a nested module with `replace … => ../..`, so it always builds against
your working tree. CI re-runs codegen and fails on any diff, so if you change the schema:

```sh
make example-generate
git add examples/quickstart
```

**`go build` is not the schema check.** gqlgen writes a stub for every schema field with no
resolver method, and a stub *compiles* — it panics when called. `make example` greps for the
leftovers; that grep is the check. See [docs/gqlgen-contract.md](docs/gqlgen-contract.md).

## Style

- **Comments explain why, not what.** Most of this library's value is in the comments that say
  what breaks if a line is removed — `All` not `Post`, the two absence signals, `RETURNING *`.
  If you move code, the comment moves with it.
- **A change to behaviour needs a test that fails without it.** The `Update`/`pg.ErrNoRows`
  interaction was found by running the round trip against a real Postgres, not by reading docs.
- **New public API needs a godoc example.** They render on pkg.go.dev and compile with the test
  suite, so they cannot go stale.
- Update `CHANGELOG.md` under `## [Unreleased]`.

## Scope

These are out, and saying so is not a judgement on the idea: auth, pagination, filtering,
dataloaders, subscriptions, file upload, migrations, a scaffolding CLI.

Subscriptions in particular are blocked by architecture rather than effort — `adaptor.HTTPHandler`
buffers the entire response, so a streaming transport cannot work through it. That one needs a
Fiber-native handler, and a design discussion first.

If you want one of the others, open an issue describing the API you want before writing it.
