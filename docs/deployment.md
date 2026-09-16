# Environment and deployment

[← back to the README](../README.md)

pgdriver's DSN parsing and Docker behaviours, then serving over TLS. All are surprising; the
Postgres ones now fail at the boot round trip rather than at parse — and some of them do not fail at
all — while the serving ones mostly fail silently.

---

## Connection URLs

### An unknown query parameter becomes a `SET`, not a parse error

pgdriver's `parseDSN` reads eleven parameters — `host`, `sslmode`, `sslrootcert`, `sslcert`,
`sslkey`, `application_name`, `timeout`, `dial_timeout`, `connect_timeout`, `read_timeout` and
`write_timeout`. Everything left over becomes a `Config.ConnParams` entry, and `newConn` sends one
`SET <name> TO <value>` per entry on every connection it opens, before that connection is used.

So `?statement_timeout=5s` works now, where go-pg's URL parser refused every parameter but three —
`sslmode`, `application_name` and `connect_timeout`. What that refusal bought was a parse-time
error, and this is what replaces it:

| DSN parameter | what happens |
|---|---|
| `?statement_timeout=5s` | `SET statement_timeout TO '5s'` on every new connection |
| `?statement_timout=5s` (typo) | the boot round trip fails, SQLSTATE **42704**, `unrecognized configuration parameter` |
| `?pool_max_conns=10` (pgx-style) | the same 42704 — strip the extras, as before |
| `?app.tenant=acme` | **accepted silently**; Postgres takes a dotted name as a custom setting |
| `?a-b=1` | the boot round trip fails, SQLSTATE **42601** — Postgres cannot read the key as an identifier |

The failures are `Connect`'s, at startup, wrapped as `ping: …` — which is the reason `Connect` does
a round trip at all. The dotted name has nothing to fail: if you rely on one, read it back from
`pg_settings`.

`sslcert` and `sslkey` are read only alongside `sslmode` or `sslrootcert`, because `parseDSN` looks
at them inside that branch. Given on their own they are left over like any other parameter, go out
as `SET`s, and fail 42704.

`password` and `sslpassword` are refused at parse, in any letter case. pgdriver reads neither, so
each would go out as `SET password TO '…'`, which Postgres rejects — and logs, value and all, at the
default `log_min_error_statement`. Put the password in the user info, percent-encoded, or set
`c.Password` in a `ConnectWith` tune.

An integer timeout is a count of seconds, and one that is **`<= 0` keeps the default**:
`queryOptions.duration` turns it into `-1`, a deadline that has already passed, and `Connect` puts
pgdriver's default back — 5s to dial, 10s to read, 5s to write — as 0.5.0 did for
`?connect_timeout=0`. Written as a duration, `?read_timeout=0s` parses to zero, which `parseDSN`
skips, with the same result. No DSN spelling means "no timeout"; that takes a `ConnectWith` tune,
and `Connect`'s doc comment says why not to.

`postgres://`, `postgresql://` and `unix:///path/to/socket` all parse.

### With `sslmode` absent, TLS is on but the certificate is not verified

`newDefaultConfig` starts every connection at `&tls.Config{InsecureSkipVerify: true}`, and
`parseDSN` enters its `sslmode` switch only when `sslmode` or `sslrootcert` is in the URL. A URL
with neither therefore keeps that default: a managed Postgres connects over TLS with nothing to
configure — **and nothing verified.**

| `sslmode` | TLS | verified |
|---|---|---|
| absent, `allow`, `prefer` | on | nothing — `InsecureSkipVerify: true`, `sslrootcert` or not |
| `require` | on | nothing, unless `sslrootcert` is given too, when it acts as `verify-ca` |
| `verify-ca` | on | chain and host name — as in 0.5.0; pgdriver alone checks the chain only |
| `verify-full` | on | chain and host name |
| `disable` | off | — |
| anything else | — | `pgdriver: sslmode 'x' is not supported`, at parse |

Use `?sslmode=verify-full` in production. This is stated plainly because a library that ships
`InsecureSkipVerify` silently is doing its users a disservice.

**`verify-ca` checks the host name, as it did in 0.5.0.** go-pg mapped `verify-ca` and
`verify-full` to the same bare `&tls.Config{}`, which verifies the host name too. pgdriver
implements Postgres's own definition instead — `InsecureSkipVerify: true` plus a
`VerifyPeerCertificate` that calls `x509.Certificate.Verify` with no `DNSName` — for `verify-ca`
and for `require` with an `sslrootcert`. `Connect` clears both, so crypto/tls verifies the same
roots and the host name, and a DSN carried over from 0.5.0 is not quietly weaker. A chain-only
check, for a certificate that names a host other than the one you dial, is a `VerifyConnection` of
your own in a `ConnectWith` tune.

luima no longer fills `ServerName`, because pgdriver sets it — for `require`, `verify-ca` and
`verify-full` alike — from the host in the URL's authority with its port stripped
(`net.SplitHostPort`). That retires the 0.2.0 fix: before it, `verify-full` could not connect at
all, crypto/tls refused the handshake with *"either ServerName or InsecureSkipVerify must be
specified"*, and the natural workaround was `?sslmode=require`, which is `InsecureSkipVerify: true`.
One shape still reaches that refusal: a host named only with `?host=`, which leaves the URL's
authority — and so `ServerName` — empty. Set `c.TLSConfig.ServerName` in a `ConnectWith` tune for
that one.

