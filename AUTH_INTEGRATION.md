# AUTH_INTEGRATION.md

**How the luima side is handled while `kal` is built against it.**

Companion to [`AUTH_HANDOUT.md`](AUTH_HANDOUT.md), which specifies *what* to build. This one covers
*how the two repositories move together*: the order the patches land, how `kal`'s `go.mod` resolves a
luima that isn't tagged yet, when the tag is cut, and what must never migrate from `kal` into luima.

> **Naming.** The library is `kal`, module path `github.com/ulas96/kal`, packages `kal/session`,
> `kal/authn`, `kal/authz`, `kal/kalerr`, `kal/oauth`, `kal/mfa`. Both documents use that name
> throughout. (An earlier draft of the handout called it `luimauth`; if you have a copy that does,
> it is stale.)

> **Status, 2026-08-05 — phases 0 and 1 are done, but not in the order below.** `v0.2.0` was
> tagged for the security remediation *before* kal M1 had exercised the seams, so the freeze gate
> was skipped rather than passed. All three patches shipped in it, each with the test that fails
> without it: P1 as `server.Config.HTTPMiddleware`, P2 as `server.Config.Configure`, P3 as the
> `opts ...func(*orm.Query) *orm.Query` variadic on `crud.Get`, `Update` and `Delete`.
>
> The cost is exactly the one "The short answer" predicts below: three seams frozen against a
> consumer that had not used them yet. So **kal starts at phase 2** — pin
> `github.com/ulas96/luima v0.2.0`, no `go.work`, no pseudo-version, no `replace`. If M1 finds a
> seam wrong, the answer is the one under "If a seam turns out wrong after `v0.2.0`": a `v0.3.0`
> with a **Changed** entry and a one-line migration, never a silent patch release. Phase 0 below
> is history; everything from "The seam contract" on is the standing go-forward contract.

---

## The short answer

**Option 1 is the destination. Option 2 is the route. Do both, in that order.**

The choice as posed contains a false trade-off. Option 2's stated cost — "kal's CI and example stay
broken" — is not actually a consequence of leaving luima untagged. It is a consequence of leaving
luima *unpushed*. Push the branch without tagging it and Go will resolve a **pseudo-version**
straight from the commit, so `kal`'s CI is green throughout.

That distinction matters, because tagging `v0.2.0` immediately has a real cost: **you would be
freezing three API seams that no consumer has used yet.** They were designed against a library that
does not exist. If `HTTPMiddleware` turns out to want a single func rather than a slice, or
`Configure` needs to run *before* `SetErrorPresenter` rather than after, you have burned a tag and
owe a `v0.3.0` with a migration note.

So:

| phase | luima | kal resolves luima via | frozen? |
|---|---|---|---|
| **0 · Bootstrap** | P1–P3 on a pushed branch `feat/auth-seams` | `go.work` locally, pseudo-version in CI | no — seams still movable |
| **1 · Freeze** | merge to `main`, tag `v0.2.0` | — | yes |
| **2 · Steady** | `main` | `require github.com/ulas96/luima v0.2.0` | yes |

**The gate between phase 0 and phase 1 is kal's M1**: passwords, sessions and the context seam
working end to end, through all three luima seams, against a real Postgres. Once a real consumer has
exercised a seam, it is safe to freeze. Not before.

Option 3 is not viable and the handout already says why (§1): without P1 there is no way to get a
typed identity into a resolver at all, and without P3 the authorization model in §8.3 cannot be
expressed. Building `kal` on luima v0.1.0 means building a different, weaker library.

---

## Phase 0 · Bootstrap

### 0.1 · Land the patches on a branch

```sh
cd luima
git checkout -b feat/auth-seams
# implement P1, P2, P3 from AUTH_HANDOUT.md §1, each with its failing test
make check          # gofmt + vet + lint + test-db + example — the documented pre-PR gate
git push -u origin feat/auth-seams
```

`make check` runs `test-db`, which needs `DATABASE_URL` from `.env`. A green `go test ./...` proves
less than it looks — confirm `--- PASS: TestCRUD`, not `--- SKIP`.

