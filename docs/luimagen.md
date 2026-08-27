# luimagen — the CRUD generator

luimagen is the library behind the `cmd/luimagen` binary: given one table's fields, it writes the
Go model struct, appends the matching GraphQL SDL to a schema file, runs the consumer's own
`go tool gqlgen generate`, and fills the five resulting resolver stubs with calls to
`luima.Get/List/Create/Update/Delete`. It never touches the database — the table is expected to
already exist, created however the consumer already creates tables.

This document records the design and its constraints. The library entry point is
`luimagen.Generate(luimagen.Options{...})`; `cmd/luimagen` is a thin flag-parsing wrapper over
it. The quickstart in `examples/quickstart` is the reference consumer: its
`schema.graphqls`, model struct, and resolver bodies are the ground truth every stage below was
shaped against.

## 1 · Where it lives, and why

The root module already documents "a scaffolding CLI" as out of scope — and luimagen *is* one.
gqlgen has no runtime schema: `generated.NewExecutableSchema` is static code produced by the
consumer's own `go tool gqlgen generate` (see `docs/gqlgen-contract.md`), so "trigger a
function, get CRUD" can only mean generating code. The tension is resolved by placement, not by
argument:

- **A separate `luimagen` package, not the module root.** Importing `github.com/ulas96/luima`
  never pulls it in; `Config`/`Mount`/`Run`/`crud/` gain no new surface, and `luima.go`
  does not re-export it. The root package's own "out of scope" list stays accurate for the library
  consumers import.
- **`cmd/luimagen`** wraps it in a flag-parsing binary; the logic lives in the importable package
  so a consumer's own tooling can call `Generate` directly instead of shelling out.
- **One package, not a second module.** luimagen adds nothing to `go.mod`: beyond the standard
  library it imports only `github.com/vektah/gqlparser/v2`, which `luimaerr` already made a direct
  dependency. There is no dependency graph to isolate and no independent versioning need.

`Generate` takes the table's fields directly — `Options.Fields []Field` — and **writes the
model struct itself** rather than reading a hand-written one. That is the meaning of "one call
builds a table's CRUD layer", and it relocates (rather than removes) the review surface: a
`Field{Name, Type, PK}` call site plus the generated model file are both visible in the diff. A
`Field.Type` outside the mapped set is rejected loudly before anything is written; a
mapped-but-wrong type (a column that should be `text` declared as `int`) is caught by nothing
here, exactly as a hand-typed go-pg tag never was — luimagen closes no correctness gap, it only
moves where a mistake is visible.

## 2 · The mechanism

`Generate` runs four stages in order: build the table description, write the model struct, append
the SDL, run gqlgen and patch the stubs it wrote. Validation in the first stage runs before any
file is written; a failure later in the pipeline leaves the files already written on disk — §2.4
covers recovery.

### 2.1 · From Fields to the model struct

`tableFromFields` validates `Options.Fields` and resolves each field's SQL column name and
GraphQL scalar:

- Exactly one `Field` must have `PK: true`, and it must be a scalar, not a slice —
  `Get`/`Update`/`Delete` all call `WherePK` against the single-column key, and an array PK
  has no single-column SDL argument. At least one non-PK field must exist too: it is what the
  `XInput` object carries, and a GraphQL input type must declare at least one field.
- Every `Field.Name`, and `Options.Type` itself, must be an **exported Go identifier**. An
  unexported name (`-field name:string:pk`, an easy CLI typo) generates a struct field gqlgen
  cannot autobind and that the patched resolver — a different package — cannot reference; a name
  with a space or a hyphen generates source that does not parse. Both are rejected up front,
  because otherwise they surface only after gqlgen has rewritten the generated code.
