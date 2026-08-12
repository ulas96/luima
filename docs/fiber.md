# Fiber v3 — the implementation detail that matters most

[← back to the README](../README.md)

The gqlgen server is an `http.Handler`. Fiber runs on fasthttp. The bridge is one line:

```go
r.All(endpoint, adaptor.HTTPHandlerWithContext(withFiberContext(srv)))
```

`adaptor.HTTPHandlerWithContext` wraps `fasthttpadaptor`, converting fasthttp's `RequestCtx` into
an `*http.Request` **per request**. Never plain `HTTPHandler` on this route: that variant hands the
resolver the raw `*fasthttp.RequestCtx`, which reports no deadline and never cancels.

> **Be clear about what that buys.** Fiber here provides routing, middleware and its ecosystem —
> **not** speed. gqlgen does exactly the work it always did, plus a conversion. Anyone telling you
> this stack is fast *because* of Fiber has not read the adaptor.

Four consequences follow, and **every one of them is a silent failure**, not a compile error.

---

## 1. `All`, never `Post`

gqlgen's transports dispatch on the HTTP method **themselves**. `transport.GET`, `transport.POST`
and `transport.Options` each inspect the request and decide whether they handle it. So `GET`,
`POST` **and** the `OPTIONS` preflight all have to reach `srv`.

Register with `r.Post` and every browser client hits a 405 on the CORS preflight — with no error
anywhere in the server logs, because the request never reached gqlgen. You will debug the client.

`All` registers every method in `Config.RequestMethods`. In `fiber/v3@v3.4.0`, `DefaultMethods` is
`GET, HEAD, POST, PUT, DELETE, CONNECT, OPTIONS, TRACE, PATCH, QUERY` — `OPTIONS` included.

**This is the single most important line in the library.** `server.TestMountRoutes` exists to pin
it, and asserts the `Allow` header specifically, because only `transport.Options` sets that — so
the assertion proves the request reached gqlgen rather than being answered by Fiber.

---

## 2. Fiber's `ErrorHandler` is unreachable — and must stay that way

The adaptor **always returns `nil`**. No resolver error can ever become a Fiber error, so
`fiber.Config.ErrorHandler` will never see one.

`PresentError` is therefore the only contract for errors a **resolver** returns. luima does not
offer a second one through `Config.Fiber.ErrorHandler` and does not document one — a consumer who
sets it and assumes their resolver errors flow through it has built a redaction layer that never
runs.

**It is not the only path to the wire, and that distinction matters.** Transport-level failures are
written by gqlgen's transport *before* an executor exists, so they never reach the presenter and
are not redacted. POST a malformed JSON body and the response is HTTP 400 with your own bytes
reflected back:

```
{"errors":[{"message":"json request body could not be decoded: invalid character 'S' looking for
 beginning of value body:{\"query\": SECRET-CANARY-abc"}],"data":null}
```

Same for `"transport not supported"`. Nothing of the *server's* is in that path — it is the
caller's own body coming back to the caller — but do not decide you need not sanitize something on
the strength of "everything goes through `PresentError`".

The good news, measured alongside: errors gqlgen *does* hand to the presenter keep their codes.
Parse (`GRAPHQL_PARSE_FAILED`), validation (`GRAPHQL_VALIDATION_FAILED`) and complexity
(`COMPLEXITY_LIMIT_EXCEEDED`) rejections come out byte-identical to gqlgen's own
`DefaultErrorPresenter` — the `*gqlerror.Error` pass-through branch carries them for free.

---

## 3. `Get("/")` is an **exact** match

`http.Handle("/", …)` in `net/http` is a *prefix* match — it serves the playground for every
unrouted path. Fiber's `Get("/")` does not. An unknown path is a plain Fiber 404.

This is the better behaviour, but it will surprise anyone porting from `net/http`, and it is the
second thing `TestMountRoutes` pins. Route a catch-all explicitly if you want one.

---

## 4. `BodyLimit`, and why subscriptions are really out

**Fiber's default `BodyLimit` is 4 MB**, where `net/http` had none. Irrelevant to queries. It
becomes relevant the day someone adds the multipart transport for file uploads — and it will
present as a mysterious rejection, not a clear error. Raise it via `Config.Fiber.BodyLimit`.

