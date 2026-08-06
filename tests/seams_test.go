package tests

import (
	"context"
	"io"
	"mime"
	"net/http"
	"strings"
	"testing"

	"github.com/99designs/gqlgen/graphql"
	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/gofiber/fiber/v3"

	"github.com/ulas96/luima/server"
)

// mwKey @notice A typed context key, as HTTPMiddleware passing identity should use.
type mwKey struct{}

// TestHTTPMiddleware @notice Pins the three properties Config.HTTPMiddleware exists for: a value
// it puts in the request context reaches the resolver, the request context it receives already
// carries the RequestTimeout deadline, and a Set-Cookie it writes survives the adaptor.
//
// @dev Each corresponds to a line in Mount that fails silently when moved. Chain the middleware
// outside withFiberContext instead of inside and the middleware receives the raw fasthttp
// context — no deadline, and its r.WithContext is discarded before gqlgen ever runs; both
// subassertions below catch that as a nil value and a missing deadline, not as an error. The
// cookie assertion pins fasthttpadaptor's header copy-back: it is what makes a login-shaped
// middleware possible at all, and nothing else in the suite would notice it breaking.
//
// @param t the test handle
func TestHTTPMiddleware(t *testing.T) {
	t.Run("context value, deadline and Set-Cookie reach through", func(t *testing.T) {
		var (
			gotValue        any
			mwSawDeadline   bool
			execSawDeadline bool
		)

		app := fiber.New()
		server.Mount(app, server.Config{
			DisablePlayground: true,
			HTTPMiddleware: []func(http.Handler) http.Handler{
				func(next http.Handler) http.Handler {
					return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						_, mwSawDeadline = r.Context().Deadline()
						http.SetCookie(w, &http.Cookie{Name: "mw", Value: "set-by-middleware", Path: "/"})
						next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), mwKey{}, "user-42")))
					})
				},
			},
			Schema: newStubSchemaFunc(func(ctx context.Context) *graphql.Response {
				gotValue = ctx.Value(mwKey{})
				_, execSawDeadline = ctx.Deadline()
				return &graphql.Response{Data: []byte(`{"ping":"pong"}`)}
			}),
		})

		res, err := app.Test(postQuery())
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()

		if res.StatusCode != http.StatusOK {
			t.Fatalf("POST /graphql = %d, want 200", res.StatusCode)
		}
		if gotValue != "user-42" {
			t.Errorf("ctx.Value(mwKey{}) = %v, want %q — the middleware's r.WithContext was discarded", gotValue, "user-42")
		}
		if !mwSawDeadline {
			t.Error("the middleware's r.Context() had no deadline — the chain is outside withFiberContext")
		}
		if !execSawDeadline {
			t.Error("the resolver's ctx.Deadline() was unset — the middleware chain detached the Fiber context")
		}

		var cookie string
		for _, c := range res.Cookies() {
			if c.Name == "mw" {
				cookie = c.Value
			}
		}
		if cookie != "set-by-middleware" {
			t.Errorf("Set-Cookie mw=%q, want %q — the middleware's header did not survive the adaptor", cookie, "set-by-middleware")
		}
	})

	t.Run("index 0 is outermost", func(t *testing.T) {
		var order []string
		mw := func(name string) func(http.Handler) http.Handler {
			return func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					order = append(order, name)
					next.ServeHTTP(w, r)
				})
			}
		}

		app := fiber.New()
		server.Mount(app, server.Config{
			DisablePlayground: true,
			HTTPMiddleware:    []func(http.Handler) http.Handler{mw("outer"), mw("inner")},
			Schema:            newStubSchema(),
		})

		mustPost(t, app)
		if len(order) != 2 || order[0] != "outer" || order[1] != "inner" {
			t.Errorf("execution order = %v, want [outer inner] — the wrap loop is not reversed", order)
		}
	})
}

// TestConfigure @notice Pins that Config.Configure receives the server that actually serves, by
// registering an operation interceptor and asserting it short-circuits the response.
//
// @dev The seam exists because srv is otherwise a local that never escapes Mount, leaving Use,
// AroundOperations, SetRecoverFunc, SetParserTokenLimit and SetDisableSuggestion all unreachable.
// A Configure applied to a copy, or before Mount's own defaults, would accept the interceptor and
// silently never run it — this test reads the denial off the wire, so either failure mode is a
// red test rather than a production surprise.
//
// graphql.OneShot is load-bearing, not style: gqlgen's own source warns that an interceptor
// short-circuiting without it makes streaming transports loop forever.
//
// @param t the test handle
func TestConfigure(t *testing.T) {
	var execRan bool

	app := fiber.New()
	server.Mount(app, server.Config{
		DisablePlayground: true,
		Configure: func(srv *handler.Server) {
			srv.AroundOperations(func(ctx context.Context, next graphql.OperationHandler) graphql.ResponseHandler {
				return graphql.OneShot(graphql.ErrorResponse(ctx, "configure says no"))
			})
		},
		Schema: newStubSchemaFunc(func(context.Context) *graphql.Response {
			execRan = true
			return &graphql.Response{Data: []byte(`{"ping":"pong"}`)}
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

	if !strings.Contains(string(b), "configure says no") {
		t.Errorf("response %s does not carry the interceptor's error — Configure never reached the served handler", b)
	}
	if execRan {
		t.Error("the resolver ran despite the interceptor short-circuiting the operation")
	}
}

// markerTransport @notice A transport that answers exactly what transport.POST answers, and says so.
//
// @dev Supports is deliberately a copy of transport.POST's — POST + application/json, no Accept
// check — because that is the shape of the collision being tested. transport.SSE's Supports is a
// strict subset of the same match, so if this one loses to POST, SSE would too.
type markerTransport struct{}

// Supports @notice Matches the same requests transport.POST does.
//
// @param r     the incoming request
// @return bool whether this transport will serve it
func (markerTransport) Supports(r *http.Request) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return false
	}
	return r.Method == http.MethodPost && mediaType == "application/json"
}

// Do @notice Writes a marker instead of executing anything, so the test can read which transport won.
//
// @param w the response writer
func (markerTransport) Do(w http.ResponseWriter, _ *http.Request, _ graphql.GraphExecutor) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"data":{"servedBy":"configure"}}`))
}

// TestConfigureCanOutrankPOST @notice Pins that a transport registered in Configure is selected
// ahead of luima's own transport.POST.
//
// @dev gqlgen picks the *first* transport whose Supports returns true (handler/server.go:132-139)
// and AddTransport appends (:78-80). transport.POST's Supports is a superset of every streaming
// transport's — SSE wants POST + application/json + Accept: text/event-stream, and POST never reads
// Accept — so registering POST before Configure runs makes a Configure-registered streaming
// transport unreachable by construction. It compiles, it mounts, it returns 200, and every
// subscription silently degrades to one buffered response. This test is the only thing that
// distinguishes that from working.
//
// @param t the test handle
func TestConfigureCanOutrankPOST(t *testing.T) {
	var execRan bool

	app := fiber.New()
	server.Mount(app, server.Config{
		DisablePlayground: true,
		Configure: func(srv *handler.Server) {
			srv.AddTransport(markerTransport{})
		},
		Schema: newStubSchemaFunc(func(context.Context) *graphql.Response {
			execRan = true
			return &graphql.Response{Data: []byte(`{"ping":"pong"}`)}
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

	if !strings.Contains(string(b), `"servedBy":"configure"`) {
		t.Errorf("response %s came from transport.POST — a Configure-registered transport is unreachable", b)
	}
	if execRan {
		t.Error("gqlgen executed the operation, so luima's own transport served the request")
	}
}
