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
	"net"
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

	// ReadTimeout @notice How long a client may take to send a whole request. Default 10s.
	// Negative disables it.
	//
	// @dev Zero means "unset", as with RequestTimeout — and here the convention is load-bearing
	// rather than tidy. Fiber fills in its own defaults for BodyLimit and Concurrency but assigns
	// this one straight through (app.server.ReadTimeout = app.config.ReadTimeout), and fasthttp
	// reads zero as no deadline at all. Without this field a zero Config lets one client hold a
	// connection slot forever by dribbling a request body a byte at a time, against a default
	// Concurrency of 262144 — and Shutdown cannot reclaim that slot either, because it does not
	// close keep-alive connections.
	//
	// There is deliberately no IdleTimeout field: fasthttp's idleTimeout() returns ReadTimeout
	// whenever IdleTimeout is zero, so this bounds the keep-alive wait too and a third field
	// would only let the two be set apart, which nothing has asked for.
	//
	// Set Fiber.ReadTimeout instead and that one wins — luima only ever fills a zero.
	// TestTimeoutPrecedence pins that.
	//
	// Applied by New, not by Mount. Mount is handed a router that already exists, so its
	// timeouts are not this function's to set, for the same reason cfg.Fiber is ignored there.
	ReadTimeout time.Duration

	// WriteTimeout @notice How long the server may take to write a response. Default 30s.
	// Negative disables it.
	//
	// @dev Cannot truncate a slow resolver, and not merely because RequestTimeout's 15s default
	// fires first. fasthttp sets the write deadline *after* the handler returns, immediately
	// before writing the response (serveConn's SetWriteDeadline, server.go) — so the two never
	// overlap at all. That is where it differs from net/http, whose WriteTimeout starts when the
	// request is read and does cover handler execution; a reader porting that intuition across
	// will raise this field to make room for a long query and change nothing.
	//
	// What it bounds is a slow *reader*: a client that sends a valid query and then stops
	// draining the socket, which no resolver deadline can see because by then the resolver has
	// already returned. Raise it for a large response over a slow link. Raise RequestTimeout for
	// a slow query.
	//
	// Fiber.WriteTimeout wins over it, and New applies it, as with ReadTimeout.
	WriteTimeout time.Duration

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

	// MaxDepth @notice Caps operation nesting depth. Default 15. Negative disables it.
	//
	// @dev Zero means "unset", as with QueryCache and ComplexityLimit. ComplexityLimit does not
	// cover this: a 40-level query costs about 40 against a limit of 1000, so a cyclic schema —
	// User.friends: [User!]! — passes it and multiplies into a resolver call per node per level.
	// gqlgen ships no depth limiter of its own.
	//
	// The walk resolves fragment spreads out of doc.Fragments. It has to: a spread node carries
	// no SelectionSet, so a walker that visits only Doc.Operations reads every fragment as a
	// leaf and a 40-deep document passes at depth 1 — measured, and a two-line change to the
	// attacking query. An inline fragment is a type condition and does not count as a level.
	//
	// 15 is chosen against the deepest document a default install serves, which is the
	// playground's own introspection query: it measures 13, so the default clears it by two.
	// TestMaxDepthAdmitsIntrospection pins that. If your schema legitimately nests deeper than
	// this, raise it — the number is a starting point, not a finding about your schema.
	MaxDepth int

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
	// It runs after every default setter — query cache, introspection, complexity, depth,
	// presenter — so it can override them rather than be silently overridden by them.
	//
	// AddTransport is the exception, and it is the one that was broken: luima's own transports
	// are registered after this runs, so a transport registered here outranks them. Before
	// 0.3.0 they were registered first, and since gqlgen selects the first transport whose
	// Supports matches, a transport added here could never be selected — see the comment on
	// the AddTransport calls in Mount.
	Configure func(*handler.Server)

	// Fiber @notice Passed through to fiber.New, with ReadTimeout and WriteTimeout filled in
	// where this leaves them zero. Ignored by Mount.
	//
	// @dev A field set here wins over the promoted one: this is a whole fiber.Config the caller
	// built deliberately, so a non-zero value in it is an explicit answer and luima does not
	// overrule it. See ReadTimeout for why a zero one cannot simply be passed through.
	//
	// Fiber's BodyLimit defaults to 4 MB where net/http had none — irrelevant to
	// queries, and the thing to raise here the day you add the multipart transport. Read the
	// CSRF cost before you add that transport: multipart/form-data is a "simple" request, so no
	// preflight protects it and a cross-site HTML form can execute a mutation with the caller's
	// cookies attached (measured — gotcha #37).
	//
	// Setting Fiber.ErrorHandler does nothing useful for resolver errors: the adaptor always
	// returns nil, and so does the timeout middleware on both its normal and its timed-out
	// path, so no resolver error ever becomes a Fiber error.
	//
	// ErrorPresenter is the only contract for errors a *resolver* returns. Transport-level
	// failures — a malformed JSON body, an unsupported content type — are written by gqlgen's
	// transport before any executor exists, so they never reach the presenter and are not
	// redacted; a malformed body is echoed back in the message. Nothing sensitive of the
	// server's is in that path, but the claim that everything goes through the presenter is
	// not one to build on.
	Fiber fiber.Config

	// Health @notice Liveness path, e.g. "/healthz". Empty disables it.
	//
	// @dev Registered by Mount alongside the GraphQL routes, so it works on an app, on a group,
	// and on a router you built yourself. Without it, writing the smallest possible route is the
	// last thing that forces a consumer to name a Fiber type.
	//
	// It is a separate route, so HTTPMiddleware does not wrap it — deliberately. A rate limiter
	// that 429s the liveness probe takes the process out of the load balancer for being healthy
	// and busy, which is the opposite of what the probe is for.
	Health string

	// HealthCheck @notice What Health calls. Nil means the path answers 200 whenever the process
	// is up. A non-nil error is 503, and the error text is not sent to the client.
	//
	// @dev A function rather than a *pg.DB field, for three reasons that all point the same way:
	// this package does not import go-pg today and this would be the only reason to start, luima
	// would otherwise have to answer who closes the pool, and a real deployment checks more than
	// one thing. go-pg's Ping already has this exact signature (go-pg/base.go:508, promoted onto
	// *pg.DB), so the common case is:
	//
	//	HealthCheck: db.Ping
	//
	// The context passed in carries a 2s deadline of its own, and that is the point of the field
	// rather than a detail of it. A liveness probe against a wedged database must answer 503; a
	// probe that inherits the request timeout and hangs for 15s instead reads to every load
	// balancer as a slow server rather than a broken one, and that is the difference between
	// being rotated out and being left in.
	//
	// The check runs on its own goroutine, so a check that ignores the context it is handed still
	// answers 503 at the deadline rather than hanging the probe. That goroutine outlives the
	// response — there is no way to stop a function that does not watch its context — so a check
	// that blocks forever leaks one goroutine per probe. Watch the context.
	HealthCheck func(context.Context) error
}

