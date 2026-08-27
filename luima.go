// Package luima @notice The boilerplate between gqlgen and Fiber v3, minus the parts you would
// get wrong.
//
// @dev It is two things: a runtime — Config to a *fiber.App with the gqlgen handler mounted
// correctly, a Postgres pool, and an error presenter that does not leak your schema — and
// resolver-body helpers, generic CRUD over go-pg that gets the error classification right.
//
// It is not, and cannot be, a replacement for gqlgen codegen. gqlgen generates code into your
// module: generated.NewExecutableSchema is a symbol only your own `go tool gqlgen generate` run
// produces, from your own schema.graphqls. So you still own gqlgen.yml, schema.graphqls,
// graph/resolver.go and the codegen step — see docs/gqlgen-contract.md, because getting it wrong
// produces runtime panics that `go build` does not catch.
//
// Out of scope in v1, deliberately: auth, pagination, filtering, dataloaders, subscriptions,
// file upload, migrations, a scaffolding CLI.
//
// The one exception is cmd/luimagen, a separate binary that scaffolds a table's CRUD layer —
// this package gains no new surface from it, since nothing in the library imports it.
//
// # The packages
//
// This package re-exports the four sub-packages so the common case needs one import:
//
//	import "github.com/ulas96/luima"
//
//	app := luima.New(luima.Config{Schema: …})
//	return luima.Create(ctx, r.DB, u, "user "+id)
//
// Import them directly when you want a narrower dependency — a package that returns a
// *CustomError but must not pull in Fiber or gqlgen's handler wants luimaerr alone:
//
//	[github.com/ulas96/luima/server]   Config, New, Run, Mount, CORS, CORSConfig, RateLimit
//	[github.com/ulas96/luima/crud]     Get, List, Create, Update, Delete
//	[github.com/ulas96/luima/luimaerr] CustomError, PresentError, SQLState
//	[github.com/ulas96/luima/db]       Connect, ConnectWith, StatementTimeout
//
// The two spellings are interchangeable, not merely similar: the types below are aliases, so
// luima.Config and server.Config are the same type and either constructor accepts either literal.
//
// The cost of that convenience, stated plainly: every exported symbol lives at two import paths,
// and a new sub-package export has to be added to this file by hand or it stays invisible from the
// root. Aliases carry field and signature changes automatically — only genuinely new symbols can
// be missed, and there are fifteen of them.
package luima

import (
	"context"
	"net/http"
	"time"

	"github.com/go-pg/pg/v10"
	"github.com/go-pg/pg/v10/orm"
	"github.com/gofiber/fiber/v3"
	"github.com/vektah/gqlparser/v2/gqlerror"

	"github.com/ulas96/luima/crud"
	luimadb "github.com/ulas96/luima/db"
	"github.com/ulas96/luima/luimaerr"
	"github.com/ulas96/luima/server"
)

// Config @notice Assembles the server. See [server.Config] for the fields and their defaults.
type Config = server.Config

// CustomError @notice Carries a message the client is allowed to see. See [luimaerr.CustomError].
type CustomError = luimaerr.CustomError

// CORSConfig @notice Which browser origins may read the GraphQL response. See [server.CORSConfig].
type CORSConfig = server.CORSConfig

// New @notice Builds a *fiber.App with the GraphQL endpoint and playground mounted.
//
// @param cfg        the server configuration; only Schema is required
// @return *fiber.App an app ready for Listen. See [server.New].
func New(cfg Config) *fiber.App { return server.New(cfg) }

// Run @notice Builds the server, listens on addr, and blocks until ctx is done — then drains
// in-flight requests and returns.
//
// @dev The whole server in one call, naming no Fiber type — which is the point of it, and is
// asserted by the compiler in tests/luima_test.go rather than by review.
//
// @param ctx    cancel it to begin the drain; SIGTERM via signal.NotifyContext is the usual source
// @param addr   the listen address, e.g. ":8080"
// @param cfg    the server configuration; only Schema is required
// @return error a bind failure, a shutdown that exceeded the drain window, or nil. See [server.Run].
func Run(ctx context.Context, addr string, cfg Config) error { return server.Run(ctx, addr, cfg) }

// Mount @notice Registers the GraphQL routes on a router you already have — an existing app, or
// a group.
//
// @param r   a *fiber.App or the result of app.Group(prefix). See [server.Mount].
// @param cfg the server configuration; only Schema is required
func Mount(r fiber.Router, cfg Config) { server.Mount(r, cfg) }

// CORS @notice Cross-origin access as portable net/http middleware, for Config.HTTPMiddleware.
//
// @param c the origins and headers to allow
// @return func(http.Handler) http.Handler middleware, outermost-first. See [server.CORS].
func CORS(c CORSConfig) func(http.Handler) http.Handler { return server.CORS(c) }

// RateLimit @notice Fixed-window limiter as portable net/http middleware, for
// Config.HTTPMiddleware. Over the limit is 429 with Retry-After.
//
// @param n   requests allowed per window
// @param per the window
// @param key what to bucket on; nil means r.RemoteAddr
// @return func(http.Handler) http.Handler middleware, outermost-first. See [server.RateLimit].
func RateLimit(n int, per time.Duration, key func(*http.Request) string) func(http.Handler) http.Handler {
	return server.RateLimit(n, per, key)
}

