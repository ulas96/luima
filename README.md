# luima

[![Go Reference](https://pkg.go.dev/badge/github.com/ulas96/luima.svg)](https://pkg.go.dev/github.com/ulas96/luima)
[![Release](https://img.shields.io/github/v/release/ulas96/luima?logo=go&label=release)](https://github.com/ulas96/luima/releases/latest)
[![CI](https://github.com/ulas96/luima/actions/workflows/ci.yml/badge.svg)](https://github.com/ulas96/luima/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/ulas96/luima)](https://goreportcard.com/report/github.com/ulas96/luima)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Luima connects a [gqlgen](https://github.com/99designs/gqlgen) GraphQL server to
[Fiber v3](https://github.com/gofiber/fiber) and provides resolver helpers for
[go-pg](https://github.com/go-pg/pg). Use it when gqlgen, Fiber, and go-pg are already part of
your application and you want one implementation of their integration and error-handling rules.

## The problems Luima solves

| Problem | Luima behavior |
|---|---|
| gqlgen is a `net/http` handler, while Fiber uses fasthttp | Mounts the handler on Fiber so request deadlines and context values reach resolvers |
| Browser clients use gqlgen's GET, POST, and OPTIONS transports | Registers all three methods and lets gqlgen dispatch them; `CORS` supplies the headers the preflight alone does not |
| gqlgen's default presenter returns raw resolver errors | Sends explicitly public errors to the client and logs and redacts other resolver errors |
| CRUD resolvers repeat the same database and GraphQL edge cases | Handles missing rows, duplicate keys, non-nil lists, scoped queries, and `RETURNING *` consistently |
| `pg.Connect` does not verify a connection immediately | Parses the PostgreSQL URL, opens the pool, and runs a startup query before returning |

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

Luima requires Go 1.27. It currently targets Fiber v3, gqlgen v0.17, and go-pg v10.

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

type User struct {
    tableName  struct{} `pg:"app_users"`
    PersonalID string   `pg:"personal_id,pk"`
    Name       string   `pg:"name"`
    Company    string   `pg:"company"`
    Projects   []string `pg:"projects,array"`
}
```

`Get`, `Update`, and `Delete` call go-pg's `WherePK`, so the `pk` tag is required. Database
columns must use exported Go fields. The `array` option makes go-pg encode `Projects` as a
PostgreSQL array instead of JSON.

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

import "github.com/go-pg/pg/v10"

type Resolver struct {
    DB *pg.DB
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
    return luima.List[model.User](ctx, r.DB, func(q *orm.Query) *orm.Query {
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
every model column unless a query modifier selects specific columns.

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
    HealthCheck: db.Ping,

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
        func(q *orm.Query) *orm.Query {
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
| `HealthCheck` | `nil` | `func(context.Context) error`; nil answers 200 while the process is up, an error is 503. Gets a 2s deadline of its own. `db.Ping` fits |

Zero means “use the default” for `RequestTimeout`, `ReadTimeout`, `WriteTimeout`, `QueryCache`,
`ComplexityLimit` and `MaxDepth`. Use a negative value to disable one of them. `HTTPMiddleware`
receives the `net/http` request and the same context seen by resolvers. See
[Fiber integration](docs/fiber.md) for adaptor behavior, middleware ordering, and CORS details.

With the transports configured by Luima, resolver errors are returned in a GraphQL response with
HTTP 200, while parse and validation errors use HTTP 422 — or HTTP 400 when the client sends
`Accept: application/graphql-response+json`. In every case, clients must inspect the response body.

### CRUD helpers

```go
func Get[T any](ctx context.Context, db orm.DB, key *T, opts ...func(*orm.Query) *orm.Query) (*T, error)
func List[T any](ctx context.Context, db orm.DB, opts ...func(*orm.Query) *orm.Query) ([]*T, error)
func Create[T any](ctx context.Context, db orm.DB, model *T, label string, opts ...func(*orm.Query) *orm.Query) (*T, error)
func Update[T any](ctx context.Context, db orm.DB, model *T, label string, opts ...func(*orm.Query) *orm.Query) (*T, error)
func Delete[T any](ctx context.Context, db orm.DB, key *T, opts ...func(*orm.Query) *orm.Query) (bool, error)
```

| Helper | Result |
|---|---|
| `Get` | Selects by primary key; returns `(nil, nil)` when no row matches |
| `List` | On success, returns a non-nil slice; modifiers supply filtering, ordering, and limits |
| `Create` | Inserts with `RETURNING *`; SQLSTATE `23505` becomes `label + " already exists"`; an insert the database suppressed returns `(nil, nil)` |
| `Update` | Updates by primary key with `RETURNING *`; no match becomes `label + " not found"` |
| `Delete` | Deletes by primary key; returns `false` when no row matches |

All helpers accept `orm.DB`, which is implemented by `*pg.DB`, `*pg.Conn`, and `*pg.Tx`. Pass a
transaction to the same helpers inside `RunInTransaction`.

`Update` is a full replacement by default. Restrict it to selected columns when implementing a
partial update:

```go
updated, err := luima.Update(ctx, db, user, "user "+user.PersonalID,
    func(q *orm.Query) *orm.Query {
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
- A direct `*gqlerror.Error`: preserves gqlgen's parse or validation message.
- Any other error: logs the error and sends `internal server error`.

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
| `GRAPHQL_PARSE_FAILED`, `GRAPHQL_VALIDATION_FAILED`, `COMPLEXITY_LIMIT_EXCEEDED` | gqlgen, passed through unchanged |

Transport-level failures — a malformed body, an unsupported content type — are written by gqlgen's
transport before an executor exists. They never reach `PresentError`, carry no code, and are not
redacted.

`SQLState` returns a PostgreSQL SQLSTATE from a wrapped go-pg error or an empty string when the
chain contains no `pg.Error`. Common integrity codes are `23505` for a unique violation, `23503`
for a foreign-key violation, `23502` for a not-null violation, and `23514` for a check violation.

### Database connection

```go
func Connect(url string) (*pg.DB, error)
func ConnectWith(url string, tune func(*pg.Options)) (*pg.DB, error)
func StatementTimeout(d time.Duration) func(*pg.Options)
```

`Connect` accepts `postgres://` and `postgresql://` URLs, creates a go-pg pool, and executes
`select 1` before returning. It closes the pool if the startup query fails. Use
`sslmode=verify-full` when the server certificate must be verified.

`ConnectWith` is the same function with a hook onto the parsed `*pg.Options`, for the tuning
`pg.ParseURL` cannot express — it accepts only `sslmode`, `application_name` and `connect_timeout`.
`Connect(url)` is `ConnectWith(url, nil)`. `StatementTimeout` is the case worth pre-writing: it
bounds every query in the server, which a context deadline cannot, because go-pg's cancellation is
best-effort. A query that exceeds it returns SQLSTATE `57014`. See
[Deployment](docs/deployment.md) for supported URL parameters, TLS behavior, environment files,
serving over TLS and behind a proxy, and production database settings.

## Documentation

| Document | Contents |
|---|---|
| [The gqlgen contract](docs/gqlgen-contract.md) | Generated files, resolver layout, autobinding, and schema checks |
| [Fiber integration](docs/fiber.md) | Methods, context propagation, middleware behavior, buffering, and CORS |
| [Deployment](docs/deployment.md) | PostgreSQL URLs, TLS verification, `.env` in Docker, serving over TLS and behind a proxy, and security posture |
| [Gotchas](docs/gotchas.md) | Known failure modes and their fixes |
| [Quickstart module](examples/quickstart) | Complete runnable server |

## Development

```sh
make test        # run tests; the database-backed CRUD test skips without DATABASE_URL
make test-db     # load .env and run the database-backed test
make lint
make example     # build the quickstart and reject unimplemented resolver stubs
```

`go test ./...` reports success when `TestCRUD` is skipped. To exercise the real driver, set
`DATABASE_URL`, run the tests with verbose output, and confirm that `TestCRUD` passes rather than
skips. See [Contributing](CONTRIBUTING.md) for the complete development workflow.

## License

MIT. See [LICENSE](LICENSE).