> **Before you add that transport, read what it opens.** Adding `transport.MultipartForm` through
> `Configure` also makes your endpoint reachable by a cross-site HTML form — `multipart/form-data`
> is a "simple" request, so no preflight protects it, and a mutation submitted from any origin will
> execute with the caller's cookies attached (measured). If you add it, require a header a form
> cannot set — `Apollo-Require-Preflight`, or your own — in an `HTTPMiddleware`, and reject requests
> without it. See [gotcha #37](gotchas.md#37-the-multipart-transport-is-a-csrf-hole).

**Subscriptions are not implemented, and until 0.3.0 this section gave the wrong reason.** It said
the adaptor buffers the entire response, so no streaming transport could work through it. That was
measured and it is false — a `Flush`ing handler streams correctly through
`adaptor.HTTPHandlerWithContext`, eight chunked frames arriving 400 ms apart, `content-length: -1`.
It is false for plain `adaptor.HTTPHandler` too, so the change between the two is not what fixed it.

Two things did block them:

- **`Mount` registered `transport.POST` before `Configure` ran.** gqlgen selects the first transport
  whose `Supports` matches, and `POST` matches `POST` + `application/json` without ever reading
  `Accept` — a strict superset of what SSE matches. So a `Configure`-registered SSE transport was
  unreachable by construction: it compiled, it mounted, it returned 200, and every subscription
  silently answered with one buffered response. **Fixed in 0.3.0** — the transports now register
  after `Configure`.
- **fasthttp does not cancel the request context when the client hangs up.** Measured: the client
  closed the connection after three frames and the resolver was still producing twenty frames later,
  with `ctx.Err() == nil`. For a query that is survivable, because `RequestTimeout` bounds it at
  15s. For a subscription it is not — a subscription has to disable `RequestTimeout` to live longer
  than 15s, and disabling it removes the only bound there is, so an abandoned subscription holds a
  goroutine and its database work forever. **This one is upstream**, and it is the real blocker.

`transport.Websocket` was not measured. The adaptor implements `http.Hijacker` as of fasthttp v1.72
(`fasthttpadaptor/adaptor.go`), which is the interface gorilla's `Upgrader.Upgrade` type-asserts, so
it is no longer structurally blocked — but this document does not claim it works.

---

## What `Mount` actually does, and why each line is there

```go
srv := handler.New(cfg.Schema)
```

Not `handler.NewDefaultServer` — deprecated in v0.17.94, and it gives no way to set an error
presenter, which makes the [error contract](../README.md#error-handling) impossible.

```go
srv.AddTransport(transport.Options{})  // answers the OPTIONS preflight
srv.AddTransport(transport.GET{})
srv.AddTransport(transport.POST{})
```

`handler.New` adds none. Without `Options{}`, see consequence 1 above.

**`transport.Options` is not CORS.** It sets exactly one header — `Allow` — and returns 200. That
fixes the 405; it does not make a cross-origin request work, because it sets no
`Access-Control-Allow-Origin`. A preflight that returns 200 without one still fails in the browser,
with an error message that names neither field. `luima.CORS` is the fix, and it goes in
`HTTPMiddleware` rather than on the app:

```go
luima.Run(ctx, ":8080", luima.Config{
    Schema: schema,
    HTTPMiddleware: []func(http.Handler) http.Handler{
        luima.CORS(luima.CORSConfig{Origins: []string{"https://app.example.com"}}),
    },
})
```

Origins are exact and listed, and a single `"*"` is the only wildcard. There is no `Credentials`
knob: `Access-Control-Allow-Credentials` with a wildcard origin is the classic misconfiguration,
and the case it serves needs an auth layer luima does not ship — leaving the field out makes the
combination unrepresentable, which is stronger than validating it.

It sets `Vary: Origin` on every response it touches, including the ones it refuses. That line is
the reason to prefer it over four hand-written headers: the grant is echoed from a request header,
so without `Vary` any shared cache — a CDN, a corporate proxy, the browser's own store — serves the
first caller's `Access-Control-Allow-Origin` to the second, and the symptom is a CORS failure that
reproduces for one user and no one else.

**One wart.** `HTTPMiddleware` runs inside the adaptor and Fiber's `timeout` middleware wraps
outside it, so a request that hits `RequestTimeout` is answered by that middleware and its 408
carries no CORS headers — the browser then reports a CORS error rather than a timeout. Fixing it
would move CORS outside the adaptor and cost the portability that is the point.

Fiber's own middleware still works, registered before `Mount`:

```go
app.Use(cors.New(cors.Config{AllowOrigins: []string{"https://app.example.com"}}))
luima.Mount(app, cfg)
```

Explicit, because `cors.New()` with no config defaults to `AllowOrigins: []string{"*"}`. That is
not catastrophic on its own — `AllowCredentials` defaults to `false`, so cookies are not sent
cross-origin — but it becomes a data leak the moment your auth is a header token your own SPA
holds, or you flip `AllowCredentials` to make cookie auth work. `rs/cors` in `HTTPMiddleware` is
the third option, and it needs no Fiber import either.

(`HEAD`, while you are here: `Options.Do` answers it with 405. Nothing depends on it.)

```go
srv.SetQueryCache(lru.New[*ast.QueryDocument](n))
```

`handler.New` starts with `graphql.NoCache`, so without this **every request re-parses and
re-validates the whole query document**. This is why `Config.QueryCache`'s zero value means 1000
rather than off — a zero-valued `Config{}` has to be the good configuration, not the pathological
one. Turning it off needs a negative number, and there is no good reason to.

```go
srv.Use(extension.Introspection{})
```

Also not added by `handler.New`. Without it the playground's documentation pane is blind, and your
users' first impression of the API is a GraphiQL that appears broken.

---

See also: [The gqlgen contract](gqlgen-contract.md) · [Deployment](deployment.md) · [Gotchas](gotchas.md)
