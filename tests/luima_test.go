package tests

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-pg/pg/v10/orm"

	"github.com/ulas96/luima"
	"github.com/ulas96/luima/crud"
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
	ID string `pg:"id,pk"`
}

var (
	// Aliases, not definitions: were these `type Config server.Config`, the round trip below
	// would not compile and a consumer could not pass a luima.Config to server.New.
	_ server.Config         = luima.Config{}
	_ luima.Config          = server.Config{}
	_ *luimaerr.CustomError = &luima.CustomError{}
	_ *luima.CustomError    = &luimaerr.CustomError{}

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
// clause it needs is ON CONFLICT, which is the only way to attempt an insert inside a transaction
// without a real 23505 aborting the whole thing.
type (
	opt      = func(*orm.Query) *orm.Query
	getFn    = func(context.Context, orm.DB, *row, ...opt) (*row, error)
	listFn   = func(context.Context, orm.DB, ...opt) ([]*row, error)
	createFn = func(context.Context, orm.DB, *row, string, ...opt) (*row, error)
	updateFn = func(context.Context, orm.DB, *row, string, ...opt) (*row, error)
	delFn    = func(context.Context, orm.DB, *row, ...opt) (bool, error)
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
		leak := errors.New("ERROR: duplicate key (SQLSTATE 23505)")
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
		// pg.ParseURL rejects any query parameter beyond its three, so this fails at parse
		// time and needs no database.
		if _, err := luima.Connect("postgres://u:p@h:5432/d?default_query_exec_mode=simple"); err == nil {
			t.Error("Connect accepted an unsupported query parameter")
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
