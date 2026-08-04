// Package server @notice Mounts a gqlgen handler on Fiber v3, correctly.
//
// @dev "Correctly" is doing real work in that sentence. Every line of Mount exists because
// leaving it out fails silently: r.All rather than r.Post (or browser clients 405 on the CORS
// preflight, with nothing in the log), SetQueryCache (or every request re-parses and
// re-validates), extension.Introspection (or the playground's docs pane is blind).
//
// This package cannot replace gqlgen codegen, and no package could. gqlgen generates code into
// your module: generated.NewExecutableSchema is a symbol only your own `go tool gqlgen generate`
// run produces, from your own schema.graphqls. So you still own gqlgen.yml, schema.graphqls,
// graph/resolver.go and the codegen step — see docs/gqlgen-contract.md, because getting it wrong
// produces runtime panics that `go build` does not catch.
package server

import (
	"context"
	"net/http"
	"time"

	"github.com/99designs/gqlgen/graphql"
	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/handler/extension"
	"github.com/99designs/gqlgen/graphql/handler/lru"
	"github.com/99designs/gqlgen/graphql/handler/transport"
	"github.com/99designs/gqlgen/graphql/playground"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"
	"github.com/gofiber/fiber/v3/middleware/timeout"
	"github.com/vektah/gqlparser/v2/ast"

	"github.com/ulas96/luima/luimaerr"
)

// Config @notice Assembles the server.
//
// @dev Only Schema is required; every zero value has a working default, so Config{Schema: ...}
// is the good configuration rather than the pathological one.
type Config struct {
	// Schema @notice Your generated executable schema, e.g.
	//   generated.NewExecutableSchema(generated.Config{Resolvers: &graph.Resolver{DB: db}})
	Schema graphql.ExecutableSchema

	// Endpoint @notice The GraphQL path. Default "/graphql".
	Endpoint string

	// Playground @notice The GraphiQL path. Default "/".
	//
	// @dev Fiber matches it exactly, where net/http's Handle("/", ...) was a prefix match — an
	// unknown path is a plain 404 here.
	Playground string

	// DisablePlayground @notice Turns GraphiQL off entirely. Do this in production.
	//
	// @dev A separate bool rather than Playground == "": the zero Config must be the good
	// configuration, so "" has to mean "unset", which leaves no spelling for "off".
	DisablePlayground bool

	// PlaygroundTitle @notice The browser tab title. Default "graphql".
	PlaygroundTitle string

	// DisableIntrospection @notice Turns schema introspection off. Do this in production.
	//
	// @dev Zero value is "on", as with the playground — introspection is what makes the docs
	// pane work, so the zero Config has to keep it.
	//
	// This is defence in depth and a smaller attack surface; it is not confidentiality, and it
	// does not hide your field names. gqlparser's validator appends "Did you mean ...?" to
	// validation errors (gqlparser/validator/core/helpers.go), and PresentError passes
	// validation errors through by design, so a caller who guesses "nam" still learns there is
	// a "name". Do not treat it as a substitute for authorization.
	DisableIntrospection bool

	// RequestTimeout @notice Deadline for the whole request, propagated into the resolver
	// context. Default 15s. Negative disables it.
	//
	// @dev Zero means "unset", as with QueryCache. This is the only bound a query gets:
	// go-pg sets no ReadTimeout or WriteTimeout default, pg.ParseURL rejects statement_timeout
	// in the DSN, and nothing else in the stack imposes one. What makes a context deadline
	// sufficient is that go-pg honours it three ways — the pool wait selects on ctx.Done()
	// (internal/pool/pool.go waitTurn), the socket deadline is ctx.Deadline() verbatim when no
	// ReadTimeout is set (internal/pool/conn.go deadline), and ctx.Done() triggers a real
	// Postgres CancelRequest against the running backend (base.go withConn).
	//
	// 15s is deliberately under go-pg's own 30s PoolTimeout default (options.go init): below
	// it, a saturated pool sheds deterministic 408s; at or above it, the failure mode is a coin
	// flip between ErrPoolTimeout and the timeout. That CancelRequest is best-effort — it dials
	// a second connection and only logs a failure — so a server-authoritative bound still wants
	// statement_timeout via pg.Options.OnConnect. See SECURITY.md.
	RequestTimeout time.Duration

	// QueryCache @notice The parsed-query LRU size. Default 1000. Negative disables it.
	//
	// @dev Zero means "unset", not "off", because a zero-valued Config must not be the
	// pathological one: handler.New starts at graphql.NoCache, and with no cache every
	// request re-parses and re-validates the whole query document. There is no good reason to
	// pass a negative here.
	QueryCache int

	// ComplexityLimit @notice Caps query complexity. Default 1000. Negative disables it.
	//
	// @dev Zero means "unset", as with QueryCache.
	ComplexityLimit int

	// ErrorPresenter @notice The error contract. Default [luimaerr.PresentError].
	ErrorPresenter graphql.ErrorPresenterFunc

	// HTTPMiddleware @notice net/http middleware wrapped around the gqlgen handler, outermost
	// first.
	//
	// @dev The one layer that has all three of: the real *http.Request with its cookies
	// parsed, an http.ResponseWriter whose headers survive back through the adaptor —
	// Set-Cookie included, because fasthttp's Header.Add routes it into the dedicated cookie
	// list rather than the generic map — and r.WithContext, which every resolver sees as its
	// own ctx, typed. Fiber middleware on the mounted group has none of the net/http shapes,
	// so anything written as func(http.Handler) http.Handler — request logging, tracing,
	// tenancy, rate limiting, a session layer — mounts here unchanged and stays portable to
	// chi, echo or plain net/http.
	//
	// The chain runs inside withFiberContext, so each middleware already sees the request
	// context that the resolvers will see: the RequestTimeout deadline is set, c.SetContext
	// values are attached, and whatever the middleware adds rides the same context down.
	// TestHTTPMiddleware pins all three properties.
	HTTPMiddleware []func(http.Handler) http.Handler

	// Configure @notice Runs against the built gqlgen server immediately before it is mounted.
	//
	// @dev One escape hatch rather than a field per knob: srv.Use, AroundOperations,
	// SetRecoverFunc, SetParserTokenLimit and SetDisableSuggestion are all reachable through
	// it, and a new gqlgen extension point needs no change here. Without it, srv is a local
	// that never escapes Mount, and a consumer wanting any of those has to abandon Mount —
	// which is the whole library.
	//
	// It runs after every default above — transports, query cache, introspection, complexity,
	// presenter — so it can override them rather than be silently overridden by them.
	Configure func(*handler.Server)

	// Fiber @notice Passed through to fiber.New. Ignored by Mount.
	//
	// @dev Fiber's BodyLimit defaults to 4 MB where net/http had none — irrelevant to
	// queries, and the thing to raise here the day you add the multipart transport.
	//
	// Setting Fiber.ErrorHandler does nothing useful for resolver errors: the adaptor always
	// returns nil, and so does the timeout middleware on both its normal and its timed-out
	// path, so no resolver error ever becomes a Fiber error. ErrorPresenter is the only error
	// contract there is.
	Fiber fiber.Config
}