- **A `_` is rejected for the same reason, and it is the one that does not look wrong.** `_` is a
  legal Go identifier rune, so it clears the check above, but `templates.ToGo` treats it as a word
  delimiter and *drops* it: SDL `owner_Name` comes back as `GadgetInput.OwnerName`. That breaks the
  "`ToGo` only re-cases a delimiter-free name" assumption every lookup in §2.4 rests on, so the run
  reports success and the consumer's module no longer compiles.
- **A `Field.Name` must also be ASCII**, which a Go identifier need not be. A GraphQL `Name` is
  `/[_A-Za-z][_0-9A-Za-z]*/`, so an accented name is a legal struct field whose derived SDL field
  gqlparser cannot parse — and `checkComposes` (§2.3) cannot be relied on to catch it, because it
  ignores a source set that does not validate standalone, which is every schema using gqlgen's
  injected `@goModel`/`@goField`. It is also what lets `snakeCase` and `lowerFirst` treat their
  rune handling as belt-and-braces rather than as the thing keeping the SDL valid.
- `Options.Table` and `Field.Column` are the inputs that are not checked identifiers — they are SQL
  names, and `tenant.users` is valid — so they are checked only against the raw-string `pg:"…"` tag
  they are written into verbatim: a backtick ends the literal early, and **anything `%q` escapes**
  — a double quote, a backslash, a tab, a control character — becomes a literal backslash sequence
  inside the raw string that go-pg reads as part of the name. The test is `strconv.Quote(v) ==
  "\""+v+"\""`, not two hand-picked characters, because the backslash case is the one that looks
  fine in the flag and mangles silently.
- Every `Field.Type` must be `string`, `int`, `float64`, `bool`, or a slice of one of
  those — exactly the Go types that round-trip through gqlgen's default bindings (GraphQL `Int`
  becomes Go `int`, `Float` becomes `float64`). Other widths (`int64`, `uint*`,
  `float32`) would generate resolver code that does not compile, so they are errors, not guesses.
- The SQL column name is `snakeCase(Field.Name)` — byte-for-byte go-pg's own `Underscore`
  convention (`PersonalID` → `personal_id`, `URLValue` → `url_value`). **`Field.Column` overrides
  it**, and the case that needs it is the one go-pg's rule cannot derive: an underscore goes in only
  when a neighbouring rune is lower-case, so a run of capitals gets none at all — `URLID` becomes
  `urlid` where the column is almost certainly `url_id`. luimagen does not create the table, so a
  derived name that disagrees with it compiles, vets and lints clean, and fails on the first query
  with `column "urlid" does not exist`. `Options.Table` is the same escape hatch one level up.
- **No two fields may collide on either derived name.** The exact-duplicate case is the likeliest
  CLI typo — a repeated `-field` flag — and would emit the struct field twice, failing at gqlgen
  with both files already written. The other two are silent: `URLValue` and `UrlValue` are distinct
  Go identifiers that `snakeCase` both maps to `url_value`, which binds two struct fields to one
  column, and `ID` and `Id` both `lowerFirst` to the GraphQL field `id`. All three are rejected
  here, where nothing has been written yet.

`writeModel` then writes `<ModelDir>/<type>.go` with the same shape as the quickstart's
hand-written model — a NatSpec doc comment explaining the three load-bearing details, `tableName
struct{}` carrying the table name, the PK tagged `,pk`, array columns tagged `,array` — and refuses
to overwrite an existing file, so a second run fails loud instead of clobbering a file that may
since have been hand-edited. The doc comment is not decoration: this file lands in the *consumer's*
module, and `revive`'s `exported` rule — which `.golangci.yml` enables here, and which a consumer
who copied this config inherits — flags a bare `type User struct` as an error on code they did not
write.

Two details of that write are not obvious. `writeModel` **creates `ModelDir`** if it is not there:
it runs before gqlgen, so on a first run — a module with a schema and no generated code yet —
nothing else can have created it, and the raw `no such file or directory` it would otherwise fail
with is indistinguishable from a wrong `ModelDir`. And the file's `package` clause is read from the
`.go` files already in that directory, **not** from `Options.ModelPkg`: `ModelPkg` is the identifier
the resolver bodies prefix, and the only reason to set it is that gqlgen aliased the import
(`model1.User`) — writing `package model1` beside gqlgen's `package model` breaks the model package
outright, after gqlgen has already regenerated.

