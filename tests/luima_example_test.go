package tests

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/go-pg/pg/v10/orm"
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
// @dev Anything in Fiber is passed through untouched — except ErrorHandler, which the adaptor
// makes unreachable by always returning nil.
func ExampleNew_production() {
	app := luima.New(luima.Config{
		Schema:               newStubSchema(),
		DisablePlayground:    true,
		DisableIntrospection: true,
		ComplexityLimit:      300,
		// RequestTimeout is left at its 15s default on purpose. Set it only when your resolvers
		// have a different shape — it is what bounds the query, since nothing else does.
		Fiber: fiber.Config{
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 30 * time.Second,
			BodyLimit:    8 << 20,
		},
	})

	log.Fatal(app.Listen(":8080"))
}

// ExampleMount_authorization @notice Passing the caller's identity to a resolver, and scoping the
// query to rows they own.
//
// @dev The two halves are one story and neither works alone. c.SetContext is what reaches the
// resolver — a field on the shared Resolver struct would be a cross-request data race, which under
// concurrency is an authorization bypass rather than a bug you find in testing. The query modifier
// is what turns the identity into a WHERE clause; without it every helper is WherePK() alone, and
// any caller can address any row.
//
// luima ships no authentication and will not. This is the seam it leaves you.
func ExampleMount_authorization() {
	type userKey struct{}

	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		user, ok := authenticate(c.Get("Authorization"))
		if !ok {
			return c.SendStatus(fiber.StatusUnauthorized)
		}
		c.SetContext(context.WithValue(c.Context(), userKey{}, user))
		return c.Next()
	})

	luima.Mount(app, luima.Config{Schema: newStubSchema(), DisablePlayground: true})

	// …and in the resolver, where db is your *pg.DB:
	deleteUser := func(ctx context.Context, db orm.DB, id string) (bool, error) {
		owner, _ := ctx.Value(userKey{}).(string)
		return luima.Delete(ctx, db, &struct {
			ID string `pg:"id,pk"`
		}{ID: id}, func(q *orm.Query) *orm.Query {
			return q.Where("owner_id = ?", owner)
		})
	}
	_ = deleteUser
}

// authenticate @notice Stands in for whatever validates your tokens.
//
// @param header the raw Authorization header
// @return string the caller's id
// @return bool   whether the header was valid
func authenticate(header string) (string, bool) { return header, header != "" }

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
