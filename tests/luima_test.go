package tests

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/vektah/gqlparser/v2/gqlerror"

	"github.com/ulas96/luima"
	"github.com/ulas96/luima/crud"
	luimadb "github.com/ulas96/luima/db"
	"github.com/ulas96/luima/luimaerr"
	"github.com/ulas96/luima/server"
)

// The root package re-exports four sub-packages, so the thing worth testing is that the two
// spellings are genuinely interchangeable rather than merely similar.

// row @notice A stand-in model for the generic signature assertions below.
//
// @dev Never queried — it exists only to instantiate the type parameters so the compiler can
// compare signatures.
type row struct {
	ID string `bun:"id,pk"`
}

var (
	// Aliases, not definitions: were these `type Config server.Config`, the round trip below
	// would not compile and a consumer could not pass a luima.Config to server.New.
	_ server.Config         = luima.Config{}
	_ luima.Config          = server.Config{}
	_ *luimaerr.CustomError = &luima.CustomError{}
	_ *luima.CustomError    = &luimaerr.CustomError{}
	_ server.CORSConfig     = luima.CORSConfig{}
	_ luima.CORSConfig      = server.CORSConfig{}

	// The non-generic wrappers. runFn is not only a shim check: spelled out in full it names no
	// Fiber type, which is the guarantee the whole Fiber-seam work exists for — checked here by
	// the compiler rather than by review. Change Run to take or return anything from Fiber and
	// this line stops building. New and Mount are the other way in, and naming Fiber is their job.
	_, _ newFn      = luima.New, server.New
	_, _ runFn      = luima.Run, server.Run
	_, _ mountFn    = luima.Mount, server.Mount
	_, _ corsFn     = luima.CORS, server.CORS
	_, _ rateFn     = luima.RateLimit, server.RateLimit
	_, _ connFn     = luima.Connect, luimadb.Connect
	_, _ connWFn    = luima.ConnectWith, luimadb.ConnectWith
	_, _ stmtFn     = luima.StatementTimeout, luimadb.StatementTimeout
	_, _ presentFn  = luima.PresentError, luimaerr.PresentError
	_, _ sqlStateFn = luima.SQLState, luimaerr.SQLState

	// The CRUD five are generic wrappers rather than aliases, so the check is that each
	// instantiates to exactly the signature of the crud original. Assigning both to one
	// typed var is the compile-time assertion — funcs cannot be compared to each other.
	// Nothing here runs, and none of it needs a database.
	_, _ getFn    = luima.Get[row], crud.Get[row]
	_, _ listFn   = luima.List[row], crud.List[row]
	_, _ createFn = luima.Create[row], crud.Create[row]
	_, _ updateFn = luima.Update[row], crud.Update[row]
	_, _ delFn    = luima.Delete[row], crud.Delete[row]
)