The default table name is `snakeCase(Type)+"s"` — naive pluralization. The real quickstart
table is `app_users`, which no default could produce, so anything irregular must set
`Options.Table` explicitly. Same known ceiling as the list-field name in §2.3.

### 2.2 · One table description, three consumers

Stages 2.1, 2.3 and 2.4 all work from the same internal `modelTable` — a type name, a PK
`column`, and the non-PK `column`s in declaration order. Each `column` carries the Go field
name, the raw `Field.Type` string (written back out verbatim into the model struct; ignored by
the SDL and patch stages), the derived SQL column name, the resolved GraphQL scalar, and whether it
is an array.

### 2.3 · Emit SDL, idempotently

The emitted shape is copied from the one hand-verified example in this repo
(`examples/quickstart/graph/schema.graphqls`), and three of its details are easy to get wrong by
guessing:

- The primary key is mapped through the same scalar switch as every other field — `String!`, not
  `ID!`.
- One `XInput` is shared between create and update — not `CreateXInput`/`UpdateXInput`.
- The primary key is its own argument on create/update/delete — not a field inside the input.

`appendSDL` has two real behaviours beyond a plain append:

1. **Refuse a duplicate.** If the schema already contains `type X {`, error out rather than
   double-declare.
2. **`type Query { }` for the first table, `extend type Query { }` for every table after** —
   decided by whether the schema already declares `type Query`/`type Mutation`, which is
   the normal case for every table after the first. A schema declaring `type MutationResponse` — a
   common convention — and no `Mutation` at all must not be read as already having one, or the
   fragment would `extend` a base type that does not exist, which gqlparser rejects.

**Both questions are answered off `parser.ParseSchema`**, gqlparser's own parse of each file: a
`Definition` named `Query` is a declaration, an `Extension` is not, and a name that appears in
either list is taken. Parsing, not `LoadSchema` — a schema using gqlgen's injected
`@goModel`/`@goField` does not *validate* standalone, but it always parses, and both questions are
about declarations rather than about a valid schema. The previous mechanism was a line-anchored
regex over the file's text, which needed its own `"""`-block stripper to stop a description reading
as a declaration, and was a second, weaker answer to a question the parser already in the file
could give.

Both checks read **every file beside `Options.SchemaFile` sharing its extension**, not just that
file, because that is what gqlparser reads: the documented layout globs the directory
(`examples/quickstart/gqlgen.yml`: `schema: - graph/*.graphqls`). The extension comes from
`SchemaFile` rather than being hardcoded — `.graphql` is the other common spelling, and under it a
fixed `*.graphqls` glob makes every peer invisible. Reading one file makes a `type Query` declared
in a sibling invisible, so the fragment declares its own and the redeclare surfaces at gqlparser —
after both files are on disk. Only the checks widen; `appendSDL` still writes to
`Options.SchemaFile` alone, so that option still targets exactly one file. The duplicate error names
the file that actually declares the type, because §2.4's recovery procedure depends on knowing which
one to edit. A peer that cannot be read or parsed is skipped; `SchemaFile` itself is not — it is the
file being appended to, and `checkComposes` splices the fragment into the source *named* by it, so a
missing entry there would leave the compose check comparing two identical schemas and passing
having checked nothing.

Both checks are read-only and run in `planSDL`, **before** `writeModel` writes anything. A schema
that already declares the type therefore fails with nothing on disk, rather than leaving a stray
`<ModelDir>/<type>.go` behind to redeclare a hand-written struct.

### 2.4 · Run gqlgen, patch exactly the stubs, and recover from failure

