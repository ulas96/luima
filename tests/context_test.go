package tests

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/99designs/gqlgen/graphql"
	"github.com/gofiber/fiber/v3"

	"github.com/ulas96/luima/server"
)

// userKey @notice A typed context key, as middleware passing identity should use.
type userKey struct{}

// postQuery @notice Builds the POST that every test here dispatches.
//
// @dev transport.POST requires application/json; without the header gqlgen answers 422 and the
// resolver never runs, so every assertion below would vacuously pass.
//
// @return *http.Request a POST /graphql carrying `{ping}`
func postQuery() *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(`{"query":"{ping}"}`))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// TestResolverContext @notice Pins that the resolver receives a real context.Context rather than
// the raw *fasthttp.RequestCtx.
//
// @dev Without adaptor.HTTPHandlerWithContext + withFiberContext, fasthttpadaptor hands the
// handler the *fasthttp.RequestCtx: Deadline reports nothing, Done closes only at server shutdown,
// and everything middleware put in c.SetContext is discarded. Each subtest below fails against
// that, and all three fail silently in production — a 200 with the right body either way.
//
// @param t the test handle
func TestResolverContext(t *testing.T) {
	// The idiomatic Fiber v3 route. This is what SECURITY.md tells a consumer to do, and before
	// the context fix it was the one that silently did not work.
	t.Run("c.SetContext reaches the resolver", func(t *testing.T) {
		var got any
		app := fiber.New()
		app.Use(func(c fiber.Ctx) error {
			c.SetContext(context.WithValue(c.Context(), userKey{}, "user-42"))
			return c.Next()
		})
		server.Mount(app, server.Config{
			DisablePlayground: true,
			Schema: newStubSchemaFunc(func(ctx context.Context) *graphql.Response {
				got = ctx.Value(userKey{})
				return &graphql.Response{Data: []byte(`{"ping":"pong"}`)}
			}),
		})

		mustPost(t, app)
		if got != "user-42" {
			t.Errorf("ctx.Value(userKey{}) = %v, want %q — the middleware's context was discarded", got, "user-42")
		}
	})

	// The legacy route, and the reason fiberContext keeps a fallback to fasthttp's user values.
	// Re-attaching c.Context() alone would root the resolver context at context.Background(),
	// which cannot see what c.Locals wrote — turning a fix into a silent auth regression for
	// anyone who found the Locals path first.
	t.Run("c.Locals reaches the resolver", func(t *testing.T) {
		var got any
		app := fiber.New()
		app.Use(func(c fiber.Ctx) error {
			c.Locals("user", "user-42")
			return c.Next()
		})
		server.Mount(app, server.Config{
			DisablePlayground: true,
			Schema: newStubSchemaFunc(func(ctx context.Context) *graphql.Response {
				got = ctx.Value("user")
				return &graphql.Response{Data: []byte(`{"ping":"pong"}`)}
			}),
		})

		mustPost(t, app)
		if got != "user-42" {
			t.Errorf(`ctx.Value("user") = %v, want %q — the locals fallback in fiberContext is gone`, got, "user-42")
		}
	})

	t.Run("the resolver context carries a deadline", func(t *testing.T) {
		var ok bool
		app := fiber.New()
		server.Mount(app, server.Config{
			DisablePlayground: true,
			Schema: newStubSchemaFunc(func(ctx context.Context) *graphql.Response {
				_, ok = ctx.Deadline()
				return &graphql.Response{Data: []byte(`{"ping":"pong"}`)}
			}),
		})

		mustPost(t, app)
		if !ok {
			t.Error("ctx.Deadline() reported no deadline — RequestTimeout did not reach the resolver")
		}
	})
}

// TestRequestTimeout @notice Pins that RequestTimeout both cancels the resolver and answers 408.
//
// @dev This is also what pins the wrapper order in Mount. Nest timeout.New *inside*
// HTTPHandlerWithContext instead of outside and the deadline is set after c.Context() has already
// been read, so the resolver never sees it: this test hangs until app.Test's own 1s timeout
// instead of returning 408.
//
// The cancellation half is the part that matters in production. go-pg turns ctx.Done() into a
// Postgres CancelRequest, so a resolver context that never cancels means an abandoned request
// still runs its query to completion.
//
// @param t the test handle
func TestRequestTimeout(t *testing.T) {
	cancelled := make(chan struct{})

	app := fiber.New()
	server.Mount(app, server.Config{
		DisablePlayground: true,
		RequestTimeout:    50 * time.Millisecond,
		Schema: newStubSchemaFunc(func(ctx context.Context) *graphql.Response {
			select {
			case <-ctx.Done():
				close(cancelled)
			case <-time.After(5 * time.Second):
			}
			return &graphql.Response{Data: []byte(`{"ping":"slow"}`)}
		}),
	})

	res, err := app.Test(postQuery())
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusRequestTimeout {
		t.Errorf("POST /graphql = %d, want 408", res.StatusCode)
	}

	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Error("the resolver's ctx.Done() never closed — the deadline did not reach it")
	}
}

