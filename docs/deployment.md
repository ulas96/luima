# Environment and deployment

[← back to the README](../README.md)

These are all `pg.ParseURL` and Docker behaviours. All are surprising, and most are startup
failures rather than warnings.

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

## Security posture

> ### luima ships no authentication
>
> Your server connects as a Postgres role. If that role is privileged — which it is for the
> default Supabase connection string — **Row Level Security does not apply to it**, and every
> query runs with full access to every row.
>
> There is no auth middleware, no token validation, no per-request role switching. Put something
> in front of luima before it faces the internet: an API gateway, a Fiber middleware of your own
> on the group you `Mount` onto, or a reverse proxy that terminates auth.
>
> The error presenter redacts driver text so an unauthenticated caller cannot read your schema off
> a failed query. That is damage control, not authorization.

If you need per-user RLS, connect as an unprivileged role and set the request's claims per
transaction — that is a real design, and it is out of scope for v1.

---

See also: [The gqlgen contract](gqlgen-contract.md) · [Fiber](fiber.md) · [Gotchas](gotchas.md)
