# luima

[![Go Reference](https://pkg.go.dev/badge/github.com/ulas96/luima.svg)](https://pkg.go.dev/github.com/ulas96/luima)
[![Release](https://img.shields.io/github/v/release/ulas96/luima?logo=go&label=release)](https://github.com/ulas96/luima/releases/latest)
[![CI](https://github.com/ulas96/luima/actions/workflows/ci.yml/badge.svg)](https://github.com/ulas96/luima/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/ulas96/luima)](https://goreportcard.com/report/github.com/ulas96/luima)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Luima connects a [gqlgen](https://github.com/99designs/gqlgen) GraphQL server to
[Fiber v3](https://github.com/gofiber/fiber) and provides resolver helpers for
[bun](https://github.com/uptrace/bun). Use it when gqlgen, Fiber, and bun are already part of
your application and you want one implementation of their integration and error-handling rules.

## The problems Luima solves

| Problem | Luima behavior |
|---|---|
| gqlgen is a `net/http` handler, while Fiber uses fasthttp | Mounts the handler on Fiber so request deadlines and context values reach resolvers |
| Browser clients use gqlgen's GET, POST, and OPTIONS transports | Registers all three methods and lets gqlgen dispatch them; `CORS` supplies the headers the preflight alone does not |
| gqlgen's default presenter returns raw resolver errors | Sends explicitly public errors to the client and logs and redacts other resolver errors |
| CRUD resolvers repeat the same database and GraphQL edge cases | Handles missing rows, duplicate keys, non-nil lists, scoped queries, and `RETURNING *` consistently |
| `sql.OpenDB` dials nothing, and `database/sql` defaults to unlimited connections and two idle | Parses the connection string, sizes the pool for a server, and proves it with a startup query before returning |

**Fiber is a compatibility target, not a performance feature.** gqlgen is an `http.Handler`, so
luima converts each `*fasthttp.RequestCtx` into an `*http.Request` before gqlgen sees it — gqlgen
does exactly the work it always did, plus a conversion. Reach for luima because Fiber is already in
your stack, not because you expect it to beat a `net/http` server. That seam also bounds
cancellation: `RequestTimeout` cancels the resolver context, but a client hanging up does not,
because fasthttp does not cancel on disconnect. See [Fiber integration](docs/fiber.md).

Luima does not generate your GraphQL schema or resolvers. You own `gqlgen.yml`,
`schema.graphqls`, `graph/resolver.go`, and every `gqlgen generate` run. Luima also does not
provide authentication, authorization, schema-level pagination or filtering APIs, dataloaders,
file uploads, migrations, or scaffolding. `CORS` and `RateLimit` exist, but they are the small
stdlib-only kind: per-process rate limiting is not a substitute for a limiter in the layer that
does auth. Subscriptions are unsupported: fasthttp does
not cancel the request context when a client disconnects, so an abandoned stream has no upper bound
once `RequestTimeout` is disabled — which a subscription requires. See
[docs/fiber.md](docs/fiber.md#4-bodylimit-and-why-subscriptions-are-really-out).

> **Security:** Luima does not identify callers or restrict which rows they can access. Add
> authentication middleware before mounting GraphQL and add ownership predicates to database
> queries. The PostgreSQL role in `DATABASE_URL` determines database privileges; a privileged
> role may bypass Row Level Security. See [Security](SECURITY.md) and
> [Deployment](docs/deployment.md#security-posture).

## Install

Luima requires Go 1.27. It currently targets Fiber v3, gqlgen v0.17, and bun v1.2 with pgdriver.

```sh
go get github.com/ulas96/luima
go get -tool github.com/99designs/gqlgen
```

The second command records the gqlgen CLI in your module so `go tool gqlgen generate` uses the
version selected by your `go.mod`.

## Quickstart

This section builds a minimal development server. The
[`examples/quickstart`](examples/quickstart) module contains the same schema and resolvers plus
a liveness path, rate limiting, CORS, middleware ordering and a graceful shutdown — and it imports
no Fiber package at all.

### 1. Create the table

```sql
create table if not exists app_users (
  personal_id text primary key,
  name        text not null,
  company     text not null,
  projects    text[] not null default '{}'
);
```

### 2. Define the GraphQL schema

Create `graph/schema.graphqls`:

```graphql
type User {
  personalId: String!
  name: String!
  company: String!
  projects: [String!]!
}

input UserInput {
  name: String!
  company: String!
  projects: [String!]!
}

type Query {
  users: [User!]!
  user(personalId: String!): User
}

type Mutation {
  createUser(personalId: String!, input: UserInput!): User!
  updateUser(personalId: String!, input: UserInput!): User!
  deleteUser(personalId: String!): Boolean!
}
```

`user` is nullable because `luima.Get` returns `(nil, nil)` when no row matches. `users` is
non-null because `luima.List` returns an empty, non-nil slice when the table is empty.

### 3. Define the database model

Create `graph/model/user.go`:

```go
package model

import "github.com/uptrace/bun"

type User struct {
    bun.BaseModel `bun:"table:app_users"`

    PersonalID string   `bun:"personal_id,pk"`
    Name       string   `bun:"name"`
    Company    string   `bun:"company"`
    Projects   []string `bun:"projects,array"`
}
```

The embedded `bun.BaseModel` is what names the table, and it is the line that fails quietly: bun
skips every unexported field that is not embedded, so a model that names its table the way other
Postgres mappers do — an unexported `tableName` field — still compiles, and every statement goes
to `users`, the pluralized type name. `Get`, `Update`, and `Delete` call `WherePK`, so the `pk` tag
is required. Database columns must use exported Go fields. The `array` option makes bun encode
`Projects` as a PostgreSQL array instead of JSON, which a `text[]` column rejects with SQLSTATE
`22P02`. A nil `[]string` is written as literal `NULL`, on insert and on update alike, never as
the column default — so against the table above it is a `23502`, and `[]string{}` is what writes
`'{}'`. That reaches resolvers, not only seed scripts and tests: gqlgen unmarshals a nullable
`[String!]` argument to nil when the client sends null or omits it, and only `[String!]!` is
always non-nil.

### 4. Configure and run gqlgen

Create `gqlgen.yml`, replacing `your/module` with the module path from your `go.mod`:

```yaml
schema:
  - graph/*.graphqls

exec:
  filename: graph/generated/generated.go
  package: generated

model:
  filename: graph/model/models_gen.go
  package: model

resolver:
  layout: follow-schema
  dir: graph
  package: graph
  filename_template: "{name}.resolvers.go"

autobind:
  - "your/module/graph/model"
```

Create the dependency root in `graph/resolver.go`:

```go
package graph

import "github.com/uptrace/bun"

type Resolver struct {
    DB *bun.DB
}
```

Generate the gqlgen code:

```sh
go tool gqlgen generate
grep -rn 'not implemented' graph/*.resolvers.go
```

The grep command must return no matches after the resolver bodies are implemented. gqlgen emits
compilable stubs that panic when called, so `go build` alone does not detect an unfinished
resolver. The [gqlgen contract](docs/gqlgen-contract.md) explains the generated and hand-written
file boundaries.

### 5. Implement the resolvers

Fill the generated resolver methods:

```go
func (r *queryResolver) Users(ctx context.Context) ([]*model.User, error) {
    return luima.List[model.User](ctx, r.DB, func(q *bun.SelectQuery) *bun.SelectQuery {
        return q.Order("personal_id").Limit(100)
    })
}

func (r *queryResolver) User(ctx context.Context, personalID string) (*model.User, error) {
    return luima.Get(ctx, r.DB, &model.User{PersonalID: personalID})
}

func (r *mutationResolver) CreateUser(ctx context.Context, personalID string, input model.UserInput) (*model.User, error) {
    return luima.Create(ctx, r.DB, newUser(personalID, input), "user "+personalID)
}

func (r *mutationResolver) UpdateUser(ctx context.Context, personalID string, input model.UserInput) (*model.User, error) {
    return luima.Update(ctx, r.DB, newUser(personalID, input), "user "+personalID)
}

func (r *mutationResolver) DeleteUser(ctx context.Context, personalID string) (bool, error) {
    return luima.Delete(ctx, r.DB, &model.User{PersonalID: personalID})
}
```

`List` does not impose an order or row limit; each list resolver must set both. `Update` writes
every model column unless a query modifier selects specific columns, and bun writes a zero-valued
field as its zero value — `''`, `0`, `FALSE` — so a column your input mapper forgets is blanked
rather than left alone.

Put input-to-model helpers such as `newUser` in `graph/resolver.go`, not a generated
`*.resolvers.go` file:

```go
func newUser(personalID string, input model.UserInput) *model.User {
    return &model.User{
        PersonalID: personalID,
        Name:       input.Name,
        Company:    input.Company,
        Projects:   input.Projects,
    }
}
```

### 6. Start the server

```go
func main() {
    db, err := luima.Connect(os.Getenv("DATABASE_URL"))
    if err != nil {
        log.Fatal(err)
    }
    defer db.Close()

    app := luima.New(luima.Config{
        Schema: generated.NewExecutableSchema(generated.Config{
            Resolvers: &graph.Resolver{DB: db},
        }),
    })

    log.Fatal(app.Listen(":8080"))
}
```

Export the database URL and run the application:

```sh
set -a
. ./.env
set +a
go run .
```

The default playground is at <http://localhost:8080/> and the GraphQL endpoint is at
<http://localhost:8080/graphql>.

## Running a server without importing Fiber

`luima.Run` builds the server, listens, and blocks until the context is cancelled — then drains
in-flight requests and returns. Timeouts, CORS, rate limiting and the liveness path all have
luima spellings, so a complete application imports `github.com/ulas96/luima` and nothing else from
the web stack. `examples/quickstart/main.go` is exactly this shape.

```go
ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
defer stop()

err := luima.Run(ctx, ":8080", luima.Config{
    Schema: generated.NewExecutableSchema(generated.Config{
        Resolvers: &graph.Resolver{DB: db},
    }),
    DisablePlayground:    true,
    DisableIntrospection: true,

    Health:      "/healthz",
    HealthCheck: db.PingContext,

    HTTPMiddleware: []func(http.Handler) http.Handler{
        luima.RateLimit(100, time.Minute, nil),
        luima.CORS(luima.CORSConfig{Origins: []string{"https://app.example.com"}}),
    },
})
if err != nil {
    log.Print(err)
}
```

`Run` returns its error rather than exiting, which is what lets a `defer db.Close()` above it
actually run — `log.Fatal` calls `os.Exit` and skips every deferred function. Read, write and
request deadlines default to 10s, 30s and 15s, so no timeout appears here.

`HTTPMiddleware` is `[]func(http.Handler) http.Handler`, outermost first. It is the only layer that
sees the same request context the resolvers see, so authentication, tracing and tenancy belong
there rather than on the app.

## Adding Luima to an existing Fiber application

Use `Mount` when the application already has Fiber middleware or routes of its own. `luima.New`
mounts GraphQL before it returns, so Fiber middleware added afterward does not run for the GraphQL
route — `Mount` onto an app you configured yourself, or use `Run` and put the middleware in
`HTTPMiddleware`.

```go
app := fiber.New(fiber.Config{
    ReadTimeout:  10 * time.Second,
    WriteTimeout: 30 * time.Second,
    BodyLimit:    1 << 20,
})

app.Use(someFiberMiddleware)

luima.Mount(app, luima.Config{
    Schema: generated.NewExecutableSchema(generated.Config{
        Resolvers: &graph.Resolver{DB: db},
    }),
    DisablePlayground:    true,
    DisableIntrospection: true,
})
```

`Mount` sets nothing on a router it did not create, so `Config.Fiber`, `ReadTimeout` and
`WriteTimeout` are ignored here and the app's own timeouts are yours to set — Fiber passes them to
fasthttp verbatim, and fasthttp reads zero as *no deadline*. The default 15-second `RequestTimeout`
does still apply. The working quickstart uses the `LUIMA_DEV` environment variable to enable the
playground and introspection during local development.

Authentication alone does not restrict rows. Apply the caller's identity as an additional query
predicate:

```go
func (r *mutationResolver) DeleteUser(ctx context.Context, personalID string) (bool, error) {
    ownerID := callerID(ctx)
    return luima.Delete(ctx, r.DB, &model.User{PersonalID: personalID},
        func(q *bun.DeleteQuery) *bun.DeleteQuery {
            return q.Where("owner_id = ?", ownerID)
        })
}
```

`Get`, `Update`, and `Delete` apply modifiers after `WherePK`. A row that does not match the
ownership predicate is reported as absent. Pass untrusted data as `?` parameters; do not use it to
construct SQL fragments or identifiers. This is the mechanism that stops the helpers being an IDOR
by construction — see [SECURITY.md](SECURITY.md).

## API

The root package re-exports the public API from four subpackages. Import a subpackage directly
when a package should not depend on the entire runtime.

| Package | Exports |
|---|---|
| [`luima`](https://pkg.go.dev/github.com/ulas96/luima) | All exports listed below |
| [`luima/server`](https://pkg.go.dev/github.com/ulas96/luima/server) | `Config`, `New`, `Run`, `Mount`, `CORS`, `CORSConfig`, `RateLimit` |
| [`luima/crud`](https://pkg.go.dev/github.com/ulas96/luima/crud) | `Get`, `List`, `Create`, `Update`, `Delete` |
| [`luima/luimaerr`](https://pkg.go.dev/github.com/ulas96/luima/luimaerr) | `CustomError`, `PresentError`, `SQLState` |
| [`luima/db`](https://pkg.go.dev/github.com/ulas96/luima/db) | `Connect`, `ConnectWith`, `StatementTimeout` |

`luima.Config` and `server.Config` are the same type because the root package uses type aliases.
The generic CRUD functions are root-package wrappers with the same signatures and behavior as
their `crud` equivalents.

### Runtime

```go
func Run(ctx context.Context, addr string, cfg Config) error
func New(cfg Config) *fiber.App
func Mount(r fiber.Router, cfg Config)

func CORS(c CORSConfig) func(http.Handler) http.Handler
func RateLimit(n int, per time.Duration, key func(*http.Request) string) func(http.Handler) http.Handler
```

`Run` is `New` plus listen plus a graceful drain, and it names no Fiber type — prefer it unless the
application needs the app itself. `New` creates a Fiber application using `Config.Fiber`, then
mounts GraphQL. `Mount` adds GraphQL to an existing app or route group and ignores `Config.Fiber`,
`ReadTimeout` and `WriteTimeout`.

`Mount` panics when `Config.Schema` is nil. It is the one programmer error luima refuses to defer:
without the check the process boots and every request panics inside gqlgen instead.

`CORS` and `RateLimit` are plain `net/http` middleware for `HTTPMiddleware`. Both are stdlib-only,
so neither adds a dependency, and both stay portable to chi, echo or plain `net/http`.

| `CORSConfig` field | Default | Behavior |
|---|---|---|
| `Origins` | `nil` | Exact origins; a single `"*"` allows any. There is no credentials option |
| `Headers` | `nil` | Added to `Content-Type` and `Authorization` |
| `MaxAge` | `10m` | Preflight cache lifetime; a negative value sends `0` |

| `Config` field | Default | Behavior |
|---|---|---|
| `Schema` | none | Required executable schema produced by gqlgen |
| `Endpoint` | `/graphql` | GraphQL endpoint |
| `Playground` | `/` | Exact playground path; unrelated paths return 404 |
| `DisablePlayground` | `false` | Set to `true` outside development |
| `PlaygroundTitle` | `graphql` | Browser page title |
| `DisableIntrospection` | `false` | Set to `true` outside development; this is not authorization |
| `RequestTimeout` | `15s` | Resolver deadline; a negative value disables it |
| `ReadTimeout` | `10s` | How long a client may take to send a request; a negative value disables it. Applied by `New` and `Run` only |
| `WriteTimeout` | `30s` | How long the server may take to write a response; a negative value disables it. Does not bound a resolver — fasthttp sets this deadline after the handler returns |
| `QueryCache` | `1000` | Parsed-query cache entries; a negative value disables the cache |
| `ComplexityLimit` | `1000` | Operation complexity limit; a negative value disables it and it does not limit returned rows |
| `MaxDepth` | `15` | Operation nesting depth limit; a negative value disables it. Complexity does not bound depth |
| `ErrorPresenter` | `luima.PresentError` | Controls which error message reaches the client |
| `HTTPMiddleware` | `nil` | `[]func(http.Handler) http.Handler`; the first item is outermost |
| `Configure` | `nil` | `func(*handler.Server)`; runs after Luima configures gqlgen and before mounting |
| `Fiber` | `fiber.Config{}` | Passed to `fiber.New` by `New`; a field set here wins over `ReadTimeout`/`WriteTimeout`. Ignored by `Mount` |
| `Health` | `""` | Liveness path, e.g. `/healthz`. Empty disables it; `HTTPMiddleware` does not wrap it |
| `HealthCheck` | `nil` | `func(context.Context) error`; nil answers 200 while the process is up, an error is 503. Gets a 2s deadline of its own. `db.PingContext` fits — `db.Ping` takes no context and does not compile here |

Zero means “use the default” for `RequestTimeout`, `ReadTimeout`, `WriteTimeout`, `QueryCache`,
`ComplexityLimit` and `MaxDepth`. Use a negative value to disable one of them. `HTTPMiddleware`
receives the `net/http` request and the same context seen by resolvers. See
[Fiber integration](docs/fiber.md) for adaptor behavior, middleware ordering, and CORS details.

With the transports configured by Luima, resolver errors are returned in a GraphQL response with
HTTP 200, while parse and validation errors use HTTP 422 — or HTTP 400 when the client sends
`Accept: application/graphql-response+json`. In every case, clients must inspect the response body.

### CRUD helpers

```go
func Get[T any](ctx context.Context, db bun.IDB, key *T, opts ...func(*bun.SelectQuery) *bun.SelectQuery) (*T, error)
func List[T any](ctx context.Context, db bun.IDB, opts ...func(*bun.SelectQuery) *bun.SelectQuery) ([]*T, error)
func Create[T any](ctx context.Context, db bun.IDB, m *T, label string, opts ...func(*bun.InsertQuery) *bun.InsertQuery) (*T, error)
func Update[T any](ctx context.Context, db bun.IDB, m *T, label string, opts ...func(*bun.UpdateQuery) *bun.UpdateQuery) (*T, error)
func Delete[T any](ctx context.Context, db bun.IDB, key *T, opts ...func(*bun.DeleteQuery) *bun.DeleteQuery) (bool, error)
```

| Helper | Result |
|---|---|
| `Get` | Selects by primary key; returns `(nil, nil)` when no row matches |
| `List` | On success, returns a non-nil slice; modifiers supply filtering, ordering, and limits |
| `Create` | Inserts with `RETURNING *`; SQLSTATE `23505` becomes `label + " already exists"`; an insert the database suppressed returns `(nil, nil)` |
| `Update` | Updates by primary key with `RETURNING *`; no match becomes `label + " not found"` |
| `Delete` | Deletes by primary key; returns `false` when no row matches |

`Create` and `Update` report absence one way: a row count of zero, with no error. bun turns an
empty result into `sql.ErrNoRows` only for a `Scan`, or for an `Exec` handed a destination, and
both helpers `Exec` without one — so a branch on `sql.ErrNoRows` there never runs. `Get` is the one
that sees it, because a single-row select does scan.

All helpers accept `bun.IDB`, which is implemented by `*bun.DB`, `bun.Conn`, and `bun.Tx`. Pass a
transaction to the same helpers inside `db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx)
error)`.

The modifier type is bun's own query for that statement, so there is one per helper. A predicate
needed by more than one — the ownership `Where` above — is written once as
`func(bun.QueryBuilder) bun.QueryBuilder` and applied with `q.ApplyQueryBuilder`, which the select,
update, and delete queries all have.

`Update` is a full replacement by default. Restrict it to selected columns when implementing a
partial update:

```go
updated, err := luima.Update(ctx, db, user, "user "+user.PersonalID,
    func(q *bun.UpdateQuery) *bun.UpdateQuery {
        return q.Column("name", "email")
    })
```

### Error handling

```go
type CustomError struct {
    UserMessage   string
    InternalError error
    Code          string
}

func PresentError(ctx context.Context, err error) *gqlerror.Error
func SQLState(err error) string
```

`PresentError` applies these rules:

- `*CustomError`, including when wrapped: sends `UserMessage` to the client.
- A direct `*gqlerror.Error` that wraps no other error, reported outside field resolution:
  preserves gqlgen's parse, validation or limit message.
- Any other error: logs the error and sends `internal server error`.

The last rule covers everything else reported while a field resolves, whatever its type: a
resolver's or a directive's error, anything sent with `graphql.AddError` or `graphql.AddErrorf`, a
panic, and a `*gqlerror.Error` built by your own code — `gqlerror.Errorf(...)`, or a list decoded
from another GraphQL server. gqlgen wraps a resolver's plain error in a `*gqlerror.Error` before
the presenter sees it, so the type alone proves nothing. The rule also covers gqlgen's own errors
from that stage: a null where the schema forbids one, and a client's bad value for a custom scalar
such as gqlgen's `Time`. (With `DisableIntrospection` set, a `__schema` or `__type` query never
gets that far: `Mount` refuses the operation first, with `INTROSPECTION_DISABLED` and no log line.) A `*CustomError` is heard from a resolver, a directive or an `UnmarshalGQL` alike, so a
scalar that should explain its format to the client has to be your own.

Do not share one `*gqlerror.Error` value between requests. gqlgen writes the first request's path
and locations into it, and two concurrent requests race on it.

Treat `CustomError.UserMessage` as public data. Do not populate it with `err.Error()` or another
database-derived string. `InternalError` remains available through `errors.Is` and `errors.As`.

`Code` becomes `extensions.code` on the wire, and is what clients should branch on — the message is
built from caller-supplied text and is not a stable contract. An empty `Code` emits no extensions
object.

| Code | Sent by |
|---|---|
| `CONFLICT` | `Create`, on SQLSTATE `23505` |
| `NOT_FOUND` | `Update`, when no row matched |
| `INTERNAL_SERVER_ERROR` | Every redacted error |
| `DEPTH_LIMIT_EXCEEDED` | `MaxDepth` |
| `GRAPHQL_PARSE_FAILED`, `GRAPHQL_VALIDATION_FAILED`, `COMPLEXITY_LIMIT_EXCEEDED` | gqlgen, passed through unchanged — except a variable default that does not parse for a custom scalar, which is redacted and still answered HTTP 422 |

Not every response goes through `PresentError`. A request no transport accepts, such as one with an
unsupported content type, is answered `transport not supported` without it, and so are the GET
transport's own refusals; neither carries a code. A malformed JSON body does reach it, and passes
through with no code, quoting the caller's own body back.

`SQLState` returns a PostgreSQL SQLSTATE from a wrapped `pgdriver.Error` or an empty string when
the chain contains none. Common integrity codes are `23505` for a unique violation, `23503` for a
foreign-key violation, `23502` for a not-null violation, and `23514` for a check violation; `57014`
is a statement cancelled by `statement_timeout`.

`pgdriver.Error` is a struct value, not a pointer and not an interface, so read it as
`errors.AsType[pgdriver.Error](err)`. The pointer spelling someone arriving from pgx writes,
`errors.AsType[*pgdriver.Error]`, compiles and never matches, leaving every SQLSTATE branch behind
it dead.

### Database connection

```go
func Connect(url string) (*bun.DB, error)
func ConnectWith(url string, tune func(*pgdriver.Config)) (*bun.DB, error)
func StatementTimeout(d time.Duration) func(*pgdriver.Config)
```

`Connect` accepts `postgres://` and `postgresql://` URLs, opens a `database/sql` pool over
pgdriver, and executes `select 1` before returning. It closes the pool if that startup query fails.
The query is the check: `sql.OpenDB` dials nothing, so without it a wrong credential or a
misspelled URL parameter surfaces one failed request at a time in production instead of once, at
boot. `Connect`'s errors carry neither the URL nor its password, so logging one leaks no
credential.

pgdriver reads `sslmode`, `sslrootcert`, `sslcert`, `sslkey`, `application_name`, `connect_timeout`,
`dial_timeout`, `timeout`, `read_timeout`, `write_timeout`, and `host` from the URL — `sslcert` and
`sslkey` only alongside `sslmode` or `sslrootcert`. Every other parameter is sent as
`SET name TO value` on each new connection, so `?statement_timeout=5s` now works — and a
misspelled name is not a parse error but a failed startup query, usually SQLSTATE `42704`. With
`sslmode` absent the connection is still TLS, with nothing verified. Use `sslmode=verify-full` when
the server certificate must be verified; `verify-ca` checks the host name too, as it did in 0.5.0,
although pgdriver on its own would check only the chain. `?password=` and `?sslpassword=` are
refused, because pgdriver would send them to the server as a `SET`, and an integer timeout `<= 0`
keeps the default. A URL with no database name connects to `$PGDATABASE`, then `postgres`, without
complaint, and a URL with no password uses `$PGPASSWORD`.

`Connect` sizes the pool, because `database/sql` does not size it for a server: ten connections per
CPU, open and idle, with a five-minute idle time, which is the pool luima ran before. Resize it on
the returned handle — `*bun.DB` embeds `*sql.DB` through its state struct, so `SetMaxOpenConns`,
`SetConnMaxIdleTime`, and `Stats` are promoted onto it. Query hooks go on the handle too:
`db.WithQueryHook(h)` returns a copy over the same pool, and `AddQueryHook` is deprecated upstream.

`ConnectWith` is the same function with a hook onto the parsed `*pgdriver.Config`, called after the
URL is applied and before anything dials, for the tuning a connection string cannot express: a
`*tls.Config` of your own, a dialer, a password or a timeout computed at startup. `Connect(url)` is
`ConnectWith(url, nil)`. `StatementTimeout` is the case worth pre-writing, and it matters more than
it used to: pgdriver never asks Postgres to cancel a statement, so a context deadline — including
the one `RequestTimeout` sets — stops the client waiting while the backend runs the query to
completion. `statement_timeout` is the only bound the server enforces. A query that exceeds it
returns SQLSTATE `57014`.

Keep that bound under both client-side deadlines, or its SQLSTATE never arrives: `RequestTimeout`,
and pgdriver's 10-second socket read timeout, which `Connect` keeps because it is the only bound on
the reads pgdriver performs without a context — the rows after a query's row description, the
drain, and `COMMIT`. A statement whose reply takes longer than that fails client-side with an i/o
timeout whatever `statement_timeout` allows; raise it with `?read_timeout=60s` or in a
`ConnectWith` tune, and raise `RequestTimeout` with it. See [Deployment](docs/deployment.md) for
supported URL parameters, TLS behavior, environment files, serving over TLS and behind a proxy, and
production database settings.

## Documentation

| Document | Contents |
|---|---|
| [The gqlgen contract](docs/gqlgen-contract.md) | Generated files, resolver layout, autobinding, and schema checks |
| [Fiber integration](docs/fiber.md) | Methods, context propagation, middleware behavior, buffering, and CORS |
| [Deployment](docs/deployment.md) | PostgreSQL URLs, TLS verification, `.env` in Docker, serving over TLS and behind a proxy, and security posture |
| [luimagen](docs/luimagen.md) | The `cmd/luimagen` CRUD generator: what it writes, what it refuses, and how to recover from a failed run |
| [Gotchas](docs/gotchas.md) | Known failure modes and their fixes |
| [Quickstart module](examples/quickstart) | Complete runnable server |

## Development

```sh
make test        # run tests; the four database-backed tests skip without DATABASE_URL
make test-db     # load .env and run the database-backed tests
make lint
make example     # build the quickstart and reject unimplemented resolver stubs
```

`go test ./...` reports success when those four skip — `TestCRUD`, `TestStatementTimeout`,
`TestStatementTimeoutNegativeDisables` and `TestConnectPoolBound`. To exercise the real driver, set
`DATABASE_URL`, run the tests with verbose output, and confirm that `TestCRUD` passes rather than
skips. See [Contributing](CONTRIBUTING.md) for the complete development workflow.

## License

MIT. See [LICENSE](LICENSE).
