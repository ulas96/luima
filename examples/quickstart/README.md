# quickstart

A complete luima server: five resolvers over one Postgres table, in ~40 lines of hand-written Go.

This is the [README quickstart](../../README.md#quickstart) as a working module. It is a **nested
module** with `replace github.com/ulas96/luima => ../..`, so it always builds against the working
tree — and so the `tool github.com/99designs/gqlgen` directive it needs does not drag gqlgen's CLI
dependencies into the library's module graph, and from there into yours.

## Run it

```sh
createdb luima_example    # or point DATABASE_URL at any Postgres

psql "$DATABASE_URL" -c "
create table if not exists app_users (
  personal_id text primary key,
  name        text not null,
  company     text not null,
  projects    text[] not null default '{}'
);"

export DATABASE_URL='postgres://...'   # unquoted in .env; see docs/deployment.md
go run .
```

Playground on <http://localhost:8080>, API on <http://localhost:8080/graphql>.

```graphql
mutation {
  createUser(personalId: "E-1042", input: {name: "Ada", company: "Acme", projects: ["apollo"]}) {
    personalId
    name
    projects
  }
}
```

Run it twice and the second one answers `"user E-1042 already exists"` — not a 500, and not a raw
`SQLSTATE 23505` with your constraint names in it.

## What's hand-written, and what isn't

| file | who writes it |
|---|---|
| `graph/schema.graphqls` | you — the source of truth |
| `graph/model/user.go` | you — the go-pg tags are the table schema |
| `graph/resolver.go` | you — the injection root, plus `newUser`. Never regenerated |
| `graph/schema.resolvers.go` | **generated shell, hand-written bodies** — five one-liners |
| `gqlgen.yml`, `main.go` | you, once |
| `graph/generated/`, `graph/model/models_gen.go` | gqlgen. Do not hand-edit |

Generated code is committed so the example builds from a clean checkout, and CI re-runs
`gqlgen generate` and fails on any diff — which proves it still matches the schema.

## The loop

```sh
go tool gqlgen generate
grep -rn 'not implemented' graph/*.resolvers.go   # must print nothing
go build ./...
```

**`go build` is not the schema check.** gqlgen writes a stub for every schema field with no
resolver method, and a stub *compiles* — it panics when called. The grep is the check. See
[docs/gqlgen-contract.md](../../docs/gqlgen-contract.md).

## One import or four

This example uses the root package, which re-exports everything:

```go
import "github.com/ulas96/luima"

app := luima.New(luima.Config{Schema: …})
return luima.Create(ctx, r.DB, u, "user "+personalID)
```

The sub-packages are equivalent — `luima.Config` *is* `server.Config`, not a copy — and worth
importing directly when you want a narrower dependency:

```go
import (
    "github.com/ulas96/luima/server"
    "github.com/ulas96/luima/crud"
    "github.com/ulas96/luima/luimaerr"
    "github.com/ulas96/luima/db"
)
```