**Push the branch. Do not tag it.** The push is what makes phase 0 work; the tag is what you are
deliberately deferring.

### 0.2 · Two repositories, side by side

```
~/Documents/Github/
├── luima/       # on feat/auth-seams
└── kal/         # new module: github.com/ulas96/kal
```

### 0.3 · `go.work` for local iteration

At the parent directory, **untracked**:

```
go 1.25.0

use (
    ./luima
    ./kal
)
```

Now an edit to `luima/server/server.go` is visible to `kal` on the next build with no push, no
`replace`, no version bump. This is the whole point of phase 0: the seams get adjusted while a real
consumer is pressing on them.

`go.work` and `go.work.sum` must be gitignored. luima already covers them (`.gitignore:24-25`) —
carry the same two lines into `kal`. A committed `go.work` overrides module resolution for anyone who
checks the repo out, which turns a local convenience into a build that only works on your machine.

### 0.4 · A pseudo-version so CI resolves

`go.work` is a local file. `kal`'s CI checks out only `kal`, sees no `go.work`, and resolves
`go.mod` normally — which is exactly what you want, because **CI's job is to prove the published
state builds**, not the state on your laptop.

So give `kal`'s `go.mod` something publishable to resolve:

```sh
cd kal
go get github.com/ulas96/luima@feat/auth-seams
```

Go writes a pseudo-version derived from the branch tip:

```
require github.com/ulas96/luima v0.1.1-0.20260804153000-abc123def456
```

That resolves from GitHub with no tag. `kal`'s CI is green from the first commit. When luima's
branch moves, re-run the `go get` to advance the pseudo-version — that step is deliberate, so kal
never silently picks up a seam change.

**Do not use a `replace` directive in `kal/go.mod` for this.** A `replace` is ignored by anyone who
imports `kal`, which means it works for you and silently fails to pin anything for a consumer, and
it is easy to forget before publishing. `go.work` + pseudo-version has neither problem.

### 0.5 · Definition of done for phase 0

| | |
|---|---|
| P1 | a middleware value **and** a deadline set via `Config.HTTPMiddleware` are both visible inside a resolver |
| P2 | an `OperationInterceptor` registered through `Config.Configure` affects the response |
| P3 | a `Delete` scoped to owner A returns `false` for owner B's key and leaves the row |
| kal M1 | login sets a `__Host-` cookie that survives the adaptor; a resolver reads a typed `Principal`; revoking a session makes the next request anonymous |
| both CIs | green, with `--- PASS:` confirmed on the DB-backed test in each |

When that table is complete, the seams have been used. Freeze them.

---

## Phase 1 · Freeze — merge and tag `v0.2.0`

### 1.1 · What the version number has to say

luima is `0.x`, and its `CHANGELOG.md` states the contract:

> While the major version is `0`, the public API may change in a minor release. Every such change
> will be listed here under **Changed** with the migration in one line.

So `v0.2.0` is correct, and one of the three patches genuinely needs a **Changed** entry:

- **P1 and P2 are purely additive.** Two new `Config` fields. Every existing consumer compiles
  unchanged.
- **P3 is additive for call sites and breaking for function values.** `luima.Get(ctx, db, key)` still
  compiles, because the new parameter is variadic. But `var f func(context.Context, orm.DB, *T) (*T, error) = luima.Get[T]`
  no longer does — a variadic parameter is part of the type. This is not hypothetical:
  `tests/luima_test.go:51-54` declares exactly those four function types as compile-time
  signature-identity assertions, and updating them is part of the patch.

Draft entry:

```markdown
## [0.2.0] — 2026-XX-XX

### Added

- **`server.Config.HTTPMiddleware`** — `[]func(http.Handler) http.Handler`, wrapped around the
  gqlgen handler, outermost first. Mounting now uses `adaptor.HTTPHandlerWithContext`, so the
  resolver context carries whatever a middleware set — including a deadline. Previously
  `c.SetContext` was silently discarded and the resolver context never cancelled.
- **`server.Config.Configure`** — `func(*handler.Server)`, run against the built server before it is
  mounted. The escape hatch for `Use`, `AroundOperations`, `SetRecoverFunc`, `SetParserTokenLimit`
  and `SetDisableSuggestion`, none of which were reachable before.

### Changed

- **`crud.Get`, `crud.Update` and `crud.Delete` take `opts ...func(*orm.Query) *orm.Query`**, the
  same as `List` — so an ownership predicate is expressible. Call sites are unaffected; assigning one
  of them to a function-typed variable needs the variadic parameter added to that type.

### Fixed

- Security review findings S-01, S-02 and C-01. See [docs/security-review.md](docs/security-review.md).
```

### 1.2 · The commit set

Four things land together:

1. **P1–P3**, each with its test.
2. **`docs/security-review.md`** — currently untracked. `AUTH_HANDOUT.md` cites it by finding ID
   throughout, so leaving it untracked makes the handout a document full of broken references. Commit
   it, and mark the three findings this release fixes so the fix list does not rot into a lie —
   a `✅ fixed in v0.2.0` marker on S-01, S-02 and C-01 in the fix-list tables is enough.
3. **`AUTH_HANDOUT.md`** and this file — also untracked. They are the specification `kal` is built
   from; they belong in the repository that owns the seams.
4. **`examples/quickstart/go.mod`** — bump `github.com/ulas96/luima v0.1.0` → `v0.2.0`. The
   `replace … => ../..` on line 53 makes this cosmetic, but a stale `require` in the example people
   copy from is exactly the kind of small inconsistency the repo's own docs complain about (Q-02).

### 1.3 · Tag and push

```sh
git checkout main && git merge --no-ff feat/auth-seams
git tag -a v0.2.0 -m "seams for auth: HTTPMiddleware, Configure, crud query options"
git push origin main --tags
```

Add the `CHANGELOG.md` link references at the bottom, matching the existing pattern:

```markdown
[Unreleased]: https://github.com/ulas96/luima/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/ulas96/luima/releases/tag/v0.2.0
[0.1.0]: https://github.com/ulas96/luima/releases/tag/v0.1.0
```

---

## Phase 2 · Steady state

```sh
cd kal
go get github.com/ulas96/luima@v0.2.0
go mod tidy
rm ../go.work ../go.work.sum
make check           # must pass with go.work gone — that is the real proof
```

`kal/go.mod`:

```
module github.com/ulas96/kal

go 1.25.0

require (
	github.com/ulas96/luima v0.2.0
	github.com/go-pg/pg/v10 v10.15.1
	github.com/99designs/gqlgen v0.17.94
	golang.org/x/crypto v0.54.0
	golang.org/x/sync v0.22.0
	github.com/golang-jwt/jwt/v5 v5.x.x
)
```

Removing `go.work` before the final check is the step people skip. Until it is gone, you have not
proven that `kal` builds against **published** luima — only against your working tree.

Pin `require github.com/ulas96/luima v0.2.0` exactly. Do not float it. An auth library taking an
unpinned dependency on the thing that propagates its identity context is an unforced risk, and
Dependabot will offer the bump when there is one.

---

## The seam contract

### The rule: nothing auth-shaped ever lands in luima

Look at what the three patches actually add:

- `HTTPMiddleware []func(http.Handler) http.Handler` — standard `net/http` middleware.
- `Configure func(*handler.Server)` — a gqlgen hook.
- `opts ...func(*orm.Query) *orm.Query` — go-pg query options.

**None of them mentions authentication.** That is deliberate and it is load-bearing. Each is useful
to someone doing request logging, tracing, tenancy, soft-delete filtering or rate limiting, and
luima keeps the "auth is out of scope, deliberately" promise it makes in `CLAUDE.md`, `SECURITY.md`,
`luima.go:14` and `README.md`.

Once the seams exist there will be pressure to move things across — a `Principal` type, a cookie
helper, "just the middleware". Resist it. **The test for whether something may land in luima: can it
be described without the word "auth"?** If not, it belongs in `kal`.