// Run @notice Builds the server, listens on addr, and blocks until ctx is done — then drains
// in-flight requests and returns. The whole server in one call, naming no Fiber type.
//
// @dev Fiber's own graceful path (ListenConfig.GracefulContext) is not used, for two reasons. It
// throws the shutdown error away — gracefulShutdown hands it to the OnPostShutdown hook and
// returns (fiber/listen.go:606-623) while Listen returns nil regardless, so a server that
// force-closed live connections because the drain timed out would be indistinguishable from a
// clean exit and the process would exit 0. And it starts its watcher before the listener exists
// (:252-256), which is the same race this function has to close by hand below.
//
// The listener is created here rather than by Listen so that a bind failure is returned
// synchronously — and so that ln.Close below has something to close. One consequence: net.Listen
// with "tcp" is dual-stack, where Fiber's ListenConfig defaults to tcp4 (fiber/listen.go:167).
// That is deliberate. Run is shaped like net/http.ListenAndServe and should bind like it.
//
// The startup banner is suppressed. Fiber prints it to stdout by default, and a library must not
// choose the caller's logging any more than db.Connect may call log.Fatal. Log your own line
// before calling Run.
//
// @param ctx  cancel it to begin the drain; SIGTERM via signal.NotifyContext is the usual source
// @param addr the listen address, e.g. ":8080"
// @param cfg  the server configuration; only Schema is required
// @return error a bind failure, a shutdown that exceeded the drain window, or nil
func Run(ctx context.Context, addr string, cfg Config) error {
	if ctx.Err() != nil {
		return nil // already cancelled: nothing to bind, nothing to drain
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	app := New(cfg)

	errc := make(chan error, 1)
	go func() { errc <- app.Listener(ln, fiber.ListenConfig{DisableStartupMessage: true}) }()

	select {
	case err := <-errc:
		return err // the listener died on its own
	case <-ctx.Done():
	}

	// 10s matches Fiber's own ListenConfig.ShutdownTimeout default (fiber/listen.go:169). Not a
	// Config field: a drain window is a property of the deployment's shutdown grace period, and a
	// caller who needs a different one is holding the app, which is what New is for.
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	shutErr := app.ShutdownWithContext(sctx)

	// Closing the listener here is what makes the receive below terminate, and it is not
	// belt-and-braces. fasthttp's Shutdown returns nil immediately when Serve has not run yet — it
	// checks s.ln == nil (fasthttp/server.go:2064), and s.ln is only appended inside Serve — so a
	// context cancelled in the first microseconds shuts down a server that is not yet listening,
	// closes nothing, and leaves Listener serving forever with nothing left to stop it. Closing it
	// makes Accept fail, which fasthttp maps to io.EOF and Serve returns nil (server.go:2119).
	// Redundant on the ordinary path, where Shutdown closed it already; a double close is an error
	// nobody needs to hear about.
	_ = ln.Close()

	if shutErr != nil {
		return shutErr
	}
	return <-errc
}

// New @notice Builds a *fiber.App with the GraphQL endpoint and playground mounted.
//
// @dev The only place in luima that owns a fiber.Config, and therefore the only place that can
// bound the transport. Passing cfg.Fiber through untouched is what made a zero Config the
// pathological one — see ReadTimeout.
//
// @param cfg        the server configuration; only Schema is required
// @return *fiber.App an app ready for Listen, with cfg.Fiber applied
func New(cfg Config) *fiber.App {
	fc := cfg.Fiber
	fc.ReadTimeout = resolveTimeout(fc.ReadTimeout, cfg.ReadTimeout, 10*time.Second)
	fc.WriteTimeout = resolveTimeout(fc.WriteTimeout, cfg.WriteTimeout, 30*time.Second)

	app := fiber.New(fc)
	Mount(app, cfg)
	return app
}

// resolveTimeout @notice Resolves one of luima's zero-means-default durations against the
// equivalent field in cfg.Fiber.
//
// @dev Not a max() and not a chain of ||, because the three inputs mean three different things
// and two of them collide on zero: Fiber's zero means "unset", luima's zero means "unset", and
// luima's negative means "no deadline" — which has to reach fasthttp as the very zero the other
// two use for "unset". Collapsing any pair of those silently disables a deadline or ignores a
// negative.
//
// @param fromFiber cfg.Fiber's field; non-zero wins outright
// @param fromLuima cfg.ReadTimeout or cfg.WriteTimeout; negative means "no deadline"
// @param def       luima's default, used only when both are zero
// @return time.Duration what fasthttp receives, where 0 is fasthttp's "no deadline"
func resolveTimeout(fromFiber, fromLuima, def time.Duration) time.Duration {
	switch {
	case fromFiber != 0:
		return fromFiber
	case fromLuima < 0:
		return 0
	case fromLuima > 0:
		return fromLuima
	default:
		return def
	}
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
	// A panic where db.Connect returns an error, and the difference is whether the caller could
	// ever recover: an unreachable database is an environment failure worth retrying, a nil schema
	// is a programmer error that cannot become valid later. Without this, Mount succeeds, boot
	// succeeds, a readiness probe that only checks the database succeeds — and then every request
	// panics inside gqlgen's executor, is recovered by gqlgen's own handler, and comes back as
	// "internal system error" with no extensions.code. That is not even luima's error contract, so
	// an alert keyed on INTERNAL_SERVER_ERROR never fires; the first symptom is the volume of
	// stack traces. Measured. TestMountRequiresSchema pins it.
	if cfg.Schema == nil {
		panic("luima: Config.Schema is nil — pass your generated.NewExecutableSchema(...)")
	}

	endpoint := cfg.Endpoint
	if endpoint == "" {
		endpoint = "/graphql"
	}

	// Not handler.NewDefaultServer: deprecated in v0.17.94, and it gives no way to set an
	// error presenter, which makes luimaerr.PresentError impossible. Everything it would have added is
	// spelled out below.
	srv := handler.New(cfg.Schema)

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
	if n := cfg.MaxDepth; n >= 0 {
		if n == 0 {
			n = 15
		}
		srv.Use(maxDepth{limit: n})
	}

	presenter := cfg.ErrorPresenter
	if presenter == nil {
		presenter = luimaerr.PresentError
	}
	srv.SetErrorPresenter(presenter)

	// Last among the gqlgen setters, deliberately: Configure sees the server with every default
	// above already applied, so it can override any of them. Moved earlier, an interceptor it
	// registers could be shadowed by luima's own defaults with no error.
	if cfg.Configure != nil {
		cfg.Configure(srv)
	}

	// Transports go *after* Configure, and that inversion is load-bearing. gqlgen picks the
	// first transport whose Supports returns true (handler/server.go:132-139) and AddTransport
	// appends (:78-80), so registration order is precedence. transport.POST's Supports is a
	// superset of every streaming transport's: SSE wants POST + application/json + an Accept of
	// text/event-stream, and POST never reads Accept. Register POST first and a
	// Configure-registered SSE transport is unreachable by construction — it compiles, it
	// mounts, it returns 200, and every subscription silently degrades to one buffered
	// response. TestConfigureCanOutrankPOST pins it. Every other default Configure must be able
	// to override is a setter, and setters do not care about this order.
	//
	// transport.Options answers the OPTIONS preflight so it reaches gqlgen instead of 405-ing.
	// That is all it does: it sets the Allow header and nothing else, so this is *not* CORS —
	// it sets no Access-Control-Allow-Origin, and a preflight that returns 200 without one still
	// fails in the browser. Put CORS(...) in HTTPMiddleware for cross-origin access; Fiber's own
	// cors middleware works too, registered before Mount. See docs/fiber.md.
	srv.AddTransport(transport.Options{})
	srv.AddTransport(transport.GET{})
	srv.AddTransport(transport.POST{})

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

	if cfg.Health != "" {
		check := cfg.HealthCheck
		r.Get(cfg.Health, func(c fiber.Ctx) error {
			if check == nil {
				return c.SendStatus(http.StatusOK)
			}

			// 2s of the probe's own, not the request's: c.Context() carries no deadline on this
			// route, since the timeout middleware wraps only the GraphQL handler.
			ctx, cancel := context.WithTimeout(c.Context(), 2*time.Second)
			defer cancel()

			// On a goroutine, because cancelling a context does not stop a function that never
			// reads it, and "503 rather than a hang" is the whole contract. Buffered by one so
			// that goroutine can always send and exit when the deadline wins the race.
			done := make(chan error, 1)
			go func() { done <- check(ctx) }()

			select {
			case err := <-done:
				if err != nil {
					return c.SendStatus(http.StatusServiceUnavailable)
				}
				return c.SendStatus(http.StatusOK)
			case <-ctx.Done():
				return c.SendStatus(http.StatusServiceUnavailable)
			}
		})
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