// New @notice Builds a *fiber.App with the GraphQL endpoint and playground mounted.
//
// @param cfg        the server configuration; only Schema is required
// @return *fiber.App an app ready for Listen, with cfg.Fiber applied
func New(cfg Config) *fiber.App {
	app := fiber.New(cfg.Fiber)
	Mount(app, cfg)
	return app
}

// Mount @notice Registers the same routes on a router you already have — an existing app, or a
// group.
//
// @dev cfg.Fiber is ignored here: the app already exists, so its configuration is not this
// function's to set.
//
// @param r   a *fiber.App or the result of app.Group(prefix)
// @param cfg the server configuration; only Schema is required
func Mount(r fiber.Router, cfg Config) {
	endpoint := cfg.Endpoint
	if endpoint == "" {
		endpoint = "/graphql"
	}

	// Not handler.NewDefaultServer: deprecated in v0.17.94, and it gives no way to set an
	// error presenter, which makes luimaerr.PresentError impossible. Everything it would have added is
	// spelled out below.
	srv := handler.New(cfg.Schema)

	// Answers the OPTIONS preflight so it reaches gqlgen instead of 405-ing. That is all it
	// does: transport.Options sets the Allow header and nothing else, so this is *not* CORS —
	// no Access-Control-Allow-Origin is set here or anywhere else in luima, and a preflight
	// that returns 200 without one still fails in the browser. Add fiber's cors middleware with
	// explicit origins if you need cross-origin access; see docs/fiber.md.
	srv.AddTransport(transport.Options{})
	srv.AddTransport(transport.GET{})
	srv.AddTransport(transport.POST{})

	if n := cfg.QueryCache; n >= 0 {
		if n == 0 {
			n = 1000
		}
		srv.SetQueryCache(lru.New[*ast.QueryDocument](n))
	}

	// handler.New adds no extensions; without this the playground's docs pane is blind.
	if !cfg.DisableIntrospection {
		srv.Use(extension.Introspection{})
	}
	if n := cfg.ComplexityLimit; n >= 0 {
		if n == 0 {
			n = 1000
		}
		srv.Use(extension.FixedComplexityLimit(n))
	}

	presenter := cfg.ErrorPresenter
	if presenter == nil {
		presenter = luimaerr.PresentError
	}
	srv.SetErrorPresenter(presenter)

	// Last among the gqlgen knobs, deliberately: Configure sees the server with every default
	// above already applied, so it can override any of them. Moved earlier, an interceptor it
	// registers could be shadowed by luima's own defaults with no error.
	if cfg.Configure != nil {
		cfg.Configure(srv)
	}

	// Reverse order so HTTPMiddleware[0] is outermost — the slice reads top-to-bottom like the
	// request path. Iterate forward instead and a logging middleware listed first silently runs
	// last, after the handler everyone assumed it observed.
	var gql http.Handler = srv
	for i := len(cfg.HTTPMiddleware) - 1; i >= 0; i-- {
		gql = cfg.HTTPMiddleware[i](gql)
	}

	// gqlgen is an http.Handler; adaptor converts fasthttp's RequestCtx into an *http.Request
	// per request. So Fiber buys routing, middleware and its ecosystem here — not speed.
	//
	// HTTPHandlerWithContext, never plain HTTPHandler: the latter hands the resolver the raw
	// *fasthttp.RequestCtx, which has no deadline and never cancels. See withFiberContext —
	// which wraps *outside* the HTTPMiddleware chain, so each middleware already holds the
	// real request context rather than discovering the fasthttp one.
	h := adaptor.HTTPHandlerWithContext(withFiberContext(gql))

	if d := cfg.RequestTimeout; d >= 0 {
		if d == 0 {
			d = 15 * time.Second
		}
		// Outermost, and that is load-bearing: timeout.New calls c.SetContext before invoking
		// h, and HTTPHandlerWithContext reads c.Context() at request time. Nested the other way
		// round it still compiles, still returns 200s, and the deadline is silently invisible
		// to every resolver.
		h = timeout.New(h, timeout.Config{Timeout: d})
	}

	// All, never Post: gqlgen's transports dispatch on the method themselves, so GET, POST and
	// the OPTIONS preflight all have to reach srv. Register with Post and every browser client
	// 405s on the preflight, with nothing in the server log because the request never reached
	// gqlgen. This is the single most important line in the library; TestMountRoutes pins it.
	r.All(endpoint, h)

	if !cfg.DisablePlayground {
		path := cfg.Playground
		if path == "" {
			path = "/"
		}
		title := cfg.PlaygroundTitle
		if title == "" {
			title = "graphql"
		}
		r.Get(path, adaptor.HTTPHandler(playground.Handler(title, endpoint)))
	}
}