// TestRequestTimeoutDisabled @notice Asserts a negative RequestTimeout means "no deadline", the
// same spelling QueryCache and ComplexityLimit use.
//
// @param t the test handle
func TestRequestTimeoutDisabled(t *testing.T) {
	var ok bool
	app := fiber.New()
	server.Mount(app, server.Config{
		DisablePlayground: true,
		RequestTimeout:    -1,
		Schema: newStubSchemaFunc(func(ctx context.Context) *graphql.Response {
			_, ok = ctx.Deadline()
			return &graphql.Response{Data: []byte(`{"ping":"pong"}`)}
		}),
	})

	mustPost(t, app)
	if ok {
		t.Error("ctx.Deadline() was set with RequestTimeout: -1, which is meant to disable it")
	}
}

// TestDisableIntrospection @notice Asserts the flag reaches the field that gates introspection,
// and that its absence leaves introspection on.
//
// @dev It asserts on OperationContext.DisableIntrospection rather than on a __schema response,
// because that is the field a *generated* schema branches on — gqlgen writes
// `if ec.DisableIntrospection { return nil, errors.New("introspection disabled") }` into
// introspectSchema and introspectType. The stub here has no generated code, so querying __schema
// would prove nothing; this reads the same flag the real gate reads.
//
// gqlgen's own default is true (off). extension.Introspection is what sets it false, so the
// assertion for the zero Config is that Mount registered the extension — without it the
// playground's docs pane is blind, which is why the zero Config has to keep it on.
//
// @param t the test handle
func TestDisableIntrospection(t *testing.T) {
	disabled := func(t *testing.T, disable bool) bool {
		t.Helper()

		var got bool
		app := server.New(server.Config{
			DisablePlayground:    true,
			DisableIntrospection: disable,
			Schema: newStubSchemaFunc(func(ctx context.Context) *graphql.Response {
				got = graphql.GetOperationContext(ctx).DisableIntrospection
				return &graphql.Response{Data: []byte(`{"ping":"pong"}`)}
			}),
		})

		mustPost(t, app)
		return got
	}

	if disabled(t, false) {
		t.Error("introspection was off for a zero Config — the docs pane needs it on by default")
	}
	if !disabled(t, true) {
		t.Error("introspection was still on with DisableIntrospection: true")
	}
}

// TestPanicLeaksNoStack @notice Asserts a panicking resolver produces no stack frame on the wire.
//
// @dev Three recovers now sit in this stack — gqlgen's DefaultRecover, the timeout middleware's,
// and fasthttpadaptor's — and adding RequestTimeout rearranged the wrapping underneath them. The
// response is a constant either way, so a regression here is invisible without this assertion.
// docs/gqlgen-contract.md establishes that an unfilled resolver stub is an unauthenticated remote
// panic that compiles clean, which is what makes the path worth pinning.
//
// @param t the test handle
func TestPanicLeaksNoStack(t *testing.T) {
	app := fiber.New()
	server.Mount(app, server.Config{
		DisablePlayground: true,
		Schema: newStubSchemaFunc(func(context.Context) *graphql.Response {
			panic("boom: postgres://app:hunter2@db.internal/app")
		}),
	})

	res, err := app.Test(postQuery())
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}

	for _, leak := range []string{"hunter2", "goroutine", ".go:", "runtime."} {
		if strings.Contains(string(b), leak) {
			t.Errorf("panic response contains %q: %s", leak, b)
		}
	}
}

// mustPost @notice Dispatches `{ping}` and fails the test unless it answers 200.
//
// @param t   the test handle
// @param app the Fiber app under test
func mustPost(t *testing.T, app *fiber.App) {
	t.Helper()

	res, err := app.Test(postQuery())
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("POST /graphql = %d, want 200", res.StatusCode)
	}
}
