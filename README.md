# luima

[![Go Reference](https://pkg.go.dev/badge/github.com/ulas96/luima.svg)](https://pkg.go.dev/github.com/ulas96/luima)
[![Release](https://img.shields.io/github/v/release/ulas96/luima?logo=go&label=release)](https://github.com/ulas96/luima/releases/latest)
[![CI](https://github.com/ulas96/luima/actions/workflows/ci.yml/badge.svg)](https://github.com/ulas96/luima/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/ulas96/luima)](https://goreportcard.com/report/github.com/ulas96/luima)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

GraphQL servers in Go on [Fiber v3](https://github.com/gofiber/fiber) +
[gqlgen](https://github.com/99designs/gqlgen) + [go-pg](https://github.com/go-pg/pg).


```go
app := luima.New(luima.Config{
    Schema: generated.NewExecutableSchema(generated.Config{
        Resolvers: &graph.Resolver{DB: db},
    }),
})
log.Fatal(app.Listen(":8080"))
```

```go
func (r *mutationResolver) CreateUser(ctx context.Context, personalID string, input model.UserInput) (*model.User, error) {
    return luima.Create(ctx, r.DB, newUser(personalID, input), "user "+personalID)
}
```

That second snippet replaces fifteen lines of `errors.As` against a driver-specific error
interface — and unlike the fifteen, it also has `RETURNING *`. [See the diff.](#what-this-replaces)

---

## What this is, and what it cannot be

**luima cannot replace gqlgen codegen.** gqlgen generates code *into your module* —
`generated.NewExecutableSchema` is a symbol that only your own `go tool gqlgen generate` run
produces, from your own `schema.graphqls`. No library can produce it, import it, or wrap it ahead
of time. Any design where you hand a library a schema *file* and get a server back is impossible
with gqlgen.

So luima is exactly two things:

1. **A runtime.** `Config` → a `*fiber.App` with the gqlgen handler mounted correctly, a Postgres
   pool, and an error presenter that does not leak your schema.
2. **Resolver-body helpers.** Generic CRUD over go-pg that gets the *error classification* right —
   which is the part everybody gets wrong, not the SQL.

You still own `gqlgen.yml`, `schema.graphqls`, `graph/resolver.go`, and the codegen step.
**[docs/gqlgen-contract.md](docs/gqlgen-contract.md) documents that, and it is not optional
reading** — get it wrong and you ship a server that panics in production and compiles clean.

> ### ⚠️ No auth, no RLS
>
> luima provides **no authentication**. Your server connects as a Postgres role, so if that role is
> privileged — which the default Supabase connection string is — Row Level Security does not apply
> to it, and every query runs with full access to every row.
>
> This is the difference between a fine internal service and a public data leak. Put an API
> gateway, your own Fiber middleware, or a reverse proxy in front of it before it faces the
> internet. See [docs/deployment.md](docs/deployment.md#security-posture).

**Not in scope, deliberately:** auth, pagination, filtering, dataloaders, subscriptions, file
upload, migrations, a scaffolding CLI. [Subscriptions in particular](docs/fiber.md#4-bodylimit-and-buffering--luimas-documented-ceiling)
are blocked by the architecture rather than by effort.

---

## Install

```sh
go get github.com/ulas96/luima
go get -tool github.com/99designs/gqlgen
```

| module | version |
|---|---|
| `github.com/99designs/gqlgen` | `v0.17.94` |
| `github.com/go-pg/pg/v10` | `v10.15.1` |
| `github.com/gofiber/fiber/v3` | `v3.4.0` |
| `github.com/vektah/gqlparser/v2` | `v2.5.36` |
| Go | ≥ 1.25 (needs ≥ 1.24 for the `tool` directive) |

---

## Quickstart

A complete working version of everything below is in
**[`examples/quickstart`](examples/quickstart)** — clone and `go run .`.

### 1. Table

```sql
create table if not exists app_users (
  personal_id text primary key,
  name        text not null,
  company     text not null,
  projects    text[] not null default '{}'
);
```

### 2. `graph/schema.graphqls`

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

`user(...)` is nullable and `users` is `[User!]!`. Both matter: the first is what lets `Get` return
`(nil, nil)` for a missing row, the second is why `List` seeds a non-nil slice.

### 3. `graph/model/user.go` — hand-written, autobound

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

A `pk` tag is mandatory, fields must be exported, and **`,array` is load-bearing** — without it a
`[]string` is encoded as JSON, which a `text[]` column rejects.
[Why, in detail.](docs/gqlgen-contract.md#the-model)

### 4. `gqlgen.yml` and `graph/resolver.go`

Copy both from [docs/gqlgen-contract.md](docs/gqlgen-contract.md#gqlgenyml). The two settings that
will bite you are `layout: follow-schema` (never `single-file`) and `autobind`.

### 5. Generate, then fill the stubs

```sh
go tool gqlgen generate
grep -rn 'not implemented' graph/*.resolvers.go   # must print nothing when you are done
```

**That grep is the schema check — `go build` is not.** gqlgen writes a stub for every schema field
with no resolver, and a stub *compiles*; it panics when called.

```go
func (r *queryResolver) Users(ctx context.Context) ([]*model.User, error) {
    return luima.List[model.User](ctx, r.DB, func(q *orm.Query) *orm.Query {
        return q.Order("personal_id")
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

`newUser` goes in `resolver.go`, **not** in `schema.resolvers.go` — codegen sweeps non-resolver
declarations out of that file into a warning comment block.

### 6. `main.go`

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

```sh
set -a && . ./.env && set +a && go run .
```

Playground on <http://localhost:8080>, API on <http://localhost:8080/graphql>.

### What this replaces

Before — hand-written against gqlgen and go-pg directly:

```go
func (r *mutationResolver) CreateUser(ctx context.Context, personalID string, input model.UserInput) (*model.User, error) {
    u := model.NewUser(personalID, input)
    if _, err := r.DB.ModelContext(ctx, u).Insert(); err != nil {
        var pgErr pg.Error
        if errors.As(err, &pgErr) && pgErr.Field('C') == "23505" {
            return nil, &CustomError{
                UserMessage:   "user " + personalID + " already exists",
                InternalError: err,
            }
        }
        return nil, err
    }
    return u, nil
}
```

After:

```go
func (r *mutationResolver) CreateUser(ctx context.Context, personalID string, input model.UserInput) (*model.User, error) {
    return luima.Create(ctx, r.DB, model.NewUser(personalID, input), "user "+personalID)
}
```

---

## API

Full reference on [pkg.go.dev](https://pkg.go.dev/github.com/ulas96/luima), with runnable examples.

### Packages

The root package re-exports everything, so the common case is one import. The sub-packages are
equivalent — `luima.Config` **is** `server.Config`, an alias rather than a copy — and worth
importing directly when you want a narrower dependency.

| package | contents |
|---|---|
| [`luima`](https://pkg.go.dev/github.com/ulas96/luima) | re-exports all of the below |
| [`luima/server`](https://pkg.go.dev/github.com/ulas96/luima/server) | `Config`, `New`, `Mount` |
| [`luima/crud`](https://pkg.go.dev/github.com/ulas96/luima/crud) | `Get`, `List`, `Create`, `Update`, `Delete` |
| [`luima/luimaerr`](https://pkg.go.dev/github.com/ulas96/luima/luimaerr) | `CustomError`, `PresentError`, `SQLState` |
| [`luima/db`](https://pkg.go.dev/github.com/ulas96/luima/db) | `Connect` |

### Runtime

```go
func New(cfg Config) *fiber.App          // a server
func Mount(r fiber.Router, cfg Config)   // GraphQL on a router you already have
```

| `Config` field | default | notes |
|---|---|---|
| `Schema` | — | required; your `generated.NewExecutableSchema(...)` |
| `Endpoint` | `/graphql` | |
| `Playground` | `/` | Fiber matches it **exactly** — unknown paths 404 |
| `DisablePlayground` | `false` | do this in production |
| `PlaygroundTitle` | `graphql` | |
| `DisableIntrospection` | `false` | do this in production; it is not confidentiality — [why](SECURITY.md#what-luima-does-do) |
| `RequestTimeout` | `15s` | **negative** disables; zero means unset. The only bound a query gets |
| `QueryCache` | `1000` | **negative** disables; zero means unset |
| `ComplexityLimit` | `1000` | **negative** disables; zero means unset. Per *field*, not per row |
| `ErrorPresenter` | `PresentError` | |
| `HTTPMiddleware` | `nil` | `func(http.Handler) http.Handler`, outermost first — the layer with the real `*http.Request`, working `Set-Cookie`, and a context resolvers see |
| `Configure` | `nil` | `func(*handler.Server)`, run last — the escape hatch to `srv.Use`, `AroundOperations`, `SetRecoverFunc`, `SetDisableSuggestion` |
| `Fiber` | `fiber.Config{}` | passed to `fiber.New`; ignored by `Mount`. Setting `ErrorHandler` here does nothing — [why](docs/fiber.md#2-fibers-errorhandler-is-unreachable--and-must-stay-that-way) |

Zero means *unset* for the three limits, not *off*, because a zero-valued `Config{}` has to be the
good configuration rather than the pathological one. Turning the query cache off needs a negative
number, and there is no good reason to.

`RequestTimeout` is the one that does security work rather than performance work: nothing else in
the stack bounds a query, and go-pg turns the resolver context's deadline into the socket deadline
and its cancellation into a Postgres `CancelRequest`. A zero `Config` is the good *development*
configuration, not a production one — see [SECURITY.md](SECURITY.md) and
`examples/quickstart/main.go`, which is deliberately the production shape.

### CRUD

```go
func Get[T any](ctx context.Context, db orm.DB, key *T) (*T, error)
func List[T any](ctx context.Context, db orm.DB, opts ...func(*orm.Query) *orm.Query) ([]*T, error)
func Create[T any](ctx context.Context, db orm.DB, m *T, label string) (*T, error)
func Update[T any](ctx context.Context, db orm.DB, m *T, label string) (*T, error)
func Delete[T any](ctx context.Context, db orm.DB, key *T) (bool, error)
```

**The point of these is the error classification, not the query.** Writing
`db.ModelContext(ctx, u).Insert()` was never hard; knowing that a duplicate key must become a
`*CustomError` or it will be redacted to `"internal server error"` is the part that takes a
production incident to learn.

| helper | classifies |
|---|---|
| `Get` | `pg.ErrNoRows` → `(nil, nil)` — a missing row renders as GraphQL `null`, not an error |
| `List` | nothing — the **non-nil seed** is the entire value |
| `Create` | `23505` → `label + " already exists"` |
| `Update` | no row matched → `label + " not found"` |
| `Delete` | nothing — absence is `false`, not an error |

They take `orm.DB`, so `*pg.DB`, `*pg.Conn` and `*pg.Tx` all satisfy it — pass the tx inside
`db.RunInTransaction(...)` and nothing else changes.

`List` ships no named `Order`/`Limit` wrappers; one closure gets the whole go-pg query API. **Do
order your lists** — Postgres gives no stable row order without `ORDER BY`, and an unordered `List`
produces intermittently reordered responses that look like a caching bug.

Three things to know: `Update` is a **full replace** (no partial updates); its absence signal is
[not what you expect](docs/gotchas.md#10-the-two-absence-signals); and both writes use
[`RETURNING *`](docs/gotchas.md#14-returning-).

### Errors

```go
type CustomError struct {
    UserMessage   string
    InternalError error
}

func PresentError(ctx context.Context, err error) *gqlerror.Error
func SQLState(err error) string
```

**Resolvers opt in to being heard.** A resolver returning `errors.New("user already exists")`
produces `"internal server error"` on the wire. This is the design, not a bug — gqlgen's default
presenter would forward raw driver strings, and with them your constraint and column names, to an
unauthenticated caller.

`PresentError` has three branches: `*CustomError` passes through (you declared it safe),
`*gqlerror.Error` passes through (gqlgen's own text about the query the *client* just sent — drop
this branch and every schema typo reads as "internal server error"), everything else is logged
server-side and redacted.

`SQLState(err)` replaces an `errors.As` dance whose target type is easy to get wrong: `pg.Error` is
an **interface**, not a struct pointer, and not pgx's `*pgconn.PgError`.

| code | meaning |
|---|---|
| `23505` | unique_violation |
| `23503` | foreign_key_violation |
| `23502` | not_null_violation |
| `23514` | check_violation |

### Talking to this server

Resolver errors return **HTTP 200** with the message in the `errors` array; gqlgen returns **422**
for parse and validation errors, so a 422 always means client/schema drift. Either way the body
carries the message, so **a client must decode the body before it looks at the status.**

---

## Documentation

| | |
|---|---|
| **[The gqlgen contract](docs/gqlgen-contract.md)** | What you still own — and why `go build` is not the schema check |
| **[Fiber](docs/fiber.md)** | `All` vs `Post`, the unreachable `ErrorHandler`, and the ceiling that blocks subscriptions |
| **[Deployment](docs/deployment.md)** | `ParseURL`'s three parameters, TLS verification, `.env` in Docker, security posture |
| **[Gotchas](docs/gotchas.md)** | 26 silent failures, with the fix for each |
| **[examples/quickstart](examples/quickstart)** | The whole thing, running |

---

## Development

```sh
make test        # no database — TestCRUD skips
make test-db     # sources .env, runs everything
make lint
make example     # build examples/quickstart and check for unfilled stubs
```

> **A green `go test ./...` proves less than it looks.** `TestCRUD` skips silently without
> `DATABASE_URL`. Run with `-v` and confirm the test **ran**. CI runs it against a `postgres:16`
> service container so it cannot rot.

See [CONTRIBUTING.md](CONTRIBUTING.md).

---

## License

MIT — see [LICENSE](LICENSE).