// The shapes the CRUD five instantiate to. Declared as aliases so the assertions above read as one
// type per line.
//
// All five take the query-modifier variadic. Get, Update and Delete need it so an ownership
// predicate is expressible at all, and List has always had it. Create was the exception, on the
// reasoning that an INSERT has no WHERE clause to scope — sound as far as it went, and wrong: the
// clause it needs is ON CONFLICT, the one way to attempt an insert inside a transaction without a
// real 23505 aborting the whole thing that costs no extra statement — a savepoint costs two.
//
// The modifier is typed per statement, four aliases for five functions, because bun has no single
// query type: Get and List build a *bun.SelectQuery, and Create, Update and Delete each build
// their own. A closure written for the wrong statement is a compile error at the call site.
type (
	middleware = func(http.Handler) http.Handler

	newFn      = func(luima.Config) *fiber.App
	runFn      = func(context.Context, string, luima.Config) error
	mountFn    = func(fiber.Router, luima.Config)
	corsFn     = func(luima.CORSConfig) middleware
	rateFn     = func(int, time.Duration, func(*http.Request) string) middleware
	connFn     = func(string) (*bun.DB, error)
	connWFn    = func(string, func(*pgdriver.Config)) (*bun.DB, error)
	stmtFn     = func(time.Duration) func(*pgdriver.Config)
	presentFn  = func(context.Context, error) *gqlerror.Error
	sqlStateFn = func(error) string

	selOpt   = func(*bun.SelectQuery) *bun.SelectQuery
	insOpt   = func(*bun.InsertQuery) *bun.InsertQuery
	updOpt   = func(*bun.UpdateQuery) *bun.UpdateQuery
	delOpt   = func(*bun.DeleteQuery) *bun.DeleteQuery
	getFn    = func(context.Context, bun.IDB, *row, ...selOpt) (*row, error)
	listFn   = func(context.Context, bun.IDB, ...selOpt) ([]*row, error)
	createFn = func(context.Context, bun.IDB, *row, string, ...insOpt) (*row, error)
	updateFn = func(context.Context, bun.IDB, *row, string, ...updOpt) (*row, error)
	delFn    = func(context.Context, bun.IDB, *row, ...delOpt) (bool, error)
)

// TestShimsDispatch @notice A smoke test: every re-exported function must actually reach the
// sub-package implementation.
//
// @dev Deliberately shallow — the behaviour is tested in the packages that own it — but it
// catches a shim wired to the wrong callee, which the signature assertions above cannot.
//
// @param t the test handle
func TestShimsDispatch(t *testing.T) {
	ctx := context.Background()

	t.Run("PresentError", func(t *testing.T) {
		leak := errors.New("ERROR: duplicate key (SQLSTATE=23505)")
		if got := luima.PresentError(ctx, leak).Message; got != "internal server error" {
			t.Errorf("luima.PresentError = %q, want the redaction", got)
		}
		if got := luima.PresentError(ctx, &luima.CustomError{UserMessage: "ok"}).Message; got != "ok" {
			t.Errorf("luima.PresentError on a CustomError = %q", got)
		}
	})

	t.Run("SQLState", func(t *testing.T) {
		if got := luima.SQLState(errors.New("not a driver error")); got != "" {
			t.Errorf("luima.SQLState = %q, want empty", got)
		}
	})

	t.Run("Connect rejects a bad url without dialing", func(t *testing.T) {
		// sslmode is one of the few parameters pgdriver's parseDSN reads itself, and it refuses a
		// value it does not know before anything is dialed. An unknown parameter would not do
		// here: parseDSN turns every name it does not recognise into a SET sent on each new
		// connection, so that URL parses and fails only once Connect dials. It comes back as an
		// error rather than a panic only because Connect guards the parse — pgdriver.WithDSN
		// panics on every DSN parseDSN refuses — and TestConnectNeverPanicsOrLeaks pins that guard.
		if _, err := luima.Connect("postgres://u:p@h:5432/d?sslmode=bogus"); err == nil {
			t.Error("Connect accepted an unsupported sslmode")
		}
	})

	t.Run("New mounts the routes", func(t *testing.T) {
		app := luima.New(luima.Config{Schema: newStubSchema(), DisablePlayground: true})

		res, err := app.Test(httptest.NewRequest(http.MethodOptions, "/graphql", nil))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()

		if res.Header.Get("Allow") == "" {
			t.Error("luima.New did not mount gqlgen at /graphql")
		}
	})

	t.Run("Mount takes a router", func(t *testing.T) {
		app := luima.New(luima.Config{Schema: newStubSchema(), DisablePlayground: true})
		luima.Mount(app.Group("/api"), luima.Config{Schema: newStubSchema(), DisablePlayground: true})

		res, err := app.Test(httptest.NewRequest(http.MethodOptions, "/api/graphql", nil))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()

		if res.Header.Get("Allow") == "" {
			t.Error("luima.Mount did not mount gqlgen under the group prefix")
		}
	})
}
