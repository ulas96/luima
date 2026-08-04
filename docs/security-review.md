# Security review

A component-by-component review of luima 0.1.0 and the four libraries it glues together. Written
against commit `cca1f19`, Go 1.26, `gqlgen v0.17.94`, `fiber/v3 v3.4.0`, `go-pg/pg/v10 v10.15.1`,
`gqlparser/v2 v2.5.36`, `fasthttp v1.72.0`.

> **Status: every P0 and P1 finding below was fixed in v0.2.0.** The analysis is left as written —
> it is the reasoning that produced the fixes, and the "what breaks if you remove this line"
> arguments are what stop them being undone. Read it as a review of 0.1.0, not as a report on the
> current release: [the fix list](#the-fix-list) carries the per-finding status, and what is still
> open is named there. `CHANGELOG.md` has the user-facing version.

This is a review, not a policy. For how to report a vulnerability, and for the posture luima has
already committed to in writing, see [SECURITY.md](../SECURITY.md) and
[deployment.md](deployment.md#security-posture). **This document deliberately does not repeat
them.** The no-auth/no-RLS scope, the redaction rationale and the `InsecureSkipVerify` default are
already stated in three places each; everything below is something those documents do not say, and
in four cases contradicts advice they currently give.

Claims about third-party behaviour cite a file and line under `$(go env GOMODCACHE)` so each one is
re-checkable. Two findings were additionally confirmed by running code; they are marked
**measured**.

## What luima already gets right

Worth stating first, so nobody "hardens" one of these back into a hole.

| | why it matters |
|---|---|
| The three-branch presenter | `luimaerr.PresentError` is the only thing standing between an unauthenticated caller and your constraint and column names, and `tests/crud_test.go:94-96` asserts the *absence* of `SQLSTATE` and the table name against a real driver error. Negative assertions against real Postgres are rare and correct. |
| `r.All`, never `r.Post` | Pinned by `tests/server_test.go:36-51` via the `Allow` header, which only `transport.Options` can set. The test proves the request reached gqlgen rather than being answered by Fiber. |
| `transport.POST` requires `application/json` | `gqlgen/graphql/handler/transport/http_post.go:36-41`. A cross-site `<form>` cannot send that content type without a preflight, so **mutations are not reachable by CSRF**. No CSRF token is needed — and will not be, until someone adds the multipart or form transport. |
| `transport.GET` rejects non-query operations | `http_get.go:96-100`. Mutations cannot ride in on a link or an `<img src>`. |
| `RETURNING *`, and `Update` not `UpdateNotZero` | Both are integrity decisions dressed as ergonomics. `UpdateNotZero` cannot clear a column, which is a silent data-retention bug; the godoc at `crud/crud.go:113-118` already argues this correctly. |
| The no-auth warning is loud and repeated | `README.md:54`, `SECURITY.md:14`, `docs/deployment.md:97`. Most libraries in this position say it once, in a footnote. |

## 1 · db/ and go-pg

### D-01 · P0 · `?sslmode=verify-full` cannot connect at all

`pg.ParseURL` maps `verify-ca` and `verify-full` to a bare `&tls.Config{}`
(`go-pg/pg/v10@v10.15.1/options.go:241-243`) and **never sets `ServerName`** — the only `tls.Client`
call in the driver is `tls.Client(cn.NetConn(), tlsConf)` at `messages.go:165`, with no host
plumbed in. crypto/tls refuses that handshake outright:

```go
// $GOROOT/src/crypto/tls/handshake_client.go:43-47
func (c *Conn) makeClientHello() (...) {
	if len(config.ServerName) == 0 && !config.InsecureSkipVerify {
		return nil, nil, nil, errors.New("tls: either ServerName or InsecureSkipVerify must be specified in the tls.Config")
	}
```

So the one mode that verifies anything fails at boot, and the pressure is to fall back to
`sslmode=require` — which `options.go:244` maps to `InsecureSkipVerify: true`. **There is currently
no way to get verified TLS through `db.Connect`.** Four places tell the reader otherwise:
`SECURITY.md:50`, `docs/deployment.md:38`, `.env.example:9`, `db/db.go:23`.

Two smaller notes on the same code path: in go-pg `verify-ca` and `verify-full` are *identical* and
both verify the hostname, which is stricter than Postgres's own definition of `verify-ca`; and
`sslmode` absent also yields `InsecureSkipVerify: true` (`options.go:251`), which is the documented
default everyone already knows about.

**Fix** — `db/db.go`, after `ParseURL`:

```go
// pg.ParseURL builds the tls.Config but never fills in ServerName, and crypto/tls refuses a
// handshake with neither ServerName nor InsecureSkipVerify. Without these four lines
// ?sslmode=verify-full — the only mode that verifies anything — cannot connect.
if t := opt.TLSConfig; t != nil && !t.InsecureSkipVerify && t.ServerName == "" {
	if host, _, err := net.SplitHostPort(opt.Addr); err == nil {
		t.ServerName = host
	}
}
```

**Test that fails without it:** `TestCRUD` against a TLS-capable Postgres with
`DATABASE_URL=…?sslmode=verify-full`. Today it fails at `Connect`. Nothing in the suite covers
this: `tests/db_example_test.go:28-34` documents `verify-full` in an `Example` with no `// Output:`
block, so it compiles and never runs.

### D-02 · P0 · a query has no timeout at any layer

Four layers, none of them bounding a query:

- **The driver.** `Options.init()` sets no `ReadTimeout` or `WriteTimeout` default — read it at
  `options.go:103-181`; only `PoolTimeout` falls back to 30s (`options.go:148-153`). Zero means no
  timeout.
- **The DSN.** `ParseURL` hard-errors on any parameter other than `sslmode`, `application_name` and
  `connect_timeout` (`options.go:273`), so `statement_timeout` and `options=-c …` cannot be put in
  the URL. The usual escape hatch is closed.
- **Postgres.** Nothing sets `statement_timeout`, so the server will run a query as long as it
  takes.
- **The context.** The resolver's `ctx` never carries a deadline — see S-01.

`PoolSize` defaults to `10 × NumCPU` (`options.go:143-145`). So roughly `10 × NumCPU` slow queries
occupy the pool indefinitely and every other request fails on the 30s pool timeout. No
authentication is required to send them.

**Fix** — make the options reachable. Adding a variadic parameter is source-compatible for every
existing caller:

```go
func Connect(url string, opts ...func(*pg.Options)) (*pg.DB, error) {
	opt, err := pg.ParseURL(url)
	// … D-01's ServerName fix …
	for _, o := range opts {
		o(opt)
	}
	// …
}
```

Then document the one recipe that cannot be expressed any other way, because `ParseURL` rejects it
in the DSN:

```go
db, err := luima.Connect(os.Getenv("DATABASE_URL"), func(o *pg.Options) {
	o.ReadTimeout = 10 * time.Second
	o.OnConnect = func(ctx context.Context, cn *pg.Conn) error {
		_, err := cn.Exec("set statement_timeout = 10000")
		return err
	}
})
```

The `luima.Connect` shim in `luima.go:81` has to grow the same parameter.

### D-03 · P1 · `Connect` prints the database password on a malformed DSN

`db/db.go:38` is `fmt.Errorf("parse database url: %w", err)`. When `url.Parse` is what failed, that
`err` is a `*url.Error`, whose `Error()` embeds the raw URL with **no redaction**:

```go
// $GOROOT/src/net/url/url.go:40
func (e *Error) Error() string { return fmt.Sprintf("%s %q: %s", e.Op, e.URL, e.Err) }
```

The quickstart then does `log.Fatal(err)` (`examples/quickstart/main.go:24`), so a typo in a
production DSN writes `parse "postgres://app:hunter2@db.internal/app": …` to stderr and into
whatever ships stderr to a log aggregator. The window is narrow — a syntactically valid DSN never
reaches this branch — but the blast radius is a live credential in a log store with different
retention and different access control from your secret manager.

**Fix:** drop the URL, keep the diagnosis.

```go
var ue *neturl.Error
if errors.As(err, &ue) {
	// url.Error embeds the raw DSN, password and all, and the call site logs this.
	return nil, fmt.Errorf("parse database url: %s: %w", ue.Op, ue.Err)
}
```

**Test that fails without it:** `Connect("postgres://u:s3cret@h/d?x=1")`, assert the error string
does not contain `s3cret`. Note `pg.ParseURL`'s *own* errors (`options.go:273`, the unsupported
option) do not include the URL, so only the `url.Parse` branch needs this.

### D-04 · P2 · a credential-less DSN silently tries default credentials

`ParseURL` defaults the user to `postgres` (`options.go:222`), and `Options.init()` falls back to
`$PGUSER`, `$PGPASSWORD` and then literal `postgres` (`options.go:132-138`). A DSN that lost its
credentials to a bad interpolation does not fail as a configuration error — it attempts
`postgres/postgres`, and on a permissive local or CI database it *succeeds*. The `select 1` ping in
`Connect` cannot distinguish "connected as the intended role" from "connected as the fallback".

Worth a line in the docs rather than code: `application_name` is one of the three parameters
`ParseURL` accepts, and setting it makes `pg_stat_activity` show you which service — and which
role — actually connected.

## 2 · server/, Fiber and the adaptor

### S-01 · P0 · the resolver context never cancels and never has a deadline

`adaptor.HTTPHandler` (`server/server.go:145`) resolves to
`fasthttpadaptor.NewFastHTTPHandler`, which hands the handler the `*fasthttp.RequestCtx` as the
request context: `h.ServeHTTP(w, r.WithContext(ctx))` (`fasthttp/fasthttpadaptor/adaptor.go:84`).
That object satisfies `context.Context`, but only nominally — fasthttp says so itself:

```go
// fasthttp/server.go:2992-2993
// Note: Because creating a new channel for every request is just too expensive, so
// RequestCtx.s.done is only closed when the server is shutting down.
func (ctx *RequestCtx) Done() <-chan struct{} { return ctx.s.done }
```

**Measured** (fiber v3.4.0 + fasthttp v1.72.0): the `ctx` reaching a handler mounted with
`adaptor.HTTPHandler` has dynamic type `*fasthttp.RequestCtx`, `Deadline()` returns `ok == false`,
and `Done()` is closed only at shutdown.

Three consequences:

1. **A client hang-up does not stop the query.** The resolver runs to completion and the row goes
   nowhere. An attacker fires expensive queries and disconnects immediately; the server does all
   the work anyway, and D-02 means nothing else stops it either.
2. **No middleware can impose a deadline.** `context.WithTimeout` in a Fiber handler is discarded —
   see S-02.
3. **Any resolver code that respects cancellation is dead code**, including go-pg's own
   context handling in `ModelContext`. It compiles, it reads as correct, and it never fires.

### S-02 · P0 · `c.SetContext` is silently discarded; `c.Locals` works only by accident

`SECURITY.md:23` and `README.md:60` say to put "your own Fiber middleware on the group you `Mount`
onto". That middleware can reject a request — `return c.SendStatus(401)` before `c.Next()` works
fine. What it cannot reliably do is *tell the resolver who the caller is*, which is what per-row
authorization needs.

**Measured**, from a middleware that wraps `c.Context()` in a 5s `WithTimeout` and a
`WithValue(userKey{}, "user-42")`, then calls `c.SetContext` with the result:

| what the resolver sees | `adaptor.HTTPHandler` (today) |
|---|---|
| `ctx` dynamic type | `*fasthttp.RequestCtx` |
| `ctx.Value(userKey{})` | `nil` — **the identity is gone** |
| `ctx.Deadline()` | not set — **the timeout is gone** |
| `ctx.Done()` | never closes |
| `ctx.Value("user")` after `c.Locals("user", …)` | `"user-42"` — works |

So the idiomatic Fiber v3 route (`SetContext`, the one that also carries deadlines and cancellation)
fails silently, while the legacy one (`Locals`) happens to work because `RequestCtx.Value` reads
fasthttp UserValues and `Ctx.Locals` writes one. That is a coincidence of the adaptor's
implementation, it is untyped and collision-prone across middleware, it cannot carry a deadline, and
**no luima document mentions either fact**. A consumer who tries the documented approach and finds
`ctx.Value` empty will reach for the shared `Resolver` struct instead — and identity on a
`Resolver` field is a cross-request data race, which is an authorization bypass under concurrency,
not a bug you find in testing.

**Fix** — eight lines, exported API only, and it fixes S-01 at the same time:

```go
// fasthttpadaptor hands the handler the *fasthttp.RequestCtx, which never cancels and drops
// anything middleware put in c.SetContext. HTTPHandlerWithContext stashes Fiber's request context
// as a user value; this re-attaches it so gqlgen — and every resolver — sees a real
// context.Context, with whatever deadline and values the middleware set.
func withFiberContext(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fc, ok := adaptor.LocalContextFromHTTPRequest(r); ok {
			r = r.WithContext(fc)
		}
		next.ServeHTTP(w, r)
	})
}

r.All(endpoint, adaptor.HTTPHandlerWithContext(withFiberContext(srv)))
```

**Measured** with that change in place: dynamic type `*context.valueCtx`, `Deadline()` set,
`Done()` live, and `ctx.Value(userKey{})` returns `"user-42"`. Both
`adaptor.HTTPHandlerWithContext` and `adaptor.LocalContextFromHTTPRequest` are exported from
`fiber/v3/middleware/adaptor` (`adaptor.go:66-86`) — no reimplementation of the ResponseWriter
bridge, no new dependency.

**Test that fails without it:** mount the stub schema behind a middleware that calls
`c.SetContext(context.WithValue(…))`, and have the stub's `Exec` assert the value is visible. This
is the single highest-value change in the review: it is what makes authenticated, cancellable,
deadline-bounded resolvers possible at all, and it makes the auth advice in `SECURITY.md` true.

### S-03 · P1 · "CORS preflight, for browser clients" is only half true

`server/server.go:112` and `docs/fiber.md:85` both read as though registering
`transport.Options{}` handles CORS. It does not. That transport sets exactly one header:

```go
// gqlgen/graphql/handler/transport/options.go:22-30
case http.MethodOptions:
	w.Header().Set("Allow", o.allowedMethods())
	w.WriteHeader(http.StatusOK)
```

No `Access-Control-Allow-Origin` is set by luima anywhere. A preflight that returns 200 without it
still fails in the browser, so a cross-origin client is blocked — the 405 is fixed, the CORS is
not. The reader who follows the comment will conclude CORS is handled, find it isn't, and reach for
`cors.New()` — whose Fiber v3 default is `AllowOrigins: []string{"*"}`
(`fiber/middleware/cors/config.go:92`).

That default is not catastrophic on its own, because `AllowCredentials` defaults to `false`
(`config.go:104`) so cookies are not sent cross-origin. It becomes a data leak the moment the
consumer's auth is a header token their own SPA holds, or they flip `AllowCredentials` to make
cookie auth work.

**Fix:** reword the comment and `docs/fiber.md` to say what `transport.Options` actually buys (the
preflight reaches gqlgen instead of 405-ing), and show the explicit-origin form:

```go
app.Use(cors.New(cors.Config{AllowOrigins: []string{"https://app.example.com"}}))
```

`HEAD` is worth a footnote while you are in there: `Options.Do` answers it with 405
(`options.go:27-29`).

### S-04 · P1 · introspection is unconditional and has no off switch

`server/server.go:124` is `srv.Use(extension.Introspection{})`, outside any conditional, and
`Config` has no field for it. `SECURITY.md:36-39` acknowledges this and answers "supply your own
handler" — i.e. abandon `Mount`, which is the entire library. `DisablePlayground` exists; its
sibling does not.

**Fix:** `DisableIntrospection bool`, documented identically to `DisablePlayground` (zero value =
on, because the zero `Config` must stay the good development configuration):

```go
if !cfg.DisableIntrospection {
	srv.Use(extension.Introspection{})
}
```

Include the honest caveat, because it is the more interesting half: turning introspection off does
**not** hide your field names. gqlparser's validator appends `Did you mean …?` suggestions to
validation errors (`gqlparser/v2@v2.5.36/validator/core/helpers.go:57` — that is what the
`agnivade/levenshtein` dependency is for), and `PresentError` passes validation errors through by
design. A caller who guesses `nam` learns there is a `name`. Introspection off is defence in depth
and a smaller attack surface; it is not confidentiality, and `SECURITY.md:38` is already right to
say obscurity protects nothing.

### S-05 · P1 · no HTTP timeouts in the shape people copy

`luima.Config{Schema: …}` leaves `cfg.Fiber` zero, and Fiber only defaults `BodyLimit`
(`fiber/app.go:709`) — `ReadTimeout`, `WriteTimeout` and `IdleTimeout` are passed through as zero
(`app.go:1518-1520`), which fasthttp reads as "no timeout". A connection that dribbles a request
body one byte at a minute holds a slot forever; `Concurrency` defaults to 262144
(`app.go:587`), so there are a lot of slots to hold and each one costs a `ReadBufferSize` buffer.

`ExampleNew_production` (`tests/luima_example_test.go:39-52`) already shows the right values — but
it has no `// Output:` block, so it only compiles, and `examples/quickstart/main.go` omits them
entirely. The example that runs is the one people copy.

**Fix:** move the timeouts into `examples/quickstart/main.go` (see Q-01).

### S-06 · P2 · the complexity limit does not bound what it looks like it bounds

`ComplexityLimit` defaults to 1000, and `SECURITY.md:41` correctly calls it "a blunt instrument…
not a rate limiter". It is worth being more specific, because the gap is not obvious:

- gqlgen's generated complexity is **per selected field**. A list field costs the same whether it
  returns one row or ten million — the count is not an input to the calculation unless you register
  a complexity function that reads a `first`/`limit` argument, and luima's schema has no such
  argument. So `ComplexityLimit` does nothing about C-02.
- There is **no depth limit anywhere in the stack**. gqlparser has no max-depth rule (no
  `maxDepth`/`MaxDepth` symbol exists in the module) and gqlgen ships no extension for one. In a
  schema with a cycle — `User.friends: [User!]!` — 400 levels of nesting costs ~400 complexity,
  passes a 1000 limit, and multiplies into a resolver call per node per level. luima ships no
  dataloader, and `docs/gqlgen-contract.md:125-138` already documents that a `resolver: true` field
  runs once per row.
- Body-size and parse cost are separate again: 4 MB of query text is fully parsed **and validated**
  before the complexity extension ever sees an operation.

**Fix:** state the above in the config table rather than only "caps query complexity". If you want
a real bound, a `MaxDepth int` implemented as an `OperationInterceptor` that walks
`opCtx.Doc.Operations` is ~25 lines and is the thing that actually stops a cyclic schema.

### S-07 · P2 · no rate limiting, and `r.All` forwards methods no transport serves

No rate limiting anywhere, by design — `SECURITY.md:43` puts it in "the layer that does auth". Fair,
but `limiter.New()` is one line of an already-installed dependency and belongs in the quickstart,
because the layer that does auth does not exist yet in the quickstart.

Separately: `r.All` sends `PUT`, `PATCH`, `DELETE` and `TRACE` to gqlgen, where no transport
`Supports` them and the response is a 422 "transport not supported". Harmless — there is no body
reflection, so no cross-site tracing concern — but it is unpinned behaviour next to a line the
project calls its most important, and one more assertion in `TestMountRoutes` would fix that.

### Bonus: the adaptor may no longer block streaming

`CLAUDE.md` and `docs/gotchas.md:18` state that subscriptions are blocked by architecture because
`adaptor.HTTPHandler` buffers the whole response. That was true; in fasthttp v1.72.0 it is only half
true. `NewFastHTTPHandler` now runs the handler in a goroutine and switches on the first event it
observes (`fasthttpadaptor/adaptor.go`, the `modeCh` block): `modeDone` buffers and calls
`ctx.Response.SetBody`, but `modeFlushed` sends headers and hands off to `SetBodyStreamWriter`. A
handler that calls `Flush` streams.

Do not act on this yet — SSE also needs a request context that cancels when the client goes away,
which is exactly what S-01 says you do not have. But "blocked by architecture, not effort" is now
"blocked by S-01", which is a different and much shorter sentence. Worth re-testing after the S-02
fix lands.

## 3 · luimaerr/

### E-01 · P1 · the `*gqlerror.Error` passthrough unwraps, so redaction is opt-out

```go
// luimaerr/errors.go:79-82
var ge *gqlerror.Error
if errors.As(err, &ge) {
	return ge
}
```

`errors.As` walks the whole chain. Any error that *wraps* a `*gqlerror.Error` anywhere inside it is
returned whole — message, path, extensions — bypassing redaction entirely:

```go
// reaches the client verbatim, table name and tenant id included
return fmt.Errorf("insert into %s failed for tenant %d: %w", table, tenantID, gqlErr)
```

The comment above it justifies the branch on the grounds that these errors are "gqlgen's own text
about the query the client just sent". That is true of the errors gqlgen produces, and false of the
type as a matcher. The branch is load-bearing and must stay — dropping it makes every schema typo
read as "internal server error" — but it should match only what it claims to match.

**Fix:** one line. Assert on the top-level error instead of unwrapping to it.

```go
// A type assertion, not errors.As: unwrapping here lets a resolver smuggle internals past
// redaction by wrapping a gqlerror. gqlgen's own parse and validation errors arrive unwrapped.
if ge, ok := err.(*gqlerror.Error); ok { //nolint:errorlint // see above
	return ge
}
```

gqlgen hands parse and validation errors to the presenter unwrapped, so the needed path is
unaffected.

**Test that fails without it:** the existing `TestPresentError` (`tests/errors_test.go:20-33`) uses
a bare `gqlerror` and passes either way. The case that fails is
`fmt.Errorf("internal detail: %w", &gqlerror.Error{Message: "x"})` — assert it presents as
`internal server error`.

### E-02 · P1 · the log line is a log-injection and PII sink

```go
// luimaerr/errors.go:86
log.Printf("resolver error: %v", err)
```

Two problems, one character apart:

- **Injection.** `err` routinely contains attacker-controlled text — a GraphQL variable echoed back
  by a constraint message, or C-04's `label`. `%v` writes newlines literally, so a caller who sends
  `personalId: "x\nresolver error: all clear"` writes a second, forged log line. Anything that
  parses these logs per-line can be lied to.
- **PII.** A Postgres error's DETAIL field carries the offending row's values —
  `Key (email)=(victim@example.com) already exists`. So the data the presenter just took the trouble
  to redact from the client lands in stdout in plaintext, in a log store with different retention
  and different access control from the database.

**Fix:** `log.Printf("resolver error: %q", err)`. `%q` escapes the newlines, and the quoting makes
the boundary of the untrusted string visible. The PII half is a scope decision, not a bug — but it
should be *stated*, because "we redact errors" reads as a stronger claim than "we redact errors on
the wire and log them verbatim". The godoc at lines 83-85 is right that a `Config.Logger` field
would duplicate `ErrorPresenter`; the missing piece is the four-line recipe that shows the wrap:

```go
cfg.ErrorPresenter = func(ctx context.Context, err error) *gqlerror.Error {
	slog.ErrorContext(ctx, "resolver error", "err", err)
	return luimaerr.PresentError(ctx, err)
}
```

That double-logs through `PresentError`'s own `log.Printf`, which is itself an argument for
extracting the redaction decision from the logging.

### E-03 · P2 · panics have no story, no test, and no knob

`Mount` never sets a `RecoverFunc`, so gqlgen's default applies:

```go
// gqlgen/graphql/recovery.go:14-20
func DefaultRecover(ctx context.Context, err any) error {
	fmt.Fprintln(os.Stderr, err)
	fmt.Fprintln(os.Stderr)
	debug.PrintStack()
	return gqlerror.Errorf("internal system error")
}
```

Safe on the wire today, because that message is a constant. But note *how* it is safe: the returned
value is a `*gqlerror.Error`, so it takes `PresentError`'s **passthrough** branch, not the redaction
branch. A consumer who writes a `RecoverFunc` that formats the panic value into the message —
the obvious thing to write — is returning it verbatim to the client, and nothing in luima warns
them. There is also no `Config` field for `RecoverFunc`, so getting structured panic logging means
abandoning `Mount`, exactly as with introspection.

This matters more here than it would in most libraries, because `docs/gqlgen-contract.md:17-43`
establishes that an unfilled resolver stub is an **unauthenticated remote panic** that compiles
clean, and the only thing standing between that and production is a `grep` in CI.

**Fix:** a `RecoverFunc graphql.RecoverFunc` field (two lines), plus the first test in the suite
that panics a resolver and asserts the response body contains no stack frame. Also worth knowing:
`fasthttpadaptor` has its *own* `recover()` around the handler goroutine
(`fasthttpadaptor/adaptor.go:65-72`), logging to `ctx.Logger()` — so a panic that escapes gqlgen is
caught by fasthttp, and `fiber.Config.ErrorHandler` never sees it either, consistent with
`docs/fiber.md:40-48`.

### E-04 · P2 · `CustomError.Error()` re-leaks what the presenter redacted

`luimaerr/errors.go:44-49` concatenates `UserMessage` with the internal cause. That is correct for
the log and documented as such. The footgun is the round trip:

```go
return nil, &luimaerr.CustomError{UserMessage: err.Error()} // ships the driver string to the client
```

`UserMessage` is returned verbatim (`errors.go:73`), so populating it from any `error` — including
another `CustomError` — undoes the redaction in one line that reads like careful error handling.
Worth naming in the godoc next to "Assume it is public".

## 4 · crud/

### C-01 · P0 · `Get`, `Update` and `Delete` cannot express an ownership predicate

`List` takes `opts ...func(*orm.Query) *orm.Query`. The other four take nothing, and `Get`,
`Update` and `Delete` are hard-wired to `WherePK()` (`crud/crud.go:43`, `:139`, `:162`). There is
therefore no way to write the query that authorization actually needs:

```sql
DELETE FROM app_users WHERE personal_id = $1 AND owner_id = $2
```

To get that second predicate you drop to raw go-pg and hand-roll the SQLSTATE classification —
which is the one thing `crud` exists to provide (`crud/crud.go:3-6` says so). The library's happy
path is therefore IDOR by construction, and the quickstart is the proof: `deleteUser(personalId:)`
deletes any row for anyone who can reach the port
(`examples/quickstart/graph/schema.resolvers.go:27-29`).

luima is right that auth is out of scope. Being unable to *express* authorization is a different
thing, and it is a gap in the API, not in the scope.

**Fix** — the symmetry `List` already has. Source-compatible for every existing caller:

```go
func Delete[T any](ctx context.Context, db orm.DB, key *T, opts ...func(*orm.Query) *orm.Query) (bool, error) {
	q := db.ModelContext(ctx, key).WherePK()
	for _, opt := range opts {
		q = opt(q)
	}
	res, err := q.Delete()
	// …
}
```

```go
// and the resolver that was previously impossible:
luima.Delete(ctx, r.DB, &model.User{PersonalID: id}, func(q *orm.Query) *orm.Query {
	return q.Where("owner_id = ?", callerID(ctx))
})
```

For `Update` and `Delete` the "no row matched" branch already exists and already produces
`label + " not found"`, so a row that exists but is not yours reports as absent — which is the
correct answer to give an unauthorized caller anyway.

**Test that fails without it:** extend `TestCRUD` with two rows of different owners; assert that a
`Delete` scoped to owner A returns `false` for owner B's key and leaves the row in place.

### C-02 · P0 · `List` with no options is an unbounded `SELECT *`

`crud/crud.go:69-81` applies exactly the options it is given, and `crud/crud.go:66` documents
"none means select every row" as a neutral fact. One `users` query against a large table
materializes every row into `[]*T`, then gqlgen marshals the whole response into memory, then
`fasthttpadaptor` buffers it again before writing (`modeDone` → `ctx.Response.SetBody`). Three
copies, no bound. S-06 explains why `ComplexityLimit` does not help: the row count is not an input
to the complexity calculation.

The quickstart's `Users` resolver applies only `Order` (`schema.resolvers.go:32-36`) and the schema
offers no `first`/`limit` argument, so the copy-pasted template is the unbounded one.

**Fix, minimum:** add `.Limit(100)` to the quickstart resolver and rewrite the godoc's neutral
sentence as a warning that names the failure. Pagination is deliberately out of scope
(`CLAUDE.md`), and that is defensible — but "we ship no pagination" and "our example template
selects every row" are different statements, and only the first one is in the docs.

**Fix, stronger, if you would rather the library refuse:** a `Limit` applied inside `List` when no
option set one. That needs inspecting the built query, which go-pg does not make pleasant, so the
honest version is a documented convention rather than an enforced one.

### C-03 · P1 · `Update`'s full replace blanks any column the input does not carry

`Update` writes every column of the model (`crud/crud.go:139`), and the model is built by a
hand-written mapper — `newUser` in `examples/quickstart/graph/resolver.go:24-30`. So a column that
exists on the struct but is not set by the mapper is written as its **zero value** on every update.
Add `Role string \`pg:"role"\`` to `model.User`, forget to touch `newUser`, and every `updateUser`
call silently resets that user's role to `""`. Same for `owner_id`, `deleted_at`, `password_hash`.
`go build` is happy, the tests are happy, and the GraphQL response — built from `RETURNING *`, which
is otherwise a virtue — shows the blanked value as though it were intended.

The godoc argues the full replace correctly on data-retention grounds (`crud/crud.go:113-118`) and
says real partial updates need "nullable input fields and a Column allowlist". Right conclusion;
the missing half is that the *current* design has a failure mode of its own, and it is a security
one whenever the blanked column is an authorization column.

Related, and one line away: `autobind` points at the model package
(`examples/quickstart/gqlgen.yml:18-19`), so any GraphQL `input` whose name matches a struct there
binds to it. Write `input User { … }` and your DB model becomes the input type — every column
client-writable, textbook mass assignment. The example avoids this by having a separate
`UserInput`; nothing enforces it.

**Fix:** document both in `docs/gotchas.md`, where the silent-failure catalogue lives. Optionally
add the allowlist form, which is five lines and makes the safe pattern the short one:

```go
// UpdateColumns @notice Writes only the named columns, leaving every other column untouched.
func UpdateColumns[T any](ctx context.Context, db orm.DB, m *T, label string, cols ...string) (*T, error)
	// … db.ModelContext(ctx, m).Column(cols...).WherePK().Returning("*").Update()
```

**Test that fails without it:** a two-column model where the second column is set in the DB and
zero in the struct; assert `UpdateColumns` leaves it, and that `Update` clears it. The second
assertion documents C-03 in code, which is the only place people reliably read.

### C-04 · P1 · `Create`'s `label` is an existence oracle and a reflected-input path

`crud/crud.go:105` builds `label + " already exists"` from caller-supplied text, and the documented
usage is `Create(ctx, db, u, "user "+id)`. On any signup-shaped mutation that is a textbook account
enumeration primitive: send an email address, learn from the error whether it is registered. It is
working as designed — the whole point of `Create` is that a unique violation reaches the client —
but the design has a name and the docs should use it.

Two second-order notes:

- The label is attacker-controlled text returned verbatim in `errors[].message`. The response is
  JSON, so there is no injection at luima's layer; a client that renders error messages into the
  DOM inherits the sink. `luimaerr/errors.go:27` already says "Assume it is public"; it should also
  say "assume it is untrusted".
- The label is the only thing distinguishing a duplicate from a redacted internal error, so
  shortening it to a constant is a real trade-off, not a free win.

**Fix:** documentation. For an enumeration-sensitive table, a constant label (`"that record already
exists"`) plus the real detail in the log is the pattern; say so where `Create` is documented, since
that is where the decision gets made.

### C-05 · P2 · the injection surface is the options closure, not the helpers

Nothing in `crud` is SQL-injectable: go-pg parameterizes values, and identifiers come from struct
tags resolved at compile time. Worth stating plainly, because it is the question every reviewer asks
first.

The surface is next door. `List` hands the resolver a `*orm.Query`, and filtering and pagination
are explicitly out of scope — so consumers *will* write that code, and `q.OrderExpr`,
`q.ColumnExpr`, `q.Having` and `q.Where(fmt.Sprintf(…))` all interpolate raw SQL by design. A
client-supplied sort column reaching `OrderExpr` is an injection; reaching `Order` is not, because
`Order` quotes a single-token identifier — a distinction nobody should have to rely on.

**Fix:** a short "if you build filtering" section. `q.Where("name = ?", v)` for values,
`pg.Ident` for identifiers and `pg.Safe` for fragments you have vetted (both exported at
`go-pg/pg/v10/pg.go:26-30`), and an allowlist — not an escape — for any client-supplied sort
column. This belongs in luima's docs specifically *because* the design pushes the code onto the
consumer.

### C-06 · P2 · no transaction boundary, so bolted-on authorization has a TOCTOU window

Each helper is one autocommitted statement. A consumer's `Get`-then-check-then-`Update` has a real
window between the check and the write. `db.RunInTransaction` plus passing the `*pg.Tx` already
works — every helper takes `orm.DB` precisely so it does (`crud/crud.go:8-9`) — and
`SELECT … FOR UPDATE` is expressible through `List`'s options. It just is not written down next to
the authorization advice, which is where someone needs it.

## 5 · The quickstart

### Q-01 · P1 · the template is the insecure shape

`examples/quickstart/main.go` is 35 lines and is what every consumer starts from. As written it has
the playground on `/`, introspection on, no timeouts, no rate limit, no CORS, an unbounded `users`
query, and `log.Fatal(err)` on a `Connect` error that may contain the password (D-03). Each
individual gap is documented *somewhere* — `README.md:282` mentions `DisablePlayground`,
`tests/luima_example_test.go:39-52` shows the timeouts — but nothing that people copy has any of
it.

The single highest-leverage documentation change in this review is to make the quickstart the
production shape, with the development conveniences switched on by an environment variable rather
than by default:

```go
app := luima.New(luima.Config{
	Schema:            generated.NewExecutableSchema(/* … */),
	DisablePlayground: os.Getenv("LUIMA_DEV") == "",
	Fiber: fiber.Config{
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
		BodyLimit:    1 << 20,
	},
})
app.Use(limiter.New())
```

Also worth fixing while there: `defer db.Close()` never runs, because `log.Fatal` calls `os.Exit`
(`main.go:26`, `:34`). Harmless for a process that is exiting, but it is a worked example of a
pattern people copy into code where it matters, and a graceful `ShutdownWithTimeout` on `SIGTERM` is
the thing a deployment doc should be demonstrating.

### Q-02 · P2 · the example README's quoted DSN is the failure the repo warns about

`examples/quickstart/README.md:23` says `export DATABASE_URL='postgres://…'`. That is fine for a
shell, and it is exactly the quoting that `.env.example:1-2` and `docs/deployment.md:49-62` warn
breaks under `docker --env-file`. Not a vulnerability; a documentation inconsistency in the one
place a reader is most likely to copy from.

## 6 · Build and supply chain

CI is in better shape than most: workflow-level `permissions: contents: read`
(`.github/workflows/ci.yml:9-10`), dependabot on both modules plus actions, `-race` on the test run,
and a grep that fails the build if `TestCRUD` skipped. Three gaps.

### B-01 · P1 · no `govulncheck`, no `gosec`

`.golangci.yml` enables errcheck, govet, ineffassign, staticcheck, unused, misspell, unconvert and
revive. Nothing scans for known vulnerabilities in the dependency graph, and `make check` — the
documented pre-PR gate — has no such step. For a library whose entire job is wiring four other
libraries together, that is the highest-value CI addition available.

```make
audit:  ## govulncheck the module and the example
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...
	cd examples/quickstart && go run golang.org/x/vuln/cmd/govulncheck@latest ./...
```

Wire it into `check` and add it as a CI job. `gosec` in golangci is a smaller win — it would flag
`InsecureSkipVerify` if luima set it directly, which it does not, since go-pg sets it — but the
`errorlint` linter is worth adding specifically because it is the linter that would have caught
E-01.

### B-02 · P2 · actions are tag-pinned and the linter is unpinned

`actions/checkout@v7`, `actions/setup-go@v7` and `golangci/golangci-lint-action@v9` are mutable
tags, and the lint job passes `version: latest` (`ci.yml:85`), so the linter binary is whatever
exists at run time. SHA-pin the actions and pin the linter version; a moving `latest` also means a
green build can turn red without a commit.

Note the `example` job runs `go tool gqlgen generate` (`ci.yml:114`), which executes module code
from the PR's own `go.mod`. That is fine as configured — `pull_request`, not
`pull_request_target`, a read-only token, and no secrets beyond a localhost DSN — and it is worth
knowing that the safety comes from those three properties, so that adding a secret to this workflow
is a decision and not a chore.

### B-03 · P2 · `go-pg` is in maintenance mode

`go-pg/pg` is no longer actively developed; its author moved to `uptrace/bun`. It is simultaneously
the component least likely to receive a timely security fix and the one holding your credentials
and TLS configuration — and D-01 is an example of the kind of thing that does not get fixed
upstream. Nothing to do today. Worth recording that `pgx` (plus `bun` if the ORM layer is wanted) is
the migration target if the driver ever needs a fix that is not coming, and that `luimaerr.SQLState`
is the seam that would have to change: its godoc at `luimaerr/errors.go:98-101` already documents
that `pg.Error` is an interface and `pgx`'s is a struct pointer, which is most of the port.

## The fix list

**P0 — the API cannot currently express the secure thing**

| | fix | where | status |
|---|---|---|---|
| S-01/S-02 | propagate the request context (`HTTPHandlerWithContext` + 8-line re-attach) | `server/server.go:145` | ✅ fixed in v0.2.0 |
| C-01 | `opts ...func(*orm.Query) *orm.Query` on `Get`, `Update`, `Delete` | `crud/crud.go`, `luima.go` | ✅ fixed in v0.2.0 |
| D-01 | set `TLSConfig.ServerName` so `verify-full` connects | `db/db.go` | ✅ fixed in v0.2.0 |
| C-02 | bound the quickstart's `List`; rewrite the godoc's neutral sentence | `schema.resolvers.go`, `crud/crud.go:66` | ✅ fixed in v0.2.0 |

**P1 — small diffs, real exposure**

| | fix | where | status |
|---|---|---|---|
| D-02 | `Connect(url, opts ...func(*pg.Options))`, plus the `statement_timeout` recipe | `db/db.go`, `luima.go:81` | ✅ fixed in v0.2.0 — as `server.Config.RequestTimeout` plus a bounded boot ping, not `Connect` opts |
| D-03 | strip the DSN from the parse error | `db/db.go:38` | ✅ fixed in v0.2.0 |
| E-01 | non-unwrapping `*gqlerror.Error` assertion, plus the wrapped-error test | `luimaerr/errors.go:79` | ✅ fixed in v0.2.0 |
| E-02 | `%q` in the log line; document the presenter-wrapping recipe | `luimaerr/errors.go:86` | ✅ fixed in v0.2.0 |
| S-04 | `DisableIntrospection bool`, with the "Did you mean" caveat | `server/server.go:124` | ✅ fixed in v0.2.0 |
| S-03 | reword the CORS comment; show an explicit-origin `cors.New` | `server/server.go:112`, `docs/fiber.md:85` | ✅ fixed in v0.2.0 |
| S-05/Q-01 | timeouts, `limiter`, `DisablePlayground` in the quickstart | `examples/quickstart/main.go` | ✅ fixed in v0.2.0 |
| C-03/C-04 | document the blanking failure, `autobind` mass assignment, and the enumeration oracle | `docs/gotchas.md`, `crud/crud.go` | ✅ fixed in v0.2.0 — and `Update`'s opts now express the `q.Column(...)` allowlist |
| B-01 | `make audit` / `govulncheck` in CI; add `errorlint` | `Makefile`, `ci.yml`, `.golangci.yml` | ✅ fixed in v0.2.0 |

**P2 — before v1**

`RecoverFunc` field plus a panic test (E-03) · `UpdateColumns` allowlist variant (C-03) · `MaxDepth`
(S-06) · the "if you build filtering" section (C-05) · `Order`/`OrderExpr`, `TRACE`, and `HEAD`
assertions in `TestMountRoutes` (S-07) · SHA-pin actions and the linter (B-02) · the credential
fallback note (D-04).

Of these, v0.2.0 landed B-02 (actions and the linter are SHA-pinned in `ci.yml`) and half of E-03:
`TestPanicLeaksNoStack` pins the no-leak default, and `Config.Configure` makes `SetRecoverFunc`
reachable, so the knob exists without a dedicated field. S-06, S-07, C-05 and D-04 remain open —
though a consumer can now build a depth limit themselves through `Configure`, which is how S-06's
mitigation is expressible today.

## Questions this review cannot answer

Scope calls, not defects — listed because each one changes what "secure by default" means for
luima, and they are the author's to make.

1. **Should the zero `Config` be the good *development* configuration or the good *production*
   one?** `CLAUDE.md` commits to the former, and it is why the playground and introspection are on.
   Q-01's answer — production by default, development behind `LUIMA_DEV` — inverts that invariant.
   It is the right inversion for something that faces the internet, and it is a breaking change to
   a documented design principle, so it needs a decision rather than a patch.
2. **Is "no auth" compatible with shipping CRUD helpers at all?** C-01 makes the secure pattern
   expressible, which is clearly right. Whether `crud` should go further — a `Scope` type, a
   required tenant predicate — is the line between "boilerplate you would get wrong" and the auth
   framework luima has said it will not be.
3. **Who owns the query timeout?** D-02 can be fixed in `Connect`, in the resolver context after
   S-02 lands, or by declaring it the consumer's problem and documenting the recipe. All three are
   defensible; silently having none is not.
