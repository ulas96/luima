# The gotcha register

[← back to the README](../README.md)

Every entry here is something that fails **silently** — no compile error, no stack trace, and in
several cases nothing in the log at all. The table is the thing to come back to; the sections
below expand the ones that need more than a row.

| # | Symptom | Cause | Fix |
|---|---|---|---|
| 1 | Server panics at runtime; `go build` was clean | gqlgen writes a compiling `panic("not implemented")` stub for every unimplemented schema field | `grep -rn 'not implemented' graph/*.resolvers.go` after every generate ([contract](gqlgen-contract.md#go-build-is-not-the-schema-check)) |
| 2 | Browser clients fail; nothing in the server log | Registered with `r.Post`; the `OPTIONS` preflight 405s before reaching gqlgen | `r.All(endpoint, …)` — luima does this ([fiber](fiber.md#1-all-never-post)) |
| 3 | `fiber.Config.ErrorHandler` never fires | `adaptor.HTTPHandler` always returns `nil` | Use the error presenter; there is no second contract ([fiber](fiber.md#2-fibers-errorhandler-is-unreachable--and-must-stay-that-way)) |
| 4 | Unknown paths 404 instead of showing the playground | Fiber's `Get("/")` is exact, `net/http`'s was a prefix | Intended. Route a catch-all explicitly ([fiber](fiber.md#3-get-is-an-exact-match)) |
| 5 | Every request is slow under load | `handler.New` defaults to `graphql.NoCache`; queries re-parse and re-validate | Leave `Config.QueryCache` at zero — zero means 1000, not off |
| 6 | Playground's docs pane is empty | `handler.New` adds no extensions | luima always adds `extension.Introspection{}` |
| 7 | File upload fails around 4 MB | Fiber's `BodyLimit` defaults to 4 MB; `net/http` had none | Raise `Config.Fiber.BodyLimit` |
| 8 | Subscriptions/SSE never stream | `adaptor` buffers the whole response | Needs a Fiber-native handler; not supported in v1 |
| 9 | `errors.As(err, &pgErr)` never matches | `pg.Error` is an **interface**, not pgx's `*pgconn.PgError` | `var pgErr pg.Error`, or just `luimaerr.SQLState(err)` |
| 10 | Update of a missing row succeeds silently | A plain `UPDATE` does not return `pg.ErrNoRows` | Check `RowsAffected() == 0` **and** `pg.ErrNoRows` ([below](#10-the-two-absence-signals)) |
| 11 | A missing row is a 500 instead of `null` | A single-row `Select()` returns `pg.ErrNoRows` | `crud.Get` translates it to `(nil, nil)` |
| 12 | Insert fails: Postgres rejects JSON for a `text[]` | Missing `,array`; go-pg falls back to `pgTypeJSONB` | `pg:"projects,array"` |
| 13 | An empty list cannot clear an array column | `UpdateNotZero` skips zero values | `Update` — luima never calls `UpdateNotZero` |
| 14 | Mutations answer with the value sent, not stored | No `RETURNING`; defaults, triggers and generated columns never seen | `Returning("*")` — luima does this ([below](#14-returning-)) |
| 15 | Empty list marshals as `null`, breaking `[T!]!` | A nil slice is not an empty slice | `crud.List` seeds `[]*T{}` |
| 16 | A resolver's clear error message reads as "internal server error" | Returned a bare `error`; the presenter redacts it | Wrap in `*CustomError`, or use the crud helpers ([below](#16-resolvers-opt-in-to-being-heard)) |
| 17 | *Every* schema typo reads as "internal server error" | Dropped the `*gqlerror.Error` branch from the presenter | Keep all three branches |
| 18 | `DB *pg.DB` on `Resolver` disappears after generate | `layout: single-file` sets `HasRoot` and re-emits the bare struct | `layout: follow-schema` |
| 19 | Helper functions vanish into a `/* !!! WARNING !!! */` block | `rewrite.RemainingSource` sweeps non-resolver declarations out of `*.resolvers.go` | Put helpers in `resolver.go` |
| 20 | Duplicate-declaration compile error after generate | Hand-wrote `Query()`/`Mutation()` or the resolver structs | Delete them; codegen emits them |
| 21 | Startup fails parsing a working connection URL | `pg.ParseURL` rejects all but three query params | Strip the extras ([deployment](deployment.md#pgparseurl-accepts-exactly-three-query-parameters)) |
| 22 | TLS "works" but nothing is verified | `sslmode` absent ⇒ `InsecureSkipVerify: true` | `?sslmode=verify-full` |
| 23 | Works locally, fails to parse the URL in Docker | `--env-file` passes quotes through literally | Unquoted `.env` values |
| 24 | `go run` sees no environment | Plain `source .env` does not export | `set -a && . ./.env && set +a` |
| 25 | A whole type's fields suddenly run concurrently | `resolver: true` makes `Field.IsConcurrent()` true | Only add it with a dataloader in hand |
| 26 | `go test ./...` green, nothing actually verified | DB-backed tests `t.Skip` without `DATABASE_URL` | Run `-v`, confirm the test ran ([below](#26-a-green-test-run-proves-less-than-it-looks)) |

---

## #10 The two absence signals

`Update` and `Delete` **succeed** when nothing matched. They do not return `pg.ErrNoRows`, so

```go
if errors.Is(err, pg.ErrNoRows) {   // never fires on an UPDATE
```

is a bug that stays invisible in testing until someone updates a row that does not exist.

But it is worse than "check `RowsAffected()` instead", because the signal **changes with the
statement**. With `RETURNING *` in it, go-pg is scanning a result set back into your struct, and
zero rows arrives as `pg.ErrNoRows` after all. luima checks both, and its round-trip test asserts
the message — this was found by running the test against a real Postgres, not by reading the
driver.

```go
res, err := db.ModelContext(ctx, m).WherePK().Returning("*").Update()
if err != nil {
	if errors.Is(err, pg.ErrNoRows) { … }   // the RETURNING path
	return nil, err
}
if res.RowsAffected() == 0 { … }            // the plain path
```

`orm.Result` is `Model()`, `RowsAffected() int` (−1 when the query cannot affect rows) and
`RowsReturned() int`.

---

## #14 `Returning("*")`

Without it, an `INSERT`/`UPDATE` answers with **the struct you handed it**, not the row Postgres
now holds. Those are the same value only when every column is written explicitly and the table has
no defaults that fire, no triggers, no identity columns and no generated columns.

A server that owns its one table can audit that and skip `RETURNING`. A library serving tables it
has never seen cannot. So luima always uses it: same statement, same round trip, and a
`DEFAULT now()` column comes back with the real timestamp instead of the zero value.

The corollary is worth stating separately: **a mutation's own response does not prove the
statement ran.** That is why luima's round-trip test reads the row back after `Update` rather than
trusting the payload.

---

## #16 Resolvers opt in to being heard

```go
return nil, errors.New("user already exists")   // client sees: internal server error
```

This is the design, not a bug. gqlgen's default presenter forwards `err.Error()` verbatim, which
would hand an unauthenticated caller raw driver strings — `SQLSTATE 23505`, plus your constraint
and column names. luima's presenter has three branches: `*CustomError` passes through,
`*gqlerror.Error` passes through, everything else is logged server-side and redacted.

```go
return nil, &luimaerr.CustomError{
	UserMessage:   "user " + id + " already exists",
	InternalError: err,   // kept for the log and for errors.Is/As
}
```

Or use `crud.Create`, which does exactly that for `23505` — which is most of why the crud helpers
exist at all.

---

## #26 A green test run proves less than it looks

`crud.TestCRUD` calls `t.Skip` when `DATABASE_URL` is unset, and a skipped test still reads as
`ok` in `go test ./...` output. Run with `-v` and confirm the test **ran**:

```sh
set -a && . ./.env && set +a && go test -v ./...
```

luima's CI runs it against a `postgres:16` service container specifically so this cannot rot into
a permanently-skipped test that nobody notices.

---

See also: [The gqlgen contract](gqlgen-contract.md) · [Fiber](fiber.md) · [Deployment](deployment.md)
