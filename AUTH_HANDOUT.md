# AUTH_HANDOUT.md

**A build specification for `github.com/ulas96/kal` — the authentication and authorization
library that completes [luima](https://github.com/ulas96/luima).**

You are the agent building this. This document is meant to be sufficient on its own: every design
decision below carries the reason it was made, so you can extend it correctly rather than guess.
Read it all before writing code. The order is deliberate — §1 is a prerequisite, not an appendix.

---

## 0 · How to read this

### The thesis

> **Sessions in your database. Identity in the request context. Authorization in the WHERE clause.**

Three claims. Each is a position against how the rest of the Go ecosystem does it, and each is
defended where it appears:

1. **Own the users table in the application's own Postgres** (§2, §7). Kratos, SuperTokens and
   Zitadel are separate services with separate databases. That means no `JOIN`, no shared
   transaction, two backups, two migration stories. For a go-pg application this is a daily tax.
2. **Opaque server-side sessions by default** (§5.4). ASVS 7.4.1, 7.4.2 and 7.5.2 — terminate a
   session, terminate all sessions on account disable, let a user see and revoke their own sessions
   — are structurally unimplementable with stateless-only tokens. The JWT leg exists for interop
   only, is minted *from* a live session, and because the session cookie is the long-lived
   credential it **deletes the entire rotating-refresh-token subsystem** that JWT-first libraries
   must build (RFC 9700 §2.2.2 reuse detection, family revocation, the two-tab race). That deletion
   is the single largest simplification in this design.
3. **Authorization is a predicate, not a boolean** (§8.3). Every authorization library in Go answers
   "can Alice read doc 7?" and none answers "which docs can Alice read?" without N checks or a
   1000-item ID list. Pushing the ownership predicate into the SQL is strictly stronger, and it is
   the thing a policy engine structurally cannot give you.

### Scope, decided

| | |
|---|---|
| **Module** | Separate: `github.com/ulas96/kal`, its own repo and `go.mod`, depending on luima |
| **luima** | Three prerequisite patches land in luima first (§1) |
| **Sessions** | Opaque DB sessions by default, plus an opt-in short-lived JWT minted from a live session |
| **v1.0 core** | Passwords · sessions · account lifecycle · RBAC + authorization · the non-optional hardening. **Mandatory — everything else is additive to this.** |
| **v1.0 also** | OAuth / OIDC social login |
| **Optional** | TOTP MFA + recovery codes + step-up. Separate package, opt-in dependency, may slip to v1.1 |
| **Out of scope** | WebAuthn / passkeys (§18), and the rest of the non-goals list |

### On the claims in this document

Every third-party behavioural claim here was read from the versions luima actually pins —
`gqlgen v0.17.94`, `fiber/v3 v3.4.0`, `go-pg/pg/v10 v10.15.1`, `fasthttp v1.72.0` — under
`$(go env GOMODCACHE)`, not recalled. This matters more than usual because **Fiber v3 and gqlgen
both behave differently from what v2-era memory predicts**, and several of those differences are
security-relevant. §4 collects the ones that will bite you.

Where luima's own [`docs/security-review.md`](docs/security-review.md) has already analysed
something, this document cites its finding ID (`S-02`, `C-01`, …) rather than repeating the
analysis. Read that document once; it is the reason several decisions here look the way they do.

---

## 1 · Patch luima first

luima 0.1.0 cannot express a secure application. Three gaps, all already written up as P0/P1
findings in `docs/security-review.md`, all under ten lines to fix. **Land these before writing any
kal code** — two of the three are seams kal physically cannot work without, and the third
is the one that makes authorization enforceable.

Each patch needs a test that fails without it (luima's stated rule), a `luima.go` shim update where
the exported surface changes, and a `CHANGELOG.md` entry under `## [Unreleased]`.

### P1 · The request context seam — fixes S-01, S-02, and gives kal its mount point

**What is broken.** `server/server.go:145` mounts with `adaptor.HTTPHandler(srv)`, which resolves to
`fasthttpadaptor.NewFastHTTPHandler` and hands the handler the `*fasthttp.RequestCtx` as its request
context. Measured consequences, from the security review:

- `c.SetContext(...)` in Fiber middleware is **silently discarded**. `ctx.Value(myKey{})` in a
  resolver is `nil`.
- `ctx.Deadline()` returns `ok == false`; `ctx.Done()` closes only at server shutdown. A client
  hang-up does not stop the query, and no middleware can impose a timeout.
- `c.Locals(k, v)` *does* reach `ctx.Value(k)` — `RequestCtx.Value` reads fasthttp user values and
  `Locals` writes one — but it is untyped, collision-prone across middleware, and cannot carry a
  deadline.

A consumer who tries the documented approach (`SECURITY.md:23` — "your own Fiber middleware on the
group you `Mount` onto"), finds `ctx.Value` empty, and reaches for the shared `Resolver` struct
instead has written a **cross-request data race on identity**, which is an authorization bypass
under concurrency, not a bug you find in testing.

**The patch.** Add one `Config` field and change one line:

```go
// HTTPMiddleware @notice net/http middleware wrapped around the gqlgen handler, outermost first.
//
// @dev The seam an auth library needs, and the only layer that has all three of: the real
// *http.Request with its cookies, an http.ResponseWriter whose headers survive back through the
// adaptor, and r.WithContext — which every resolver sees as its own ctx, typed.
//
// Fiber middleware on the mounted group cannot do this: adaptor discards c.SetContext.
HTTPMiddleware []func(http.Handler) http.Handler
```

```go
// fasthttpadaptor hands the handler the *fasthttp.RequestCtx, which never cancels and drops
// anything middleware put in c.SetContext. HTTPHandlerWithContext stashes Fiber's request context
// as a user value; withFiberContext re-attaches it, so gqlgen — and every resolver — sees a real
// context.Context with whatever deadline and values the middleware set.
func withFiberContext(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fc, ok := adaptor.LocalContextFromHTTPRequest(r); ok {
			r = r.WithContext(fc)
		}
		next.ServeHTTP(w, r)
	})
}

// … in Mount, replacing r.All(endpoint, adaptor.HTTPHandler(srv)):
var h http.Handler = srv
// Reverse order so the first element is outermost — the order a reader expects.
for i := len(cfg.HTTPMiddleware) - 1; i >= 0; i-- {
	h = cfg.HTTPMiddleware[i](h)
}
r.All(endpoint, adaptor.HTTPHandlerWithContext(withFiberContext(h)))
```

Both `adaptor.HTTPHandlerWithContext` and `adaptor.LocalContextFromHTTPRequest` are exported from
`fiber/v3/middleware/adaptor` in v3.4.0. No new dependency, no reimplementation of the
ResponseWriter bridge.

**Test that fails without it:** mount the existing `stubSchema` behind `Config.HTTPMiddleware`
carrying a `context.WithValue` and a `context.WithTimeout`; have the stub's `Exec` assert both the
value is visible and `Deadline()` is set. Today both assertions fail.

### P2 · The gqlgen extension seam

**What is broken.** `srv` is a local inside `Mount` and never escapes. So `srv.Use`,
`AroundOperations`, `AroundFields`, `SetRecoverFunc`, `SetParserTokenLimit` and
`SetDisableSuggestion` are all unreachable through luima's API, and `Config` has a field for none of
them. kal needs `Use` and `AroundOperations` for the anti-batching extension (§9.1) and the
conditional-introspection mutator (§9.3). A consumer wanting any of these today must abandon
`Mount`, which is the whole library.

**The patch.** One field rather than five, because one unlocks all of them:

```go
// Configure @notice Runs against the built gqlgen server immediately before it is mounted.
//
// @dev One escape hatch rather than a field per knob. srv.Use, AroundOperations, SetRecoverFunc,
// SetParserTokenLimit and SetDisableSuggestion are all reachable through it, and a new gqlgen
// extension point needs no change here.
//
// It runs after luima's own defaults, so it can override them.
Configure func(*handler.Server)
```

```go
// … in Mount, after SetErrorPresenter:
if cfg.Configure != nil {
	cfg.Configure(srv)
}
```

**Test that fails without it:** a `Configure` that registers an `OperationInterceptor` returning a
fixed error; assert the response body carries it.

### P3 · Ownership predicates on Get, Update and Delete — fixes C-01

**What is broken.** `crud.List` takes `opts ...func(*orm.Query) *orm.Query`. `Get` (`crud.go:43`),
`Update` (`:139`) and `Delete` (`:162`) take nothing and are hardwired to `WherePK()`. There is
therefore **no way to write the query authorization actually needs**:

```sql
DELETE FROM app_users WHERE personal_id = $1 AND owner_id = $2
```

To get that second predicate you drop to raw go-pg and hand-roll the SQLSTATE classification, which
is the one thing `crud` exists to provide. luima's happy path is IDOR by construction, and the
quickstart proves it: `deleteUser(personalId:)` deletes any row for anyone who can reach the port.

**The patch.** The symmetry `List` already has. Source-compatible for every existing caller:

```go
func Get[T any](ctx context.Context, db orm.DB, key *T, opts ...func(*orm.Query) *orm.Query) (*T, error) {
	q := db.ModelContext(ctx, key).WherePK()
	for _, opt := range opts {
		q = opt(q)
	}
	if err := q.Select(); err != nil { /* … unchanged … */ }
	return key, nil
}
```

Same shape for `Update` and `Delete`. Then update the five wrappers in `luima.go` and the four type
aliases in `tests/luima_test.go:51-54`, which are compile-time signature-identity assertions and
will break — that is the mechanism working as designed.

**Why this is the highest-leverage line in the whole design.** For `Update` and `Delete` the "no row
matched" branch already exists and already produces `label + " not found"`. So a row that exists but
is not yours reports as **absent** — which is the correct answer to give an unauthorized caller
anyway. Correct authorization and correct information-disclosure behaviour fall out of one variadic
parameter.

**Test that fails without it:** extend `TestCRUD` with two rows of different owners; assert a
`Delete` scoped to owner A returns `false` for owner B's key and leaves the row in place.

### What is deliberately *not* a prerequisite

The other security-review findings (`D-01` TLS `ServerName`, `D-02` query timeouts, `E-01` the
unwrapping gqlerror branch, `E-02` the log line, `S-04` introspection, `S-05` HTTP timeouts) are all
real and should land — but kal can be built without them. Do not block on them. Two of them
change what kal must do defensively, and those are called out where they matter: `E-01` in
§11, `S-04` in §9.3.

---

## 2 · The landscape, and the gap this fills

You are not the first person to write a Go auth library. Here is what exists, what each gets right,
and why none of them is the thing luima needs. **Read this section as a list of mistakes not to
repeat** — the failure modes are remarkably consistent.

### The primitives tier — correct, but you assemble the protocol

`golang-jwt/jwt`, `x/crypto/argon2`, `pquerna/otp`, `go-webauthn/webauthn`. Each does its one thing
well. The CVEs are not in these libraries; they are in the assembly. `pquerna/otp` will happily tell
you a TOTP code is valid for the second time in a row, because "has this been used" is not its job
(§14). `x/crypto/argon2` gives you `IDKey(...)  []byte` and nothing else: no salt generation, no
encoding, no constant-time compare, no answer to the concurrency question (§5.1).

### The framework tier — opinionated, and opinionated about the wrong shape

**`markbates/goth`** — OAuth provider aggregation, 70+ providers. Three structural problems:

```go
var SetState = func(req *http.Request) string {
	state := req.URL.Query().Get("state")
	if len(state) > 0 { return state }   // caller-supplied state wins
	// …else 64 random bytes
}
```

An attacker who can induce a victim to hit `/auth/google?state=ATTACKER_VALUE` **fixes the anti-CSRF
value for that flow** — a login-CSRF and account-linking primitive. Second: `gothic.Store` and the
provider registry are package-level `var`s, so two independently configured auth surfaces in one
binary are impossible and per-tenant OAuth credentials need hacks. Third: PKCE exists for a handful
of providers with no generic API (issue #516), so it cannot comply with RFC 9700 §2.1.1, which makes
PKCE a MUST for public clients.

**Take away:** the `Provider`/`Session` split and the normalized user struct are good ideas. Globals
and letting request input define security state are not.

**`volatiletech/authboss`** — the module system is genuinely well thought out, and the
**selector/verifier token pattern** (indexable non-secret lookup key + constant-time-compared
secret) is the right idea for emailed tokens. But it replaces the type system with runtime
assertions: `MustBeAuthable`, `MustBeConfirmable`, `EnsureCanCreate` all *panic*. Enable a module,
miss an interface method, find out in production. And it is HTML-form-shaped — bending it to a
GraphQL mutation surface means reimplementing `BodyReader` and `Responder` per module and fighting
redirect semantics. Canonical import path is `volatiletech/authboss/v3` while development moved to
`aarondl/authboss`; two repos and one module path is why people leave.

**`go-pkgz/auth`** — `ClaimsUpd`/`Validator` hooks are the right extension points and
`AllowedRedirectHosts` is a real open-redirect defence. But its CSRF is **naive double-submit**
(cookie ↔ header, no binding), which OWASP explicitly calls bypassable by anyone who can write
cookies on the domain — a vulnerable sibling subdomain is enough. `AllowedRedirectHosts` is opt-in
"for backwards compatibility", which is the wrong trade in a security library. And an **avatar store
is a required dependency of the auth service**. That is the canonical scope-creep story of this
ecosystem: the thing that makes an auth library unadoptable is not missing features, it is bundled
ones.

### The service tier — complete, and it takes your users table with it

**Ory Kratos**, **SuperTokens**, **Zitadel**, **Casdoor**, **Authelia**. Mature, well-engineered,
and the right answer for many teams. The cost is always the same and it is decisive here:

- **Your identity data leaves your database.** No `JOIN users ON orders.user_id`. "List users with
  their order counts, sorted, paginated" becomes an N+1 across a network. For a gqlgen application
  this is acutely painful, and gqlgen has no dataloader in luima to soften it.
- **No shared transaction.** "Create the user and the organization atomically" is not expressible.
- **Another deployment, another database, another backup.** SuperTokens additionally needs a JVM.
- Kratos makes you build the entire self-service UI from its `ui.nodes` array, and has two
  operational modes with *different security models* (browser flows have anti-CSRF cookies; API
  flows have bearer tokens). Using API flows from a browser silently removes CSRF protection.

**Steal from SuperTokens** anyway: its rotating-refresh-token-family design, where a session row
carries both the current token hash and its parent, so presenting the parent proves the chain forked
and revokes the family — reuse detection with no blacklist. We do not need it (§5.5 explains why),
but it is the reference implementation of RFC 9700 §2.2.2 if you ever do.

### `alexedwards/scs` — the best API in the survey, and why we still don't use it

```go
type Store interface {
	Delete(token string) error
	Find(token string) (b []byte, found bool, err error)
	Commit(token string, b []byte, expiry time.Time) error
}
```

Three methods. The store knows nothing about cookies, HTTP, users, or expiry policy — opaque bytes,
opaque token, a TTL. Optional capability interfaces (`CtxStore`, `IterableStore`) are detected by
type assertion at the manager layer, so you get context propagation and session enumeration *if* the
store supports them. `RenewToken` is the session-fixation fix. `HashTokenInStore` means a database
dump yields no usable sessions. Study this API.

**We do not adopt it**, for one reason: an interface exists to let you swap implementations, and
here there is nothing to swap. Postgres is the premise of the whole library — the pitch in §2 is
that your sessions live next to your data so you can join and transact against them. A `Store`
interface would be an abstraction with exactly one implementation, and it would forbid the one thing
we actually want, which is `SELECT` with a `JOIN`. See §18 for the ceiling this puts on us.

Its weak defaults are worth inverting rather than copying: `Cookie.Secure` defaults to `false`, and
the codec is `encoding/gob`.

### The authorization tier

**Casbin** — the model/policy split is a legitimately good idea; you can move ACL→RBAC→ABAC by
editing a config file. The costs: `Enforce` is O(n) over the whole policy set with an *interpreted*
expression evaluator per row (reported: ~1.6M rows → 12–18s to construct the enforcer, 1.2–1.5s per
check — issues #516, #681, #778); the entire policy sits in memory in every process; watcher-based
invalidation is eventually consistent **with no version token**, so you get Zanzibar's "new enemy
problem" with no way to detect it; and the matcher string is effectively `eval()` over request data.

**OpenFGA / Ory Keto** — Zanzibar's relationship-tuple model is the right abstraction for
sharing-heavy hierarchical permissions (documents in folders in orgs, user-defined sharing), and
`BatchCheck` is genuinely the right primitive for GraphQL field-level authorization. It is overkill
for "users have roles, roles have permissions", it needs a running server, and it **re-splits your
data**: "list docs I can view, joined with metadata, sorted, paginated" becomes `ListObjects` → up
to 1000 IDs → `WHERE id = ANY($1)` → sort in Go. That is the Kratos problem wearing a different hat.

### The gap, stated once

> There is no **embedded, Postgres-native, protocol-complete** Go auth library: one that owns the
> users table *inside your database*, has zero package-level state, uses compile-time contracts
> rather than `MustBeX` panics, ships secure defaults with loud escape hatches, and integrates with
> GraphQL and Postgres RLS as first-class concerns rather than as an afterthought.

That is what you are building. The two things it will have that nothing else does are the **schema
coverage check** (§8.2) and **`Scope` predicates** (§8.3).

---

## 3 · Module layout

Mirror luima's shape. It is a good shape and consistency is worth something.

```
github.com/ulas96/kal
├── kal.go        # hand-maintained root shim: Config, New, the common types
├── session/           # tokens, cookies, issue/rotate/revoke/list, the JWT leg
├── authn/             # argon2id, registration, login, verify, reset, invite
├── authz/             # Principal, roles, Scope, the @auth directive, AssertAuthCoverage
├── kalerr/       # the error contract. Imports nothing else in kal
├── migrations/        # *.sql behind an embed.FS
├── oauth/             # optional: zitadel/oidc RP. Its deps stay out of the core
├── mfa/               # optional: TOTP. Its deps stay out of the core
└── tests/             # every test and runnable example, package tests
```

Rules carried over from luima, and why:

- **`kal.go` is a hand-maintained shim.** Types are aliases so field changes carry for free;
  generic functions are wrappers because Go has no alias for a generic function. A genuinely new
  exported symbol is invisible from the root until added by hand — assert this in `tests/` the way
  `tests/luima_test.go:29-55` does.
- **`kalerr` imports nothing else in kal**, so a package that must return a typed auth
  error need not pull in gqlgen or Fiber.
- **All tests live in one `package tests` outside the packages they exercise**, so they can only
  reach the exported surface — the same view a consumer has. If a test cannot be written from
  outside, the exported surface is insufficient and *that* is the bug.
- **`oauth/` and `mfa/` are separate packages** specifically so `zitadel/oidc` and `pquerna/otp` stay
  out of the dependency graph of anyone who does not use them. This is why MFA can slip to v1.1
  without touching the core.

### Dependencies, and what each one costs

| dependency | why | already in luima's graph? |
|---|---|---|
| `github.com/ulas96/luima` | crud, the error contract, `Mount` | — |
| `github.com/go-pg/pg/v10` | the driver | yes, direct |
| `github.com/99designs/gqlgen` | directive and extension types | yes, direct |
| `golang.org/x/crypto` | `argon2`, `hkdf` | **yes, indirect** |
| `golang.org/x/sync` | `semaphore` for the Argon2 bound | **yes, indirect** |
| `github.com/golang-jwt/jwt/v5` | the optional JWT leg only | no — new |
| `github.com/zitadel/oidc/v3` | `oauth/` only | no — new, opt-in |
| `github.com/pquerna/otp` | `mfa/` only | no — new, opt-in |

The core adds **one** new dependency (`golang-jwt/jwt/v5`), and only for the JWT leg. Argon2 — the
single most important primitive in the library — costs nothing new, because `golang.org/x/crypto` is
already in luima's graph. Prefer this outcome; every dependency in an auth library is a supply-chain
surface you are asking consumers to accept.

**Do not add:** an SMTP client, a template engine, a QR encoder, a UUID library (`crypto/rand` +
`encoding/hex` covers it, and go-pg already pulls `google/uuid` if you want it), a validation
library, a policy engine, a migration framework.

---

## 4 · The context seam

This is the part nobody else has solved for this stack, so it is worth getting exactly right.

### The architectural call: `net/http` middleware, mounted inside the adaptor

kal's core is written against `net/http` and `context.Context`, not against `fiber.Ctx`. It
plugs in through `luima.Config.HTTPMiddleware` (patch P1, §1).

**Why, in four facts read from the pinned versions:**

1. `adaptor.HTTPHandler` gives the handler a `*fasthttp.RequestCtx` as its request context, so
   Fiber's `c.SetContext` is discarded and no deadline survives (§1, S-02).
2. A `func(http.Handler) http.Handler` mounted inside the adaptor receives a real `*http.Request`
   with cookies already parsed, and `r.WithContext(...)` is exactly what gqlgen passes down —
   so a resolver's `ctx.Value` sees it, typed, with no `Locals` string keys.
3. Response headers survive. `fasthttpadaptor` copies them with `ctx.Response.Header.Add(k, v)` per
   value, and fasthttp's `Add` routes `Set-Cookie` into its dedicated cookie list rather than the
   generic header map. **`http.SetCookie(w, …)` works through the adaptor.**
4. It is portable. The same middleware runs on chi, echo, or plain `net/http`. kal is not
   Fiber-locked, which is a strictly larger audience for the same code.

### Writing a cookie from a resolver

A login mutation must set a session cookie, but a resolver only has `context.Context`. gqlgen's
transports call `w.WriteHeader` after `DispatchOperation` returns, so in practice resolvers run
first — but do not rely on that, because it is a transport implementation detail and it differs
between `transport.POST` and the streaming paths.

The robust shape is a jar plus a wrapped `ResponseWriter`:

```go
// jar @notice Collects cookies a resolver asks for, and flushes them exactly once, at the moment
// the response headers are written.
//
// @dev Resolvers may run concurrently (gqlgen runs sibling fields in goroutines), so the mutex is
// load-bearing, not defensive. And the flush is on first Write/WriteHeader rather than after
// ServeHTTP returns because net/http ignores header mutations after WriteHeader — relying on
// fasthttpadaptor's buffering, which happens to read the header map at the end, would work here
// and break on any other router.
type jar struct {
	mu      sync.Mutex
	cookies []*http.Cookie
}

func (j *jar) add(c *http.Cookie) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.cookies = append(j.cookies, c)
}

type flushWriter struct {
	http.ResponseWriter
	jar  *jar
	once sync.Once
}

func (w *flushWriter) flush() {
	w.once.Do(func() {
		w.jar.mu.Lock()
		defer w.jar.mu.Unlock()
		for _, c := range w.jar.cookies {
			http.SetCookie(w.ResponseWriter, c)
		}
	})
}

func (w *flushWriter) WriteHeader(code int)      { w.flush(); w.ResponseWriter.WriteHeader(code) }
func (w *flushWriter) Write(b []byte) (int, error) { w.flush(); return w.ResponseWriter.Write(b) }
```

**Cookie-name ownership.** fasthttp's `Add` appends rather than replaces, so if Fiber-side middleware
(`csrf`, `session`) sets a cookie with the same name that a resolver also sets, you emit two
`Set-Cookie` headers with that name. Browsers take the last, but the order is incidental. **One layer
owns each cookie name.** kal owns `__Host-kal_session`; document that and do not let Fiber's
session middleware near it.

### `Principal` — what a resolver actually gets

```go
// Principal @notice The authenticated caller, as a resolver sees it.
//
// @dev Everything on it is resolved once per request by the middleware, so a directive or a
// resolver can check it without touching the database. That is deliberate: a directive on a field
// of a list type runs once per row, and an authorization check that costs a query is an N+1 that
// only appears under load.
type Principal struct {
	UserID    string    // never empty for an authenticated principal
	SessionID string    // the session this request authenticated with
	Roles     []string  // resolved at login and refreshed on session rotation
	AuthAt    time.Time // when the session was established — for absolute-age policies
	MFAAt     time.Time // zero if MFA was never satisfied on this session; drives step-up
	Email     string
	Verified  bool
}

// From @notice Returns the caller, and whether there is one.
func From(ctx context.Context) (*Principal, bool)

// Require @notice Returns the caller, or a typed UNAUTHENTICATED error for a resolver to return.
func Require(ctx context.Context) (*Principal, error)
```

Two functions, no `MustFrom`. A panicking accessor in an auth library turns a missing middleware
into a 500 with a stack trace; `Require` turns it into an `UNAUTHENTICATED` error, which is both
correct and what the client needs.

Use an **unexported struct key type** for the context value. An exported or string key lets any
package in the binary forge a principal.

### Anonymous is not an error

**The middleware never returns 401.** It resolves a session or it does not, populates the context
accordingly, and calls `next`. The graph decides.

This matches gqlgen's own recipe, and the reason is structural: one GraphQL endpoint serves public
and private fields in the same document. A transport-level 401 makes every public field unreachable
for logged-out users, and turns "show me the public feed and, if I'm logged in, my unread count"
into two round trips.

The corollary: **an invalid or expired session token is also not an error** at the middleware layer.
Clear the cookie, proceed anonymously. The only thing the middleware rejects outright is a malformed
request it cannot parse.

---

## 5 · Component decisions

Each entry: what it is, why this one, what it costs, and what breaks if you deviate.

### 5.1 · Password hashing — Argon2id, hand-rolled over `x/crypto/argon2`

**Parameters:** `m=19456` (19 MiB), `t=2`, `p=1`, 16-byte salt, 32-byte key. This is one of OWASP's
listed equivalent-security configurations and the best balance of the set for a web login path.

**`p` is pinned to 1 and must never be `runtime.NumCPU()`.** `alexedwards/argon2id` — the wrapper
everyone reaches for — defaults `Parallelism: uint8(runtime.NumCPU())`. That makes the cost of
hashing a password depend on which machine happened to serve the request: a 4-vCPU box and a
64-vCPU box do different work for the same password, capacity planning becomes impossible, and an
autoscaler silently changes your security posture. Verification still works, because `p` is encoded
in the stored hash, which is precisely why the bug survives review. **This is the reason to write
~60 lines instead of taking the dependency.**

**Encoding: PHC string format**, so parameters travel with the hash:

```
$argon2id$v=19$m=19456,t=2,p=1$<base64-salt>$<base64-hash>
```

This buys **rehash-on-login**: on a successful verify, if the stored parameters are weaker than the
configured ones, re-hash with the current parameters and update the row inside the same transaction.
It is the only mechanism that lets you raise cost over time without a password reset.

**Comparison uses `crypto/subtle.ConstantTimeCompare`** on the derived key. Note the sharp edge:
`ConstantTimeCompare` returns in O(1) when the *lengths* differ, so it leaks length. Irrelevant here
(the key is always 32 bytes) but relevant anywhere you compare variable-length secrets — there, hash
both sides first.

**The memory-cost DoS, which every Argon2 tutorial omits.** 19 MiB × concurrent logins. One hundred
concurrent login attempts is 1.9 GB of allocation, and nothing in the stack bounds it — luima has no
rate limiting, and `PoolSize` defaults to `10 × NumCPU`, so the database is not the bottleneck
either. **This is a remote unauthenticated OOM.** Bound it:

```go
// hashSem @notice Bounds concurrent Argon2 work.
//
// @dev m=19456 means every in-flight hash holds 19 MiB. Without this, N concurrent login attempts
// allocate 19N MiB and an unauthenticated caller can OOM the process — the parameter that makes
// the hash strong is the same parameter that makes it a DoS primitive. Requests beyond the bound
// queue; the acquire honours ctx, so a client that gave up does not hold a slot.
//
// ponytail: a process-local semaphore, so the bound is per-replica. Behind N replicas the real
// ceiling is 19·limit·N MiB — size it against the pod memory limit, not the node's.
var hashSem = semaphore.NewWeighted(int64(runtime.GOMAXPROCS(0)))
```

Acquire with the request context and a short timeout; on failure return a `429`-shaped error, not a
500. `golang.org/x/sync` is already in luima's graph.

**Timing equalization.** On an unknown user, verify the submitted password against a fixed dummy
hash computed at package init with the same parameters:

```go
// dummyHash @notice A real Argon2id hash of a value nothing can match.
//
// @dev Verified against when the account does not exist, so the unknown-user path costs the same
// as the wrong-password path. Not a random sleep: a sleep has a different distribution from a
// hash, and an attacker averaging over many samples sees the difference. Traefik shipped exactly
// this bug in its BasicAuth middleware (GHSA-g3hg-j4jv-cwfr).
```

Both paths then return the **same** error (§11). ASVS 6.3.8 requires that valid users not be
deducible from messages, status codes, *or response times*.

**Policy** (ASVS 6.2): minimum 8 characters, 15 recommended; **maximum at least 64**; **no
composition rules**; **no periodic rotation**; verify the password exactly as received — no
truncation, no case-folding, no Unicode normalisation that loses information; paste and password
managers must work.

**Why not bcrypt.** Its 72-byte truncation is incompatible with "accept at least 64 characters and
verify exactly as received", and working around it by pre-hashing is a trap: a SHA-2 digest
containing a null byte truncates the effective input, and it enables password shucking. Support
*verifying* legacy bcrypt hashes so people can migrate — rehash-on-login handles the transition for
free — but never mint one.

**Breach checking (optional, default off).** HIBP Pwned Passwords k-anonymity: send the first 5 hex
characters of `SHA-1(password)` to `api.pwnedpasswords.com/range/{prefix}` with `Add-Padding: true`,
match suffixes locally. No API key, no rate limit. It must be **off by default** — it is an outbound
network call in the authentication path and a library must not add one silently — and when on it
must **fail open** with a ~2s timeout and a cached response. The SHA-1 here is a lookup key, not a
security decision; do not let a reviewer talk you out of it.

### 5.2 · Session tokens

32 bytes from `crypto/rand`, base64url-encoded (43 characters). Store **only `SHA-256(token)`**.

**Why SHA-256 and not Argon2.** The token is 256 bits of CSPRNG output — there is nothing to
brute-force. A password-hashing KDF would cost ~100 ms of CPU *on every authenticated request* and
buy exactly zero security. Slow hashing defends low-entropy secrets; this is not one.

**Why hash at all.** A read-only SQL injection, a leaked backup, or a log that captured a query
yields hashes, not live sessions. This is the same reasoning as `scs`'s `HashTokenInStore`, which is
rare and correct.

**Why not selector/verifier.** authboss's pattern exists because you need an *indexable* lookup key
that is not itself the secret. Hashing already gives you exactly that — the hash is derived,
non-secret, and indexable — so the split adds a column and a code path for nothing. Use hash-and-index
for session tokens *and* for emailed tokens (§5.6); one design, one set of tests.

**Lookup is a single indexed equality on the hash.** That is not constant-time, and it does not need
to be: the attacker cannot enumerate 256-bit preimages, so B-tree probe timing reveals nothing
actionable.

### 5.3 · The cookie

```
Set-Cookie: __Host-kal_session=<token>; Path=/; Secure; HttpOnly; SameSite=Lax
```

- **`__Host-`** is the important part. The prefix makes the browser *reject* the cookie unless it has
  `Secure`, `Path=/`, and **no `Domain` attribute** — which means a compromised or attacker-controlled
  sibling subdomain **cannot overwrite it**. That property is what makes session fixation via cookie
  injection impossible, and it is what would make a signed double-submit CSRF token safe if you ever
  needed one. ASVS 3.3.1 and 3.3.3.
- **`SameSite=Lax`**, not `Strict`. `Strict` drops the cookie on top-level navigation from another
  site, which breaks every link you send by email — including your own verification links, so the
  user clicks "verify" and lands logged out. `Lax` is not sufficient on its own; §9.2 covers the rest.
- **`HttpOnly`** always. Nothing in the token is useful to client-side script.
- **No `Insecure` config knob.** `Secure` cookies are accepted from `http://localhost` by Chrome and
  Firefox, so local development works unmodified. Safari is stricter; the answer there is a local
  development certificate (`mkcert`), not a flag. A config field that disables `Secure` will be
  found by someone and set in production.
- **`Partitioned`** (CHIPS) only if the application is embedded cross-site in an iframe. It forces
  `SameSite=None`, which gives up the CSRF property, so it is opt-in with that cost documented.

### 5.4 · Sessions

The default and primary credential. Table in §7.

**Operations:**

```go
func (s *Sessions) Issue(ctx, tx orm.DB, userID string, meta Meta) (token string, err error)
func (s *Sessions) Lookup(ctx, db orm.DB, token string) (*Principal, error)
func (s *Sessions) Rotate(ctx, tx orm.DB, sessionID string) (token string, err error)
func (s *Sessions) Revoke(ctx, tx orm.DB, sessionID string) error
func (s *Sessions) RevokeAllForUser(ctx, tx orm.DB, userID string) error
func (s *Sessions) List(ctx, db orm.DB, userID string) ([]SessionInfo, error)
```

**Rotation on every privilege change** — login, MFA satisfaction, password change, and any
impersonation start or stop. Not just login. This is ASVS 7.2.4 and it is the session-fixation fix:
a token an attacker planted before authentication must not survive authentication.

**Two expiries, both enforced:** an *idle* timeout (default 12h) and an *absolute* lifetime (default
14d). ASVS 7.3.1 and 7.3.2 want both, documented and justified. One without the other is either a
session that lives forever for an active user, or one that logs out a working user mid-task.

**`last_seen_at` write amplification.** Refreshing the idle window on every request means a write per
request. Bound it in the `WHERE`:

```sql
update auth_sessions
   set last_seen_at = now(), idle_expires_at = now() + $2
 where id = $1
   and last_seen_at < now() - interval '60 seconds'
```

```go
// ponytail: idle expiry has 60s granularity because refreshing it on literally every request turns
// one read into one write. Tighten the interval if your idle timeout is measured in minutes.
```

**The honest cost of sessions versus a JWT:** one indexed `SELECT` per authenticated request. That is
the price of revocation, and it is worth paying. If it ever measurably is not, the upgrade path is a
short-TTL in-process cache keyed on the token hash — at which point revocation lags by the TTL, so
the cache TTL becomes your revocation SLA. Do not ship the cache in v1; measure first.

### 5.5 · The JWT leg — opt-in, minted *from* a session

**This exists for exactly one reason: a service that cannot reach your session table needs to verify
a caller.** If it can reach the table, it should; the session is strictly better.

```go
// Token @notice Mints a short-lived bearer token for a service that cannot reach the session table.
//
// @dev Derived from a live session, never a replacement for one. The session cookie remains the
// long-lived credential, which is why this library ships no refresh token: there is nothing to
// rotate, no reuse-detection family to track, and no two-tab race to lose sleep over. That entire
// subsystem — the largest single chunk of every JWT-first auth library — does not exist here.
func (s *Sessions) Token(ctx context.Context, ttl time.Duration, audience string) (string, error)
```

Non-negotiable rules, each with its reason:

- **EdDSA (Ed25519) only.** Not HS256 — a shared secret means every verifier can also *mint*, so any
  service you hand it to can impersonate any user. Not RS256 — larger, slower, and more parameters to
  get wrong.
- **The algorithm is fixed at construction and never read from the token header.** This kills the
  algorithm-confusion class structurally rather than by discipline. Pass
  `jwt.WithValidMethods([]string{"EdDSA"})` as well, and type-assert `token.Method` inside the
  keyfunc — belt and braces, because the keyfunc receives the *parsed but unverified* token, meaning
  `alg` and `kid` are attacker-controlled at that point.
- **`aud`, `iss` and `exp` are all mandatory and all validated.** Use
  `jwt.WithAudience`, `jwt.WithIssuer`, `jwt.WithExpirationRequired()` — note that v5 does **not**
  require `exp` by default, so a token without one validates forever. Hasura's documentation calls
  failing to validate `aud` "a major security vulnerability", and it is right: against a shared
  multi-tenant JWKS, skipping `aud` means a token minted for *any other tenant of that provider*
  authenticates against your API.
- **TTL is hard-capped at 5 minutes in code**, not in config. There is no revocation, so **the TTL
  *is* the revocation window**. A config field inviting `24h` is a config field someone sets.
- **A `sid` claim** carries the session ID, so a verifier that *can* reach the database may check
  liveness and close the gap entirely.
- **Never return a joined error from verification.** This is the CVE-2024-51744 lesson:
  `ParseWithClaims` returned joined errors, so `errors.Is(err, jwt.ErrTokenExpired)` was true even
  when the signature was invalid, and the near-universal "expired, let me refresh" branch happily
  processed forgeries. Return one error, fail closed, and check fatal conditions before benign ones.
- **`jwt.NewValidator`** validates claims *without* the signature. Never expose it, never call it.

**Key management.** One Ed25519 keypair; `kid` is the base64url SHA-256 thumbprint of the public key.
Ship a JWKS handler (~25 lines) because a non-Go verifier has no other way in, and support two active
keys so rotation is not an outage.

**Why not PASETO.** PASETO v4 is genuinely better designed — no `alg` header, key types that make
misuse a compile error, `NewParser()` checking expiry by default with the escape hatch named
`NewParserWithoutExpiryCheck()`. But interop is the *only* reason this leg exists, and the service on
the other end expects a JWT. For internal artifacts where we would be both issuer and verifier
(reset tokens, OAuth state) we use random DB-backed tokens instead, which are simpler and revocable —
so there is nothing left for PASETO to do, and the dependency does not get added.

### 5.6 · Emailed tokens — one design for verify, reset and invite

32 bytes from `crypto/rand`, base64url. Store `SHA-256`, a `purpose`, an expiry and a `consumed_at`.

**Consumption is a single statement.** This is the whole design:

```sql
update auth_tokens
   set consumed_at = now()
 where token_sha256 = $1
   and purpose      = $2
   and consumed_at is null
   and expires_at   > now()
returning user_id;
```

Zero rows means invalid, expired, or already used — indistinguishable, which is correct. **Never
read-then-write**: that leaves a window in which a double-submitted reset link is consumed twice, and
"user double-clicked the email link" is not a rare event.

**TTLs:** email verification 24h, invite 7d, **password reset 15 minutes** (OWASP says ≤1 hour, ideally
much less).

**Password reset rules, each of which is a real incident somewhere:**

- **Do not mutate the account on a reset *request*.** Do not lock it, do not clear the password, do
  not set a flag that changes login behaviour. Anything you change on request is a denial-of-service
  that anyone can trigger against any email address.
- **Revoke every session on a successful reset** (`RevokeAllForUser`), and **do not auto-login**.
  Send the user through normal login — which also re-triggers MFA.
- **The reset URL origin comes from `Config.BaseURL`, never from the request `Host` header.** Host
  header injection turns your reset email into an attacker-controlled link. Do not offer a way to
  derive it from the request.
- **The token is in a URL, so it leaks via `Referer`.** Any third-party asset on the reset page —
  analytics, a font, a CDN script — receives it. Mitigations, in the documentation the consumer
  reads: send `Referrer-Policy: no-referrer` on that page, serve it with zero third-party assets, and
  best, exchange the URL token immediately for a short-lived session-bound token and
  `history.replaceState` the URL so it leaves history.
- **Rate-limit both the request and the submission**, per account and per IP.

**Enumeration.** Registration, reset-request, and login all return an identical response and take
comparable time whether or not the account exists. For registration, that means: always answer "check
your email", and send *either* a verification email *or* a "someone tried to register with your
address" email. The second one is the part people skip, and it is what makes the identical response
honest rather than merely opaque.

### 5.7 · `Mailer` is one method

```go
// Mailer @notice Delivers the library's transactional messages.
//
// @dev One method, and the library ships no SMTP client and no template engine. Bundling email
// rendering is how auth libraries become unadoptable — go-pkgz/auth requires an avatar store, which
// is the same mistake one step further along.
type Mailer interface {
	Send(ctx context.Context, to string, msg Message) error
}

// Message @notice What to send. Subject and a plain-text body with the URL already built.
type Message struct {
	Kind    MessageKind // Verify, Reset, Invite, ...
	Subject string
	Text    string
	URL     string // pre-built from Config.BaseURL; provided separately so a consumer can re-template
}
```

`Config.Mailer` is required with no default. Provide a `LogMailer` for development that writes the
URL to a logger, and make its name obviously unsuitable for production. Do **not** provide a silent
no-op default: "I forgot to configure email" must fail loudly at construction, not silently at 3am
when nobody can reset a password.

---

## 6 · The `crud.Create` collision — read this before writing registration

luima's `crud.Create` classifies SQLSTATE 23505 into a client-visible message:

```go
// crud/crud.go:104-106
if luimaerr.SQLState(err) == "23505" { // unique_violation
	return nil, &luimaerr.CustomError{UserMessage: label + " already exists", InternalError: err}
}
```

with the documented usage `Create(ctx, db, u, "user "+id)`. On a signup-shaped mutation **that is a
textbook account-enumeration oracle**: send an email address, learn from the error whether it is
registered. It is finding C-04 in the security review, and it is working exactly as designed — the
whole point of `Create` is that a duplicate reaches the client.

**Therefore: `kal` must not use `luima.Create` for `auth_users`, or for any table keyed on an
email address.** Registration inserts directly, treats 23505 as *success* from the client's point of
view, and branches only on which email to send.

This is called out in its own section because it is the documented happy path, it is the shortest
code, and an implementing agent will reach for it by reflex. Write a test that fails if the
registration response differs between a new address and an existing one — byte for byte.

The same caution applies to the `label` generally: it is caller-supplied text returned verbatim in
`errors[].message`. luima's godoc says "assume it is public"; it should also say "assume it is
untrusted". Never build a label from user input in kal.

---

## 7 · The Postgres schema

Plain `.sql` files in `migrations/`, exposed through an `embed.FS` so a consumer can run them with
whatever migration tool they already have. **No migration framework** — running SQL in order is not a
problem that needs a dependency, and every consumer already has an opinion about it.

Prefix every table `auth_`. Ship a `Config.TableSchema` for people who want them in their own Postgres
schema, applied with `pg.Ident` (never string concatenation).

```sql
create table auth_users (
    id              uuid        primary key default gen_random_uuid(),
    email           text        not null,
    password_hash   text,                       -- null for OAuth-only accounts
    email_verified  boolean     not null default false,
    created_at      timestamptz not null default now(),
    updated_at      timestamptz not null default now(),
    deleted_at      timestamptz
);

-- Case-insensitive uniqueness over LIVE rows only.
--
-- Partial, because a plain unique index would block a user from ever re-registering an address
-- belonging to a soft-deleted account. And NOT `unique (lower(email), deleted_at)` — that is
-- backwards: NULLs are distinct in a unique constraint, so it would permit unlimited *active*
-- duplicates while forbidding what we actually want.
create unique index auth_users_email_live_key
    on auth_users (lower(email)) where deleted_at is null;
```

**Why `lower()` and not `citext`.** Postgres's own `citext` documentation recommends against it, and
one of its limitations is a security problem here: *"The schema containing the citext operators must
be in the current `search_path`; if it is not, the normal case-sensitive text operators will be
invoked instead."* A `search_path` change would silently turn `Admin@x.com` and `admin@x.com` into
two distinct accounts, with no error. A `lower()` expression index has no extension dependency (which
also matters on RDS/Cloud SQL/Neon), no `search_path` hazard, and explicit, greppable semantics.
Normalise to lowercase in Go on write as well, so the index is belt-and-braces rather than the only
thing standing between you and a duplicate.

```sql
create table auth_sessions (
    id              uuid        primary key default gen_random_uuid(),
    token_sha256    bytea       not null unique,     -- only the hash is stored
    user_id         uuid        not null references auth_users(id) on delete cascade,
    created_at      timestamptz not null default now(),
    last_seen_at    timestamptz not null default now(),
    idle_expires_at timestamptz not null,
    expires_at      timestamptz not null,            -- absolute lifetime
    mfa_at          timestamptz,                     -- when MFA was last satisfied; drives step-up
    user_agent      text,
    ip              inet,
    revoked_at      timestamptz
);
create index auth_sessions_user_live_idx
    on auth_sessions (user_id) where revoked_at is null;

create table auth_tokens (
    token_sha256 bytea       primary key,
    user_id      uuid        not null references auth_users(id) on delete cascade,
    purpose      text        not null,   -- 'verify' | 'reset' | 'invite'
    expires_at   timestamptz not null,
    consumed_at  timestamptz,
    created_at   timestamptz not null default now()
);
create index auth_tokens_user_idx on auth_tokens (user_id, purpose);

create table auth_roles (
    name        text primary key,
    description text not null default ''
);
create table auth_user_roles (
    user_id uuid not null references auth_users(id) on delete cascade,
    role    text not null references auth_roles(name) on delete cascade,
    primary key (user_id, role)
);

create table auth_oauth_accounts (
    issuer     text        not null,     -- namespaced: a bare `sub` collides across providers
    subject    text        not null,
    user_id    uuid        not null references auth_users(id) on delete cascade,
    created_at timestamptz not null default now(),
    primary key (issuer, subject)
);

create table auth_oauth_states (
    state_sha256 bytea       primary key,
    verifier     text        not null,   -- PKCE code_verifier
    nonce        text        not null,
    issuer       text        not null,
    return_to    text        not null,
    expires_at   timestamptz not null,
    consumed_at  timestamptz
);

create table auth_login_attempts (
    scope       text        not null,    -- 'user' | 'ip'
    key         text        not null,    -- user id, or the client address
    failures    int         not null default 0,
    last_fail   timestamptz not null default now(),
    primary key (scope, key)
);
```

MFA tables ship with the `mfa/` module (§14), including `auth_totp_used(user_id, time_step)` whose
composite primary key is the entire replay defence.

**`gen_random_uuid()`** is built in since Postgres 13; no `pgcrypto`, no UUID library.

---

## 8 · Authorization — three layers

Each layer catches what the one above it misses. Ship all three; they are not alternatives.

### 8.1 · Layer one: the `@auth` directive (coarse, declarative)

**One composed directive. Never a stack.**

```graphql
directive @auth(
  requires: AuthLevel! = AUTHENTICATED
  roles:    [String!]
  mfa:      Boolean
) on FIELD_DEFINITION | OBJECT

enum AuthLevel { ANONYMOUS AUTHENTICATED }
```

which gqlgen generates as:

```go
type DirectiveRoot struct {
	Auth func(ctx context.Context, obj any, next graphql.Resolver,
		requires AuthLevel, roles []string, mfa *bool) (res any, err error)
}
```

Note the argument mapping, which you will get wrong otherwise: **a schema argument becomes a
trailing positional Go parameter in declaration order; non-null gives a value type, nullable gives a
pointer, and a default value does *not* make a nullable argument non-pointer.**

**Why one directive and not `@auth @hasRole(ADMIN)`.** gqlgen chains directives **inside-out** — the
generated code builds `directive1` wrapping `directive0`, then `directive2` wrapping `directive1`,
so the *last*-declared directive is the outermost caller and runs **first**. Written left to right,
`@auth @hasRole(role: ADMIN)` runs `hasRole` before `auth`, which is the opposite of how every reader
parses it. Input and argument directives execute earlier still. Rather than document an ordering
nobody will remember, compose the checks into one directive where the order is yours.

**The implementation must be O(1) from the context and must never touch the database.** A directive
on a field of a list type runs once per row: a 100-item list is 100 invocations. An authorization
check that costs a query is an N+1 that only shows up under production load, and luima ships no
dataloader to soften it. Everything the directive needs is already on `Principal` (§4).

**A nil directive implementation is a runtime error, not a compile error.** Forget to wire
`c.Directives.Auth` and every annotated field fails at request time with `"directive auth is not
implemented"`. There is no startup validation in gqlgen. §8.2 fixes that.

**Nullability interacts with denial.** A directive returning an error on a nullable field yields
`null` plus an error entry; on a non-null field it nulls the *parent*, which can blank an entire
object. Document this, and prefer nullable types for fields that are conditionally visible.

### 8.2 · `AssertAuthCoverage` — the forty lines nothing else ships

```go
// AssertAuthCoverage @notice Fails if any mutation or non-public field can be reached without an
// @auth annotation, or if any directive implementation is nil.
//
// @dev Deny-by-default, enforced by a test rather than by discipline. The failure mode of
// resolver-level authorization is a *forgotten* check, and a forgotten check is invisible: it
// compiles, it passes review, and it returns data. Walking the schema is the only way to see the
// absence of something.
//
// Call it from a test, not from New — a schema author adding a public field should get a red test
// they can annotate away, not a server that refuses to boot.
//
// @param schema   the generated executable schema
// @param exempt   field paths deliberately public, e.g. "Query.health", "Mutation.login"
// @return error   naming every unannotated field, or nil
func AssertAuthCoverage(schema graphql.ExecutableSchema, exempt ...string) error
```

Walk `schema.Schema()`: for every field of `Mutation`, and every field of every type not carrying
`@auth(requires: ANONYMOUS)` at the object level, require an `auth` directive on the field or its
parent. Report all failures at once, not the first — an implementer wants the whole list.

Also assert the `DirectiveRoot` fields are non-nil, which catches the runtime-error trap from §8.1.

This is the single highest-value component in the library. It is why a growing schema stays secure.

### 8.3 · Layer two: `Scope` — authorization in the WHERE clause

```go
// Scope @notice The caller's ownership predicate, for luima's crud options.
//
// @dev The real enforcement. A boolean from a policy engine tells you whether Alice may read doc 7;
// it does not stop the list query from returning every document, and it cannot be applied to a
// DELETE without a read-then-check round trip that has a TOCTOU window. Composing the predicate
// into the statement has neither problem, and the database enforces it.
//
// Returns a no-op for a principal holding the configured bypass role, so an admin path is one
// explicit branch rather than a second code path.
//
// @param ctx    the resolver context
// @param column the owning column on the model's table
// @return func  a query option; passes through unchanged when the caller is anonymous, which
//               yields a predicate that matches nothing
func Scope(ctx context.Context, column string) func(*orm.Query) *orm.Query
```

```go
// The resolver that was previously impossible:
func (r *mutationResolver) DeleteDoc(ctx context.Context, id string) (bool, error) {
	return luima.Delete(ctx, r.DB, &model.Doc{ID: id}, authz.Scope(ctx, "owner_id"))
}
```

**What falls out for free.** A row that exists but is not yours matches nothing, so
`RowsAffected() == 0`, so `crud.Delete` returns `false` and `crud.Update` returns
`label + " not found"`. **"Not yours" and "does not exist" are indistinguishable to the caller** —
which is the correct answer to give an unauthorized caller anyway. You get correct authorization and
correct information disclosure from one variadic parameter, which is why patch P3 (§1) is the
prerequisite that matters most.

**The column name is not user input.** It comes from the resolver, at compile time. If you ever build
a variant that takes it from a request, use `pg.Ident` and an allowlist — `q.Where(fmt.Sprintf(...))`
interpolates raw SQL by design.

**Escape hatch by function type, not interface.** `Scope` returns a `func(*orm.Query) *orm.Query`, so
anyone whose authorization genuinely needs OpenFGA calls `BatchCheck` inside their own closure and
returns `q.Where("id = any(?)", pg.Array(ids))`. No interface with one implementation, no plugin
registry, no `Authorizer` abstraction that exists to be overridden once.

**TOCTOU.** A `Get`-then-check-then-`Update` has a real window between the check and the write.
Every crud helper takes `orm.DB`, so `db.RunInTransaction` plus passing the `*pg.Tx` closes it, and
`SELECT … FOR UPDATE` is expressible through the options. Document it next to `Scope`, because that is
where someone needs it.

### 8.4 · Layer three: Postgres RLS (opt-in, defence in depth)

The point of this layer is that it survives a forgotten check in the layers above.

```go
// WithRLS @notice Runs fn in a transaction whose Postgres session variables carry the caller.
//
// @dev One round trip for both settings, and set_config with a bound parameter rather than
// `SET LOCAL app.user_id = …` — SET is not a parameterizable statement, so the string form would
// force concatenation, which is SQL injection.
func WithRLS(ctx context.Context, db *pg.DB, fn func(orm.DB) error) error {
	p, _ := From(ctx)
	return db.RunInTransaction(ctx, func(tx *pg.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`select set_config('app.user_id', ?, true), set_config('app.roles', ?, true)`,
			p.UserID, strings.Join(p.Roles, ",")); err != nil {
			return err
		}
		return fn(tx)
	})
}
```

```sql
alter table docs enable row level security;
alter table docs force  row level security;      -- see gotcha 3

create policy docs_owner on docs
  to app_user                                     -- always name the role
  using      (owner_id = (select current_setting('app.user_id', true))::uuid)
  with check (owner_id = (select current_setting('app.user_id', true))::uuid);
```

**Four gotchas. Each has silently broken a production deployment.**

1. **`SET LOCAL` outside a transaction is a no-op** — Postgres emits a warning and otherwise does
   nothing. With a connection pool, `db.Exec("SET LOCAL …")` followed by a query lands the setting on
   one pooled connection and the query on another. Everything must be inside `RunInTransaction`.
2. **Plain `SET` (session-scoped) outlives the transaction and leaks to the next request that borrows
   that connection.** That is a cross-tenant data leak, and it only manifests under concurrency. The
   `true` third argument to `set_config` is what makes it transaction-local; it is not optional.
3. **Table owners bypass RLS** unless `ALTER TABLE … FORCE ROW LEVEL SECURITY`, and so do superusers
   and any role with `BYPASSRLS`. Most migration setups connect as the table owner, which means RLS
   silently does nothing. **This is the single most common way RLS deployments are quietly broken.**
   luima's `SECURITY.md` already warns that a privileged connection role makes RLS inapplicable.
4. **Never write a policy where a NULL setting is permissive.** `current_setting('app.user_id', true)`
   returns NULL when unset — the `true` (`missing_ok`) argument is mandatory or it raises instead.
   A policy like `using (current_setting(...) is null or owner_id = ...)` **fails open** on every
   connection that has not been configured. Fail closed, always.

Also: `SET LOCAL` is compatible with PgBouncer in *transaction* pooling mode precisely because it
dies at `COMMIT`. Session-level `SET` is not. Statement pooling breaks this entirely.

Prefer a GUC over `SET ROLE`: a client with any SQL-injection foothold can issue `RESET ROLE` and
escape back to the authenticator role, whereas a GUC-based policy hands out no role-switching
primitive.

### 8.5 · What we refuse to build, and why

- **Casbin** — an interpreted matcher evaluated per policy row, eventually-consistent invalidation
  with no version token, and a second DSL for the team to learn. For "owner_id = me OR role = admin"
  it is strictly worse than one `WHERE` clause on latency, consistency and auditability.
- **OPA / Rego** — more expressive than Casbin, and the practical difficulty is never the policy
  language, it is getting the data to the engine. In-process embedding drags a large dependency for
  a decision Postgres already has the data to make.
- **A Zanzibar tuple store** — right for sharing-heavy hierarchical graphs, wrong for role-based
  access, and it re-splits your authorization data away from your domain data.
- **A policy DSL of our own** — the worst of all of them.

The escape hatch is `Scope`'s function type (§8.3). Anyone who needs one of the above can call it
from inside a closure without the library knowing.

---

## 9 · GraphQL-specific attacks

### 9.1 · Batching and aliasing brute force — the one that must ship in v1

Rate limiters count HTTP requests. GraphQL executes many operations per request:

```graphql
mutation {
  a: login(email: "victim@x.com", password: "aaaa") { token }
  b: login(email: "victim@x.com", password: "aaab") { token }
  # … ×500, one HTTP request, one rate-limit hit
}
```

The same trick against `verifyOtp` reduces a six-digit TOTP to a handful of requests. There is a
PortSwigger lab for exactly this.

Ship it as a gqlgen `OperationInterceptor` registered through `Config.Configure` (patch P2):

- **Deduplicate sensitive fields by policy.** A document containing more than one selection of
  `login`, `verifyOtp`, `requestReset` or `redeemToken` is rejected outright. Not rate-limited —
  rejected. There is no legitimate client that sends two logins in one document.
- **Cap the alias/selection count and the document depth.** gqlparser has no max-depth rule and
  gqlgen ships no extension for one, so this is ~25 lines walking `opCtx.Doc.Operations`. luima's
  `ComplexityLimit` does not help: gqlgen's complexity is per selected field, so 400 levels of
  nesting through a cyclic schema costs ~400 and passes a 1000 limit.
- **Reject array-batched requests on auth operations.**
- **Count failures inside the resolver**, per account *and* per IP (§10) — the transport layer cannot
  see how many login attempts a document contained.

**The one line that will bite you:**

```go
// Denial must be wrapped in graphql.OneShot(graphql.ErrorResponse(...)).
//
// gqlgen's own source warns twice: "If you choose to short-circuit the request and return an error
// without calling next(), you MUST wrap your error response in graphql.OneShot(...). Otherwise,
// streaming transports like WebSockets will loop infinitely."
//
// An auth extension that denies a request is exactly the short-circuit case, so this is not a
// theoretical footnote — it is a hot infinite loop instead of a clean rejection.
return graphql.OneShot(graphql.ErrorResponse(ctx, "too many operations"))
```

### 9.2 · CSRF and the transport rules

**luima's defaults are already sound, and the reason is worth understanding before you change
anything.** Verified in gqlgen v0.17.94:

- `transport.POST.Supports` requires `mediaType == "application/json"`. A cross-site `<form>` cannot
  send that content type without triggering a preflight, so **mutations are not reachable by simple
  CSRF**. No token is required today.
- `transport.GET` rejects non-query operations outright (`"GET requests only allow query
  operations"`), so a mutation cannot ride in on a link or an `<img src>`.

**The rule that must be written down, because everyone gets it backwards:**

> **Never register `transport.UrlEncodedForm`, `transport.MultipartForm` or `transport.GRAPHQL`
> while cookie authentication is on.**

All three are `POST` with **CORS-simple** content types and **no operation-type restriction**, which
means a cross-origin HTML form or `fetch` can execute mutations with ambient cookies and no
preflight. That is a strictly bigger hole than `transport.GET`, which at least refuses mutations.

**Why `SameSite=Lax` is not sufficient on its own:** it permits the cookie on top-level cross-site
navigation using safe methods, and GraphQL-over-GET is a first-class thing (automatic persisted
queries, CDN caching) — so a top-level navigation to a query URL carries the session. It also applies
to the whole registrable domain, so a compromised sibling subdomain defeats it, which is exactly the
hole the `__Host-` prefix closes.

**The defence kal ships** — Apollo's shape, roughly ten lines in the middleware, no token and
no state:

> Require every request to carry at least one of: a `Content-Type` outside
> {`text/plain`, `application/x-www-form-urlencoded`, `multipart/form-data`}, **or** a non-empty
> custom header (`X-Kal-Operation` or `X-Requested-With`).

Those three content types plus a header-free GET are precisely the CORS *simple request* set — the
requests a browser sends cross-origin without a preflight. Requiring anything outside it forces a
preflight the attacker's origin cannot pass. Add an `Origin` / `Sec-Fetch-Site` check as defence in
depth, and remember luima sets **no** `Access-Control-Allow-Origin` anywhere (finding S-03) — the
consumer must configure `cors.New` with an explicit origin list, never `*` with credentials.

If a consumer genuinely needs multipart uploads, they need Fiber v3's `csrf` middleware with
`TrustedOrigins`. Warn them that the v3 config changed (`KeyLookup` → `Extractor`, `Expiration` →
`IdleTimeout`) and that it **panics at startup** if the extractor reads the same cookie it sets.

### 9.3 · Introspection and suggestions

**The counter-intuitive fact:** gqlgen's executor defaults to `DisableIntrospection: true`
(`executor.go:59`), and `extension.Introspection` exists to turn it **on** (`introspection.go:31`).
luima adds it unconditionally at `server/server.go:124` with no off switch (finding S-04).

Ship a per-request mutator through `Config.Configure`, which runs *after* identity is in the context,
so introspection becomes role-gated in one place:

```go
type conditionalIntrospection struct{ Allow func(context.Context) bool }

func (conditionalIntrospection) ExtensionName() string                    { return "ConditionalIntrospection" }
func (conditionalIntrospection) Validate(graphql.ExecutableSchema) error  { return nil }
func (c conditionalIntrospection) MutateOperationContext(ctx context.Context, opCtx *graphql.OperationContext) *gqlerror.Error {
	opCtx.DisableIntrospection = !c.Allow(ctx)
	return nil
}
```

**Also set `srv.SetDisableSuggestion(true)` in production**, and say why in the doc comment:
gqlparser appends `Did you mean …?` to validation errors — that is what luima's `agnivade/levenshtein`
dependency is for — and `luimaerr.PresentError` passes validation errors through *by design*. So with
introspection off, a caller who guesses `nam` still learns there is a `name`. Introspection off is a
smaller attack surface, not confidentiality.

### 9.4 · Subscriptions

Out of scope in luima today. When they land, the rule is gqlgen's own and it is easy to miss:
**a subscription authenticated at connect time can outlive its credential by hours.** Authentication
for a long-lived stream must be re-verified on publish, against the live session, not checked once in
`WebsocketInitFunc`. Cookies are also unreliable for WebSocket auth — authenticate from the init
payload and return an augmented context.

---

## 10 · Rate limiting and lockout

**Hard account lockout is an availability vulnerability wearing a security control's clothes.**
Anyone who knows a victim's email address can lock them out. ASVS 6.1.1 says the quiet part out loud:
you must document how anti-automation defends against credential stuffing *"while preventing
malicious account lockout"*.

Ship, in order:

1. **Exponential backoff per account** — 0s, 1s, 2s, 4s … capped at 30s. Costs an attacker
   everything; costs a user who fat-fingered their password nothing.
2. **Per-IP counters as well as per-account.** Credential stuffing is one attempt against *many*
   accounts, which a per-account counter never sees. This is not optional; it is the only signal that
   catches the actual attack.
3. **Notify the user** on a suspicious pattern — many failures then a success, or a new location.
4. Lockout only as a last resort: time-boxed, self-recoverable, never permanent.

**The delay is applied before the Argon2 verify**, not after. Otherwise the backoff still costs you
19 MiB and a CPU slice per attempt, and the throttle becomes the DoS.

**Storage is Postgres, not Redis, in v1.** You already have it, it is transactional with the login
itself, and an in-memory limiter is simply wrong behind more than one replica — it silently
multiplies your effective limit by the replica count.

```go
// ponytail: per-key counters in Postgres. Every failed login writes one row, so a targeted attack
// against one account is a hot row. Fine to a few hundred attempts/sec; past that the upgrade is
// Redis or a probabilistic counter, and the seam is this one function.
```

Fiber's `limiter` middleware is worth adding at the edge as a blunt second layer, but note its default
`KeyGenerator` is `c.IP()` and it counts **HTTP requests** — so it cannot see the batching attack in
§9.1 at all. The operation-aware counting has to live in the extension and the resolver.

---

## 11 · Errors

`kalerr` mirrors `luimaerr`: it imports nothing else in kal, so any package can return a
typed auth error without pulling in gqlgen or Fiber.

```go
// Error @notice A client-visible auth error with a stable machine-readable code.
type Error struct {
	Code     string // UNAUTHENTICATED | FORBIDDEN | INVALID_CREDENTIALS | RATE_LIMITED | ...
	Message  string // safe for the client. Assume public AND untrusted.
	Internal error  // for the log and for errors.Is/As. Never reaches the client.
}
```

**One error for authentication failure.** Unknown user, wrong password, unverified email, locked
account and disabled account all return the identical `INVALID_CREDENTIALS` on the wire. The real
reason goes to the log. This is ASVS 6.3.8, and it is the same discipline that CVE-2024-51744
punished the absence of: **never return a joined or multi-valued error from a security verification**,
because a caller's `errors.Is` on a benign sentinel will mask a fatal one.

**Presentation.** luima's `PresentError` never sets `gqlerror.Error.Extensions`, so there is no
existing convention — kal establishes one:

```go
// PresentError @notice luima's presenter, plus an extensions.code for auth errors.
//
// @dev Wraps rather than replaces: luimaerr.PresentError's three branches are the redaction
// contract and must keep running. This only adds the code a client needs to distinguish
// "log in again" from "you may not do that".
func PresentError(ctx context.Context, err error) *gqlerror.Error
```

**Two luima behaviours to defend against:**

- **Finding E-01:** luima's `*gqlerror.Error` branch uses `errors.As`, which walks the whole chain, so
  **any error wrapping a gqlerror is returned to the client whole** — message, path and extensions —
  bypassing redaction. Therefore: **kal must never wrap a `*gqlerror.Error`.** If E-01 is fixed
  upstream this stops mattering; until then it is a rule.
- **Finding E-04:** `CustomError.Error()` concatenates the internal cause, so populating a
  `UserMessage` from any `err.Error()` undoes redaction in a line that reads like careful error
  handling. Never build a `Message` from an `error`.

---

## 12 · Config

**Deliberate inversion of luima's invariant.** luima's zero `Config` is the good *development*
configuration — that is why the playground and introspection are on. For an auth library that
polarity is wrong:

> **The zero `kal.Config` is the good *production* configuration, and there is no development
> mode that weakens a security property.**

Anything a developer needs — a shorter TTL, a logging mailer, a seeded admin — is an ordinary field
with an obvious name, never a mode. A `Dev bool` that relaxes a cookie attribute or skips a check is
a vulnerability shipped as a convenience, and it will reach production, because that is what
environment flags do.

Required with no possible default — construction fails loudly if any is missing:

| field | why it cannot default |
|---|---|
| `DB *pg.DB` | there is nothing to default to |
| `BaseURL string` | deriving it from the request `Host` is header injection (§5.6) |
| `Mailer` | a silent no-op means password reset fails at 3am with no error |

Everything else defaults safely: cookie name, idle and absolute TTLs, Argon2 parameters, the hash
concurrency bound, backoff curve, token TTLs. Validate at construction — reject an absolute lifetime
shorter than the idle timeout, a JWT TTL over 5 minutes, a `BaseURL` that is not `https://` outside
localhost.

---

## 13 · OAuth / OIDC

Build on **`github.com/zitadel/oidc/v3/pkg/client/rp`**. It is the only OpenID-certified Go relying
party, and the only one with a generic PKCE API. Do not write another provider aggregator.

**Do not use `markbates/goth`** — the reason is in §2, and it is not a matter of taste: its `state`
value can be fixed by a request query parameter, which is a login-CSRF and account-linking primitive.

### Requirements, each non-negotiable

- **PKCE `S256` on every flow, including confidential clients.** Reject `plain`. RFC 9700 §2.1.1
  makes it a MUST, and §4.8 adds: reject a token request carrying a `code_verifier` when no
  `code_challenge` was present, or the downgrade is free.
- **`state` is one-time, CSPRNG, and stored server-side** in `auth_oauth_states` with a ≤10 minute
  TTL and the same single-statement atomic consume as §5.6. We are not a stateless server; we have
  Postgres. (The encrypted-cookie approach — zitadel's `CookieHandler` — is the correct *stateless*
  answer if you ever need one.) **Never self-encode the verifier into `state`**: `state` echoes back
  through the authorization server and lands in its logs.
- **`nonce`** on every OIDC flow, validated against the ID token, with the token discarded until
  validation succeeds.
- **`redirect_uri` matched by exact string comparison** against a registered allowlist. No wildcards,
  no prefix matching, no "starts with". RFC 9700 §2.1. Open redirects on the redirect URI are how
  authorization codes leak.
- **`iss` (RFC 9207)** whenever more than one authorization server is configured — the mix-up
  defence. And reject a response *missing* `iss` if the server is known to support it, or stripping
  the parameter reopens the hole.
- **No ROPC / password grant.** RFC 9700 §2.4: MUST NOT.
- **Validate `aud`.** Same reasoning as §5.5.

### Account linking is where the takeovers are

Never link an OAuth identity to an existing account by email alone. The rules:

1. Link automatically **only** if the provider asserts `email_verified: true`, **and** the provider is
   on an explicit trusted list in config. A provider that lets users set an arbitrary unverified
   email is an account-takeover machine.
2. Otherwise, require the user to authenticate with the existing credential first and link
   explicitly.
3. **Namespace the subject as `(issuer, subject)`** — the primary key in `auth_oauth_accounts`. A bare
   `sub` collides across providers and lets a user of IdP A claim a user of IdP B (ASVS 6.8.1).
4. When the IdP reports authentication strength (`amr` / `acr`), verify it rather than assuming
   (ASVS 6.8.4).

### Shape

The callback is a redirect; it is not GraphQL-shaped and should not be forced into a mutation.
`kal/oauth` ships two Fiber handlers — `/auth/{provider}` and `/auth/{provider}/callback` —
mounted alongside luima's endpoint. The flow ends by issuing the same session cookie every other path
issues, so nothing downstream knows or cares how the user authenticated.

---

## 14 · TOTP MFA — optional module

A separate package (`kal/mfa`) so `pquerna/otp` stays out of the core's dependency graph, and so
this can slip to v1.1 without touching anything else.

**Parameters: SHA-1, 6 digits, 30-second period.** Deviating breaks compatibility with every
authenticator app people actually have installed. This is interoperability, not cryptography.

**Replay prevention is the thing `pquerna/otp` does not give you, and every integration misses it.**
`totp.Validate` answers "is this code valid right now", not "has this code been used". A stolen code
is reusable for its whole window. ASVS 6.5.1 requires single use. Enforce it with the schema:

```sql
create table auth_totp_used (
    user_id   uuid not null references auth_users(id) on delete cascade,
    time_step bigint not null,
    used_at   timestamptz not null default now(),
    primary key (user_id, time_step)
);
```

Insert inside the verification transaction. **A 23505 means replay** — the composite primary key is
the entire defence, with no cache, no lock and no cleanup job beyond a periodic delete of old rows.

Other requirements:

- **`Skew: 1` maximum** (±30s). Skew already triples the attacker's valid window; anything larger is
  a gift.
- **Recovery codes: 10 codes of 24 base32 characters (~120 bits).** The length is a deliberate
  decision: above ASVS 6.5.2's 112-bit threshold, a fast hash is permitted, so they can be stored as
  `SHA-256(pepper ‖ code)` rather than costing an Argon2 verification each. Single-use via the same
  atomic-consume statement, individually marked, with the remaining count shown to the user.
- **Enrollment requires the current password *and* a successful first code** before MFA becomes
  active. Without the password check, a hijacked session enrolls MFA and locks out the real owner —
  which converts a session compromise into permanent account loss.
- **Step-up** via `auth_sessions.mfa_at` and `@auth(mfa: true)`: the directive requires MFA satisfied
  within a window (default 15 minutes). This is what makes "re-authenticate before changing your
  email" real, and it is ASVS 7.5.1.
- **The forgotten-password flow must not bypass MFA** (ASVS 6.4.3). Easy to get wrong, because reset
  feels like it precedes authentication.
- **Validate against the server clock**, never a client-supplied timestamp (ASVS 6.5.8).
- **No bundled QR encoder.** Emit the `otpauth://` provisioning URI and let the consumer render it —
  rendering is not this library's job, and it would be a new dependency for a string.

---

## 15 · Build order

Six milestones. The first four are v1.0's mandatory core and none may be skipped.

| | milestone | done when |
|---|---|---|
| **M0** | luima patches P1–P3 (§1) | the three failing tests pass; `luima.go` and `CHANGELOG.md` updated |
| **M1** | passwords + sessions + the context seam | a resolver can read a typed `Principal`; login sets a `__Host-` cookie; revoking a session makes the next request anonymous |
| **M2** | account lifecycle | verify, reset and invite all work; reset revokes every session; register and reset responses are byte-identical for existing and non-existing accounts |
| **M3** | RBAC + `@auth` + `Scope` + `AssertAuthCoverage` | `AssertAuthCoverage` fails on an unannotated mutation; a scoped delete returns false for another owner's key |
| **M4** | hardening | two `login` selections in one document are rejected; backoff applies before the Argon2 verify; the RLS helper sets both GUCs in one transaction |
| **M5** | OAuth / OIDC | a full round trip against a real provider, with PKCE and server-side state |
| **M6** | TOTP MFA *(optional, may slip to v1.1)* | a replayed code is rejected; step-up gates an `@auth(mfa: true)` field |

The library is **usable and revocable at the end of M1**. That is the milestone worth optimizing for.

---

## 16 · Repo conventions

Inherit luima's. They are unusually good and consistency across the two modules is worth something.

- **NatSpec doc comments inside ordinary godoc comments:** open with the symbol name, then `@notice`
  (what), `@dev` (why it is written this way), `@param` / `@return`.
- **Comments explain what breaks if the line is removed.** If code moves, the comment moves with it.
  This document is full of examples of the register to aim for.
- **A behaviour change needs a test that fails without it.** New public API needs a godoc example in
  `tests/`. Update `CHANGELOG.md` under `## [Unreleased]`.
- **Every test in one `package tests`** outside the packages it exercises. If a test cannot be
  written from there, the exported surface is insufficient.
- **The DB-backed test creates and drops its own tables** and skips without `DATABASE_URL` — and CI
  **greps `--- PASS:` out of `-v` output**, because a skipped test still reports `ok`. This is
  non-negotiable in an auth library: a green suite that never touched Postgres proves nothing about
  session revocation, unique constraints, or replay prevention. Copy luima's `postgres:16` service
  container and its grep step verbatim.
- **Root shim assertions:** `tests/` asserts that every alias is a genuine alias and every generic
  wrapper instantiates to the sub-package signature, the way `tests/luima_test.go:29-55` does.

**Two things luima lacks that this library must have:**

```make
audit:  ## scan for known vulnerabilities and insecure patterns
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...
	go run github.com/securego/gosec/v2/cmd/gosec@latest ./...
```

wired into `make check` and CI (luima's finding B-01), and **`errorlint` enabled in
`.golangci.yml`** — it catches exactly the `errors.As`-versus-type-assertion class that produced
finding E-01.

---

## 17 · The security tests

Phrased the way luima's review phrases them: *the test that fails without the control*. Every one of
these should exist before v1.0 ships.

| control | the test that fails without it |
|---|---|
| Timing equalization | unknown-user and wrong-password login latencies within a tight band over N samples |
| Enumeration | register and reset-request responses **byte-identical** for an existing and a new address |
| Session fixation | the cookie value before login differs from the value after |
| Revocation | revoke a session, then assert the next request resolves as anonymous |
| Revoke-all on reset | reset a password, then assert every previously issued session is dead |
| Emailed token single-use | second use fails; two concurrent submissions of the same token yield exactly one success |
| Reset does not mutate | request a reset, then assert the old password still logs in |
| TOTP replay | the same code twice — second attempt rejected with a 23505-backed error |
| `Scope` | two rows, two owners; delete scoped to A returns `false` for B's key **and leaves the row** |
| Redaction | no `SQLSTATE`, no table name, no column name in any auth error on the wire — a negative assertion against a real driver error, the way `tests/crud_test.go:94-96` does it |
| Coverage | `AssertAuthCoverage` fails on a schema with an unannotated `Mutation` field |
| Batching guard | a document with two `login` selections is rejected before any resolver runs |
| Argon2 bound | N concurrent hashes never exceed the semaphore's weight |
| Rehash-on-login | a hash stored with weaker parameters is upgraded on a successful login |
| JWT | a token re-signed with HS256 using the Ed25519 public key as the secret is rejected |
| Cookie attributes | the `Set-Cookie` header carries `__Host-`, `Secure`, `HttpOnly`, `SameSite=Lax`, `Path=/` and **no `Domain`** |

The JWT and cookie-attribute rows are cheap and catch regressions that are otherwise invisible until
someone is exploiting them.

---

## 18 · Non-goals

Stated explicitly so a later implementer reads them as decisions, not oversights.

- **WebAuthn / passkeys.** Excluded outright, not deferred. Two ceremonies, a server-side challenge
  store that must survive between begin and finish, a credential table with two distinct lookup
  paths (by credential ID and by user handle), and sign-count plus backup-state writeback on every
  login — a second authentication system's worth of surface. If it is ever added, `auth_sessions.mfa_at`
  and `@auth(mfa: true)` (§14) are the seam it plugs into, so nothing here has to change.
- **An admin UI, and a scaffolding CLI.**
- **Email templating** beyond a one-method `Mailer` and plain-text defaults. This is the go-pkgz
  lesson: bundled features are what make an auth library unadoptable.
- **Avatar or profile storage.** Not auth.
- **A policy DSL.**
- **SMS / PSTN as a second factor.** ASVS 6.6.1 classes it "restricted"; L3 must not offer it at all.
- **Magic links as a primary factor.** ASVS 6.3.6: email must not be a single- or multi-factor
  mechanism at L3. Can be a documented v2 opt-in with that cost stated.
- **A pluggable `Store` interface.** Postgres is the premise (§2). An interface here would be an
  abstraction with one implementation that forbids the `JOIN` which is the entire point.
  ```go
  // ponytail: no Store interface. Postgres is the premise, so the seam would have one
  // implementation and would forbid joining sessions to domain tables. If a second backend ever
  // becomes real, the SQL is confined to one file per package — that is the seam.
  ```

### The go-pg risk, stated honestly

`go-pg` is in maintenance mode by its own README, and upstream points at `uptrace/bun`. It is not
dead — `v10.15.1` shipped 2026-05-29 and was itself a security fix — but building a security-sensitive
component on a feature-frozen driver is a real risk and should be recorded rather than hidden.

**The mitigation is not an abstraction layer.** That is the interface-with-one-implementation trap
again, and it would cost you go-pg's query builder, which `Scope` depends on. The mitigation is
containment: **all SQL lives in one file per package, and the migrations are plain `.sql`.** A port to
`bun` or `pgx` is then a mechanical rewrite of a bounded, greppable surface, not an archaeology
project. Note also that a driver swap changes the error-classification seam — `pg.Error` is an
*interface*, not pgx's `*pgconn.PgError`, which is why `luimaerr.SQLState` exists and why every
23505 check in kal must go through it rather than type-asserting.

---

## References

Standards and specifications
- OWASP ASVS 5.0 (May 2025) — V3 Web Frontend, V6 Authentication, V7 Session Management,
  V8 Authorization, V9 Self-contained Tokens, V10 OAuth/OIDC · https://asvs.dev/v5.0.0/
- RFC 9700 — OAuth 2.0 Security Best Current Practice (BCP 240, 2025) ·
  https://www.rfc-editor.org/rfc/rfc9700.html
- RFC 9207 — OAuth 2.0 Authorization Server Issuer Identification ·
  https://www.rfc-editor.org/rfc/rfc9207.html
- RFC 7636 (PKCE), RFC 6238 (TOTP)
- NIST SP 800-63B Rev. 4 (July 2025)

OWASP cheat sheets
- Password Storage · https://cheatsheetseries.owasp.org/cheatsheets/Password_Storage_Cheat_Sheet.html
- Forgot Password · https://cheatsheetseries.owasp.org/cheatsheets/Forgot_Password_Cheat_Sheet.html
- CSRF Prevention · https://cheatsheetseries.owasp.org/cheatsheets/Cross-Site_Request_Forgery_Prevention_Cheat_Sheet.html
- Multifactor Authentication · https://cheatsheetseries.owasp.org/cheatsheets/Multifactor_Authentication_Cheat_Sheet.html

Vulnerabilities cited
- CVE-2024-51744 / GHSA-29wx-vh33-7x7r — golang-jwt joined errors ·
  https://github.com/golang-jwt/jwt/security/advisories/GHSA-29wx-vh33-7x7r
- CVE-2025-30204 / GHSA-mh63-6h87-95cp — golang-jwt header DoS
- CVE-2020-26160 — jwt-go audience type confusion
- GHSA-g3hg-j4jv-cwfr — Traefik BasicAuth timing
- golang/go#47001 — `ConstantTimeCompare` length leak

Libraries
- gqlgen: directives · https://gqlgen.com/reference/directives/ · authentication recipe ·
  https://gqlgen.com/recipes/authentication/ · complexity · https://gqlgen.com/reference/complexity/
- gqlgen directive ordering · https://github.com/99designs/gqlgen/discussions/1936
- Fiber v3 adaptor · https://github.com/gofiber/fiber/blob/main/middleware/adaptor/adaptor.go ·
  what's new · https://docs.gofiber.io/next/whats_new
- zitadel/oidc · https://pkg.go.dev/github.com/zitadel/oidc/v3
- alexedwards/scs · https://pkg.go.dev/github.com/alexedwards/scs/v2
- go-pg · https://github.com/go-pg/pg

Postgres
- Row-Level Security · https://www.postgresql.org/docs/17/ddl-rowsecurity.html
- `SET` / `SET LOCAL` · https://www.postgresql.org/docs/17/sql-set.html
- Admin functions (`current_setting`, `set_config`) · https://www.postgresql.org/docs/17/functions-admin.html
- `citext` and its limitations · https://www.postgresql.org/docs/17/citext.html

Attacks
- PortSwigger — bypassing GraphQL brute-force protections ·
  https://portswigger.net/web-security/graphql/lab-graphql-brute-force-protection-bypass
- Apollo — CSRF prevention · https://www.apollographql.com/docs/graphos/routing/security/csrf
- HIBP Pwned Passwords range API · https://haveibeenpwned.com/API/v3

luima's own documents
- [`docs/security-review.md`](docs/security-review.md) — findings S-01, S-02, S-03, S-04, C-01, C-04,
  E-01, E-04, B-01, referenced throughout
- [`docs/gotchas.md`](docs/gotchas.md) · [`docs/gqlgen-contract.md`](docs/gqlgen-contract.md) ·
  [`docs/fiber.md`](docs/fiber.md) · [`SECURITY.md`](SECURITY.md)
