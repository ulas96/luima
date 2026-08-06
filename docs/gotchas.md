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
| 27 | A column your mapper forgot is `NULL` after every update | `Update` is a full replace, and go-pg writes `NULL` — not `""` — for a zero-valued field | Name the columns: `q.Column("name", "email")` ([below](#27-update-writes-every-column-as-null)) |
| 28 | Every column is client-writable | `autobind` bound your DB model as the GraphQL `input` | Separate `input` type; never name one after a model struct ([below](#28-autobind-mass-assignment)) |
| 29 | Anyone can read or delete anyone's row | `WherePK()` alone; no ownership predicate | Pass one: `q.Where("owner_id = ?", …)` ([security](../SECURITY.md#getting-the-callers-identity-into-a-resolver)) |
| 30 | Your signup mutation confirms which emails are registered | `Create`'s `label` reaches the client on a unique violation | Constant label for enumeration-sensitive tables ([below](#30-create-s-label-is-an-existence-oracle)) |
| 31 | SQL injection through a "safe" ORM | `OrderExpr`/`ColumnExpr`/`Having` interpolate raw SQL by design | Allowlist client-supplied identifiers ([below](#31-the-injection-surface-is-the-options-closure)) |
| 32 | Check-then-write races under load | Each helper is one autocommitted statement | `db.RunInTransaction` + `q.For("UPDATE")` |
| 33 | A DSN that lost its credentials connects anyway | go-pg falls back to `$PGUSER`/`$PGPASSWORD`, then literal `postgres` | Set `application_name` and check `pg_stat_activity` |
| 34 | The client IP is the proxy's, even with `TrustProxy: true` | `TrustProxy` changes `fiber.Ctx` accessors; resolvers hold the adaptor's `*http.Request`, which it never touches | Parse `X-Forwarded-For` in `HTTPMiddleware` ([deployment](deployment.md#behind-a-proxy-the-request-looks-plaintext--and-that-is-fine)) |
| 35 | `Secure` cookies are dropped, or a redirect loop, behind a TLS proxy | `r.TLS` is `nil` — the hop into the process really is plaintext | Gate on `X-Forwarded-Proto`, never on `r.TLS` ([deployment](deployment.md#behind-a-proxy-the-request-looks-plaintext--and-that-is-fine)) |
| 36 | The playground loads through the proxy, then every query 404s | Its fetch URL embeds `Endpoint` verbatim, and the proxy stripped the path prefix | Pass the prefix through, or set `Endpoint` to the externally visible path ([deployment](deployment.md#behind-a-proxy-the-request-looks-plaintext--and-that-is-fine)) |

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

## #27 `Update` writes every column, as `NULL`

`crud.Update` is a full replace — [deliberately](../crud/crud.go), because `UpdateNotZero` cannot
clear a column and that is a silent data-retention bug. The cost is the mirror image. Your model is
built by a hand-written mapper, and any column on the struct the mapper does not set is written
anyway:

```go
type User struct {
    PersonalID string `pg:"personal_id,pk"`
    Name       string `pg:"name"`
    Role       string `pg:"role"`      // ← added later
}

func newUser(id string, in model.UserInput) *User {
    return &User{PersonalID: id, Name: in.Name}   // ← Role forgotten
}
```

Every `updateUser` call now clobbers `role`. `go build` is happy, the tests are happy, and the
response — built from `RETURNING *`, which is otherwise a virtue — shows the clobbered value as
though it were intended.

**And it writes `NULL`, not `""`.** go-pg's `Field.AppendValue` emits `NULL` for any zero-valued
field whose tag lacks `,use_zero`. So what actually happens depends on the column, and the more
dangerous case is the quiet one:

| column | result |
|---|---|
| `role text not null` | `ERROR #23502 null value in column "role"` — loud, and the constraint is the only reason |
| `role text` | silently `NULL` |
| `pg:"role,use_zero"` | silently `''` |

This is a security bug whenever the clobbered column is an authorization column — `owner_id`,
`deleted_at`, `password_hash`. The fix is to name the columns you mean to write:

```go
luima.Update(ctx, db, u, "user "+id, func(q *orm.Query) *orm.Query {
    return q.Column("name", "email") // SET name = ?, email = ? — nothing else touched
})
```

`TestCRUD/partial_update` asserts both halves, so neither can drift.

---

## #28 `autobind` mass assignment

`gqlgen.yml` points `autobind` at your model package, so **any** GraphQL type whose name matches a
struct there binds to it — inputs included. Write `input User { … }` and your database model
becomes the input type: every column client-writable, including `role`, `owner_id` and anything
else you never meant to expose. Combined with #27, the client then also controls what a partial
update leaves behind.

The quickstart avoids this by having a separate `UserInput`. Nothing enforces it — keep input
types and model structs in disjoint namespaces, and treat a codegen diff that starts binding an
input to a model as the security review it is.

---

## #30 `Create`'s `label` is an existence oracle

`crud.Create` builds `label + " already exists"` from caller-supplied text and returns it on a
unique violation. That is the whole point of the helper — a duplicate has to reach the client. On a
signup-shaped mutation it is also textbook account enumeration: send an email address, learn from
the error whether it is registered.

Working as designed, but the design has a name. For an enumeration-sensitive table use a constant
label and keep the real detail in the log:

```go
if _, err := luima.Create(ctx, db, u, "that record"); err != nil { … }
```

The trade-off is real, not free: the label is the only thing distinguishing a duplicate from a
redacted internal error, so shortening it costs the client that distinction.

Second-order: the label is attacker-controlled text returned verbatim in `errors[].message`. The
response is JSON, so there is no injection at luima's layer — a client that renders error messages
into the DOM inherits the sink.

---

## #31 The injection surface is the options closure

Nothing in `crud` is SQL-injectable. go-pg parameterizes values, and identifiers come from struct
tags resolved at compile time. The surface is next door.

`List` hands you a `*orm.Query`, and filtering and pagination are out of scope — so you *will*
write that code, and `q.OrderExpr`, `q.ColumnExpr`, `q.Having` and `q.Where(fmt.Sprintf(…))` all
interpolate raw SQL by design. A client-supplied sort column reaching `OrderExpr` is an injection;
reaching `Order` is not, because `Order` quotes a single-token identifier — a distinction nobody
should have to rely on.

```go
q.Where("name = ?", v)        // values: parameterized
q.Where("x = ?", pg.Ident(c)) // identifiers: quoted
pg.Safe("count(*) > 3")       // a fragment you have read and vetted
```

For a client-supplied sort column, use an allowlist, not an escape:

```go
var sortable = map[string]string{"name": "name", "created": "created_at"}
col, ok := sortable[input.SortBy]
if !ok {
    return nil, &luimaerr.CustomError{UserMessage: "unknown sort field"}
}
return q.Order(col)
```

This belongs in luima's docs precisely *because* the design pushes the code onto you.

---

See also: [The gqlgen contract](gqlgen-contract.md) · [Fiber](fiber.md) · [Deployment](deployment.md) · [Security](../SECURITY.md)
