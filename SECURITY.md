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
own Fiber middleware on the group you `Mount` onto. If you need per-user RLS, connect as an
unprivileged role and set the request's claims per transaction.

## What luima does do

**The error presenter redacts.** gqlgen's default presenter forwards `err.Error()` verbatim, which
would hand an unauthenticated caller raw driver strings — `SQLSTATE 23505` and with it your table,
column and constraint names. `luimaerr.PresentError` passes through only errors a resolver has
explicitly marked safe (`*CustomError`) and gqlgen's own validation text about the query the
client just sent; everything else is logged server-side and returned as `internal server error`.

This is damage control on information disclosure, not access control. Do not mistake it for one.

**Introspection and the playground are on by default.** Both are the right default for
development and the wrong one for a public production endpoint. Set `DisablePlayground: true`; if
you also want introspection off, supply your own handler rather than relying on obscurity —
hiding the schema does not protect the data behind it.

**Complexity is capped at 1000 by default.** That is a blunt instrument against pathological
nested queries, not a rate limiter. Tune `ComplexityLimit` for your schema, and put real rate
limiting in the layer that does auth.

## Transport

With `sslmode` absent from your connection URL, `pg.ParseURL` returns
`&tls.Config{InsecureSkipVerify: true}` — TLS is on, but the certificate is **not verified**. Use
`?sslmode=verify-full`. See [docs/deployment.md](docs/deployment.md).

## Supported versions

While the major version is `0`, only the latest minor release receives fixes.
