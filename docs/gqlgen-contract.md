# The gqlgen contract — what you still own

[← back to the README](../README.md)

luima cannot replace gqlgen codegen, and no library could. gqlgen generates code *into your
module*: `generated.NewExecutableSchema` is a symbol that only your own `go tool gqlgen generate`
run produces, from your own `schema.graphqls`. Nothing can produce it, import it, or wrap it ahead
of time. Any design where you hand a library a schema *file* and get a server back is impossible
with gqlgen.

So you own `gqlgen.yml`, `schema.graphqls`, `graph/resolver.go`, and the codegen step. This page is
that contract. **It is not optional reading** — get [the next section](#go-build-is-not-the-schema-check)
wrong and you ship a server that panics in production and compiles clean.

---

## `go build` is **not** the schema check

**This is the most important operational fact in luima's documentation.**

gqlgen writes a stub for every schema field with no resolver method:

```go
func (r *queryResolver) Users(ctx context.Context) ([]*model.User, error) {
	panic(fmt.Errorf("not implemented: Users - users"))
}
```

A stub **compiles**. So SDL with no working resolver is a **runtime panic**, not a build failure,
and `go build ./...` will tell you everything is fine.

The check is a grep. Put it in your Makefile:

```make
generate:
	go tool gqlgen generate
	@grep -rn 'not implemented' graph/*.resolvers.go \
		&& echo '^^ unfilled resolver stubs — these COMPILE and panic at runtime; go build will not catch them' \
		|| true
```

`grep` exits 1 when it finds nothing, so the `|| true` is what keeps a clean generate from failing
the target.

---

## `gqlgen.yml`

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
  - "github.com/you/yourapp/graph/model"
```

Three keys carry the weight:

### `resolver:` is what switches resolver generation on

`ResolverConfig.IsDefined()` is `Filename != "" || DirName != ""`. Declaring `dir` is what makes
gqlgen write a stub for every unimplemented schema field *and* copy your hand-written bodies
forward on the next run. Without it you get no resolver scaffolding at all.

### `layout: follow-schema`, never `single-file`

`single-file` sets `HasRoot: true` and re-emits `type Resolver struct{}` on **every** run — wiping
the `DB *bun.DB` field you put there.

`follow-schema`'s `generatePerSchema` never sets `HasRoot`, and writes `graph/resolver.go` at all
only when the file does not already exist. That single fact is what keeps the injection root
hand-editable.

### `autobind` is how a hand-written model becomes the GraphQL type

With the model package autobound, gqlgen finds `model.User` by name and generates **no** `User`
struct and **no** `UserResolver` — `generated.go` marshals straight off `obj.Name`. Without it,
gqlgen generates a second, competing `User` and you get two types with the same name.

---

## `graph/resolver.go` — hand-written, never regenerated

```go
package graph

import "github.com/uptrace/bun"

// Resolver is gqlgen's dependency-injection root.
type Resolver struct{ DB *bun.DB }
```

`*bun.DB` and not `bun.IDB`: this field holds the concrete handle `luima.Connect` returns. The five
CRUD helpers take `bun.IDB`, which `*bun.DB`, `bun.Conn` and `bun.Tx` all satisfy, so a resolver
that wants a transaction passes the `bun.Tx` that `r.DB.RunInTx(ctx, nil, …)` hands it to the same
helpers with nothing else changed.

Anything else the resolvers need — a logger, a cache, a second pool — goes here too. This file is
safe from codegen only because of the `follow-schema` behaviour above.

---

## Two rules for `*.resolvers.go`

**1. Resolver methods and nothing else.** `rewrite.RemainingSource` sweeps every other declaration
in that file into a `/* !!! WARNING !!! */` comment block on the next `gqlgen generate`. Your
helper function does not error — it silently becomes a comment. Helpers belong in `resolver.go` or
another file.

**2. Never hand-write the `Query()`/`Mutation()` accessors, or the `queryResolver`/
`mutationResolver` structs.** Codegen emits all of them; a second copy is a duplicate-declaration
compile error.

---

## Do not reach for `resolver: true` on fields

`Field.IsConcurrent()` is `MethodHasContext || IsResolver`. A plain field binding is neither, so
the generated `_User` contains no `Concurrently` call at all — the fields marshal inline. Adding

```yaml
fields:
  someField:
    resolver: true
```

puts **every** field of that type into its own goroutine. And a field with a real source — a join
table, a remote call — then runs **once per row**, so it needs a dataloader the same day you add
it. Both are fine; neither is free, and neither should be stumbled into.

---

## Keep `graph/model` unable to import `graph`

It cannot anyway — that is an import cycle — and the consequence is worth naming: your error types
physically cannot reach the data layer. That layering is compiler-enforced rather than a
convention, which is the one thing the standard gqlgen layout gives you for free.

Do not "fix" it by moving error types into `model`. luima's `luimaerr` package exists partly so
you never have to: it imports nothing else in luima, so any package can return a `*CustomError`.

---

## The model

```go
type User struct {
	bun.BaseModel `bun:"table:app_users"`

	PersonalID string   `bun:"personal_id,pk"`
	Name       string   `bun:"name"`
	Company    string   `bun:"company"`
	Projects   []string `bun:"projects,array"`
}
```

1. **A `pk` tag is mandatory.** `Get`, `Update` and `Delete` all call `WherePK()`.
2. **Fields must be exported, and the tag names the column.** Three separate things read the
   fields: gqlgen's `equalFieldName` (strips `_`, compares with `EqualFold`, so schema `personalId`
   binds to Go `PersonalID`), bun's column mapping, and `encoding/json`. Leave the name out of the
   tag and bun derives one with `internal.Underscore` — which is byte-identical to go-pg's function
   of the same name, so a model ported tag-for-tag from go-pg keeps binding to exactly the columns
   it did before. What that rule cannot derive is a run of capitals: an underscore goes in only next
   to a lower-case rune, so `URLID` becomes `urlid` where the column almost certainly says `url_id`.
   Write that one out.
3. **`,array` is load-bearing on every slice field.** Without it, bun's appender table maps
   `reflect.Slice` to `schema.AppendJSONValue` (`schema/append_value.go`), so the insert sends
   `'["apollo"]'` — which Postgres rejects for a `text[]` column, SQLSTATE 22P02. The tag is what
   makes pgdialect's `onField` swap in the array appender and scanner instead
   (`dialect/pgdialect/dialect.go`). Nothing catches a missing one but the first `Create`: a
   `pg_typeof(projects)` assertion reports the *column's* declared type and cannot see the tag, so
   it pins the table you are pointed at, not the model.
4. **The table name comes from the embedded `bun.BaseModel`, and from nowhere else.** This is the
   one that fails quietly. bun skips every unexported field that is not embedded, without a word
   (`(*schema.Table).processFields`), so the `tableName struct{}` spelling that go-pg and other
   Postgres mappers use still compiles here and is ignored — and bun then names the table after the
   type, underscored and pluralized (`(*schema.Table).init`). A `User` meant for `app_users` queries
   `users`. Where no such table exists every helper fails with a redacted error; where one does, the
   queries succeed against the wrong table.

**A nullable list arrives as a nil slice, and a `NOT NULL` array column refuses it.** bun writes a
nil `,array` slice as a literal `NULL` rather than as `DEFAULT` (pgdialect's `appendStringSlice`),
so it never falls back to the `default '{}'` on `projects text[] not null default '{}'` — the insert
is a 23502, which the presenter redacts to "internal server error". gqlgen hands you nil in two
cases, both of them a **nullable** `[String!]`: one the client sent as `null` (`codegen/type.gotpl`
opens a nullable unmarshaller with `if v == nil { return nil, nil }`) and one the client omitted
(the generated `unmarshalInput` loop `continue`s over an absent key, leaving the Go field zero). So
shape one side or the other. Declare the list non-null — `[String!]!`, where the generated
unmarshaller is `make([]string, len(vSlice))` and is never nil, which is what the quickstart does —
or add `,nullzero` to the model field, which makes bun write `DEFAULT` for the nil and lets the
column's own default supply `'{}'`. Normalizing nil to `[]string{}` in the resolver works too, and
means an explicit `null` clears the column rather than restoring its default.

---

## `go.mod`

```
require github.com/99designs/gqlgen v0.17.94

tool github.com/99designs/gqlgen
```

The Go 1.24+ `tool` directive is what makes `go tool gqlgen generate` work with no `tools.go` file
and no separate install step. luima itself is whatever version `go get github.com/ulas96/luima`
records; no line above pins it, because the model and resolver shapes on this page are the bun ones
and a stale pin would name a go-pg release that does not have them.

---

See also: [Fiber](fiber.md) · [Deployment](deployment.md) · [Gotchas](gotchas.md)
