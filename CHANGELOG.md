# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

While the major version is `0`, the public API may change in a minor release. Every such change
will be listed here under **Changed** with the migration in one line.

## [Unreleased]

luima runs on bun now, so nothing that names a go-pg type compiles until it is re-typed. The
compiler finds every one of those, and the migration list below is the mapping. It does not find the
two that matter most: a model's table name moves from an unexported `tableName` field to an embedded
`bun.BaseModel`, which compiles and then queries a different table, and a zero-valued field is now
stored as its zero value where go-pg sent `DEFAULT` or `NULL`. Read both before deploying this
against a table that already holds rows.

### Changed

- **luima runs on bun `v1.2.18` — `bun`, `dialect/pgdialect` and `driver/pgdriver` — where every
  release through `0.5.0` ran on go-pg `v10`.** luima's signatures named go-pg's types, so this
  breaks the public API, and the break is kept to exactly what the switch forces: every function
  keeps its name, its parameters' meaning and its absence contract, `server/` is unchanged, and
  `crud` still classifies 23505 as `CONFLICT` and a missing row as `NOT_FOUND` or `(nil, nil)`.

  pgdriver rather than pgx, for the least drift: the same TLS posture when `sslmode` is absent, the
  same `connect_timeout` and `application_name`, and an error value that still carries the Postgres
  error fields `SQLState` reads. Its costs are the runtime notes below, and they are real.

  **Migration**, one line per break:

  - `*pg.DB` → `*bun.DB` — what `Connect` and `ConnectWith` return, and what your `Resolver` holds.
  - `orm.DB` → `bun.IDB` in all five CRUD helpers. `*bun.DB`, `bun.Conn` and `bun.Tx` satisfy it,
    and the last two have value receivers, so pass `tx`, not `&tx`.
  - One option-closure type per statement, in place of the single `func(*orm.Query) *orm.Query`:
    `func(*bun.SelectQuery) *bun.SelectQuery` for `Get` and `List`, `*bun.InsertQuery` for
    `Create`, `*bun.UpdateQuery` for `Update`, `*bun.DeleteQuery` for `Delete`. bun builds each
    statement with its own type, and the interface three of them share, `bun.QueryBuilder`, reaches
    only the WHERE clause — so a predicate wanted by more than one is written once as a
    `func(bun.QueryBuilder) bun.QueryBuilder` and passed through `ApplyQueryBuilder`, which the
    select, update and delete queries all have. One option type for all five would have to be a
    wrapper over bun, which is the thing this package refuses to be.
  - `q.OnConflict("DO NOTHING")` → `q.On("CONFLICT DO NOTHING")`;
    `db.RunInTransaction(ctx, func(tx *pg.Tx) error)` →
    `db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error)`; `db.AddQueryHook(h)` →
    `db.WithQueryHook(h)`, which returns a copy of the handle over the same pool rather than
    mutating it (`AddQueryHook` still exists, marked deprecated upstream); and `pg.Ident`/`pg.Safe`
    → `bun.Ident`/`bun.Safe`, the identifier and vetted-fragment wrappers `docs/gotchas.md` #31
    tells you to reach for.
  - `q.UpdateNotZero()` is the one that is **not** a rename. go-pg's was a terminal executor —
    `UpdateNotZero(scan ...interface{}) (Result, error)` — so no `crud.Update` option can ever have
    held it; bun's `q.OmitZero()` returns the query, so one now can, and raw go-pg code that swaps
    one spelling for the other builds a statement nobody runs. `Update` still does not apply it
    itself: skipping zero-valued fields means an empty string cannot clear a text column, and
    `false` or `0` cannot be written at all — a data-retention bug wearing a partial-update
    feature's clothes. `q.Column(…)` is the escape hatch that names what it narrows.
  - `pg.ErrNoRows` → `sql.ErrNoRows`, and only for a single-row `Scan`. `Create` and `Update` `Exec`
    their `RETURNING *` with no destination, and `(*baseQuery)._scan` raises the error only when it
    has one — so an insert `ON CONFLICT DO NOTHING` or a `BEFORE INSERT` trigger suppressed, and an
    update that matched no row, are now `RowsAffected() == 0` with a nil error. luima's helpers keep
    their contract, `(nil, nil)` and `NOT_FOUND`; a hand-written query that branches on `ErrNoRows`
    after an insert or an update compiles after the rename and then never fires again.
    `docs/gotchas.md` #10.
  - `pg:"…"` struct tags → `bun:"…"`. Column names are unaffected: bun's `internal.Underscore` is
    byte-identical to go-pg's, so an untagged field still derives the same column. `,pk` and
    `,array` carry over as written. `,use_zero` has no bun spelling, because it is bun's behaviour
    now — leave it in place and bun prints one `has unknown tag option: "use_zero"` WARN per field
    the first time it builds that model's table. `discard_unknown_columns` on the table tag has no
    bun spelling either, and only WARNs: bun reads that switch from the `*bun.DB`, and
    `ConnectWith` now builds every handle with `bun.WithDiscardUnknownColumns()`, so a column your
    model does not declare is ignored for every model. Without it, `RETURNING *` in `Create` and
    `Update` failed the scan with `bun: T does not have column` after the statement had committed —
    a stored row answered as an error, and the client's retry answered `CONFLICT`.
  - **`tableName struct{}` `pg:"app_users"` → an embedded `bun.BaseModel` tagged
    `bun:"table:app_users"`. This is the one that fails quietly.** bun reads the table name from
    the embedded `BaseModel` and from nowhere else, and it skips every unexported field that is not
    embedded without a word (`(*schema.Table).processFields`) — so a model that keeps the go-pg
    spelling still compiles, and every statement then goes to the pluralized type name: `users`,
    not `app_users`. Where no such table exists the helpers fail with an error `PresentError`
    redacts; where one does exist, they succeed against the wrong table. `docs/gotchas.md` #38 has
    the one-line check that needs no database.
  - `ConnectWith`'s tune func and `StatementTimeout`'s return value are now `func(*pgdriver.Config)`
    in place of `func(*pg.Options)`. `StatementTimeout` still sends the same `SET`: pgdriver has no
    `OnConnect` hook, so it merges one key into `Config.ConnParams`, which `newConn` sends as
    `SET statement_timeout TO <ms>` on every connection it opens.
  - `HealthCheck: db.Ping` → `HealthCheck: db.PingContext`. `*bun.DB` embeds `*sql.DB` through its
    state struct, and its `Ping` takes no context, so the old spelling no longer compiles — which is
    the good outcome: it would have run on `context.Background()` and ignored the 2s deadline
    `Health` gives the probe.
  - `luimaerr.SQLState` reads `pgdriver.Error`, and that type is a struct **value** — `readError`
    builds it as `Error{m: m}`, nothing takes its address, and every method has a value receiver.
    `errors.AsType[*pgdriver.Error]` (or `var e *pgdriver.Error; errors.As(err, &e)`) compiles and
    never matches, so every SQLSTATE branch behind it is dead code that reads as correct. This is
    the inverse of the trap `pg.Error` set, where the same `*` failed to compile, and it is the
    spelling someone arriving from pgx writes, because `*pgconn.PgError` is a pointer.
  - `luimagen`'s `checkTag` now rejects `,` and `:` in `Options.Table` and `Field.Column`, where
    before it checked only what `%q` escapes. Both bytes are separators in bun's tag grammar
    (`internal/tagparser`), and a name holding one is read back short with nothing but a `WARN`
    about the option left over: `bun:"pro,jects,array"` binds the column `pro`, and
    `bun:"table:ten,ant"` selects `FROM "ten"`. A `:` in the tag's name half opens an option
    instead, so `Tag.Name` comes back empty and `(*schema.Table).newField` binds its own
    `Underscore(sf.Name)` — the same one-line WARN, and nothing at all about the name substituted
    for the one you wrote. A schema-qualified `tenant.users` is still accepted and still renders
    `"tenant"."users"`.
  - `luimagen`'s generated model changes shape with everything else: the file now opens with
    `import "github.com/uptrace/bun"` and embeds `bun.BaseModel` tagged `bun:"table:…"` where it
    used to write an unexported `tableName struct{}`, and the `List` resolver it patches in takes a
    `func(q *bun.SelectQuery) *bun.SelectQuery`. Regenerate and the files differ; `writeModel` still
    refuses to overwrite a model you have since hand-edited.
  - Your own `go.mod` swaps `github.com/go-pg/pg/v10` for `github.com/uptrace/bun`, plus
    `driver/pgdriver` wherever you name `ConnectWith`'s tune type. The graph gains
    `go.opentelemetry.io/otel` indirectly as well, because pgdriver sets `EnableTracing: true` in
    `newDefaultConfig` and opens a span per statement — a no-op until you install a provider, and
    `c.EnableTracing = false` in a `ConnectWith` tune if you would rather it were not there. It is
    not a DSN parameter: `?tracing=false` would become a `SET` and fail 42704.

