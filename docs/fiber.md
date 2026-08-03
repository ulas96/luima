# Fiber v3 — the implementation detail that matters most

[← back to the README](../README.md)

The gqlgen server is an `http.Handler`. Fiber runs on fasthttp. The bridge is one line:

```go
r.All(endpoint, adaptor.HTTPHandler(srv))
```

`adaptor.HTTPHandler` wraps `fasthttpadaptor`, converting fasthttp's `RequestCtx` into an
`*http.Request` **per request**.

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

`adaptor.HTTPHandler` **always returns `nil`**. No resolver error can ever become a Fiber error, so
`fiber.Config.ErrorHandler` will never see one.

`PresentError` is therefore the *only* error contract. luima does not offer a second one through
`Config.Fiber.ErrorHandler` and does not document one — a consumer who sets it and assumes their
resolver errors flow through it has built a redaction layer that never runs.

---

## 3. `Get("/")` is an **exact** match

`http.Handle("/", …)` in `net/http` is a *prefix* match — it serves the playground for every
unrouted path. Fiber's `Get("/")` does not. An unknown path is a plain Fiber 404.

This is the better behaviour, but it will surprise anyone porting from `net/http`, and it is the
second thing `TestMountRoutes` pins. Route a catch-all explicitly if you want one.

---

## 4. `BodyLimit` and buffering — luima's documented ceiling

- **Fiber's default `BodyLimit` is 4 MB**, where `net/http` had none. Irrelevant to queries. It
  becomes relevant the day someone adds the multipart transport for file uploads — and it will
  present as a mysterious rejection, not a clear error. Raise it via `Config.Fiber.BodyLimit`.
- **The adaptor buffers the entire response** before handing it to fasthttp. So a streaming
  transport — SSE, or websocket subscriptions — **cannot work through `adaptor`**. It needs a
  Fiber-native handler.

That second point is the honest reason subscriptions are out of scope: **architecture, not
effort.** Adding `transport.Websocket` to the server would compile, mount, and never stream.

---

## What `Mount` actually does, and why each line is there

```go
srv := handler.New(cfg.Schema)
```

Not `handler.NewDefaultServer` — deprecated in v0.17.94, and it gives no way to set an error
presenter, which makes the [error contract](../README.md#errors) impossible.

```go
srv.AddTransport(transport.Options{})  // CORS preflight, for browser clients
srv.AddTransport(transport.GET{})
srv.AddTransport(transport.POST{})
```

`handler.New` adds none. Without `Options{}`, see consequence 1 above.

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