`Options.ResolverFile` defaults to `Options.SchemaFile` with its extension replaced by
`.resolvers.go`, because that is where gqlgen's documented `resolver.layout: follow-schema` puts the
stubs — beside the schema file they came from. A fixed `graph/schema.resolvers.go` is right only for
the default `-schema`, and for any other one it reads a file with none of the five methods in it,
which surfaces as "the resolver file declares no queryResolver.Widget" *after* gqlgen has already
regenerated the module.

gqlgen's stub text is fixed (`docs/gqlgen-contract.md`): a one-statement
`panic(fmt.Errorf("not implemented: ..."))` body. luimagen does not re-derive gqlgen's naming
rules (`personalId` → `personalID`): it lets gqlgen write the stub — the correct signature and
parameter names — and then replaces only the body between the braces, by byte offsets from the AST
of the generated file, matching each method by receiver type (`queryResolver`/`mutationResolver`)
and name and reading the parameter names off the actual declaration.

**Read the names, never predict them**, and this is the rule the whole patch stage turns on.
gqlgen runs every SDL name through `templates.ToGo`, which re-capitalizes common initialisms — a
list of 24 that includes `ID`, `URL`, `API`, `UUID`. So `Field{Name: "OwnerId"}` becomes SDL
`ownerId` and comes back as `UserInput.OwnerID`, and `Options.Type: "URL"` becomes SDL field `uRL`
and comes back as the method `URl`. Assuming either name round-trips generates code that does not
compile, or a lookup that misses. luimagen reads both instead:

- **Method names** are matched with `strings.EqualFold`. For a delimiter-free name `ToGo` only
  ever re-cases — `wordWalker` never inserts, drops, or substitutes a rune — so two names that
  differ only in case are the same method, and case-insensitive is exact here, not sloppy.
- **Generated input field names** come from parsing the model package: `inputFieldNames` globs
  `<ModelDir>/*.go` for `type <Type>Input struct` and reads the field names off it. The generated
  file's name is a `gqlgen.yml` setting, so the whole directory is tried. Only the `input.X` side
  of each assignment goes through this map — the key side is luimagen's own model struct, whose
  names are `Field.Name` verbatim.

Importing gqlgen's `codegen/templates` for the real `ToGo` was the alternative, and would drag
`golang.org/x/tools` into `luima`'s module graph — what `examples/quickstart/go.mod`'s own comment
keeps the `tool` directive out of the library module to avoid. Parsing the generated source costs
no dependency and cannot drift when gqlgen adds an initialism.

The patched bodies call `luima.Get/List/Create/Update/Delete`. `Create`/`Update` inline the
model literal (`&model.User{PersonalID: personalID, Name: input.Name, ...}`) rather than
generating a helper into the hand-owned `resolver.go` — a few duplicated lines buys touching
exactly one file. A non-string PK generates the create/update label through `fmt.Sprintf`, with the
type name baked into the format string (`fmt.Sprintf("counter %v", id)`), since `"counter "+id`
does not compile for an `int` key. The generated `List` caps at 100 rows and says so in a comment in the
generated file: luima ships no pagination (`CLAUDE.md`, "out of scope, deliberately"), an uncapped
list is a footgun on a growing table, and a *silent* cap is indistinguishable from a table that
really holds 100 rows. Raise or drop the `Limit` to suit the table.

One shape is inherited from the reference schema and is worth naming as a ceiling.
`createX(...): X!` is non-null, copied from `examples/quickstart/graph/schema.graphqls`, but
`crud.Create` documents a `(nil, nil)` result for an insert the database declined to perform — and
one path there needs no options at all: a `BEFORE INSERT` trigger returning NULL, the ordinary way
to write a soft-ignore. On such a table the mutation answers
`Cannot return null for non-nullable field Mutation.createX` with nothing logged server-side. Make
the field nullable by hand (`createX(...): X`) if the table has one. `updateX` is unaffected —
`crud.Update` returns a `NOT_FOUND` `*luimaerr.CustomError`, not absence.