This also protects luima's dependency graph. It has four direct dependencies; `kal` will have
several more. Keeping the boundary sharp is what lets someone use luima without accepting Argon2,
`golang-jwt` and an OAuth client into their build.

### Testing is two-sided, and neither side substitutes for the other

**luima proves the seam exists and propagates.** Using the existing `stubSchema` in
`tests/helpers_test.go`, with no knowledge of what a consumer will put through it:

```go
// TestHTTPMiddlewareContext @notice A value and a deadline set by middleware reach the resolver.
//
// @dev The whole point of P1. Before it, adaptor.HTTPHandler handed the resolver a bare
// *fasthttp.RequestCtx: c.SetContext was discarded and Deadline() was never set. This test fails
// against v0.1.0, which is what makes it worth having.
```

**kal proves it uses the seam correctly**, against a real generated schema and a real Postgres: a
real `Principal` reaching a real resolver, a real `Set-Cookie` surviving the fasthttp adaptor, a
real scoped `Delete` missing another owner's row.

A luima test cannot prove the second thing — it has no generated schema and no session table. A kal
test cannot prove the first cleanly, because a failure there is ambiguous between "the seam is
broken" and "kal used it wrong". Write both.

### kal's CI

Copy luima's workflow, including the two parts that matter most and are easy to drop:

- the `postgres:16` service container, and
- the step that greps `--- PASS:` out of `-v` output, because a skipped test still reports `ok`.

For an auth library this is not a nice-to-have. A green suite that never touched Postgres proves
nothing about session revocation, the partial unique index on `lower(email)`, or the TOTP replay
constraint — all three of which are enforced by the *database*, not by Go.

Add what luima itself lacks (finding B-01): `govulncheck` and `gosec` in `make check` and in CI, and
`errorlint` in `.golangci.yml`.

---

## If a seam turns out wrong after `v0.2.0`

It is `0.x`, and the `CHANGELOG` already licenses a breaking change in a minor release with a
one-line migration. So the answer is `v0.3.0`, a **Changed** entry, and `kal` bumps its `require`.
Not a crisis.

But prefer not to need it, which is the entire argument for phase 0. The cheapest time to discover
that `Configure` should have run before `SetErrorPresenter` is while a `go.work` is still in place
and nothing is tagged.

If it happens anyway: put the seams back on a branch, restore the `go.work`, iterate, and tag once.
Do not ship a `v0.2.1` that quietly changes a seam's behaviour without a `Changed` entry — the
0.x contract in the CHANGELOG is a promise about *discoverability*, and a silent behaviour change in
a patch release breaks it in the way that costs someone a weekend.

---

## Checklist

**Phase 0**
- [ ] `feat/auth-seams` branch in luima with P1, P2, P3 and a failing-without-it test for each
- [ ] `make check` green, `--- PASS: TestCRUD` confirmed
- [ ] branch **pushed**, **not tagged**
- [ ] `go.work` at the parent directory; `go.work`/`go.work.sum` gitignored in `kal` (luima already has them)
- [ ] `kal/go.mod` pinned to the luima pseudo-version; kal CI green
- [ ] kal M1 complete: cookie round-trips, typed `Principal` in a resolver, revocation works

**Phase 1**
- [ ] `CHANGELOG.md` — Added ×2, Changed ×1 (the crud signature), Fixed (S-01, S-02, C-01)
- [ ] `docs/security-review.md` committed, with S-01/S-02/C-01 marked fixed
- [ ] `AUTH_HANDOUT.md` and `AUTH_INTEGRATION.md` committed
- [ ] `examples/quickstart/go.mod` require bumped to `v0.2.0`
- [ ] merged to `main`, tagged `v0.2.0`, pushed with `--tags`
- [ ] CHANGELOG link references updated

**Phase 2**
- [ ] `kal` requires `github.com/ulas96/luima v0.2.0`, pinned exactly
- [ ] `go.work` deleted
- [ ] `make check` green in `kal` **with `go.work` gone**