// Connect @notice Opens the pool the resolvers query through and proves it works.
//
// @param url     a postgres:// or postgresql:// connection string
// @return *pg.DB a live pool, already proven with a round trip. See [db.Connect].
// @return error  a parse failure, or the ping failure with the pool already closed
func Connect(url string) (*pg.DB, error) { return luimadb.Connect(url) }

// ConnectWith @notice Connect, plus the pg.Options tuning a DSN cannot express.
//
// @param url     a postgres:// or postgresql:// connection string
// @param tune    called with the parsed options; nil is exactly Connect
// @return *pg.DB a live pool, already proven with a round trip. See [db.ConnectWith].
// @return error  a parse failure, or the ping failure with the pool already closed
func ConnectWith(url string, tune func(*pg.Options)) (*pg.DB, error) {
	return luimadb.ConnectWith(url, tune)
}

// StatementTimeout @notice A tune func for ConnectWith that bounds every query server-side.
//
// @param d the bound; Postgres cancels a statement that exceeds it. Zero or negative disables it.
// @return func(*pg.Options) a tune func for ConnectWith. See [db.StatementTimeout].
func StatementTimeout(d time.Duration) func(*pg.Options) { return luimadb.StatementTimeout(d) }

// PresentError @notice The error contract, and Config.ErrorPresenter's default.
//
// @dev A function rather than a var: a package-level var would let any consumer reassign the
// error contract for every other consumer in the binary.
//
// @param ctx  the resolver context, read only for the GraphQL field path
// @param err  the error a resolver returned
// @return *gqlerror.Error the message the client receives. See [luimaerr.PresentError].
func PresentError(ctx context.Context, err error) *gqlerror.Error {
	return luimaerr.PresentError(ctx, err)
}

// SQLState @notice Returns the Postgres SQLSTATE of err, or "" if err is not a driver error.
//
// @param err     any error, including nil and wrapped chains
// @return string the five-character SQLSTATE, or "". See [luimaerr.SQLState].
func SQLState(err error) string { return luimaerr.SQLState(err) }

// The CRUD five are wrappers rather than aliases because Go has no alias for a generic function,
// and an uninstantiated generic cannot be assigned to a var. Each re-declares its type parameter
// and instantiates the crud version.
//
// The parameter is named d, not db: this file imports a package called db.

// Get @notice Selects one row by primary key; a missing row is (nil, nil).
//
// @param ctx   the resolver context
// @param d     orm.DB — *pg.DB, *pg.Conn and *pg.Tx all satisfy it
// @param key   a model with only its primary key populated
// @param opts  query modifiers applied left to right, after WherePK
// @return *T   the stored row, or nil when no row matched. See [crud.Get].
// @return error any driver error other than pg.ErrNoRows
func Get[T any](ctx context.Context, d orm.DB, key *T, opts ...func(*orm.Query) *orm.Query) (*T, error) {
	return crud.Get[T](ctx, d, key, opts...)
}

// List @notice Selects rows, applying each opt to the query in order.
//
// @param ctx   the resolver context
// @param d     orm.DB — *pg.DB, *pg.Conn and *pg.Tx all satisfy it
// @param opts  query modifiers applied left to right; none means "select every row"
// @return []*T the rows, never nil. See [crud.List].
// @return error any driver error
func List[T any](ctx context.Context, d orm.DB, opts ...func(*orm.Query) *orm.Query) ([]*T, error) {
	return crud.List[T](ctx, d, opts...)
}

// Create @notice Inserts m and returns the stored row, classifying 23505 as a conflict.
//
// @param ctx    the resolver context
// @param d      orm.DB — *pg.DB, *pg.Conn and *pg.Tx all satisfy it
// @param m      the model to insert
// @param label  names the thing in the conflict message
// @param opts   query modifiers, e.g. q.OnConflict("DO NOTHING")
// @return *T    the stored row with RETURNING * applied, or nil if the insert was suppressed.
// See [crud.Create].
// @return error a *CustomError on 23505, nil on a suppressed insert, the bare driver error
// otherwise
func Create[T any](ctx context.Context, d orm.DB, m *T, label string, opts ...func(*orm.Query) *orm.Query) (*T, error) {
	return crud.Create[T](ctx, d, m, label, opts...)
}

// Update @notice Replaces every column of the row with m's primary key.
//
// @param ctx    the resolver context
// @param d      orm.DB — *pg.DB, *pg.Conn and *pg.Tx all satisfy it
// @param m      the complete model, primary key included; every column is written
// @param label  names the thing in the not-found message
// @param opts   query modifiers applied left to right, after WherePK; q.Column(...) narrows the
// SET clause, q.Where(...) scopes the update to rows the caller owns
// @return *T    the stored row. See [crud.Update].
// @return error a *CustomError when no row matched, the bare driver error otherwise
func Update[T any](ctx context.Context, d orm.DB, m *T, label string, opts ...func(*orm.Query) *orm.Query) (*T, error) {
	return crud.Update[T](ctx, d, m, label, opts...)
}

// Delete @notice Removes the row with key's primary key, reporting whether one was there.
//
// @param ctx    the resolver context
// @param d      orm.DB — *pg.DB, *pg.Conn and *pg.Tx all satisfy it
// @param key    a model with only its primary key populated
// @param opts   query modifiers applied left to right, after WherePK; this is where an ownership
// predicate goes
// @return bool  true when a row was deleted, false when none matched. See [crud.Delete].
// @return error any driver error
func Delete[T any](ctx context.Context, d orm.DB, key *T, opts ...func(*orm.Query) *orm.Query) (bool, error) {
	return crud.Delete[T](ctx, d, key, opts...)
}