// fiberContext @notice The Fiber request context, with c.Locals still reachable through Value.
//
// @dev Two contexts have to be merged, because each has exactly what the other lacks.
// c.Context() carries the deadline, the cancellation and anything middleware put in
// c.SetContext — but Fiber roots it at context.Background() when nobody called SetContext, so it
// cannot see fasthttp's user values. The *fasthttp.RequestCtx is the opposite: no deadline, never
// cancels, but its Value reads the user values that c.Locals writes.
//
// Drop the locals fallback and every consumer passing identity through c.Locals gets a silent nil
// in the resolver — no compile error, no log line, and c.Locals is the only mechanism that worked
// before this change. TestMountRoutes/"c.Locals reaches the resolver" pins it.
type fiberContext struct {
	context.Context
	locals context.Context
}

// Value @notice Reads from the Fiber user context first, then falls back to fasthttp's locals.
//
// @return any the value, or nil when neither context has the key
func (c fiberContext) Value(key any) any {
	if v := c.Context.Value(key); v != nil {
		return v
	}
	return c.locals.Value(key)
}

// withFiberContext @notice Re-attaches Fiber's request context to the *http.Request gqlgen sees.
//
// @dev fasthttpadaptor hands a net/http handler the *fasthttp.RequestCtx as its request context.
// That satisfies context.Context only nominally: Deadline reports nothing, and Done is closed on
// server shutdown rather than on the client hanging up ("creating a new channel for every request
// is just too expensive", fasthttp/server.go). Three things follow, and all three are silent —
// a client disconnect does not stop the query, no middleware can impose a deadline, and any
// resolver code that respects cancellation is dead code that reads as correct.
//
// HTTPHandlerWithContext stashes Fiber's own request context as a fasthttp user value; this
// unwraps it so gqlgen — and every resolver under it — sees a context that actually cancels and
// actually carries what middleware put there.
//
// @param next the gqlgen handler
// @return http.Handler next, wrapped
func withFiberContext(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fc, ok := adaptor.LocalContextFromHTTPRequest(r); ok {
			r = r.WithContext(fiberContext{Context: fc, locals: r.Context()})
		}
		next.ServeHTTP(w, r)
	})
}
