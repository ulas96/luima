# The gotcha register

[← back to the README](../README.md)

Every entry here is something that fails **silently** — no compile error, no stack trace, and in
several cases nothing in the log at all. The table is the thing to come back to; the sections
below expand the ones that need more than a row.

| # | Symptom | Cause | Fix |
|---|---|---|---|
| 1 | Server panics at runtime; `go build` was clean | gqlgen writes a compiling `panic("not implemented")` stub for every unimplemented schema field | `grep -rn 'not implemented' graph/*.resolvers.go` after every generate ([contract](gqlgen-contract.md#go-build-is-not-the-schema-check)) |
| 2 | Browser clients fail; nothing in the server log | Registered with `r.Post`; the `OPTIONS` preflight 405s before reaching gqlgen | `r.All(endpoint, …)` — luima does this ([fiber](fiber.md#1-all-never-post)) |
| 3 | `fiber.Config.ErrorHandler` never fires | The adaptor always returns `nil`, so no resolver error becomes a Fiber error | Use the error presenter; there is no second contract for *resolver* errors ([fiber](fiber.md#2-fibers-errorhandler-is-unreachable--and-must-stay-that-way)) |
| 4 | Unknown paths 404 instead of showing the playground | Fiber's `Get("/")` is exact, `net/http`'s was a prefix | Intended. Route a catch-all explicitly ([fiber](fiber.md#3-get-is-an-exact-match)) |
| 5 | Every request is slow under load | `handler.New` defaults to `graphql.NoCache`; queries re-parse and re-validate | Leave `Config.QueryCache` at zero — zero means 1000, not off |
| 6 | Playground's docs pane is empty | `handler.New` adds no extensions | luima always adds `extension.Introspection{}` |
| 7 | File upload fails around 4 MB | Fiber's `BodyLimit` defaults to 4 MB; `net/http` had none | Raise `Config.Fiber.BodyLimit` — and read [#37](#37-the-multipart-transport-is-a-csrf-hole) before adding the transport |
| 8 | Subscriptions/SSE never stream | Not the adaptor — it streams. `Mount` selected `transport.POST` first (fixed in 0.3.0), and fasthttp never cancels on client hangup | Not supported. The hangup half is upstream ([fiber](fiber.md#4-bodylimit-and-why-subscriptions-are-really-out)) |
| 9 | Every `SQLSTATE` branch is dead code; no error ever matches | `pgdriver.Error` is a struct **value**, and `*pgdriver.Error` is an error too — so the pointer spelling compiles and never matches | Drop the `*`: `errors.AsType[pgdriver.Error](err)`, or just `luimaerr.SQLState(err)` |
| 10 | Update of a missing row succeeds silently | An `UPDATE` that matched nothing is not an error, `RETURNING *` or not | `RowsAffected() == 0` is the whole check ([below](#10-absence-has-one-signal)) |
| 11 | A missing row is a 500 instead of `null` | A single-row `Scan` returns `sql.ErrNoRows` | `crud.Get` translates it to `(nil, nil)` |
| 12 | Insert fails 22P02: Postgres rejects JSON for a `text[]` | Missing `,array`; bun's default appender for a slice is `AppendJSONValue` | `bun:"projects,array"` |
| 13 | An update never writes `""`, `0` or `false` | `OmitZero` skips every zero-valued field, a nil slice included — an empty slice is not zero, so it still clears an array column | `Update` — luima never calls `OmitZero` |
| 14 | Mutations answer with the value sent, not stored | No `RETURNING`; defaults, triggers and generated columns never seen | `Returning("*")` — luima does this ([below](#14-returning)) |
| 15 | Empty list marshals as `null`, breaking `[T!]!` | A nil slice is not an empty slice | `crud.List` seeds `[]*T{}` |
| 16 | A resolver's clear error message reads as "internal server error" | Returned anything but a `*CustomError` — a bare `error`, `gqlerror.Errorf` — and the presenter redacts it | Wrap in `*CustomError`, or use the crud helpers ([below](#16-resolvers-opt-in-to-being-heard)) |
| 17 | *Every* schema typo reads as "internal server error" | Dropped the `*gqlerror.Error` branch from the presenter | Keep all three branches |
| 18 | `DB *bun.DB` on `Resolver` disappears after generate | `layout: single-file` sets `HasRoot` and re-emits the bare struct | `layout: follow-schema` |
| 19 | Helper functions vanish into a `/* !!! WARNING !!! */` block | `rewrite.RemainingSource` sweeps non-resolver declarations out of `*.resolvers.go` | Put helpers in `resolver.go` |
| 20 | Duplicate-declaration compile error after generate | Hand-wrote `Query()`/`Mutation()` or the resolver structs | Delete them; codegen emits them |
| 21 | The URL parses, then `Connect` fails at its boot round trip | pgdriver reads eleven parameters and sends every other one as `SET <name> TO <value>` on each new connection | Read the SQLSTATE — 42704 names the typo, and `?statement_timeout=5s` now works ([deployment](deployment.md#an-unknown-query-parameter-becomes-a-set-not-a-parse-error)) |
| 22 | TLS "works" but nothing is verified | `sslmode` absent ⇒ `InsecureSkipVerify: true` | `?sslmode=verify-full` |
| 23 | Works locally, fails to parse the URL in Docker | `--env-file` passes quotes through literally | Unquoted `.env` values |
| 24 | `go run` sees no environment | Plain `source .env` does not export | `set -a && . ./.env && set +a` |
| 25 | A whole type's fields suddenly run concurrently | `resolver: true` makes `Field.IsConcurrent()` true | Only add it with a dataloader in hand |
| 26 | `go test ./...` green, nothing actually verified | DB-backed tests `t.Skip` without `DATABASE_URL` | Run `-v`, confirm the test ran ([below](#26-a-green-test-run-proves-less-than-it-looks)) |
| 27 | A column your mapper forgot is blanked after every update | `Update` is a full replace, and bun writes a zero-valued field as `''`, `0` or `FALSE` — which a `not null` column accepts | Name the columns: `q.Column("name", "email")` ([below](#27-update-writes-every-column-zero-values-included)) |
| 28 | Every column is client-writable | `autobind` bound your DB model as the GraphQL `input` | Separate `input` type; never name one after a model struct ([below](#28-autobind-mass-assignment)) |
| 29 | Anyone can read or delete anyone's row | `WherePK()` alone; no ownership predicate | Pass one: `q.Where("owner_id = ?", …)` ([security](../SECURITY.md#getting-the-callers-identity-into-a-resolver)) |
| 30 | Your signup mutation confirms which emails are registered | `Create`'s `label` reaches the client on a unique violation | Constant label for enumeration-sensitive tables ([below](#30-creates-label-is-an-existence-oracle)) |
| 31 | SQL injection through a "safe" ORM | `OrderExpr`/`ColumnExpr`/`Having` interpolate raw SQL by design | Allowlist client-supplied identifiers ([below](#31-the-injection-surface-is-the-options-closure)) |
| 32 | Check-then-write races under load | Each helper is one autocommitted statement | `db.RunInTx(ctx, nil, …)` + `q.For("UPDATE")`, passing the `bun.Tx` to the helpers |
| 33 | A DSN that lost its credentials, or its database name, connects anyway | pgdriver fills from `$PGHOST`/`$PGPORT`/`$PGUSER`/`$PGDATABASE`, then `localhost:5432`/`postgres`/`postgres`; `Connect` fills an empty password from `$PGPASSWORD` | Set `application_name` and check `pg_stat_activity` ([deployment](deployment.md#a-dsn-that-lost-its-credentials-still-connects)) |
| 34 | The client IP is the proxy's, even with `TrustProxy: true` | `TrustProxy` changes `fiber.Ctx` accessors; resolvers hold the adaptor's `*http.Request`, which it never touches | Parse `X-Forwarded-For` in `HTTPMiddleware` ([deployment](deployment.md#behind-a-proxy-the-request-looks-plaintext--and-that-is-fine)) |
| 35 | `Secure` cookies are dropped, or a redirect loop, behind a TLS proxy | `r.TLS` is `nil` — the hop into the process really is plaintext | Gate on `X-Forwarded-Proto`, never on `r.TLS` ([deployment](deployment.md#behind-a-proxy-the-request-looks-plaintext--and-that-is-fine)) |
| 36 | The playground loads through the proxy, then every query 404s | Its fetch URL embeds `Endpoint` verbatim, and the proxy stripped the path prefix | Pass the prefix through, or set `Endpoint` to the externally visible path ([deployment](deployment.md#behind-a-proxy-the-request-looks-plaintext--and-that-is-fine)) |
| 37 | A cross-site HTML form executes a mutation on your server | You added `transport.MultipartForm`; `multipart/form-data` is a "simple" request, so no preflight protects it | Require a header a form cannot set, in `HTTPMiddleware` ([below](#37-the-multipart-transport-is-a-csrf-hole)) |
| 38 | Every query hits the wrong table, or a table that does not exist | bun takes the table name from an embedded `bun.BaseModel`; go-pg's unexported `tableName` field is skipped without a word | Embed `bun.BaseModel` tagged `table:app_users` ([below](#38-every-query-hits-the-wrong-table-after-porting-a-go-pg-model)) |
| 39 | Insert fails 23502 against a `not null default '{}'` column | A nil `,array` slice is written as a literal `NULL`, and a NULL is a value, so the `DEFAULT` never applies | Send `[]string{}`, or tag the field `,nullzero` ([below](#39-insert-fails-23502-on-a-not-null-default-array-column)) |
| 40 | `RequestTimeout` fires, the client gets an error, Postgres keeps working | pgdriver never sends a CancelRequest; a deadline is a socket deadline and nothing more | `luima.StatementTimeout(d)`, kept under `ReadTimeout` ([below](#40-a-timed-out-query-keeps-running-on-postgres)) |

---

## #10 Absence has one signal

`Update` and `Delete` **succeed** when nothing matched. They do not return `sql.ErrNoRows`, so

```go
if errors.Is(err, sql.ErrNoRows) {   // never fires on an UPDATE
```

is a bug that stays invisible in testing until someone updates a row that does not exist. The row
count is the whole check:

```go
res, err := db.NewUpdate().Model(m).WherePK().Returning("*").Exec(ctx)
if err != nil {
	return nil, err
}
n, err := res.RowsAffected()
if err != nil {
	return nil, err
}
if n == 0 { … }   // no row matched — and that is the whole signal, RETURNING * or not
```

Under go-pg that was *two* checks, because `RETURNING *` made it scan a result set and zero rows
came back as `pg.ErrNoRows` after all. bun does not work that way, and the thing to know is what the
signal now follows: **how you ran the statement, not what is in it.** `Exec(ctx)` passes `hasDest`
only when it is handed a destination, `Scan` always passes it, and `(*baseQuery)._scan` raises
`sql.ErrNoRows` only when it is set. So writing `q.Scan(ctx)` where the helpers write `q.Exec(ctx)`
puts `sql.ErrNoRows` back — returned bare, logged and redacted, where luima answers `NOT_FOUND`.

`Exec` returns `database/sql`'s `sql.Result`, so `RowsAffected()` is `(int64, error)` — two values,
where go-pg's `orm.Result.RowsAffected()` returned a bare int.

**`Create` has the same absence, and that surprises people** — an INSERT looks like it always
inserts. It does not: `q.On("CONFLICT DO NOTHING")` suppresses one, and so does a `BEFORE INSERT`
trigger returning `NULL`, which needs no cooperation from the caller at all. Both arrive as
`RowsAffected() == 0` with no error, and `crud.Create` answers `(nil, nil)`, like `Get`. Before
0.3.0 it returned go-pg's `pg.ErrNoRows` bare and the presenter redacted it, so a soft-ignore
trigger made every suppressed insert answer `"internal server error"`. **Check the result** — a
caller assuming non-nil nil-dereferences the second time the same key is inserted.

And note the ordering trap if you write this yourself: `res` is `nil` whenever `err` is non-nil, so
the `RowsAffected()` check has to come *after* the error branch returns. Copying the shape above
without noticing writes a panic.

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
and column names. luima's presenter has three branches: `*CustomError` passes through; a
`*gqlerror.Error` that wraps no other error and was reported outside field resolution (gqlgen's
parse, validation and limit errors) passes through; everything else is logged server-side and
redacted.

That last branch includes every `*gqlerror.Error` reported while a field resolves, and it has to.
gqlgen wraps a resolver's plain error in one before the presenter sees it, copying `err.Error()`
into the message, so a presenter that trusts the type sends the driver's text to the client. And a
resolver can return one it did not write — a list decoded from an upstream GraphQL response has no
cause — so a presenter that trusts a missing cause forwards whatever that server said. So
`gqlerror.Errorf(...)` from a resolver or a directive is redacted too, and so is a panic, and so is
everything sent with `graphql.AddError` or `graphql.AddErrorf` that is not a `*CustomError`.

One consequence reads like a bug. A client's bad value for a custom scalar — `"yesterday"` for
gqlgen's built-in `Time` — is answered `internal server error`, because the presenter cannot tell
gqlgen's unmarshalling text from yours. `UnmarshalGQL` can return a `*CustomError` and be heard, on
the argument's path, which for `Time` means binding a scalar of your own. (`DisableIntrospection`
would read the same way, and does not: `Mount` refuses a `__schema` or `__type` operation before any
field runs, with `INTROSPECTION_DISABLED`.)

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

`TestCRUD`, in `tests/`, calls `t.Skip` when `DATABASE_URL` is unset, and a skipped test still reads
as `ok` in `go test ./...` output. Run with `-v` and confirm the test **ran**:

```sh
set -a && . ./.env && set +a && go test -v ./...
```

luima's CI runs it against a `postgres:16` service container specifically so this cannot rot into
a permanently-skipped test that nobody notices.

---

## #27 `Update` writes every column, zero values included

`crud.Update` is a full replace — [deliberately](../crud/crud.go), because `OmitZero` skips every
zero-valued field, so an empty string could not clear a text column and `false` or `0` could never
be written at all. That is not a partial-update feature, it is a silent data-retention bug. The cost
is the mirror image. Your model is built by a hand-written mapper, and any column on the struct the
mapper does not set is written anyway:

```go
type User struct {
    bun.BaseModel `bun:"table:app_users"`

    PersonalID string `bun:"personal_id,pk"`
    Name       string `bun:"name"`
    Role       string `bun:"role"`      // ← added later
}

func newUser(id string, in model.UserInput) *User {
    return &User{PersonalID: id, Name: in.Name}   // ← Role forgotten
}
```

Every `updateUser` call now clobbers `role`. `go build` is happy, the tests are happy, and the
response — built from `RETURNING *`, which is otherwise a virtue — shows the clobbered value as
though it were intended.

**And what it writes is the zero value, not `NULL`.** `(*schema.Field).appendValue` sends `DEFAULT`
only for a nil pointer or for a zero field tagged `,nullzero`; everything else goes out as itself.
So what lands in the column depends on the field:

| the field, as tagged | `UPDATE` sends | what the column ends up holding |
|---|---|---|
| `Role string`, `bun:"role"` | `''` | `''` — and `not null` accepts it, so nothing is loud |
| `Role string`, `bun:"role,nullzero"` | `DEFAULT` | the column's default; `NULL` when it has none, and 23502 when it is `not null` without one |
| `Role *string`, nil | `DEFAULT` | the same |
| `Projects []string`, nil, `bun:"projects,array"` | `NULL` | `NULL`, or 23502 on a `not null` column — see [#39](#39-insert-fails-23502-on-a-not-null-default-array-column) |

**That is quieter than 0.5.0, not louder.** go-pg wrote `NULL` for the same forgotten field, so a
`not null` column raised 23502 and the bug announced itself on the first request. `''` satisfies
`not null`. The constraint that used to catch this catches nothing now, and the only column type
still loud about it is the nil array.

It is a security bug whenever the clobbered column is an authorization column — `owner_id`,
`deleted_at`, `password_hash`. The fix is to name the columns you mean to write, not to reach for a
tag; `,nullzero` only changes *which* wrong value gets written:

```go
luima.Update(ctx, db, u, "user "+id, func(q *bun.UpdateQuery) *bun.UpdateQuery {
    return q.Column("name", "email") // SET name = ?, email = ? — nothing else touched
})
```

`TestCRUD/partial_update` pins every row of that table it can reach: that `Column` writes the column
it names and leaves the rest alone, that a plain `Update` blanks `owner` to `''` and not to `NULL`,
and that the same field tagged `,nullzero` writes `DEFAULT` — `NULL`, since `owner` has no default.

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

Nothing in `crud` is SQL-injectable. bun renders every `?` argument through the dialect's appenders
— a string is quoted and its own quotes doubled (`BaseDialect.AppendString`) — and identifiers come
from `bun:` tags resolved at compile time. Note what that is *not*: nothing in this stack binds a
parameter server-side, bun formats the finished statement and pgdriver sends it as text, so the
escaping is the whole defence. The surface is next door.

`List` hands you a `*bun.SelectQuery`, and filtering and pagination are out of scope — so you *will*
write that code, and `q.OrderExpr`, `q.ColumnExpr`, `q.Having` and `q.Where(fmt.Sprintf(…))` all
interpolate raw SQL by design. A client-supplied sort column reaching `OrderExpr` is an injection;
reaching `Order` is not, because `Order` quotes what it is handed (`addOrder` → `AppendIdent`, which
quotes each dot-separated part and reads anything after a space as a sort direction, dropping one it
does not recognise) — a distinction nobody should have to rely on.

```go
q.Where("name = ?", v)         // values: quoted and escaped by the dialect
q.Where("x = ?", bun.Ident(c)) // identifiers: quoted
bun.Safe("count(*) > 3")       // a fragment you have read and vetted
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

## #37 The multipart transport is a CSRF hole

**A default luima mount is closed.** Measured against one, every shape a cross-site HTML form can
send is refused before it reaches an executor:

```
POST x-www-form-urlencoded : HTTP 400  "transport not supported"
POST multipart/form-data   : HTTP 400  "transport not supported"
POST text/plain            : HTTP 400  "transport not supported"
POST no Content-Type       : HTTP 400  "transport not supported"
GET  ?query=mutation{...}  : HTTP 406  "GET requests only allow query operations"
```

That is not luck. `transport.POST` requires `application/json`, which an HTML form cannot send, and
a cross-site `fetch` with that content type triggers a preflight luima answers without an
`Access-Control-Allow-Origin`. `transport.GET` refuses non-query operations outright. This is why
luima ships no CSRF field: it registers no transport that needs one.

**Adding the multipart transport removes all of it, in one line.** `Configure` makes that one line
easy:

```
=== Configure CAN add MultipartForm: a cross-site HTML form now reaches a MUTATION ===
  multipart mutation  : HTTP 200  {"data":{"ping":"pong"}}
  resolver saw RawQuery : "mutation{drop}"
```

A plain `<form enctype="multipart/form-data">` on any site, auto-submitted, executing a mutation on
your server with the victim's cookies attached. No preflight, because multipart is a *simple*
request. This is precisely the hole Apollo's `csrfPrevention` exists to close, and it is silent —
you get a 200 and a successful mutation.

**If you add the transport, add the check with it.** Require a header a form cannot set —
`Apollo-Require-Preflight`, or your own — and reject requests without it:

```go
luima.Config{
    Configure: func(srv *handler.Server) { srv.AddTransport(transport.MultipartForm{}) },
    HTTPMiddleware: []func(http.Handler) http.Handler{
        func(next http.Handler) http.Handler {
            return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
                if r.Header.Get("Apollo-Require-Preflight") == "" {
                    http.Error(w, "preflight required", http.StatusForbidden)
                    return
                }
                next.ServeHTTP(w, r)
            })
        },
    },
}
```

`transport.UrlEncodedForm` is measurably less dangerous — it reached an anonymous query (`{ping}` →
200) while every mutation shape tried came back 422 at parse. Do not rely on that. It is a parsing
accident, not a guarantee.

---

## #38 Every query hits the wrong table after porting a go-pg model

bun reads the table name from an **embedded `bun.BaseModel`**, and from nowhere else:

```go
type User struct {
    bun.BaseModel `bun:"table:app_users"`   // delete this line and everything still compiles
    …
}
```

go-pg's spelling was an unexported `tableName struct{}` field, and `(*schema.Table).processFields`
skips every unexported field that is not embedded — without a word, without a tag error, without a
log line. So a model ported tag by tag still builds, and `(*schema.Table).init` names the table
after the *type* instead: `internal.Underscore` and then `inflection.Plural`, so `User` becomes
`users`, and the `app_users` in the tag you did port is never read.

Where no such table exists, every helper fails with a redacted `internal server error` and the
relation name only in your log — annoying, but loud. The dangerous case is where one does: a `users`
table beside your `app_users`, a legacy table, another tenant's. Then the queries **succeed against
the wrong table**, and nothing anywhere reports it.

`luimagen` emits the embedded line; a hand-ported model is the one to check. The check is one
statement, and it needs no database:

```go
fmt.Println(db.NewSelect().Model((*User)(nil)).String())   // SELECT … FROM "app_users" AS "user"
```

---

## #39 Insert fails 23502 on a `NOT NULL DEFAULT` array column

```
projects text[] not null default '{}'
```

A nil `[]string` tagged `,array` reaches Postgres as a literal `NULL` — pgdialect's
`appendStringSlice` returns `NULL` for a nil slice, on `INSERT` and on `UPDATE` alike — and a NULL
is a *value*, not a missing one, so the column's `DEFAULT '{}'` never applies and the `not null`
fires: SQLSTATE 23502, redacted to `internal server error` on the way out. go-pg sent `DEFAULT` for
the same zero field, so a model that inserted fine in 0.5.0 fails here.

An empty slice is not a nil one. `[]string{}` writes `'{}'`, which is why every fixture in
`TestCRUD` that does not care about `projects` still sets it.

**This is not only a Go-built model.** gqlgen hands a resolver a nil slice for a **nullable** list —
`projects: [String!]` — whenever the client sends `null` or leaves it out: the generated
unmarshaller returns `nil, nil` for a null (`codegen/type.gotpl`), and an omitted argument never
reaches the unmarshaller at all (`graphql.ProcessArgField` returns the zero value). Only a non-null
`[String!]!` is always non-nil, because that branch builds the slice with `make`. So a nullable list
input mapped straight onto a `not null` array column is a 23502 on every request that omits it.

Three ways out, in order of preference: map an absent list to `[]string{}` in your input mapper; tag
the field `bun:"projects,array,nullzero"`, which sends `DEFAULT` for a nil slice and lets the
column's `'{}'` apply while an empty slice still writes `'{}'`; or make the column nullable and mean
it. `TestCRUD/nil_slice` pins the failure, and with it that luima redacts the 23502 instead of
telling the client which column is `not null`.

---

## #40 A timed-out query keeps running on Postgres

`RequestTimeout` bounds the *request*. It does not bound the statement. pgdriver never sends
Postgres a CancelRequest — there is no such message anywhere in the driver — so a context deadline
becomes a socket deadline and nothing more (`(*Conn).deadline`): the read fails with an i/o timeout,
the connection is discarded, and the backend runs the statement to completion, holding its locks and
its snapshot the whole time. Under load the server sheds requests while the database gets *busier*.
go-pg sent the cancel, so this is a bound an upgrading service loses without changing a line.

The bound that outlives the client is the server's:

```go
luima.ConnectWith(url, luima.StatementTimeout(5*time.Second))   // or ?statement_timeout=5s
```

Keep it under `RequestTimeout` **and** under pgdriver's `ReadTimeout` — 10s, unless raised with
`?read_timeout=` or a `ConnectWith` tune. Past either one the client gives up first, and the caller
gets an i/o timeout with no SQLSTATE where `57014` would have said what happened.

It is not free either. `checkBadConn` counts 57014 as a bad connection, so a timeout read by an
`Exec`, or by a query before its row description arrives, costs the pool that connection and the
next request pays a dial, a TLS handshake, a startup and the `SET`s again; one read later, during
row iteration, it does not. Inside a transaction a closed connection fails every later statement,
`COMMIT` included, with `driver.ErrBadConn`.

---

See also: [The gqlgen contract](gqlgen-contract.md) · [Fiber](fiber.md) · [Deployment](deployment.md) · [Security](../SECURITY.md)