The patch is safe to run more than once, and the failure mode is deliberate:

- **Only still-stub bodies are touched.** gqlgen's `follow-schema` layout copies hand-written
  bodies forward on the next generate, and the patcher checks via the AST that a body is still
  exactly the one-statement `panic(...)` stub — a method already filled in, by luimagen or by
  hand, is left alone. A second run finds no stubs and fails informatively. The check reads the
  panic's *argument*, not just the fact that the body is a `panic`: a deliberate placeholder like
  `panic("TODO: needs an ownership predicate before this ships")` is a body somebody wrote, and
  replacing it with an unscoped `luima.Delete` is exactly the silent damage the guarantee exists
  to prevent.
- **The signature must be the one luimagen generated SDL for.** The parameter count after `ctx` is
  checked for exact equality, not a minimum: too few would index out of range, and too *many* means
  the field carries an argument luimagen's SDL never declared — splicing anyway would discard a
  value the client sent, silently, at runtime.
- **All five must be there.** After the splice, `checkComplete` re-parses the file and fails if any
  of the five methods is missing, or still panics. A partially patched file still *compiles* — a
  stub is valid Go — so a surviving `panic("not implemented")` would only fire on the first query
  that reaches it, in production. That failure is the one `docs/gqlgen-contract.md` calls the most
  important operational fact about this seam, so it is an error and not a cheerful success line.
  **The file is written before that error is returned**, not after: every splice the patcher made is
  correct whatever `checkComplete` then finds, and discarding them would leave the caller holding
  the model file, the appended SDL and a fully regenerated module with none of the bodies filled —
  a state no re-run can redo, because `planSDL`'s duplicate guard now rejects the type.
- **The import block is rewritten canonically** (stdlib group, blank line, third-party group,
  sorted) from a re-parse of the post-splice source. Specs are collected from *every* import
  declaration and merged into the first, and any later block is deleted — reading only the first
  makes an import declared in a second one invisible, so `luima` gets appended to the first while
  the second still declares it, which is a redeclare and a build failure. `fmt` is dropped once no
  surviving code uses it — keyed on the *binding* name, so an aliased `f "fmt"` whose `f` is now
  dead goes too, which is what stops it surviving unused beside a freshly added `"fmt"` — and added
  back when a non-string PK label needs it. `luima` and `orm` are added only when a surviving
  identifier references them: a hand-written `List` left alone must not drag in an `orm` import
  nothing uses, which would break the build. Every add is keyed on the *name* being free, not only
  on the path being absent — a file that already binds `luima` to a fork gets a clear error naming
  the conflict, rather than a second unaliased import and `luima redeclared in this block`, which
  `format.Source` cannot catch because it does not typecheck.
- **A failed mid-run call is recovered manually, on purpose.** Every read-only check runs first, so
  a bad `Field`, a duplicate type, or a missing schema file fails with nothing written. Past that
  point `writeModel` and `appendSDL` run before gqlgen and the patch, so if either later stage
  fails, the model file and the SDL fragment are already on disk. The same idempotency guards that make a *repeated* run fail informatively
  make a *retry* fail immediately, at the guard, instead of redoing the work. Recovery: remove
  `<ModelDir>/<type>.go`, delete the appended SDL block by hand, fix whatever gqlgen or the
  patch step actually complained about, then run `Generate` again from a clean slate. There is no
  automatic rollback — it is the same "fail loud, don't silently clobber a hand-edited file"
  posture the guards themselves exist for.

Consumer prerequisites, inherited from the library's own contract rather than invented here: a
gqlgen module whose `gqlgen.yml` autobinds the model package (so gqlgen resolves the GraphQL type
to the generated struct), a resolver root carrying the pool as `DB` (so `r.DB` compiles), and
gqlgen available as a `go tool` — the `tool` directive in `go.mod` — since the generation step
runs the consumer's own `go tool gqlgen generate`.

