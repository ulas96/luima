# Environment and deployment

[← back to the README](../README.md)

`pg.ParseURL` and Docker behaviours, then serving over TLS. All are surprising; the Postgres ones
are mostly startup failures rather than warnings, the serving ones mostly fail silently.

---

## Connection URLs

### `pg.ParseURL` accepts exactly three query parameters

`sslmode`, `application_name`, `connect_timeout`. Anything else is a hard error:

```
pg: options other than 'sslmode', 'application_name' and 'connect_timeout' are not supported
```

So pgx-style URL tuning — `?default_query_exec_mode=simple_protocol`, `?pool_max_conns=10` — is a
**startup failure**, not a no-op. If you are migrating from pgx, strip the extras.

Both `postgres://` and `postgresql://` schemes parse.

### With `sslmode` absent, TLS is on but the certificate is not verified

`ParseURL` returns `&tls.Config{InsecureSkipVerify: true}` when `sslmode` is not in the URL. A
managed Postgres therefore connects over TLS with nothing to configure — **and nothing verified.**

| `sslmode` | result |
|---|---|
| absent | TLS on, `InsecureSkipVerify: true` |
| `verify-ca`, `verify-full` | TLS on, certificate verified |
| `allow`, `prefer`, `require` | TLS on, `InsecureSkipVerify: true` |
| `disable` | no TLS |
| anything else | `pg: sslmode 'x' is not supported` |

Use `?sslmode=verify-full` in production. This is stated plainly because a library that ships
`InsecureSkipVerify` silently is doing its users a disservice.

The verified rows in that table are only true because `db.Connect` fixes up the `tls.Config`.
`pg.ParseURL` maps `verify-ca` and `verify-full` to a bare `&tls.Config{}` and never sets
`ServerName`, and the driver plumbs no host into its `tls.Client` call either — so crypto/tls
refuses the handshake outright with *"either ServerName or InsecureSkipVerify must be specified"*.
Before 0.2.0 `verify-full` therefore could not connect at all, and the natural workaround was
`?sslmode=require`, which is `InsecureSkipVerify: true`. `Connect` now fills `ServerName` in from
the address. Two smaller notes on the same code path: go-pg treats `verify-ca` and `verify-full` as
identical and both verify the hostname, which is stricter than Postgres's own definition of
`verify-ca`; and `?connect_timeout=N` bounds the whole boot round trip here, not just the dial.

### A DSN that lost its credentials still connects

`ParseURL` defaults the user to `postgres`, and go-pg then falls back to `$PGUSER`, `$PGPASSWORD`
and finally the literal `postgres`. A connection string mangled by a bad interpolation does not
fail as a configuration error — it attempts `postgres/postgres`, and on a permissive local or CI
database it *succeeds*. The `select 1` in `Connect` cannot tell "connected as the intended role"
from "connected as the fallback".

Set `?application_name=your-service` — one of the three parameters `ParseURL` accepts — so
`pg_stat_activity` shows which service, and which role, actually connected.

### Supabase: use the session pooler on port 5432

That is what this stack is tested against.

---

## Environment

### `.env` values must be unquoted

`docker --env-file` passes quotes through **literally**, so

```sh
DATABASE_URL="postgres://user:pass@host:5432/db"
```

works fine on your machine and fails to parse *inside the container* — the value includes the
quote characters. Write it bare:

```sh
DATABASE_URL=postgres://user:pass@host:5432/db
```

### Exporting `.env` for `go run` needs `set -a`

Plain `source .env` defines shell variables without exporting them, so Go's `os.Getenv` sees
nothing and you get a confusing `DATABASE_URL is not set`. The incantation is:

```sh
set -a && . ./.env && set +a && go run .
```

luima's own Makefile does this; copy the `ENV` variable from it.

---

## Connecting

`Connect` issues an eager `select 1`, because `pg.Connect` is lazy and dials nothing. Without that
round trip a bad credential surfaces one failed request at a time in production instead of once,
loudly, at boot.

It returns its error rather than calling `log.Fatal`, and takes the URL rather than reading
`os.Getenv` itself. A library must not kill your process, choose your logging, or read
configuration behind your back:

```go
db, err := luima.Connect(os.Getenv("DATABASE_URL"))
if err != nil {
	log.Fatal(err)   // your call, not luima's
}
defer db.Close()
```

---

## Serving over TLS

luima never calls `Listen`. `New` returns a `*fiber.App` and hands it back, and `Mount` registers
routes on a router you already have — so how the process reaches the network is yours to decide,
and both shapes below are supported.

### Terminating TLS in the process

Fiber's `ListenConfig`, unchanged by luima:

```go
app := luima.New(luima.Config{Schema: …})

log.Fatal(app.Listen(":443", fiber.ListenConfig{
	CertFile:    "cert.pem",
	CertKeyFile: "key.pem",
}))
```

`ListenConfig.TLSConfig` takes a `*tls.Config` for mTLS or a pinned cipher list, and
`AutoCertManager` takes an `*autocert.Manager` for ACME. `AutoCertManager` and `CertFile` together
are an error, not a precedence rule.

**This is HTTP/1.1 only.** Fiber advertises `NextProtos: []string{"http/1.1", "acme-tls/1"}`, and
fasthttp ships no HTTP/2 implementation, so there is no h2 to negotiate and no way to configure one
in. If you want HTTP/2 to the client, you want the next section — a proxy speaks h2 outward and
HTTP/1.1 to this process.