- **Stored data changes shape: bun writes a zero-valued field as that zero value.** go-pg sent
  `DEFAULT` on insert and `NULL` on update for anything zero unless the field was tagged
  `,use_zero`; bun sends the empty string, `0`, `FALSE` or the zero time, and reaches for `DEFAULT`
  only when the field is a nil pointer, or is zero *and* tagged `,nullzero` or `default:`
  (`(*InsertQuery).marshalsToDefault`, `(*schema.Field).appendValue`). On an UPDATE that `DEFAULT`
  is the column's own default, so it is `NULL` only for a column that has none. Four consequences
  worth reading before you deploy this against an existing table — `docs/gotchas.md` #27 has the
  tag-by-tag table:

  - A column with a `DEFAULT now()` that your input mapper leaves zero is now stored as the zero
    time rather than filled in by Postgres. Tag it `,nullzero`, or make the field a pointer.
  - An identity or serial key wants `,autoincrement`; `,identity` alone still sends `0`, so the
    first `Create` stores `0` and the second fails 23505 — reaching the client as a `CONFLICT` over
    a key it never chose. A `GENERATED ALWAYS AS IDENTITY` key does not even get that far: Postgres
    accepts only `DEFAULT` for one, so the literal `0` is refused **428C9** and every `Create`
    fails.
  - **A generated column has the same shape and is easier to miss**, because nothing about it looks
    like a key: Postgres takes `DEFAULT` there and nothing else, so a field bun sends as `''` or `0`
    refuses every insert with 428C9 too. Tag it `,scanonly`, which keeps it out of the INSERT while
    `RETURNING *` still reads the computed value back. Both 428C9s reach the client as
    `internal server error`, because `PresentError` redacts them — where go-pg's zero → `DEFAULT`
    made both table shapes work without a tag.
  - **A nil `,array` slice is written as a literal `NULL`, on insert and on update alike**
    (pgdialect's `appendStringSlice`), and a `NULL` is a value, so the column's `DEFAULT` never
    applies: a nil `[]string` into `projects text[] not null default '{}'` is now **23502**, which
    `PresentError` redacts to `internal server error`. This is not only models built in Go. gqlgen
    hands a resolver a nil slice for a **nullable** list — a `[String!]` argument that is null or
    omitted generates `if v == nil { return nil, nil }` ahead of the loop (`codegen/type.gotpl`) —
    and only a non-null `[String!]!` is guaranteed non-nil, because there the generated
    unmarshaller is `make([]string, len(vSlice))`. Send `[]string{}`, which still writes `'{}'` and
    still clears the column, or tag the field `,nullzero`. `docs/gotchas.md` #39.

  `,nullzero` is how to ask for go-pg's old shape, field by field. Adding it wholesale is not
  recommended and is not what luima's own models do: it makes an empty string, a `0` and a `false`
  unwritable, which is a silent data-retention bug rather than a partial-update feature.

- **Query cancellation reaches only the client now, so `statement_timeout` is the only bound
  Postgres enforces.** go-pg answered a cancelled context by dialing a second connection and
  sending a Postgres CancelRequest. pgdriver sends none — it reads the backend key at startup and
  never uses it — so `RequestTimeout`, or any context deadline, becomes a socket deadline
  (`(*Conn).deadline`), the read fails, pgdriver closes the connection, and the statement runs on
  in Postgres until it finishes, holding its backend and its locks. `luima.StatementTimeout(d)`, or
  `?statement_timeout=` in the DSN, is what the server enforces whether or not the client is still
  there. When it fires Postgres answers 57014 and pgdriver returns that error unchanged, so
  `SQLState` still reads it — but the pooled connection may not survive: `checkBadConn` counts
  57014 as a bad connection, and it runs in `ExecContext` and `QueryContext`, so a 57014 raised
  before a query's row description costs the pool that connection while one raised during row
  iteration does not. `docs/gotchas.md` #40, and `SECURITY.md` for what a deadline still buys.

- **`Connect` keeps pgdriver's socket timeouts — `ReadTimeout` 10s, `WriteTimeout` 5s
  (`newDefaultConfig`) — and they are a ceiling on every statement.** They are the only bound on
  the I/O pgdriver does without the caller's context: the rows after a query's row description,
  the drain in `rows.Close`, COMMIT and ROLLBACK, and the startup of every connection
  `database/sql` dials in its background opener. Zeroing them would leave all of that unbounded
  against a server that stops answering. What they cost: a statement whose reply takes longer than
  10s fails client-side with an i/o timeout whatever `RequestTimeout` allows, and a
  `statement_timeout` at or above 10s means the caller gets that i/o timeout with no SQLSTATE
  instead of a readable 57014. Raise the read bound with `?read_timeout=60s` or a `ConnectWith`
  tune setting `c.ReadTimeout`, and keep `statement_timeout` under both it and `RequestTimeout` —
  that is why the quickstart's own `StatementTimeout` drops from 10s to 5s in this release.

- **The DSN accepts far more than it used to, and rejects far less.** pgdriver's `parseDSN` reads
  `sslmode`, `sslrootcert`, `sslcert`, `sslkey`, `application_name`, `connect_timeout`,
  `dial_timeout`, `timeout`, `read_timeout`, `write_timeout` and `host` — `sslcert` and `sslkey`
  only alongside `sslmode` or `sslrootcert`, and only as a pair; **every other parameter
  becomes `SET <name> TO <value>` on each new connection**, where `pg.ParseURL` knew three and
  made anything else a parse error. So `?statement_timeout=5s` now works — and a misspelled name
  is no longer caught while parsing: it fails `Connect`'s boot round trip instead, as 42704 for an
  unknown setting, or 42601 for a key such as `a-b`, or never at all for a dotted name such as
  `app.x`, which Postgres takes as a custom setting. Two names are refused while parsing, in any
  case: `?password=` and `?sslpassword=`, which libpq reads and pgdriver does not — sent as a `SET`,
  Postgres rejects the statement and, at the default `log_min_error_statement`, logs it with the
  value in it. A timeout written as a plain integer `0` or less still means the default, as
  `pg.ParseURL` read `?connect_timeout=0`: pgdriver's `(*queryOptions).duration` answers `-1`, a
  deadline already passed, and `Connect` puts pgdriver's default back. `$PGPASSWORD` still fills a
  DSN with no password — `Connect` reads it, since pgdriver does not — but there is no further
  fallback to `postgres`, as go-pg had. One more silence to know about: a DSN with no database name
  connects to `$PGDATABASE`, then `postgres`, where the old parser refused it. Set `application_name` and check `pg_stat_activity` if you want to see which
  database and user you actually got. `docs/deployment.md` walks the whole DSN.

- **`?sslmode=verify-ca` still verifies the host name, though pgdriver's does not.** go-pg treated
  `verify-ca` as `verify-full`; pgdriver implements libpq's meaning, a `VerifyPeerCertificate` that
  checks the chain and deliberately not the DNS name, and uses it for `?sslmode=require` with an
  `sslrootcert` too. `Connect` hands both back to crypto/tls's full check, so a `verify-ca` DSN
  carried over is not quietly weaker, and `require` with an `sslrootcert` — which `0.5.0` refused —
  verifies as much. A chain-only check is a `VerifyConnection` of your own in a `ConnectWith` tune.
  `docs/deployment.md` has the mode table.

- **The pool is `database/sql`'s, and `Connect` sizes it.** `database/sql` defaults to unlimited
  open connections and two idle (`defaultMaxIdleConns`), which turns a burst of requests into a
  burst of connections and, past `max_connections`, into 53300 for every client of that server.
  `Connect` sets `SetMaxOpenConns` and `SetMaxIdleConns` to `10 * runtime.NumCPU()` and
  `SetConnMaxIdleTime` to 5 minutes — the sizing `0.5.0` ran with, its driver's defaults. Resize it
  on the returned handle: `*bun.DB` embeds `*sql.DB` through its state struct, so those setters and
  `Stats` are promoted to it. There is no pool-wait timeout any more; the wait is bounded by the
  caller's context.

- **`db.Connect` no longer panics, and refuses a DSN whose user info ends early.**
  `pgdriver.WithDSN` panics on any parse failure, and `parseDSN` builds its options as it goes, so
  `postgres://:secret@host/db` and `postgres://@host/db` panic out of `WithUser` — a panic skips
  the caller's error handling and prints the unredacted parse error on the way out. Both now come
  back as an error. Separately, a DSN whose user info an unescaped `/`, `?` or `#` cuts short is
  refused before anything dials, because pgdriver would otherwise dial part of the password and
  name it in the dial error: `postgres://app:p@ss/x@db/app` parses as the user `app` with the
  password `p` against the host `ss`, and `postgres://127.0.0.1:2024#x@db/app` as no user info at
  all against `127.0.0.1:2024`, where `2024` is where the password started. `0.5.0` happened to
  refuse the `?` and `#` shapes, because they leave the path empty and `pg.ParseURL` answered
  `database name not provided`; it accepted the `/` shape, and pgdriver defaults a missing database
  anyway. **Migration:** percent-encode `/ ? # %` in the user or password, and write any `@` past
  the host as `%40`.

### Removed

- **luima's TLS `ServerName` fill.** `db.Connect` used to set `TLSConfig.ServerName` from the URL's
  host for `sslmode=verify-full`, because go-pg left it empty and `crypto/tls` verifies no host
  name without it. pgdriver sets it itself, from the authority with its port stripped, for
  `verify-full`, `verify-ca` and `require` alike — so the fill is now dead code over a value
  pgdriver already wrote. `TestConnectVerifyFull` stays, as a regression guard on that. One
  case the fill never had to cover, because `pg.ParseURL` refused every parameter but its three: a
  host given only as `?host=`, which pgdriver reads and which leaves the URL's authority — and so
  `ServerName` — empty, making `crypto/tls` refuse every `verify-full` handshake. Set
  `c.TLSConfig.ServerName` in a `ConnectWith` tune for that one.

### Fixed

- **`db.StatementTimeout` with a negative duration disables the bound, as its doc has said since
  `0.4.0`.** The duration reached Postgres unclamped — `-1s` as `SET statement_timeout = -1000` —
  so the `SET` failed on every new pooled connection, `ConnectWith`'s own boot ping first. It was
  `pg.Options.OnConnect` that ran it in `0.5.0`, where this was measured, and it is
  `Config.ConnParams` under pgdriver; the bug is the same either way. Measured against `0.5.0`, in
  go-pg's rendering: `ping: ERROR #22023 -1000 ms is outside the valid range for parameter
  "statement_timeout" (0 ms .. 2147483647 ms)`. A negative duration, documented as disabling the
  bound, produced a `ConnectWith` that could only return an error; zero, the other documented
  spelling, always worked, as did a negative duration shorter than a millisecond, which truncates
  to zero. A negative duration is now clamped to `0`, which is Postgres for no timeout, and
  `luima.StatementTimeout` picks the fix up through its wrapper. Nothing can have depended on the
  old behaviour, since no connection configured that way could run a query. Only the lower end is
  clamped: a duration above `2147483647ms`, about 24.8 days, is still refused the same way — as
  pgdriver renders it now, `ERROR: … (SQLSTATE=22023)`.

### Security

- **`luimaerr.PresentError` now redacts what resolvers report — it never did.** gqlgen does not
  hand the presenter a resolver's error as returned: `graphql.ResolveField` passes it through
  `graphql.AddFieldLocationToError`, `graphql.AddError` passes it through `graphql.ErrorOnPath`,
  and both wrap an error that holds no `*gqlerror.Error` in one whose message is `err.Error()`
  verbatim. The pass-through meant for gqlgen's own errors matched any top-level
  `*gqlerror.Error`, so in every release so far a bare driver error — `relation "app_users" does
  not exist`, a constraint name — reached the client as written, with no `extensions.code` and no
  log line, while every test that called `PresentError` directly passed. A `*gqlerror.Error` now
  passes through only if it wraps no other error and was reported outside field resolution, which
  is how gqlgen reports parse, validation, variable and complexity errors and a malformed body, and
  how luima reports its depth limit. Anything else that is not a `*CustomError` is logged and
  answered `internal server error` with `INTERNAL_SERVER_ERROR`, keeping its `path` and
  `locations` when they belong to the request being answered.

  Clients now get that answer, where they used to get the error's own text, for: an error returned
  by a resolver, a field directive or `AroundFields` middleware, or by an argument directive;
  anything sent with `graphql.AddError` or `graphql.AddErrorf` while a field resolves; every
  `*gqlerror.Error` reported while a field resolves, with or without a cause, its own `extensions`
  included — `gqlerror.Errorf(...)`, one built by hand, a `gqlerror.List` decoded from an upstream
  GraphQL response; a resolver panic, whether gqlgen's default recover function answers it (its
  `internal system error` carried no code) or one installed with `SetRecoverFunc` does; gqlgen's
  null violations (`must not be null`, `the requested element is null which the schema does not
  allow`); `introspection disabled`, for a `__schema` or `__type` query when
  `DisableIntrospection` is set; a client's bad value for a custom scalar, gqlgen's built-in `Time`
  included; and a variable default that does not parse for a custom scalar, which is still answered
  HTTP 422 but with `INTERNAL_SERVER_ERROR` in place of `GRAPHQL_VALIDATION_FAILED`. Each of these
  also writes a `resolver error` log line, so any client can produce one at will — a `__schema`
  query with introspection disabled is enough — and an alert keyed on `INTERNAL_SERVER_ERROR` sees
  it.

  **Migration:** send a message meant for the client as a `*luimaerr.CustomError`, whose `Code`
  becomes `extensions.code`. It is heard from a resolver, a directive, a recover function or an
  `UnmarshalGQL` alike, through gqlgen's wrapper, so a scalar that should explain its format to the
  client has to be your own rather than gqlgen's built-in binding. Do not share one
  `*gqlerror.Error` value between requests: gqlgen writes the first request's path and locations
  into it.

- **`db.Connect`'s parse error could carry the start of the password.** `0.2.0` dropped the raw DSN
  from that error by keeping only the `*url.Error`'s `Op` and `Err`, and the narrower leak survived
  every release through `0.5.0`: `Err` quotes the text `url.Parse` rejected, and an unescaped `/`,
  `?` or `#` in a password ends the URL's authority early, so the port it names is where the
  password started — `postgres://app:Xk9/Q@db/app` came back as
  `parse database url: parse: invalid port ":Xk9" after host`. The call site logs it; the
  quickstart calls `log.Fatal` on it, which writes a live credential to stderr and into whatever
  ships stderr onward. Every error `Connect` now returns is scrubbed instead. The `*url.Error`
  branch keeps `Op` and replaces every quoted fragment of `Err` — `%q`, and `strconv.Quote` in
  `EscapeError`, `InvalidHostError` and netip's `ParseAddr` error — then names the usual cause, so
  the diagnosis survives without the text. Every other error has the DSN replaced wherever it
  occurs, and is withheld whole if the decoded password still occurs in what is left, because a
  marker at each occurrence of a short password spells the password out. Neither branch wraps the
  original with `%w`, since anything walking the chain would print what was removed.
  `TestConnectRedactsDSN` and `TestConnectNeverPanicsOrLeaks` pin it, the second over thirteen
  malformed DSNs plus a password short enough to occur in pgdriver's own wording.

## [0.5.0] — 2026-08-27

One call now scaffolds a table's CRUD layer. `luimagen.Generate` — and the `cmd/luimagen` binary
over it — writes the Go model struct, appends the matching SDL, runs the consumer's own
`go tool gqlgen generate`, and fills in the five resolver stubs it produces. It is a separate
package that nothing in the library imports, so `github.com/ulas96/luima` gains no surface from it
and the "no scaffolding CLI" line below still describes the library consumers import. The only
thing this release asks of an existing consumer is a Go 1.27 toolchain.

### Added

- **`luimagen`** (`github.com/ulas96/luima/luimagen`, plus a `cmd/luimagen` CLI) — generates a
  table's CRUD layer from its fields in one call: `luimagen.Generate(luimagen.Options{Type:
  "User", Fields: [...]})` **writes the Go model struct** (not just SDL and resolvers — a plain
  `Field{Name, Type, PK}` slice is the only input, no hand-written struct required), appends the
  matching GraphQL SDL to `schema.graphqls`, runs the consumer's own `go tool gqlgen generate`,
  and fills in the five resulting resolver stubs with calls to
  `luima.Get`/`List`/`Create`/`Update`/`Delete`. It is a separate package, not re-exported through
  `luima.go` — importing `github.com/ulas96/luima` gains no new surface. luimagen never touches
  the database itself: the table is expected to already exist, created however the consumer
  already creates tables. See `docs/luimagen.md` for the design and its constraints.

  Validation happens before any file is written — every check is read-only and runs ahead of the
  first `os.WriteFile`, so a rejected call leaves nothing on disk. `Options.Type` and every
  `Field.Name` must be an exported Go identifier, no two fields may collide on the derived column
  name or the derived GraphQL field name (`URLValue` and `UrlValue` both become `url_value`; `ID`
  and `Id` both become `id`), and the mapped Go types are exactly the ones
  that round-trip through gqlgen's default bindings: an array-typed PK, a PK-only table, and
  `int64`/`uint*`/`float32` (gqlgen turns GraphQL `Int` into Go `int` and `Float` into
  `float64`, so other widths generate resolver code that does not compile — use `string`/`int`/
  `float64`/`bool`) are all errors. A `_` anywhere in `Options.Type` or a `Field.Name` is an error
  too, and it is the one that does not look wrong: `_` is a legal Go identifier rune, but
  `templates.ToGo` treats it as a word delimiter and drops it, so `Owner_Name` comes back as
  `OwnerName` and the generated resolver references a field that does not exist. Both must also be
  ASCII, which a Go identifier need not be: a GraphQL `Name` is `/[_A-Za-z][_0-9A-Za-z]*/`, so an
  accented name is a legal struct field whose derived SDL field cannot parse. `Options.Table` and
  `Field.Column` are checked against everything `%q` escapes — a backslash or a tab passes a
  two-character check and lands in the raw-string `pg:"…"` tag as a literal escape sequence go-pg
  reads as part of the name. `snakeCase` matches go-pg's own column-naming boundary for initialisms
  (`URLValue` → `url_value`, not `urlvalue`), and **`Field.Column` overrides it** (`-field
  Name:Type[:pk][:column=<sql name>]`) for the case that rule cannot derive: a run of capitals gets
  no separator at all, so `URLID` becomes `urlid` where the table almost certainly says `url_id`.
  luimagen does not create the table, so that mismatch compiles, vets and lints clean and fails on
  the first query — `Options.Table` was already the same escape hatch one level up. Non-string primary keys generate the create/update label through
  `fmt.Sprintf("user %v", id)` — the type name is baked into the format string — instead of a
  string concat, and the generated `List` caps at 100 rows with a comment saying so: luima ships
  no pagination.

  The last check is the composed schema itself: before anything is written, the existing schema
  sources plus the fragment about to be appended are loaded through gqlparser — the
  same parser gqlgen runs — so a collision the type-name guard cannot see is an error rather than
  a half-written tree. A hand-declared `Query.settings`, a pre-existing `input SettingInput`, and
  two types whose naive `+"s"` plurals meet (`User` and `Users` both claiming `Query.users`) are
  all rejected up front. A set that does not parse *before* the fragment is added is left alone:
  `gqlgen.yml` may glob files luimagen never reads, so an unresolved reference in that partial
  view is not luimagen's to report.

  **luimagen reads the names gqlgen generated rather than predicting them.** gqlgen runs SDL names
  through `templates.ToGo`, which re-capitalizes the common initialisms, so `Field{Name: "OwnerId"}`
  comes back as `UserInput.OwnerID` and `Type: "URL"` comes back as the resolver method `URl`.
  Resolver methods are matched case-insensitively (for a delimiter-free name `ToGo` only re-cases,
  never changes letters, so this is exact), and so is the lookup for the generated input struct
  itself — `Type: "ApiKey"` declares SDL `input ApiKeyInput` and gets back `type APIKeyInput`.
  Generated input field names are read out of `<ModelDir>/*.go`. Neither costs a dependency, and
  neither drifts when gqlgen's initialism list changes. The patched signature must have exactly the
  parameter count luimagen's SDL generates: too many means the field carries an argument luimagen
  never declared, and splicing anyway would discard a value the client sent. After the splice, all
  five methods must be present **and none of them may still panic** — a partially patched file
  compiles, so a surviving `panic` would otherwise only fire at query time. That second half
  covers the body luimagen deliberately does *not* touch: a hand-written
  `panic("TODO: needs an ownership predicate")` is left alone, which is right, but finishing the
  run silently would have the CLI report all five as filled over a live panic. The patched file is
  written *before* that error is returned: every splice the patcher made is correct whatever the
  completeness check then finds, and discarding them would leave the caller holding the model file,
  the appended SDL and a regenerated module with none of the bodies filled — a state no re-run can
  redo, since the duplicate-type guard now rejects the type.

  The patched resolver file's import declarations are merged into one canonical block — stdlib
  group, blank line, third-party group, sorted — from the post-splice AST, and every `fmt` import is
  pruned by whether its *binding* is still used rather than by text matching, so an aliased
  `f "fmt"` the splice made dead is dropped instead of surviving unused beside a freshly added
  `"fmt"`. Adding an import is keyed on the same thing: an aliased `l "github.com/ulas96/luima"`
  already satisfies "this path is imported" while binding no name a spliced `luima.Get` can use,
  so the check is for an *unaliased* import — two imports of one path is legal Go, a body naming
  an unbound package is not. An add also requires the *name* to be free: a resolver file that binds
  `luima` to a fork gets an error naming the conflict rather than a second unaliased import and
  `luima redeclared in this block`, which `format.Source` cannot catch because it does not typecheck. A body that is not gqlgen's own
  `panic(fmt.Errorf("not implemented: …"))` stub is left alone, hand-written placeholders included.

  The generated model file carries a NatSpec doc comment, because it lands in the consumer's module
  where `revive`'s `exported` rule would flag a bare `type User struct` as an error on code they did
  not write; `writeModel` creates `ModelDir` if it does not exist yet, and takes the file's
  `package` clause from the `.go` files already there rather than from `Options.ModelPkg`, which is
  the identifier the resolver bodies prefix and differs whenever gqlgen aliased the import. The
  declare-vs-extend probe and the duplicate guard are answered from `parser.ParseSchema` — so a
  `type Query` inside a `"""` description or a `#` comment is not a declaration, and a name taken by
  an `input` or `scalar` counts too — across every file beside `Options.SchemaFile` sharing its
  extension, which is what gqlparser reads and what the documented layout globs. `appendSDL` still
  writes to `Options.SchemaFile` alone.

  `Options.Dir` is the consumer module's root: `go tool gqlgen generate` runs there, and
  `ModelDir`/`SchemaFile`/`ResolverFile` are relative to it unless absolute, and `ResolverFile`
  itself defaults to `SchemaFile` with `.resolvers.go` for its extension — where gqlgen's
  `resolver.layout: follow-schema` puts the stubs, so moving `-schema` alone does not leave luimagen
  patching a file with none of the five methods in it. `cmd/luimagen` exposes `Dir` as `-dir`,
  alongside `-type`, `-field`, `-table`, `-model-dir`, `-schema`, `-resolvers` and `-model-pkg`.

  `make luimagen-roundtrip` (also a step of the `example` CI job) runs the whole pipeline against a
  scratch copy of `examples/quickstart` and builds the result. It is the only check that reaches the
  gqlgen half: `luimagen/internal_test.go` is deliberately no-exec, and `format.Source` does not
  typecheck, so `go build` on real generated code is the only thing that proves a spliced body and a
  rewritten import block compile.

### Changed

- **luima now requires a Go 1.27 toolchain** (was 1.25). Both `go.mod` files declare `go 1.27.0`.

  What this costs a consumer, stated plainly: your own module's `go` directive does **not** have to
  move — a module still declaring `go 1.25.0` compiles against this release unchanged. What must
  move is the toolchain. With `GOTOOLCHAIN=auto` (the default) that is an automatic download and
  you will not notice it; pin `GOTOOLCHAIN` to an older release and the build stops with
  `go: go.mod requires go >= 1.27.0`. If that pin is not yours to change, stay on 0.4.0.

  No exported symbol changed, no dependency was added, removed or upgraded. Internally `errors.As`
  became `errors.AsType` in the three places that only ever needed the matched value —
  `db.Connect`'s URL-error branch, `luimaerr.PresentError`'s `*CustomError` branch, and
  `luimaerr.SQLState`. Identical chain-walking semantics, one line less each.
  `PresentError`'s `*gqlerror.Error` branch is deliberately **not** among them: it stays a bare
  type assertion, because that is the redaction contract — an `errors.As`-family call there would
  walk the chain and let any resolver ship server internals to the client by wrapping a
  `*gqlerror.Error`.

## [0.4.0] — 2026-08-12

A consumer can now write a complete, production-shaped luima server without importing Fiber.
`examples/quickstart/main.go` is the proof: it imports `github.com/ulas96/luima` and nothing from
`github.com/gofiber`, where before it named a Fiber type for timeouts, for a rate limiter and for
graceful shutdown. Everything below is additive except the two entries under **Changed**.

### Added

- **`Run(ctx, addr, cfg)`** — build, listen, and block until `ctx` is done, then drain in-flight
  requests and return. The whole server in one call, naming no Fiber type; a signature assertion in
  `tests/luima_test.go` makes the compiler enforce that rather than review.

  It replaces `New` + `app.Listen(addr, fiber.ListenConfig{GracefulContext: ctx})`, and not only for
  the import. Fiber's own graceful path throws the shutdown error away — it hands it to the
  `OnPostShutdown` hook while `Listen` returns nil regardless, so a server that force-closed live
  connections because the drain timed out is indistinguishable from a clean exit and the process
  exits 0. `Run` returns it. It also removes the ordering trap that kept the quickstart off `New`:
  `New` mounts before it returns, so a later `app.Use` lands behind `/graphql` and never runs. With
  `Run` there is no app to register on and `HTTPMiddleware` is the only seam — which is where
  middleware belonged anyway, because it is the only layer that sees the resolvers' context.

  The drain window is 10s, matching Fiber's own `ListenConfig.ShutdownTimeout` default. `Run` binds
  dual-stack `tcp`, where `app.Listen` defaults to `tcp4`.

- **`CORS(CORSConfig{...})`** — cross-origin access as `func(http.Handler) http.Handler`, for
  `Config.HTTPMiddleware`. luima already answers the OPTIONS preflight — `transport.Options` is why
  it is a 200 and not a 405 — but it set no `Access-Control-Allow-Origin`, so the browser refused
  the response anyway and its error named neither field. The misconfiguration was the default.

  Sets `Vary: Origin` on every response it touches, including refusals, which is the reason to
  prefer it over four hand-written headers: the grant is echoed from a request header, and without
  `Vary` a shared cache serves the first caller's grant to the second. Methods are fixed at GET,
  POST and OPTIONS. There is deliberately no `Credentials` knob — the combination that makes a
  wildcard origin dangerous is not representable. Use `rs/cors` in `HTTPMiddleware` if you need it;
  that needs no Fiber import either.

  One wart, documented at the symbol: `HTTPMiddleware` runs inside the adaptor and Fiber's timeout
  middleware wraps outside it, so a request that hits `RequestTimeout` is answered without CORS
  headers and the browser reports a CORS error rather than a timeout.

- **`RateLimit(n, per, key)`** — fixed-window limiter as `func(http.Handler) http.Handler`, 429 with
  `Retry-After` over the limit. This is the bound `ComplexityLimit` cannot supply: row count is not
  an input to the complexity calculation, so an unbounded `{ users { id } }` costs the same as one
  field. Fixed window, not sliding, so the real ceiling is 2n across a boundary. Counters are
  dropped wholesale at each rollover — a per-key map with no eviction would be a memory-exhaustion
  bug inside the feature that exists to prevent one. Per process: two replicas enforce 2n. `key` is
  nil for `r.RemoteAddr`; read a header instead when you are behind a proxy you control, because
  otherwise every caller shares the proxy's single bucket.

- **`Config.Health` and `Config.HealthCheck`** — a liveness path, e.g. `/healthz`. Empty disables
  it. A nil check answers 200 whenever the process is up; a non-nil error is 503 and the error text
  stays server-side. `db.Ping` already has the signature, so the common case is `HealthCheck:
  db.Ping`.

  Registered by `Mount`, so it works on a group and on an app you built yourself, and it is *not*
  wrapped by `HTTPMiddleware` — a rate limiter must not 429 the probe. The check gets a 2s deadline
  of its own and runs on its own goroutine, so a check that never reads its context still answers
  503 rather than hanging; a probe that hangs for 15s reads to a load balancer as a slow server
  rather than a broken one.

- **`db.ConnectWith(url, tune)` and `db.StatementTimeout(d)`** — the `pg.Options` tuning a DSN
  cannot express. `pg.ParseURL` accepts only `sslmode`, `application_name` and `connect_timeout`, so
  reaching anything else meant re-implementing the two things `Connect` does that are easy to lose:
  the TLS `ServerName` fill without which `?sslmode=verify-full` cannot complete a handshake, and
  the bounded boot round trip. `tune` runs after both and before `pg.Connect`.

  `StatementTimeout` is the case worth pre-writing, and the gap `SECURITY.md` already named:
  `RequestTimeout` reaches Postgres as a `CancelRequest`, which is best-effort — go-pg dials a
  second connection to send it and only logs a failure — so a query can outlive its own
  cancellation while holding a pooled connection. `statement_timeout` is enforced by the server
  whether or not the client is still there. A query that exceeds it comes back as SQLSTATE `57014`,
  readable with `luimaerr.SQLState`.

  **`Connect(url)` is unchanged** and is now `ConnectWith(url, nil)` internally, so there is one
  implementation of the TLS fix and the boot ping rather than two.

- **`Config.ReadTimeout` and `Config.WriteTimeout`** — transport deadlines, defaulting to 10s and
  30s. Zero means unset and negative disables, as with `RequestTimeout`. Setting one no longer
  requires naming a Fiber type. `Config.Fiber` still takes precedence where both are set, so an
  existing `Fiber: fiber.Config{ReadTimeout: …}` keeps working unchanged. There is no
  `IdleTimeout`: fasthttp falls back to `ReadTimeout` when it is zero, so the keep-alive wait is
  bounded by the same field.

### Changed

- **`Mount` now panics when `Config.Schema` is nil**, where before it mounted cleanly. This was
  measured, not reasoned about: `Mount(app, Config{})` returned normally, the process booted, a
  readiness check that only proved the database passed — and then every request panicked inside
  gqlgen's executor, was recovered by gqlgen's own handler, and came back as
  `{"errors":[{"message":"internal system error"}]}` with **no `extensions.code`**. That is gqlgen's
  message, not luima's, so an alert keyed on `INTERNAL_SERVER_ERROR` never fired and the first
  symptom was the volume of stack traces. A panic is right here where `db.Connect` returning an
  error is right there: an unreachable database is an environment failure worth retrying, a nil
  schema is a programmer error that cannot become valid later.

  **Migration:** none. A build this affects was already broken; it now says so at boot instead of
  once per request.

- **A zero `Config` now ships a 10s read deadline and a 30s write deadline**, where before it
  shipped neither. Fiber fills in its own defaults for `BodyLimit` and `Concurrency` but passes
  the timeouts through verbatim, and fasthttp reads zero as *no deadline* — so one client could
  hold a connection slot indefinitely by dribbling a request body a byte at a time, against a
  default `Concurrency` of 262144, and `Shutdown` could not reclaim that slot either because it
  does not close keep-alive connections. This contradicted the invariant that a zero `Config` is
  the good configuration; it no longer does.

  Only `New` — and therefore `Run` — is affected. `Mount` is handed a router that already exists
  and never set its configuration, so an app you built with `fiber.New` yourself is unchanged.

  **Migration:** if you were relying on an unbounded write deadline for a long-running query, set
  `WriteTimeout: -1`. Realistically nobody is: `RequestTimeout` already bounds the resolver at 15s,
  a full fifteen seconds before the new default, so this can only bite a consumer who disabled
  `RequestTimeout` with a negative value too.

## [0.3.0] — 2026-08-06

Two bug fixes that were filed as features, one new bound, and a documentation pass that removes a
claim this project had been repeating for three releases without measuring it.

Everything is source-compatible except one thing: `luimaerr.CustomError` gains a third field, so
an **unkeyed** composite literal — `&CustomError{"msg", err}` — no longer compiles. Use keyed
fields: `&CustomError{UserMessage: "msg", InternalError: err}`. Keyed literals, which `go vet`'s
`composites` check has always pushed you toward for a struct from another module, are unaffected.

The one behaviour change to know about: operations nested deeper than 15 levels are now rejected. If
your schema legitimately nests deeper than that, set `Config.MaxDepth` before upgrading.

### Added

- **`Config.MaxDepth`** — caps operation nesting depth. Default 15, negative disables, zero means
  unset, as with `QueryCache` and `ComplexityLimit`. `SECURITY.md` had already named the gap:
  complexity does not bound depth, because a 40-level query costs about 40 against a limit of
  1000, so a cyclic schema — `User.friends: [User!]!` — passes it and multiplies into a resolver
  call per node per level. gqlgen ships no depth limiter. The walk resolves fragment spreads out of
  `doc.Fragments`, and that is not an optimisation: a spread node carries no selection set, so a
  limiter that walks only the operation reads every named fragment as a leaf, and a 40-deep document
  behind `...F` measures 1 and executes. An inline fragment is a type condition, not a level. The
  default is chosen against the deepest document a default install serves — the playground's own
  introspection query, which measures 13.
- **`crud.Create` takes query modifiers**, like the other four. The clause it needed was
  `ON CONFLICT`, which is the only way to attempt an insert inside a transaction without a real
  23505 aborting the whole thing — a suppressed conflict does not abort it.
- **`luimaerr.CustomError.Code`** — becomes `extensions.code` on the wire. `crud.Create` sends
  `CONFLICT`, `crud.Update` sends `NOT_FOUND`, and every redacted error sends
  `INTERNAL_SERVER_ERROR`. Clients had to string-match a message that `crud.Create` builds from
  caller-supplied text, which `docs/gotchas.md` #30 already documents as attacker-influenced. An
  empty `Code` emits no extensions object, so responses that do not opt in are byte-identical to
  `0.2.1`. Nothing here is auth-shaped: `UNAUTHENTICATED` and `FORBIDDEN` are not a library that
  ships no auth's to define.
- **`docs/deployment.md` § "Serving over TLS"** — the HTTP side of deployment, which the document
  did not previously cover at all. Both shapes: `fiber.ListenConfig` for terminating TLS in the
  process (and why that is HTTP/1.1 only — fasthttp ships no h2), and running behind a
  TLS-terminating proxy, with a table of what a resolver actually sees on such a request. Three
  silent failures are named: `Config.Fiber.TrustProxy` changes `fiber.Ctx` accessors and so is
  invisible to resolvers, which hold an `*http.Request`; `r.TLS` is `nil`, so middleware that
  infers HTTPS from it drops `Secure` cookies or redirect-loops; and a prefix-stripping proxy
  breaks the playground's fetch URL, whose scheme is otherwise handled automatically.
- **`docs/gotchas.md` rows 34–37.** 34–36 are the three TLS failures above. 37 is new: adding
  `transport.MultipartForm` through `Configure` makes your endpoint reachable by a cross-site HTML
  form, because `multipart/form-data` is a "simple" request that no preflight protects — a
  mutation submitted from any origin executes with the caller's cookies. Measured. The docs
  recommended that transport in three places with no warning attached. luima grows no CSRF field,
  because it registers no transport that needs one, and `SECURITY.md` now records that the default
  is closed.
- **A request-ID `HTTPMiddleware` and a `Configure` block in the quickstart.** Both seams shipped in
  `0.2.0` with no worked example outside the test suite.

### Fixed

- **`crud.Create` no longer redacts an insert the database deliberately suppressed.** `Create`
  issues `RETURNING *`, so go-pg scans a result set — and an insert that produced no row arrives
  as `pg.ErrNoRows`, which `Create` returned bare and `PresentError` redacted. A `BEFORE INSERT`
  trigger returning `NULL` is the ordinary way to write a soft-ignore, needs no cooperation from the
  caller, and made every such mutation answer `"internal server error"` while having done exactly
  what the schema told it to. It now returns `(nil, nil)`, like `Get`. **Check the result** — a
  caller assuming non-nil nil-dereferences the second time the same key is inserted.
- **The default transports are registered after `Configure` runs.** gqlgen selects the first
  transport whose `Supports` matches and `AddTransport` appends, so registration order is
  precedence. `transport.POST` matches `POST` + `application/json` and never reads `Accept`, which
  makes it a strict superset of what SSE matches — so a transport registered through `Configure`
  could never be selected. It compiled, it mounted, it returned 200, and a subscription silently
  answered with one buffered response. Nothing can have depended on the old order, since nothing
  registered that way ever ran.
- **The reason given for subscriptions being out of scope was false**, in six files including the
  `0.1.0` entry below, which is left as written because it records what was believed then. The
  adaptor does not buffer: a `Flush`ing handler streams eight chunked frames 400 ms apart through
  `adaptor.HTTPHandlerWithContext`, and through plain `adaptor.HTTPHandler` too. What blocked
  subscriptions was the transport order above, now fixed, and fasthttp not cancelling the request
  context when a client disconnects — measured at twenty frames produced after hangup with
  `ctx.Err() == nil`. That second one is the real blocker and it is upstream: a subscription must
  disable `RequestTimeout` to outlive 15s, and disabling it removes the only bound there is.
  `transport.Websocket` was not measured and nothing claims it now.
- **`ErrorPresenter` is not the only path to the wire**, contrary to three files. gqlgen's
  transports write their own errors before an executor exists, so a malformed JSON body comes back
  as HTTP 400 with the body echoed into the message, unpresented and unredacted. Only the caller's
  own bytes are exposed — but the claim is what someone leans on when deciding they need not
  sanitize something. Measured alongside: errors gqlgen *does* hand to the presenter keep their
  codes, so parse, validation and complexity rejections are byte-identical to gqlgen's own
  `DefaultErrorPresenter`.
- **Panics are fully answered by `Configure`, and nothing further is owed.** A panic in
  `HTTPMiddleware` returns HTTP 500 and the process survives, with `RequestTimeout` enabled or not;
  a `SetRecoverFunc` installed through `Configure` routes the recovered value through
  `PresentError`, which redacts it — a DSN embedded in a panic came out as `"internal server
  error"` with the detail on stderr only. This closes the last item `docs/security-review.md` left
  half-open.
- **Two anchors in `docs/gotchas.md` never resolved** — `#14-returning-` and `#30-create-s-label`.

### Changed

- **`luimaerr.CustomError` gains a `Code` field.** Unkeyed composite literals stop compiling;
  migrate with `&CustomError{UserMessage: msg, InternalError: err}`.

### Removed

- **`docs/security-review.md`, `AUTH_HANDOUT.md` and `AUTH_INTEGRATION.md`** — the three documents
  `v0.2.1` added are no longer tracked here. No code changed; the fixes they describe are still in
  the library and still described in the `0.2.0` entry below.

## [0.2.1] — 2026-08-05

Documentation only. No code changed, so there is nothing to upgrade for — `v0.2.0` and `v0.2.1`
are the same library.

### Added

- **`docs/security-review.md`** — the component-by-component review of
  0.1.0 that produced `v0.2.0`, and the reasoning behind each fix. It was written but never
  tracked, which `v0.2.0` shipped a dangling reference to: `tests/context_test.go` cites
  "D-02 in docs/security-review.md" by finding ID. Left as written, against 0.1.0 — the
  "what breaks if you remove this line" arguments are what stop the fixes being undone — with a
  status banner and per-finding markers so it cannot be misread as a report on the current
  release. What is still open (S-06, S-07, C-05, D-04, and half of E-03) is named in the fix list.
- **`AUTH_HANDOUT.md`** and **`AUTH_INTEGRATION.md`** — the build specification for
  [kal](https://github.com/ulas96/kal), the auth library built against the seams this release
  line froze. They live here because this is the repository that owns those seams:
  `HTTPMiddleware`, `Configure` and the `crud` query options each exist for a reason recorded
  only in these two documents, and the next person to touch `Mount` needs to know what is
  pressing on them. luima itself still ships no auth, and none is planned — see `SECURITY.md`.

## [0.2.0] — 2026-08-05

A security remediation pass, plus the two seams it showed were missing. Everything below is
source-compatible: the CRUD signature changes are variadic additions, and the four new `Config`
fields have working zero values. No migration required.

The one behaviour change to know about: requests now carry a 15s deadline by default. If you have
resolvers that legitimately run longer, set `RequestTimeout` before upgrading.

### Security

- **`luimaerr.PresentError` no longer unwraps to find a `*gqlerror.Error`.** It matched with
  `errors.As`, which walks the chain, so any error *wrapping* a `gqlerror` was returned whole —
  `fmt.Errorf("insert into %s failed for tenant %d: %w", table, tenantID, gqlErr)` reached the
  client verbatim. Redaction was effectively opt-out. It is now a type assertion on the top-level
  error; gqlgen delivers its own parse and validation errors unwrapped, so nothing legitimate
  changes.
- **The resolver error log escapes its input.** `%v` → `%q`. `err` routinely carries
  attacker-controlled text, and `%v` writes newlines literally, so a caller sending
  `"x\nresolver error: all clear"` could forge a log line.
- **`db.Connect` no longer puts the connection string in its error.** A malformed DSN produced a
  `*url.Error`, which embeds the raw URL — password included — and the quickstart calls
  `log.Fatal` on it.
- **`crud.Get`, `crud.Update` and `crud.Delete` can express an ownership predicate.** They were
  hard-wired to `WherePK()`, so `WHERE personal_id = $1 AND owner_id = $2` was inexpressible
  without dropping to raw go-pg and hand-rolling the SQLSTATE classification the package exists to
  provide. luima still ships no auth; it no longer prevents you from writing it.

### Added

- **`Config.RequestTimeout`** — a deadline on the whole request, propagated into the resolver
  context. Default 15s, negative disables. Previously nothing bounded a query at any layer: go-pg
  sets no read or write timeout, `pg.ParseURL` rejects `statement_timeout` in the DSN, and the
  resolver context had no deadline to inherit.
- **`Config.DisableIntrospection`** — the sibling `DisablePlayground` never had. Turning
  introspection off previously meant abandoning `Mount`.
- **The resolver receives a real `context.Context`.** `Mount` now uses
  `adaptor.HTTPHandlerWithContext` and re-attaches Fiber's request context. Before, it was the raw
  `*fasthttp.RequestCtx`: no deadline, cancelled only at server shutdown, and everything a
  middleware put in `c.SetContext` was silently discarded. `c.Locals` still reaches
  `ctx.Value` — the re-attached context falls back to fasthttp's user values, so consumers on that
  path are unaffected.
- **`Config.HTTPMiddleware`** — `[]func(http.Handler) http.Handler`, wrapped around the gqlgen
  handler, outermost first, inside the context re-attach: a middleware receives the real request
  context — `RequestTimeout` deadline included — whatever it adds with `r.WithContext` reaches
  every resolver typed, and a `Set-Cookie` it writes survives the adaptor. The mount point for
  anything written against `net/http`: logging, tracing, tenancy, rate limiting, a session layer.
- **`Config.Configure`** — `func(*handler.Server)`, run after luima's defaults and immediately
  before mounting, so it can override them. The escape hatch for `Use`, `AroundOperations`,
  `SetRecoverFunc`, `SetParserTokenLimit` and `SetDisableSuggestion`, none of which were reachable
  while `srv` stayed a local inside `Mount`.
- **`opts ...func(*orm.Query) *orm.Query`** on `Get`, `Update` and `Delete`, matching `List`. On
  `Update`, `q.Column(...)` is also the partial update the full replace otherwise rules out.
- **`make audit`** — `govulncheck` over both modules, wired into `make check` and CI.

### Fixed

- **`?sslmode=verify-full` can connect.** `pg.ParseURL` builds the `tls.Config` for `verify-ca`
  and `verify-full` but never sets `ServerName`, and crypto/tls refuses a handshake without it. The
  one mode that verifies anything failed at boot, and four documents said otherwise. `Connect` now
  fills it in from the address.
- **`db.Connect` cannot hang forever.** The boot-time `select 1` ran with no context and no
  deadline — `DialTimeout` bounds the dial only, and go-pg leaves the read and write timeouts at
  zero — so a host that completed the TCP handshake and then stalled blocked startup indefinitely.
  It is now bounded by `?connect_timeout=N`, defaulting to 5s.

### Changed

- `examples/quickstart` is the production shape, with the playground and introspection opened by
  `LUIMA_DEV` rather than closed by its absence. It also sets the HTTP timeouts, adds a rate
  limiter, bounds its `users` query at 100 rows, and shuts down gracefully on SIGTERM. The library
  default is unchanged: a zero `Config` is still the good *development* configuration.
- `errorlint` added to `.golangci.yml` — the linter that would have caught the redaction bug above.
  CI actions are SHA-pinned and `golangci-lint` is version-pinned.
- `Connect` keeps its single-argument signature. A `Connect(url, opts ...func(*pg.Options))` was
  considered and rejected: a context deadline already bounds the pool wait, the socket and the
  running backend, so `RequestTimeout` covers what the variadic was for. Tuning `pg.Options`
  genuinely needs is a matter of calling `pg.ParseURL` and `pg.Connect` yourself.

## [0.1.0] — 2026-08-03

Initial release.

### Added

- **`luima`** — root package re-exporting the four below, so the common case is one import.
  `luima.Config` is a type alias for `server.Config`, not a copy, so the two spellings are
  interchangeable.
- **`luima/server`** — `Config`, `New`, `Mount`. Mounts a gqlgen handler on Fiber v3 with the
  query cache, introspection extension, complexity limit and error presenter all configured;
  `New` builds an app, `Mount` takes any `fiber.Router` including a group.
- **`luima/crud`** — `Get`, `List`, `Create`, `Update`, `Delete`. Generic helpers over go-pg that
  classify driver errors: `pg.ErrNoRows` becomes `(nil, nil)` so a missing row renders as GraphQL
  `null`, `23505` becomes a client-visible conflict, and a `List` seeds a non-nil slice so an empty
  table marshals as `[]`. All take `orm.DB`, so they work inside a transaction unchanged.
- **`luima/luimaerr`** — `CustomError`, `PresentError`, `SQLState`. `PresentError` passes
  `*CustomError` and `*gqlerror.Error` through and redacts everything else, so raw driver text
  never reaches an unauthenticated caller. `SQLState` wraps the `errors.As` dance whose target type
  (`pg.Error`, an interface) is easy to get wrong.
- **`luima/db`** — `Connect`. Returns its error rather than calling `log.Fatal`, and proves the
  connection with an eager `select 1` because `pg.Connect` dials nothing.
- **`examples/quickstart`** — a complete server as a nested module, built by CI.
- Documentation for the gqlgen contract, the Fiber integration, deployment, and a 26-entry gotcha
  register.

### Notes

- `Create` and `Update` issue `RETURNING *`, which the server this was extracted from deliberately
  did not. A library serves tables it has never seen: without it, a table with a `DEFAULT now()`,
  a trigger, an identity column or a generated column makes a mutation answer with the value the
  client *sent* rather than the value Postgres *stored*. Same statement, same round trip.
- `Update` is a full replace. `UpdateNotZero` would skip zero values, which means an empty slice
  could not clear an array column — a silent data-retention bug rather than a partial-update
  feature. Partial updates need a real design and are not in this release.
- `Update`'s absence signal is checked two ways. A plain `UPDATE` succeeds with
  `RowsAffected() == 0`, but with `RETURNING *` go-pg reports zero rows as `pg.ErrNoRows` instead.
  Found by running the round-trip test against a real Postgres.

### Not included, deliberately

Auth, pagination, filtering, dataloaders, subscriptions, file upload, migrations, a scaffolding
CLI. Subscriptions are blocked by architecture rather than effort: `adaptor.HTTPHandler` buffers
the whole response, so a streaming transport cannot work through it.

[Unreleased]: https://github.com/ulas96/luima/compare/v0.5.0...HEAD
[0.5.0]: https://github.com/ulas96/luima/releases/tag/v0.5.0
[0.4.0]: https://github.com/ulas96/luima/releases/tag/v0.4.0
[0.3.0]: https://github.com/ulas96/luima/releases/tag/v0.3.0
[0.2.1]: https://github.com/ulas96/luima/releases/tag/v0.2.1
[0.2.0]: https://github.com/ulas96/luima/releases/tag/v0.2.0
[0.1.0]: https://github.com/ulas96/luima/releases/tag/v0.1.0