## 3 · Files

| path | what it is |
|---|---|
| `luimagen/luimagen.go` | the exported surface: `Field`, `Options`, `Generate` |
| `luimagen/gen.go` | stages 2.1–2.3: fields → table → model file → SDL (unexported) |
| `luimagen/patch.go` | stage 2.4: run gqlgen, AST-splice the stubs, rewrite imports (unexported) |
| `luimagen/internal_test.go` | white-box tests for the unexported stages — no exec, no gqlgen; `t.TempDir` for the ones that touch disk |
| `cmd/luimagen/main.go` | flag parsing only (repeatable `-field Name:Type[:pk]`); calls `luimagen.Generate` |
| `tests/luimagen_test.go` | `ExampleGenerate`: the exported API as seen from a consumer |

## 4 · What must not land in v1

Each row names the ceiling in one line and the upgrade path. The first row is the one reversal —
model generation *is* in scope — and its cost is the trade-off stated in §1.

| candidate | verdict |
|---|---|
| Writing the model struct | **in scope.** The wrong-`Field.Type` mistake stays visible in the diff; closing the "valid but wrong" case would need column-type knowledge luimagen does not have |
| Relations / joins / dataloaders | **no.** Out of scope for luima itself; a generator shouldn't manufacture scope the library doesn't have |
| Partial updates (a `Column` allowlist) | **no.** `crud.Update` is full-replace only, deliberately |
| Composite / array primary keys | **no.** Exactly one scalar `PK: true` is the whole scope; anything else errors |
| Auth predicates in the generated `opts` | **no.** luima ships no auth; a guessed ownership predicate would be worse than the hand-written escape hatch the library provides |
| Scalar types beyond `string`/`int`/`float64`/`bool` (+ slices) | **no.** Error clearly, naming the gqlgen binding mismatch |
| SQL column name override | **done.** `Field.Column` (`-field Name:Type:column=<sql name>`). go-pg's rule inserts no separator inside a run of capitals, so `URLID` derives `urlid` where the column is `url_id` — and since luimagen does not create the table, the mismatch only surfaces as a query-time `column does not exist`. `Options.Table` is the same hatch one level up |
| GraphQL field name override | **no, not yet.** The field name derives from `Field.Name` through `lowerFirst`; unlike a column name it has no external table to disagree with, so the override stays deferred for names no convention derives (reserved words) |
| `Options.ResolverFile` independent of `SchemaFile` | **done.** It defaults to `SchemaFile` with `.resolvers.go` for its extension, which is where `resolver.layout: follow-schema` puts the stubs. Still overridable for a layout that puts them elsewhere |
| Re-running `Generate` for a type whose model file exists | **no.** `writeModel` refuses to overwrite, like `appendSDL`'s duplicate guard |
| Table name defaults beyond naive `snake_case(Type)+"s"` | **no.** `Options.Table` is the escape hatch for a prefix (`app_users`) or irregular pluralization |
| Multi-file schema awareness | **partly.** `planSDL`'s declare-vs-extend probe and duplicate guard read every `*.graphqls` in `SchemaFile`'s directory, because that is what gqlparser reads; `appendSDL` still writes to `SchemaFile` alone. Choosing *which* file to append to is what stays out |
| A nullable `createX` field, or a nil check in the generated Create body | **no.** `createX(...): X!` matches the hand-verified reference schema. The `(nil, nil)` case needs a `BEFORE INSERT` trigger returning NULL; §2.4 names it and the one-word fix |
| `Options.Dir` as the base for `ModelDir`/`SchemaFile`/`ResolverFile` | **done.** `Dir` is the consumer module's root and the other three are relative to it. The old contract — `Dir` moving only the gqlgen subprocess — was four coupled knobs where one does: setting `Dir` alone had gqlgen rewrite one module's generated code while luimagen read and appended to another's, and `cmd/luimagen` did not expose `-dir` at all. An absolute path is still left alone |
| Exporting the individual stages for partial pipelines | **no, not yet.** `Generate` is the only exported symbol |

