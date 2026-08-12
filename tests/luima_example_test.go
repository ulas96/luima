package tests

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
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

// ExampleNew_production @notice Production settings: no playground, a tighter complexity limit, a
// raised body limit.
//
// @dev No timeout appears here, and that is the example. Read and write default to 10s and 30s
// and RequestTimeout to 15s, so the bounds are already in place — the point of putting them in
// the library rather than in three lines every consumer has to remember.
//
// Fiber remains the spelling for everything luima does not promote, BodyLimit here, and anything
// set there wins over the promoted field. It is passed through untouched — except ErrorHandler,
// which the adaptor makes unreachable by always returning nil.
func ExampleNew_production() {
	app := luima.New(luima.Config{
		Schema:               newStubSchema(),
		DisablePlayground:    true,
		DisableIntrospection: true,
		ComplexityLimit:      300,
		Fiber:                fiber.Config{BodyLimit: 8 << 20},
	})

	log.Fatal(app.Listen(":8080"))
}

// ExampleNew_slowResolvers @notice Raising the bound that actually governs a slow query, and the
// one that does not.
//
// @dev RequestTimeout is the deadline on the resolver's context, which go-pg turns into a socket
// deadline and a Postgres CancelRequest. It is the one a slow query needs.
//
// WriteTimeout is not a second copy of it. fasthttp starts that deadline after the handler
// returns, so it bounds writing the response and never the query — raising it to make room for a
// slow resolver is a no-op, and it is raised here only because a 60-second query tends to produce
// a response body large enough that a slow client needs longer than 30s to read it.
func ExampleNew_slowResolvers() {
	app := luima.New(luima.Config{
		Schema:         newStubSchema(),
		RequestTimeout: 60 * time.Second,
		WriteTimeout:   90 * time.Second,
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

// ExampleRun @notice The whole server in one call, naming no Fiber type.
//
// @dev Run is New plus Listen plus the drain, and the reason to prefer it is the ordering trap it
// removes rather than the lines it saves: New mounts before it returns, so anything registered on
// the app afterwards lands behind /graphql and never runs. With Run there is no app to register
// on, and HTTPMiddleware is the only place middleware can go — which is where it always belonged,
// because it is the only layer that sees the request context the resolvers see.
//
// Returning an error rather than calling log.Fatal is what makes the defer above it run: os.Exit
// skips deferred functions, so a log.Fatal on the listen call closes no pool.
func ExampleRun() {
	db, err := luima.Connect(os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	// Cancel on SIGTERM, which is what an orchestrator sends before it kills the container.
	// Run then stops accepting, lets in-flight requests finish, and returns.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// In a real application this is your generated schema.
	if err := luima.Run(ctx, ":8080", luima.Config{Schema: newStubSchema()}); err != nil {
		log.Print(err)
	}
}

// ExampleRun_health @notice A liveness path for the orchestrator.
//
// @dev db.Ping already has the signature HealthCheck wants, so the common case is one field.
// The context it receives carries a 2s deadline of its own — a probe that inherits the request
// timeout and hangs for 15s reads to a load balancer as a slow server rather than a broken one.
//
// The path is not wrapped by HTTPMiddleware, so a rate limiter cannot 429 the probe.
func ExampleRun_health() {
	db, err := luima.Connect(os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := luima.Run(ctx, ":8080", luima.Config{
		Schema:      newStubSchema(),
		Health:      "/healthz",
		HealthCheck: db.Ping,
	}); err != nil {
		log.Print(err)
	}
}

// ExampleCORS @notice Letting a browser SPA on another origin read the response.
//
// @dev luima answers the preflight already — transport.Options is why it is a 200 and not a 405 —
// but it sets no Access-Control-Allow-Origin, so without this the browser refuses the response
// and reports an error that names neither field. This is the fix, and it is net/http middleware,
// so it is portable to chi, echo or plain net/http.
//
// Origins are exact and listed. There is no Credentials knob, deliberately: the combination that
// makes a wildcard dangerous is not representable.
func ExampleCORS() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := luima.Run(ctx, ":8080", luima.Config{
		Schema: newStubSchema(),
		HTTPMiddleware: []func(http.Handler) http.Handler{
			luima.CORS(luima.CORSConfig{Origins: []string{"https://app.example.com"}}),
		},
	}); err != nil {
		log.Print(err)
	}
}

// ExampleRateLimit @notice Bounding what one caller can spend.
//
// @dev The hole this plugs is the one crud.List documents: an unbounded list field is reachable by
// anyone who can send { users { id } }, and ComplexityLimit cannot see it because row count is not
// an input to the complexity calculation.
//
// nil keys on r.RemoteAddr. Behind a proxy that is the proxy's address for every caller — one
// bucket for all of them, and a limiter that limits nothing — so pass a key func reading whichever
// header your proxy sets, and only if you control that proxy.
func ExampleRateLimit() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := luima.Run(ctx, ":8080", luima.Config{
		Schema: newStubSchema(),
		HTTPMiddleware: []func(http.Handler) http.Handler{
			// Outermost first: refuse before the query is parsed, not after.
			luima.RateLimit(100, time.Minute, nil),
			luima.CORS(luima.CORSConfig{Origins: []string{"https://app.example.com"}}),
		},
	}); err != nil {
		log.Print(err)
	}
}