### Behind a proxy, the request looks plaintext — and that is fine

Terminate TLS at nginx, Caddy, an ALB or Cloudflare and forward to `http://127.0.0.1:8080`. Nothing
in luima reads the scheme, `r.TLS`, or any `X-Forwarded-*` header, so there is nothing to configure
and nothing that breaks. This is the better-supported shape, and the one the quickstart assumes.

What a resolver — or an `HTTPMiddleware` layer — actually sees on such a request:

| | value |
|---|---|
| `r.TLS` | **`nil`** — the hop into this process really is plaintext |
| `r.URL.Scheme` | **`""`** — origin-form request line, there is no scheme to parse |
| `r.URL.String()` | the path, e.g. `/graphql` |
| `r.Host` | the `Host` header, as the proxy sent it |
| `r.RemoteAddr` | **the proxy's address**, never the client's |
| `X-Forwarded-*` | present and unmodified — the adaptor copies every header verbatim |

Three consequences, none of which produces an error:

**`Config.Fiber.TrustProxy` does not do what it looks like it does.** It changes `c.Scheme()`,
`c.IP()` and `c.Host()` — accessors on `fiber.Ctx`. Your resolvers never hold a `fiber.Ctx`; they
hold the `*http.Request` the adaptor built, which `TrustProxy` does not touch. Setting it is not
wrong, it is simply invisible downstream of `Mount`. Read the header yourself:

```go
cfg.HTTPMiddleware = []func(http.Handler) http.Handler{
	func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip, _, _ := strings.Cut(r.Header.Get("X-Forwarded-For"), ",")
			ctx := context.WithValue(r.Context(), clientIPKey{}, strings.TrimSpace(ip))
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	},
}
```

That is trustworthy only because your proxy *overwrites* `X-Forwarded-For` rather than appending to
whatever the client sent. Exposed directly, it is client-controlled input and means nothing.

**Middleware that infers HTTPS from `r.TLS != nil` will conclude the connection is insecure.** A
session layer drops the `Secure` cookie attribute, a redirect-to-https helper bounces forever.
Neither logs anything. Gate on the header instead:

```go
secure := r.Header.Get("X-Forwarded-Proto") == "https"
```

**Do not let the proxy strip a path prefix.** The playground's scheme is safe automatically — it
builds its fetch URL as `location.protocol + '//' + location.host + endpoint`, so the browser's
HTTPS carries — but `endpoint` is embedded verbatim. Serve luima under `/api/` with the prefix
stripped and the page loads, then posts to `/graphql` while the world only routes `/api/graphql`.
Pass the prefix through, or set `Config.Endpoint` to the externally visible path.

---

## Security posture

> ### luima ships no authentication
>
> Your server connects as a Postgres role. If that role is privileged — which it is for the
> default Supabase connection string — **Row Level Security does not apply to it**, and every
> query runs with full access to every row.
>
> There is no auth middleware, no token validation, no per-request role switching. Put something
> in front of luima before it faces the internet: an API gateway, your own middleware in
> `Config.HTTPMiddleware`, a Fiber middleware on the group you `Mount` onto, or a reverse proxy
> that terminates auth. `luima.RateLimit` is not that something — it bounds volume, not access.
>
> The error presenter redacts driver text so an unauthenticated caller cannot read your schema off
> a failed query. That is damage control, not authorization.

Your middleware passes identity to a resolver with `c.SetContext`, and the CRUD helpers take the
query modifier that scopes a row to its owner. Both are in
[SECURITY.md](../SECURITY.md#getting-the-callers-identity-into-a-resolver), which is the one place
that walks the whole path.

If you need per-user RLS, connect as an unprivileged role and set the request's claims per
transaction — that is a real design, and it is out of scope for v1.

`examples/quickstart/main.go` is the deployable shape: a server-side `statement_timeout`, a
`/healthz` liveness path, a rate limiter, CORS, playground and introspection behind `LUIMA_DEV`, a
bounded list query and a graceful shutdown on SIGTERM through `luima.Run`. HTTP timeouts do not
appear in it, because read, write and request now default to 10s, 30s and 15s. It is the one file
in the repo that deliberately does not use the zero `luima.Config`, because a library call has no
deployment context and an application does — and it imports no Fiber package, which is the property
to preserve when copying it.

**The liveness path.** `Health` is registered by `Mount`, so it works on an app you built yourself
too, and `HTTPMiddleware` does not wrap it — the rate limiter cannot 429 the probe and take a
healthy-but-busy process out of rotation. `HealthCheck` receives a context with a 2s deadline and
runs on its own goroutine, so a wedged database answers 503 rather than hanging: a probe that hangs
reads to a load balancer as a slow server rather than a broken one, and slow servers are left in.
`db.Ping` already has the signature.

**Draining.** `luima.Run` returns its error instead of exiting, which is what lets a
`defer db.Close()` above it run — `log.Fatal` calls `os.Exit` and skips every deferred function.
The drain window is 10s. Set your orchestrator's termination grace period above that, or it will
`SIGKILL` the process mid-drain.

---

See also: [The gqlgen contract](gqlgen-contract.md) · [Fiber](fiber.md) · [Gotchas](gotchas.md)
