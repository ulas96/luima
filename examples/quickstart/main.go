package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/99designs/gqlgen/graphql/handler"

	"github.com/ulas96/luima"

	"github.com/ulas96/luima/examples/quickstart/graph"
	"github.com/ulas96/luima/examples/quickstart/graph/generated"
)

// main @notice The whole server: connect, run, drain on SIGTERM.
//
// @dev generated.NewExecutableSchema is the seam luima cannot cross — it is produced by your own
// `go tool gqlgen generate` run from your own schema.graphqls, so it exists only in this module.
// Everything after it is luima's, and nothing after it names a Fiber type. That is the property
// worth copying: an application on luima imports luima.
//
// This deliberately does *not* use the zero luima.Config, and it is the one place in the repo that
// does not. A zero Config is the good configuration for a library call, which has no deployment
// context and should not invent one. A deployed application has that context, so the development
// conveniences here are opened by LUIMA_DEV rather than closed by its absence — the cost of
// forgetting the flag should be a developer without a playground, not a public endpoint with one.
//
// log.Fatal is the call site's choice, not the library's: Connect returns its error rather than
// exiting, so a server that wants to retry or fall back still can. Note that it is not used on
// Run: log.Fatal calls os.Exit, which skips the deferred db.Close above.
func main() {
	dev := os.Getenv("LUIMA_DEV") != ""

	// ConnectWith rather than Connect for the one thing a DSN cannot say. RequestTimeout bounds
	// the resolver's context and go-pg turns that into a Postgres CancelRequest, but that cancel
	// is best-effort — it dials a second connection and only logs a failure. statement_timeout is
	// the bound the server enforces whether or not the client is still there.
	db, err := luima.ConnectWith(os.Getenv("DATABASE_URL"), luima.StatementTimeout(10*time.Second))
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	// Cancelled on SIGTERM, which is what an orchestrator sends before it kills the container.
	// Run stops accepting, lets in-flight requests finish, and returns — so the defer above runs.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// No timeouts here, and that is the example: read, write and request default to 10s, 30s and
	// 15s. They were three hand-written lines in this file until the library grew the fields.
	err = luima.Run(ctx, ":8080", luima.Config{
		Schema: generated.NewExecutableSchema(generated.Config{
			Resolvers: &graph.Resolver{DB: db},
		}),
		DisablePlayground:    !dev,
		DisableIntrospection: !dev,

		// The path an orchestrator probes. It is not wrapped by HTTPMiddleware, so the rate
		// limiter below cannot 429 the probe and take a healthy-but-busy process out of rotation.
		// db.Ping already has the signature this field wants, and the context it gets carries a 2s
		// deadline of its own — a probe that hangs reads as a slow server rather than a broken one.
		Health:      "/healthz",
		HealthCheck: db.Ping,

		// HTTPMiddleware is the seam for anything that needs the real *http.Request, and it is
		// where every cross-cutting concern belongs — not on the app. A Fiber handler cannot reach
		// the context gqlgen builds, so a value set with c.Locals is not what a resolver's
		// ctx.Value sees.
		//
		// Outermost first, so this slice reads top-to-bottom like the request path: refuse over
		// the limit before parsing, then answer the browser, then tag the request.
		HTTPMiddleware: []func(http.Handler) http.Handler{
			// The bound ComplexityLimit cannot supply: row count is not an input to the complexity
			// calculation, so an unbounded { users { id } } in a loop costs the same as one call.
			// nil keys on RemoteAddr — put a header here instead if you are behind a proxy you
			// control, because otherwise every caller shares the proxy's one bucket.
			luima.RateLimit(100, time.Minute, nil),

			// luima answers the OPTIONS preflight already; without this it answers it with no
			// Access-Control-Allow-Origin, and the browser refuses the response anyway.
			luima.CORS(luima.CORSConfig{Origins: []string{"http://localhost:3000"}}),

			// The smallest useful custom one: a request id put into the resolver context, which is
			// what makes a resolver's log line joinable to the access log. Anything reading a
			// header — a trace parent, a tenant, an Authorization header — has the same shape. A
			// resolver reads it back with ctx.Value(graph.RequestIDKey{}), which is why the key is
			// declared in graph rather than here.
			func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					id := r.Header.Get("X-Request-Id")
					if id == "" {
						id = strconv.FormatInt(time.Now().UnixNano(), 36)
					}
					w.Header().Set("X-Request-Id", id)
					next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), graph.RequestIDKey{}, id)))
				})
			},
		},

		// Configure is the seam for gqlgen itself, and it runs against the server that actually
		// serves. SetDisableSuggestion turns off the "Did you mean ...?" hint gqlparser adds to
		// an unknown-field error, which otherwise walks a caller through a schema that
		// DisableIntrospection was meant to keep closed.
		//
		// Since 0.3.0 it can also register a transport — srv.AddTransport(...) — because luima's
		// own transports are now registered after this runs. Before that, gqlgen selected
		// transport.POST first and anything added here was silently unreachable.
		Configure: func(srv *handler.Server) {
			srv.SetDisableSuggestion(!dev)
		},
	})
	if err != nil {
		log.Print(err)
	}
}
