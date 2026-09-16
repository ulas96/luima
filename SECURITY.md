# Security

## Reporting a vulnerability

Report privately through
[GitHub Security Advisories](https://github.com/ulas96/luima/security/advisories/new). Please do
not open a public issue for a vulnerability.

Include what you did, what happened, and the versions of luima, Go and Postgres. You can expect an
acknowledgement within a week.

## What luima does not do

**luima ships no authentication and no authorization.** This is not an oversight; it is the
documented scope. A server built with luima is open to anyone who can reach the port.

Your server connects as a single Postgres role. If that role is privileged — as the default
connection string for most managed Postgres providers is — then **Row Level Security does not
apply to it**, and every GraphQL query runs with full access to every row. Nothing in luima
narrows that.

Put authentication in front of it: an API gateway, a reverse proxy that terminates auth, your own
middleware in `Config.HTTPMiddleware`, or a Fiber middleware on the group you `Mount` onto.

### Getting the caller's identity into a resolver

Middleware can reject a request outright — `return c.SendStatus(401)` before `c.Next()`. To tell a
resolver *who* the caller is, which is what per-row authorization needs, use `c.SetContext`:

```go
app.Use(func(c fiber.Ctx) error {
    user, err := authenticate(c)
    if err != nil {
        return c.SendStatus(fiber.StatusUnauthorized)
    }
    c.SetContext(context.WithValue(c.Context(), userKey{}, user))
    return c.Next()
})
```

`ctx.Value(userKey{})` then works in the resolver. `c.Locals("user", u)` also reaches
`ctx.Value("user")`, but prefer `SetContext`: it takes a typed key rather than a collision-prone
string, and it is the same context that carries the deadline and the cancellation.

Middleware written against `net/http` — a session layer, an OAuth proxy's verifier, anything
shaped `func(http.Handler) http.Handler` — mounts through `Config.HTTPMiddleware` instead. It
receives the real `*http.Request` with cookies parsed, its response headers (`Set-Cookie`
included) survive the adaptor, and `r.WithContext` reaches resolvers the same way `SetContext`
does.

**Do not put identity on the `Resolver` struct.** gqlgen constructs it once and shares it across
every request, so a per-request field on it is a cross-request data race — which under concurrency
is an authorization bypass, not a bug you find in testing.

Then scope the query. Every CRUD helper takes query modifiers, and that is where the ownership
predicate goes — on `Get`, `List`, `Update` and `Delete`, the four that match a row that already
exists:

```go
luima.Delete(ctx, r.DB, &model.User{PersonalID: id}, func(q *bun.DeleteQuery) *bun.DeleteQuery {
    return q.Where("owner_id = ?", callerID(ctx))
})
```

The closure type is bun's statement type, so it differs per helper: `*bun.SelectQuery` for `Get` and
`List`, `*bun.UpdateQuery` for `Update`, `*bun.DeleteQuery` for `Delete`, `*bun.InsertQuery` for
`Create`. One ownership predicate therefore faces three statement types, and copying it into each is
how two paths end up scoped and the third does not. Write it once instead, as a
`func(bun.QueryBuilder) bun.QueryBuilder` — that interface is the WHERE clause and nothing else —
and apply it with `q.ApplyQueryBuilder(owned)`, which the select, update and delete queries all
have. `*bun.InsertQuery` does not, and needs none: there is no stored row to own yet. Nothing in the
compiler reports the helper you forgot.

A row that exists but is not the caller's then reports as absent, which is the right answer to
give an unauthorized caller — it discloses no existence.

If you need per-user RLS instead, connect as an unprivileged role and set the request's claims per
transaction.

## What luima does do

**The error presenter redacts.** gqlgen's default presenter forwards `err.Error()` verbatim, which
would hand an unauthenticated caller raw driver strings — pgdriver renders one as
`ERROR: duplicate key value violates unique constraint "users_email_key" (SQLSTATE=23505)`, and
with it your table, column and constraint names. `luimaerr.PresentError` passes through only errors
a resolver has explicitly marked safe (`*CustomError`) and gqlgen's own text about the query the
client just sent; everything else is logged server-side and returned as `internal server error`.

It recognises gqlgen's text as a `*gqlerror.Error` that wraps no other error and was reported
outside field resolution, and it needs both halves. gqlgen hands a resolver's plain error to the
presenter wrapped in a `*gqlerror.Error` that carries `err.Error()` as its message, so the type
proves nothing. And a resolver can return a `*gqlerror.Error` with no cause that it did not write —
a list decoded from an upstream GraphQL response has none — so inside a field a missing cause
proves nothing either. Through 0.5.0 both reached the client verbatim.

This is damage control on information disclosure, not access control. Do not mistake it for one.

Three limits worth stating plainly:

- **It redacts on the wire, not in the log.** The redacted error is written to stderr in full —
  your table, column and constraint names, and whatever of the client's own input Postgres quoted
  back into the message. Not the row's values: `(pgdriver.Error).Error` renders the severity, the
  message and the SQLSTATE and stops, so the DETAIL field that carries them
  (`Key (email)=(victim@example.com) already exists`) is reachable with `Field('D')` and is nowhere
  in that line. Your log store therefore inherits your schema, if not your rows. Wrap
  `PresentError` with a `Config.ErrorPresenter` if you need to filter or route that.
- **`CustomError.UserMessage` is returned verbatim.** Never build it from another error —
  `&CustomError{UserMessage: err.Error()}` undoes the redaction in one line that reads like
  careful error handling. Treat it as untrusted too: the usual way to build it is from client
  input, so a client that renders error messages into the DOM inherits that sink.
- **Any client can make it log.** Every redacted error writes a log line, and producing one takes
  no resolver bug: a `__schema` query while introspection is disabled, a malformed value for a
  custom scalar, or a variable default that does not parse is answered `INTERNAL_SERVER_ERROR` and
  logged. Bound the volume, and do not page on that code alone.

**Introspection and the playground are on by default.** Both are the right default for
development and the wrong one for a public production endpoint. Set `DisablePlayground: true` and
`DisableIntrospection: true`.

Turning introspection off is defence in depth and a smaller attack surface. On its own it is **not**
confidentiality: gqlparser appends `Did you mean …?` suggestions to validation errors, and the
presenter passes validation errors through by design, so a caller who guesses `nam` still learns
there is a `name`. Close that too if you are relying on it:

```go
luima.Config{
    DisableIntrospection: true,
    Configure: func(srv *handler.Server) { srv.SetDisableSuggestion(true) },
}
```

Even with both, hiding the schema does not protect the data behind it. Treat it as raising the cost
of reconnaissance, never as access control.

**Requests are bounded on this side of the socket only.** `Config.RequestTimeout` defaults to 15s
and puts a deadline on the resolver's context, which the database layer stops waiting on in three
places: `database/sql` has no pool timeout of its own, so a request queued for a pooled connection
gives up only when its context is done (`(*sql.DB).conn` selects on `ctx.Done()`); pgdriver makes
that deadline the socket deadline for writing the query and reading the reply
(`(*pgdriver.Conn).deadline`) — all of it for an `Exec`, and up to the row description for a query;
and for the rows after that, which pgdriver reads on `context.TODO()`, `database/sql` watches the
context itself and closes the `Rows` when it fires (`(*sql.Rows).awaitDone`). Without it, a caller
can fire expensive queries and disconnect while each one holds a pooled connection, with the queue
behind it unbounded.

**It does not stop the query.** pgdriver never sends a Postgres `CancelRequest`: it reads the
backend's process id and cancel key during startup (`proto.go`, the `BackendKeyData` message) and
never uses them again. A statement whose client has timed out runs on until it finishes, holding a
backend and its locks, with nobody left to read the answer — so one caller, one expensive query, and
a deadline bounds only what the server is willing to wait for. The bound Postgres enforces itself:

```go
db, err := luima.ConnectWith(os.Getenv("DATABASE_URL"), luima.StatementTimeout(5*time.Second))
```

`?statement_timeout=5s` in the DSN does the same, now that pgdriver sends a parameter it does not
recognize as a `SET` on every new connection. Setting it on the Postgres role does it one layer
down, and is the better answer when more than one application shares the role.

**Keep it under both client-side limits, or its SQLSTATE never arrives.** `Connect` keeps pgdriver's
socket timeouts — 10s to read, 5s to write (`newDefaultConfig`) — because they are the only bound on
the I/O pgdriver does without a caller's context: the rows after a query's row description, `COMMIT`
and `ROLLBACK`, and the startup of every connection `database/sql` dials in the background. The read
waiting for a statement's answer therefore ends at the earlier of that 10s and the request's
deadline. A `statement_timeout` at or above either one still stops the statement in Postgres, but by
then the caller has had an i/o timeout, and nothing on this side can tell that is what bounded it.
Raise `ReadTimeout` — with `?read_timeout=` or a `ConnectWith` tune — when a statement is allowed to
run long, and raise `RequestTimeout` with it.

Telling the two apart: a statement Postgres cancelled comes back as SQLSTATE `57014`, which
`luima.SQLState` reads. A client-side bound carries no SQLSTATE at all, and which error it does
carry depends on which of those three places the request was standing in — so do not branch on one
spelling of it. Blocked on the socket, it is that deadline expiring: a `net.Error` reading
`i/o timeout`, never `context.DeadlineExceeded`, whether the request's deadline set it or — reading
rows, which pgdriver does without the context — `ReadTimeout` alone did. Queued for a connection it
is the context's own error, and so is a deadline that lands between two rows: `awaitDone` stores
`ctx.Err()`, stops `Next`, and returns it from `Rows.Err()`, which is what bun's scan returns.

**And a timeout costs a connection.** `(*pgdriver.Conn).checkBadConn` closes the connection on
57014, and on any error that is not a `pgdriver.Error` at all — a client-side i/o timeout included.
It runs only in `ExecContext` and `QueryContext`, so a 57014 read there, by an `Exec` or by a query
before its row description arrives, costs the pool that connection, while one message later, during
row iteration (`rows.next`), the same error leaves it pooled. A client-side timeout costs it at the
first point for the same reason, and at the second only when the drain behind it cannot finish:
`rows.Close` reads the rest of the result set — on `context.TODO()`, the caller's deadline already
past — and closes the connection for any error but `io.EOF`. The next request then pays a dial, a
TLS handshake, a startup and the `SET`s again, so a flood of timeouts is also a flood of reconnects;
and inside a transaction, a closed connection fails every later statement, `COMMIT` included, with
`driver.ErrBadConn`.

The HTTP timeouts are set for you as of `ReadTimeout`/`WriteTimeout`: read defaults to 10s and write
to 30s, and read also bounds the keep-alive wait, since fasthttp falls back to it when `IdleTimeout`
is zero. That matters because Fiber defaults only `BodyLimit` and passes the three timeouts through
verbatim, and fasthttp reads zero as no timeout at all — a zero `Config` used to serve 262144
connection slots a client could hold open forever by dribbling a request body. This applies to `New`
and `Run`; an app you built with `fiber.New` and handed to `Mount` still owns its own.

**Volume is not bounded by default.** `luima.RateLimit(n, per, key)` in `Config.HTTPMiddleware` is
the smallest useful bound and answers 429 with `Retry-After`. It is per process — two replicas
enforce 2n — and it is a fixed window, so a caller straddling a boundary lands 2n inside one
window's width. Real rate limiting, with a shared store and a notion of who is calling, belongs in
the layer that does auth.

**Complexity is capped at 1000 by default.** That is a blunt instrument against pathological
queries, and not the same thing as the rate limiter above — it bounds one query's shape, not how
many a caller may send. It is narrower than it sounds:

- The cost is **per selected field**. A list field costs the same whether it returns one row or ten
  million, so the limit does nothing about an unbounded `SELECT`. Bound your lists with
  `q.Limit(n)` — luima ships no pagination, so nothing else will.
- It does **not** bound nesting depth. In a schema with a cycle — `User.friends: [User!]!` — 400
  levels of nesting costs about 400 complexity, passes the limit, and multiplies into a resolver
  call per node per level. `MaxDepth` is the answer to that one, below.
- Body-size and parse cost are separate again: the query text is fully parsed and validated before
  the complexity extension sees an operation. gqlgen sets **no** default token limit, so a large
  document pays the full parse before anything rejects it. Measured on one such document: 53 ms
  served, 36 ms to reject on complexity, 4 ms to reject on a token limit. Set one if you take
  untrusted queries:

  ```go
  Configure: func(srv *handler.Server) { srv.SetParserTokenLimit(10000) },
  ```

  It is a `Configure` line rather than a `Config` field on purpose — the right number depends on
  your largest legitimate query, and a wrong one rejects it.

Tune `ComplexityLimit` for your schema, and put real rate limiting in the layer that does auth.

**Nesting depth is capped at 15 by default.** Since 0.3.0. `Config.MaxDepth` rejects an operation
nested deeper than the limit with `extensions.code: DEPTH_LIMIT_EXCEEDED`, the same way gqlgen's own
complexity limit rejects. Zero means unset, negative disables it.

The walk resolves fragment spreads. It has to: a spread node carries no selection set of its own, so
a limiter that walks only the operation reads every named fragment as a leaf, and a 40-deep document
hidden behind `...F` measures 1 and executes. That is a two-line change to the attacking query, and
it is the difference between a depth limit and the appearance of one. An inline fragment is a type
condition, not a level, so `... on User { name }` costs nothing.

15 is chosen against the deepest document a default install serves — the playground's own
introspection query, which measures 13. If your schema legitimately nests deeper, raise it.

## Transport

**Cross-site request forgery: the default is closed.** luima ships no CSRF field, and that is not an
omission — it registers no transport that needs one. Measured against a default mount:

```
POST x-www-form-urlencoded : HTTP 400  "transport not supported"
POST multipart/form-data   : HTTP 400  "transport not supported"
POST text/plain            : HTTP 400  "transport not supported"
POST no Content-Type       : HTTP 400  "transport not supported"
GET  ?query=mutation{...}  : HTTP 406  "GET requests only allow query operations"
```

`transport.POST` requires `application/json`, which an HTML form cannot send, and a cross-site
`fetch` with that content type triggers a preflight luima answers without an
`Access-Control-Allow-Origin`. `transport.GET` refuses non-query operations.

Adding `transport.MultipartForm` through `Configure` removes all of that, and a cross-site form then
executes mutations with the caller's cookies. If you add it, add a required header with it —
[gotcha #37](docs/gotchas.md#37-the-multipart-transport-is-a-csrf-hole) has the code.

**Not every error is redacted, because not every error reaches the presenter.** A request no
transport accepts — an unsupported content type — is answered `transport not supported` by gqlgen's
handler, and the GET transport writes its own refusals; `PresentError` sees neither. A malformed
JSON body does reach it, and passes through as gqlgen's own text with the body echoed back in the
message. What is disclosed on these paths is the caller's own bytes, not the server's, but do not
treat "everything goes through `PresentError`" as a reason to skip sanitizing something.

With `sslmode` absent from your connection URL, TLS is on and the certificate is **not verified**,
`sslrootcert` or not. With neither parameter present `parseDSN` never enters its `sslmode` switch,
so every connection keeps the `&tls.Config{InsecureSkipVerify: true}` pgdriver's `newDefaultConfig`
starts from; an `sslrootcert` on its own does enter it — the switch is guarded on either parameter —
and lands in the empty case beside `allow` and `prefer`, which sets `InsecureSkipVerify` itself and
then loads roots nothing checks. Use `?sslmode=verify-full`.

**`?sslmode=verify-ca` checks the host name, as it did in 0.5.0.** pgdriver implements Postgres's
own definition — `InsecureSkipVerify: true` plus a `VerifyPeerCertificate` that calls
`x509.Certificate.Verify` with no `DNSName` — for `verify-ca`, and for `require` with an
`sslrootcert`. On its own that lets any certificate your roots trust pass, whatever host it names.
`db.Connect` clears both fields and hands the connection back to crypto/tls, which verifies the same
roots and the host name, so neither mode is weaker than `verify-full`. A chain-only check, when you
want one, is a `VerifyConnection` of your own in a `ConnectWith` tune.

**`?password=` and `?sslpassword=` are refused.** pgdriver reads neither, so each would go to the
server as `SET password TO '…'` once authentication had succeeded without it — trust, peer, a client
certificate, or the same password in the user info. Postgres rejects that statement and, at the
default `log_min_error_statement`, writes it to the server log, value included, where no redaction
of luima's reaches. `Connect` refuses both names, in any letter case, before anything dials.

luima no longer fills `ServerName`, because pgdriver sets it from the URL's authority for `require`,
`verify-ca` and `verify-full` alike. One shape still reaches crypto/tls's refusal to handshake with
neither `ServerName` nor `InsecureSkipVerify` set — a host named only with `?host=`, whose authority
is empty. Set `c.TLSConfig.ServerName` in a `ConnectWith` tune for that one. Falling back to
`?sslmode=require` is the workaround the same refusal invited before 0.2.0, and it is
`InsecureSkipVerify: true`. See [docs/deployment.md](docs/deployment.md).

`Connect`'s errors never contain the connection string or its password, so logging one does not leak
the credential. That is worth stating because through 0.5.0 it was not quite true: `url.Parse`
quotes the input it rejects, and an unescaped `/`, `?` or `#` in a password ends the URL's authority
early, so `postgres://app:Xk9/Q@db/app` failed with `invalid port ":Xk9" after host` — the
password's first part, into whatever the call site does with a startup error. Every quoted fragment
is now replaced, and a DSN whose user info was cut short is refused before pgdriver can dial part of
the password and name it in a `connection refused`.

## Supply chain

`make audit` runs `govulncheck` over both modules, and CI runs it on every push. Most of what it
reports for a module this size is the Go standard library, where "fixed in" means a toolchain
patch release rather than a dependency bump — `crypto/tls` and `crypto/x509` are reachable from
`Connect`, so keep the toolchain current.

## Supported versions

While the major version is `0`, only the latest minor release receives fixes.
