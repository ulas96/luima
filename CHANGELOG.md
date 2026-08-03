# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

While the major version is `0`, the public API may change in a minor release. Every such change
will be listed here under **Changed** with the migration in one line.

## [Unreleased]

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

[Unreleased]: https://github.com/ulas96/luima/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/ulas96/luima/releases/tag/v0.1.0
