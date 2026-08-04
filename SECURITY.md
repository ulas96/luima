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

Put authentication in front of it: an API gateway, a reverse proxy that terminates auth, or your
own Fiber middleware on the group you `Mount` onto.

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

`CancelRequest` is best-effort — it dials a second connection and only logs a failure. For a bound
Postgres enforces itself, set `statement_timeout` on the role, or build `*pg.Options` yourself with
an `OnConnect` that does it and call `pg.Connect` instead of `luima.Connect`.

Set the HTTP timeouts too. Fiber defaults only `BodyLimit`; `ReadTimeout`, `WriteTimeout` and
`IdleTimeout` pass through as zero, which fasthttp reads as no timeout at all. See
`examples/quickstart/main.go`.

**Complexity is capped at 1000 by default.** That is a blunt instrument against pathological
queries, not a rate limiter, and it is narrower than it sounds:

- The cost is **per selected field**. A list field costs the same whether it returns one row or ten
  million, so the limit does nothing about an unbounded `SELECT`. Bound your lists with
  `q.Limit(n)` — luima ships no pagination, so nothing else will.
- There is **no depth limit anywhere in the stack**. In a schema with a cycle —
  `User.friends: [User!]!` — 400 levels of nesting costs about 400 complexity, passes the limit,
  and multiplies into a resolver call per node per level.
- Body-size and parse cost are separate again: the query text is fully parsed and validated before
  the complexity extension sees an operation.

Tune `ComplexityLimit` for your schema, and put real rate limiting in the layer that does auth.

## Transport

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