## 5 · Conventions

- **The three exported symbols carry NatSpec-style doc comments** (`@notice`/`@dev`/`@param`/
  `@return`), per `CLAUDE.md`; `revive`'s `exported` rule checks them. Everything in
  `gen.go` and `patch.go` stays unexported — §4's last row is why — and those two files are named
  in `CLAUDE.md`'s exemption list: they are one internal pipeline, and their reasoning is carried as
  prose there and in §2 rather than split across thirty tag blocks. The *generated* model does carry
  a NatSpec block, for the reason in §2.1.
- **Tests are split deliberately.** The unexported logic (`tableFromFields`, `sdl`, `modelSource`,
  `planSDL`, `writeModel`, `patchSource`, `checkComplete`, `inputFieldNames`) is tested white-box
  in `luimagen/internal_test.go` — `t.TempDir` for the stages that touch disk, no gqlgen; the
  exported surface is exercised black-box from `tests/`, the same consumer view every other
  package's tests use. That split is also why `ExampleGenerate` carries no `Output:` comment: a
  real `Generate` round trip needs a gqlgen module on disk, which an Example cannot guarantee,
  and an Example without an `Output:` block is compiled but never executed — so it pins the call
  shape at compile time without side effects. It does *not* show up under `luimagen.Generate` on
  pkg.go.dev: godoc binds an Example by directory, and `tests/` is `package tests`, the same
  trade-off `CLAUDE.md` records for every other package's examples.
- **No new dependency.** luimagen imports `github.com/vektah/gqlparser/v2` — already direct, through
  `luimaerr` — and otherwise the standard library. Nothing is added to `go.mod`.
- **`make check` stays green.** Both packages are inside the root module, so the repo-wide
  build/vet/lint/gofmt cover them — plus `make luimagen-roundtrip`, which is the only check that
  reaches the gqlgen half; see §6.

## 6 · Verification

Hermetic first: `go test ./luimagen/...`, `go vet ./...`, `gofmt -l .`, `golangci-lint run`.

Then **`make luimagen-roundtrip`**, and it is the load-bearing one. `internal_test.go` is
deliberately no-exec, so `runGqlgenGenerate`, `inputFieldNames` against a real `models_gen.go` and
`patchStubs` against real gqlgen output are covered by nothing else — which is exactly how two
naming bugs sat under a green suite. The target copies the repo to a scratch directory (never the
committed example: `Generate` rewrites the schema, the model directory and the resolver file in
place), scaffolds a throwaway type, and then `go build`s the result:

```sh
go run github.com/ulas96/luima/cmd/luimagen -type ApiKey -table api_keys \
  -field ApiKeyID:string:pk -field OwnerId:string -field 'Scopes:[]string'
```

`ApiKey` rather than `User` on purpose. It exercises `extend type Query` (the quickstart already
declares `Query` and `Mutation`), and it is the drift case: gqlgen renames `ApiKeyInput` to
`APIKeyInput` and `OwnerId` to `OwnerID` through `templates.ToGo`, so a stage that predicts a name
instead of reading it fails here and only here. `go build` is what proves the spliced bodies and the
rewritten import block compile — `format.Source` does not typecheck, so nothing upstream can. Quote
a slice type (`'Scopes:[]string'`): an unquoted `[]string` is a glob in zsh. The same script runs as
a step of the `example` CI job, the one place gqlgen is available as a `go tool`.

For a deeper pass, do it by hand against `User` with `-table app_users` and compare the three
artifacts to the committed quickstart files — same table name, columns and tags, behaviourally the
same five resolver bodies (the quickstart routes its literal through a hand-written `newUser`
helper where luimagen inlines it, so they are equivalent, not textually equal) — and call
`luimagen.Generate` directly once to prove the library entry point works without the binary.
