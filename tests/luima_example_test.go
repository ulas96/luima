package tests

import (
	"log"
	"os"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/ulas96/luima"
)

// ExampleNew @notice A complete main: connect, build the app, listen.
//
// @dev Schema is the only required field — every other zero value is already the good
// configuration.
func ExampleNew() {
	db, err := luima.Connect(os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	// In a real application this is your generated schema:
	//
	//	generated.NewExecutableSchema(generated.Config{
	//	    Resolvers: &graph.Resolver{DB: db},
	//	})
	app := luima.New(luima.Config{Schema: newStubSchema()})

	log.Fatal(app.Listen(":8080"))
}

// ExampleNew_production @notice Production settings: no playground, a raised body limit, and
// timeouts.
//
// @dev Anything in Fiber is passed through untouched — except ErrorHandler, which
// adaptor.HTTPHandler makes unreachable.
func ExampleNew_production() {
	app := luima.New(luima.Config{
		Schema:            newStubSchema(),
		DisablePlayground: true,
		ComplexityLimit:   300,
		Fiber: fiber.Config{
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 30 * time.Second,
			BodyLimit:    8 << 20,
		},
	})

	log.Fatal(app.Listen(":8080"))
}

// ExampleMount @notice Adding GraphQL to an app you already have.
//
// @dev Including under a group prefix — here the endpoint ends up at /api/graphql, sharing the
// group's middleware.
func ExampleMount() {
	app := fiber.New()

	app.Get("/healthz", func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	luima.Mount(app.Group("/api"), luima.Config{
		Schema:            newStubSchema(),
		DisablePlayground: true,
	})

	log.Fatal(app.Listen(":8080"))
}
