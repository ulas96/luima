package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

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
