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

Then scope the query. Every CRUD helper except `Create` takes query modifiers, which is where the
ownership predicate goes:

```go
luima.Delete(ctx, r.DB, &model.User{PersonalID: id}, func(q *orm.Query) *orm.Query {
    return q.Where("owner_id = ?", callerID(ctx))
})
```

A row that exists but is not the caller's then reports as absent, which is the right answer to
give an unauthorized caller — it discloses no existence.

If you need per-user RLS instead, connect as an unprivileged role and set the request's claims per
transaction.

## What luima does do

**The error presenter redacts.** gqlgen's default presenter forwards `err.Error()` verbatim, which
would hand an unauthenticated caller raw driver strings — `SQLSTATE 23505` and with it your table,
column and constraint names. `luimaerr.PresentError` passes through only errors a resolver has
explicitly marked safe (`*CustomError`) and gqlgen's own validation text about the query the
client just sent; everything else is logged server-side and returned as `internal server error`.

This is damage control on information disclosure, not access control. Do not mistake it for one.

Two limits worth stating plainly:

- **It redacts on the wire, not in the log.** The redacted error is written to stdout in full, and
  a Postgres error's DETAIL field carries the offending row's values —
  `Key (email)=(victim@example.com) already exists`. Your log store therefore inherits the
  database's confidentiality requirements. Wrap `PresentError` with a `Config.ErrorPresenter` if
  you need to filter or route that.
- **`CustomError.UserMessage` is returned verbatim.** Never build it from another error —
  `&CustomError{UserMessage: err.Error()}` undoes the redaction in one line that reads like
  careful error handling. Treat it as untrusted too: the usual way to build it is from client
  input, so a client that renders error messages into the DOM inherits that sink.

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

**Requests are bounded.** `Config.RequestTimeout` defaults to 15s and puts a deadline on the
resolver's context. That is the only bound a query gets — go-pg sets no read or write timeout by
default, and `pg.ParseURL` rejects `statement_timeout` in the connection string — so it is doing
real work: go-pg turns a context deadline into the socket deadline, bounds the connection-pool
wait with it, and turns cancellation into a Postgres `CancelRequest` against the running backend.
Without it, an unauthenticated caller can fire expensive queries and disconnect, and the server
completes every one of them.

`CancelRequest` is best-effort — it dials a second connection and only logs a failure, so a query
can outlive its own cancellation while holding a pooled connection every other request is queued
behind. For a bound Postgres enforces itself whether or not the client is still there:

```go
db, err := luima.ConnectWith(os.Getenv("DATABASE_URL"), luima.StatementTimeout(10*time.Second))
```

A query that exceeds it comes back as SQLSTATE `57014`, which `luima.SQLState` reads; a client-side
deadline gives you `context.DeadlineExceeded` and no SQLSTATE, which is how you tell the two apart.
Setting `statement_timeout` on the role does the same thing one layer down, and is the better answer
when more than one application shares the role.

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

**Not every error is redacted, because not every error reaches the presenter.** A transport-level
failure — a malformed JSON body, an unsupported content type — is written by gqlgen's transport
before an executor exists, so `PresentError` never sees it and a malformed body is echoed back in
the message. What is disclosed there is the caller's own bytes, not the server's, but do not treat
"everything goes through `PresentError`" as a reason to skip sanitizing something.

With `sslmode` absent from your connection URL, `pg.ParseURL` returns
`&tls.Config{InsecureSkipVerify: true}` — TLS is on, but the certificate is **not verified**. Use
`?sslmode=verify-full`. See [docs/deployment.md](docs/deployment.md).

`luima.Connect` fills in the `ServerName` that `pg.ParseURL` leaves empty, because without it
crypto/tls refuses the handshake outright and `verify-full` cannot connect at all. Before 0.2.0 it
could not; if you worked around that by falling back to `?sslmode=require`, that is
`InsecureSkipVerify: true` and worth changing back.

`Connect`'s errors never contain the connection string, so logging one does not leak the password.

## Supply chain

`make audit` runs `govulncheck` over both modules, and CI runs it on every push. Most of what it
reports for a module this size is the Go standard library, where "fixed in" means a toolchain
patch release rather than a dependency bump — `crypto/tls` and `crypto/x509` are reachable from
`Connect`, so keep the toolchain current.

## Supported versions

While the major version is `0`, only the latest minor release receives fixes.
