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
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/limiter"

	"github.com/ulas96/luima"

	"github.com/ulas96/luima/examples/quickstart/graph"
	"github.com/ulas96/luima/examples/quickstart/graph/generated"
)

// main @notice The whole server: connect, build the app, listen.
//
// @dev generated.NewExecutableSchema is the seam luima cannot cross — it is produced by your own
// `go tool gqlgen generate` run from your own schema.graphqls, so it exists only in this module.
// Everything after it is luima's.
//
// This deliberately does *not* use the zero luima.Config, and it is the one place in the repo that
// does not. A zero Config is the good configuration for a library call, which has no deployment
// context and should not invent one. A deployed application has that context, so the development
// conveniences here are opened by LUIMA_DEV rather than closed by its absence — the cost of
// forgetting the flag should be a developer without a playground, not a public endpoint with one.
//
// log.Fatal is the call site's choice, not the library's: Connect returns its error rather than
// exiting, so a server that wants to retry or fall back still can.
func main() {
	dev := os.Getenv("LUIMA_DEV") != ""

	db, err := luima.Connect(os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	// fiber.New plus luima.Mount, not luima.New: Fiber matches routes in registration order and
	// luima.New mounts before it returns, so an app.Use after it would land behind /graphql and
	// never run — the rate limiter would be dead on the one route it exists to protect.
	app := fiber.New(fiber.Config{
		// Fiber defaults only BodyLimit; the three timeouts pass through as zero, which fasthttp
		// reads as "no timeout". A connection dribbling a request body one byte per minute then
		// holds a slot forever, and Concurrency defaults to 262144 of them.
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
		BodyLimit:    1 << 20,
	})
	app.Use(limiter.New())

	luima.Mount(app, luima.Config{
		Schema: generated.NewExecutableSchema(generated.Config{
			Resolvers: &graph.Resolver{DB: db},
		}),
		DisablePlayground:    !dev,
		DisableIntrospection: !dev,

		// HTTPMiddleware is the seam for anything that needs the real *http.Request. This one is
		// the smallest useful example: a request id put into the resolver context, which is what
		// makes a resolver's log line joinable to the access log. Anything reading a header —
		// a trace parent, a tenant, an Authorization header — has the same shape.
		//
		// It has to be here rather than in app.Use: a Fiber handler cannot reach the context
		// gqlgen builds, so a value set with c.Locals is not what a resolver's ctx.Value sees.
		// A resolver reads it back with ctx.Value(graph.RequestIDKey{}), which is why the key is
		// declared in graph rather than here.
		HTTPMiddleware: []func(http.Handler) http.Handler{
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
	// RequestTimeout is deliberately not set: it defaults to 15s, which is the point of putting it
	// in the library rather than in a line every consumer has to remember.

	// SIGTERM rather than log.Fatal on Listen: log.Fatal calls os.Exit, so the deferred db.Close
	// above would never run. Harmless in a process that is exiting anyway — and exactly the
	// pattern people copy into code where it is not.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := app.Listen(":8080", fiber.ListenConfig{GracefulContext: ctx}); err != nil {
		log.Print(err)
	}
}