No mode but `disable` falls back to plaintext. `allow` and `prefer` are TLS here, and a server that
answers the SSL request with anything but `S` is `pgdriver: SSL is not enabled on the server`
(`enableSSL`) — so a Postgres without TLS, a CI container or a unix socket, needs
`?sslmode=disable`. And `?connect_timeout=N` bounds the whole boot round trip, not just the dial:
`ConnectWith` gives its `select 1` a context deadline of `DialTimeout`, which is what
`connect_timeout` sets.

### A DSN that lost its credentials still connects

`newDefaultConfig` fills the config from the environment before the DSN is applied at all:
`$PGHOST`, `$PGPORT`, `$PGUSER` and `$PGDATABASE`, falling back to `localhost`, `5432`, `postgres`
and `postgres`. A connection string mangled by a bad interpolation does not fail as a configuration
error — it attempts `postgres/postgres`, and on a permissive local or CI database it *succeeds*. The
`select 1` in `Connect` cannot tell "connected as the intended role" from "connected as the
fallback".

Three fallbacks changed with the driver:

- **`$PGPASSWORD` is read by luima, not the driver.** pgdriver takes a password only from the URL's
  user info; `Connect` fills an empty one from `$PGPASSWORD`, as go-pg did, before any `ConnectWith`
  tune runs. go-pg fell back once more, to `postgres`; luima does not, so a deployment with neither
  connects with no password — accepted by a `trust` server, refused at boot by a
  password-authenticated one.
- **A DSN with no database name is silent.** `parseDSN` sets the database only when the URL has a
  path longer than `/`, so `postgres://u:p@host:5432` connects to `$PGDATABASE`, then `postgres`.
  Under go-pg luima raised that as a parse error. Now it is a working connection to the wrong
  database.
- **`$PGPORT` applies only when the DSN names no host.** With a host, `parseDSN` uses it and appends
  `:5432` when it contains no colon — so an IPv6 literal, which is all colons, gets no port at all:
  `postgres://u:p@[::1]/db` fails to dial with `missing port in address` until it is written
  `[::1]:5432`. `?host=` is dialed as written, with no port added either.

**Percent-encode `/`, `?`, `#` and `%` in a user or a password.** An unescaped one ends the URL's
authority early, and what happens next depends on what is left. `url.Parse` rejects
`postgres://app:Xk9/Q@db/app` outright, for the non-numeric port `:Xk9` — the password's first part,
quoted in the error. It accepts `postgres://127.0.0.1:2024#x@db/app`, where the user becomes the
host and `2024`, the start of the password, becomes the port, for pgdriver to dial and name in
`dial tcp 127.0.0.1:2024: connect: connection refused`; and `postgres://app:p@ss/x@db/app`, which
dials the host `ss`. `Connect` refuses the shapes that parse before anything dials: what they have
in common, and a well-formed DSN almost never has, is the `@` that should have ended the user info
sitting outside the authority — in the path, query or fragment — and a DSN that means it can write
that one `%40`. The shape `url.Parse` itself rejects comes back with every quoted fragment
replaced and the same advice appended. No error `Connect` returns quotes the DSN or the password;
in 0.5.0 the parse error carried the password's first part.

Set `?application_name=your-service` so `pg_stat_activity` shows which service, and which role,
actually connected.

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

`Connect` returns a `*bun.DB` and issues an eager `select 1` on it, because `sql.OpenDB` and
`bun.NewDB` dial nothing — pgdialect's `Init` is empty. Without that round trip a bad credential
surfaces one failed request at a time in production instead of once, loudly, at boot; and now that
an unrecognized DSN parameter is a `SET` rather than a parse error, the round trip is also where a
typo in one is caught.

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

`Connect` also sizes the pool, because `database/sql`'s defaults are wrong for a server: unlimited
open connections turn a burst of requests into a burst of connections and then into Postgres's
`53300 too_many_connections`, and two idle connections mean every burst past two concurrent queries
re-dials, re-handshakes and re-`SET`s the extras it just closed. It sets ten per CPU open and idle
with a 5 minute idle time — the pool 0.5.0 ran with, which was go-pg's own default sizing. Resize it
on the handle if that is wrong for you: `*bun.DB` embeds `*sql.DB` through its state struct, so
`SetMaxOpenConns`, `SetMaxIdleConns`, `SetConnMaxIdleTime` and `Stats` are all promoted onto it.

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
`db.PingContext` already has the signature — `*bun.DB` embeds `*sql.DB` through its state struct.
Not `db.Ping`, which takes no context, does not compile in the field, and would have run the probe
on `context.Background()` and ignored that 2s deadline.

**Draining.** `luima.Run` returns its error instead of exiting, which is what lets a
`defer db.Close()` above it run — `log.Fatal` calls `os.Exit` and skips every deferred function.
The drain window is 10s. Set your orchestrator's termination grace period above that, or it will
`SIGKILL` the process mid-drain.

---

See also: [The gqlgen contract](gqlgen-contract.md) · [Fiber](fiber.md) · [Gotchas](gotchas.md)
