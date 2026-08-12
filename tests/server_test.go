package tests

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/ulas96/luima/server"
)

// TestMountRoutes @notice Pins the two Fiber behaviours that fail silently.
//
// @dev The OPTIONS case is the one that matters: register the endpoint with r.Post instead of
// r.All and every browser client 405s on the CORS preflight, with nothing in the server log
// because the request never reaches gqlgen.
//
// @param t the test handle
func TestMountRoutes(t *testing.T) {
	app := server.New(server.Config{Schema: newStubSchema(), PlaygroundTitle: "luima-under-test"})

	t.Run("POST reaches gqlgen", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(`{"query":"{ping}"}`))
		req.Header.Set("Content-Type", "application/json")
		assertBody(t, app, req, http.StatusOK, "pong")
	})

	t.Run("GET reaches gqlgen", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/graphql?query=%7Bping%7D", nil)
		assertBody(t, app, req, http.StatusOK, "pong")
	})

	t.Run("OPTIONS preflight reaches gqlgen", func(t *testing.T) {
		res, err := app.Test(httptest.NewRequest(http.MethodOptions, "/graphql", nil))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()

		if res.StatusCode != http.StatusOK {
			t.Fatalf("OPTIONS /graphql = %d, want 200 — a 405 here means the route was registered with Post, not All", res.StatusCode)
		}
		// transport.Options is the only thing that sets Allow, so this proves the request
		// reached gqlgen rather than being answered by Fiber.
		if allow := res.Header.Get("Allow"); allow == "" {
			t.Error("no Allow header — the preflight did not reach gqlgen's Options transport")
		}
	})

	// r.All forwards every method, and gqlgen answers the ones no transport Supports with 422
	// rather than 405 — so this is what "All" costs, and it is worth pinning next to the line the
	// project calls its most important. Harmless: gqlgen reflects no request body, so TRACE
	// discloses nothing.
	t.Run("methods no transport serves reach gqlgen and are refused", func(t *testing.T) {
		for _, method := range []string{http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodTrace} {
			res, err := app.Test(httptest.NewRequest(method, "/graphql", nil))
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(res.Body)
			res.Body.Close()

			if res.StatusCode == http.StatusOK {
				t.Errorf("%s /graphql = 200 — no transport should serve it", method)
			}
			if strings.Contains(string(body), "pong") {
				t.Errorf("%s /graphql executed the query: %s", method, body)
			}
		}
	})

	t.Run("playground on /", func(t *testing.T) {
		assertBody(t, app, httptest.NewRequest(http.MethodGet, "/", nil), http.StatusOK, "luima-under-test")
	})

	// net/http's Handle("/", ...) was a prefix match and served the playground for every
	// unrouted path. Fiber's Get("/") is exact. This is the better behaviour and it surprises
	// everyone porting from net/http.
	t.Run("unknown path is 404, not the playground", func(t *testing.T) {
		res, err := app.Test(httptest.NewRequest(http.MethodGet, "/definitely-not-a-route", nil))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()

		if res.StatusCode != http.StatusNotFound {
			t.Errorf("GET /definitely-not-a-route = %d, want 404", res.StatusCode)
		}
	})
}

// TestDisablePlayground @notice Asserts that DisablePlayground removes the route entirely rather
// than serving an empty page.
//
// @param t the test handle
func TestDisablePlayground(t *testing.T) {
	app := server.New(server.Config{Schema: newStubSchema(), DisablePlayground: true})

	res, err := app.Test(httptest.NewRequest(http.MethodGet, "/", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusNotFound {
		t.Errorf("GET / with DisablePlayground = %d, want 404", res.StatusCode)
	}
}

// TestMountOnGroup @notice Covers the entry point for an app that already exists.
//
// @dev Mounting on a group puts the endpoint under the group's prefix, which is the whole reason
// Mount takes a fiber.Router rather than a *fiber.App.
//
// @param t the test handle
func TestMountOnGroup(t *testing.T) {
	app := fiber.New()
	server.Mount(app.Group("/api"), server.Config{
		Schema:            newStubSchema(),
		DisablePlayground: true,
	})

	req := httptest.NewRequest(http.MethodPost, "/api/graphql", strings.NewReader(`{"query":"{ping}"}`))
	req.Header.Set("Content-Type", "application/json")
	assertBody(t, app, req, http.StatusOK, "pong")
}

// TestCustomEndpoint @notice Asserts that a non-default Endpoint is honoured, and that the
// playground is told about it.
//
// @dev The second assertion is the load-bearing one: the playground hardcodes the endpoint into
// the HTML it serves, so pointing it at the default while the handler lives elsewhere produces a
// GraphiQL that 404s on every query.
//
// @param t the test handle
func TestCustomEndpoint(t *testing.T) {
	app := server.New(server.Config{
		Schema:     newStubSchema(),
		Endpoint:   "/api/v1/graphql",
		Playground: "/playground",
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/graphql", strings.NewReader(`{"query":"{ping}"}`))
	req.Header.Set("Content-Type", "application/json")
	assertBody(t, app, req, http.StatusOK, "pong")

	// The playground has to point at the endpoint it was configured with, not the default.
	assertBody(t, app, httptest.NewRequest(http.MethodGet, "/playground", nil), http.StatusOK, "/api/v1/graphql")
}

// TestZeroConfigHasTimeouts @notice Asserts that a zero Config ships transport deadlines.
//
// @dev The regression this exists for is invisible from the outside: fiber.New assigns cfg.Fiber's
// timeouts to fasthttp verbatim, fasthttp reads zero as no deadline, and a server with no deadline
// answers every request in every test exactly like one that has them. It fails at 3am instead,
// when one client dribbles a request body and never lets the slot go. Reading the fasthttp server
// back is the only way to see it.
//
// app.Server() returns the *fasthttp.Server, and selecting a field off it needs no import of
// fasthttp here — which is why this assertion costs the test module nothing.
//
// @param t the test handle
func TestZeroConfigHasTimeouts(t *testing.T) {
	srv := server.New(server.Config{Schema: newStubSchema()}).Server()

	if srv.ReadTimeout <= 0 {
		t.Errorf("ReadTimeout = %v, want a positive default — zero lets a client hold a connection slot forever", srv.ReadTimeout)
	}
	if srv.WriteTimeout <= 0 {
		t.Errorf("WriteTimeout = %v, want a positive default", srv.WriteTimeout)
	}
}

// TestTimeoutPrecedence @notice Pins the stated winner when a timeout has both spellings.
//
// @dev Two spellings for one value is tolerable only with a documented precedence, and a
// documented precedence is worth nothing unless something fails when it flips. Config.Fiber wins:
// it is a whole fiber.Config the consumer built deliberately.
//
// @param t the test handle
func TestTimeoutPrecedence(t *testing.T) {
	t.Run("Fiber wins over the promoted field", func(t *testing.T) {
		srv := server.New(server.Config{
			Schema:       newStubSchema(),
			ReadTimeout:  time.Second,
			WriteTimeout: time.Second,
			Fiber:        fiber.Config{ReadTimeout: 2 * time.Second, WriteTimeout: 3 * time.Second},
		}).Server()

		if srv.ReadTimeout != 2*time.Second {
			t.Errorf("ReadTimeout = %v, want 2s from Config.Fiber", srv.ReadTimeout)
		}
		if srv.WriteTimeout != 3*time.Second {
			t.Errorf("WriteTimeout = %v, want 3s from Config.Fiber", srv.WriteTimeout)
		}
	})

	t.Run("the promoted field fills a zero Fiber", func(t *testing.T) {
		srv := server.New(server.Config{
			Schema:       newStubSchema(),
			ReadTimeout:  4 * time.Second,
			WriteTimeout: 5 * time.Second,
		}).Server()

		if srv.ReadTimeout != 4*time.Second {
			t.Errorf("ReadTimeout = %v, want 4s", srv.ReadTimeout)
		}
		if srv.WriteTimeout != 5*time.Second {
			t.Errorf("WriteTimeout = %v, want 5s", srv.WriteTimeout)
		}
	})

	// The one case a simpler resolver gets wrong. Negative is luima's spelling for "no deadline",
	// and fasthttp's spelling for that is zero — the same zero luima reads as "unset". Anything
	// that treats the two as one value either ignores the negative or hands fasthttp a negative
	// duration, and a negative deadline is one that has already passed.
	t.Run("negative disables", func(t *testing.T) {
		srv := server.New(server.Config{
			Schema:       newStubSchema(),
			ReadTimeout:  -1,
			WriteTimeout: -1,
		}).Server()

		if srv.ReadTimeout != 0 {
			t.Errorf("ReadTimeout = %v, want 0 — fasthttp's spelling for no deadline", srv.ReadTimeout)
		}
		if srv.WriteTimeout != 0 {
			t.Errorf("WriteTimeout = %v, want 0", srv.WriteTimeout)
		}
	})
}

// TestMountRequiresSchema @notice Asserts that a nil Config.Schema fails at Mount, not per request.
//
// @dev Without the guard this is not a compile error and not a boot error: Mount succeeds, the
// process starts, and every request panics inside gqlgen's executor — recovered by gqlgen's own
// handler and returned as "internal system error" with no extensions.code, which is not luima's
// error contract, so nothing keyed on INTERNAL_SERVER_ERROR fires. Measured before the fix.
//
// @param t the test handle
func TestMountRequiresSchema(t *testing.T) {
	for name, call := range map[string]func(){
		"Mount": func() { server.Mount(fiber.New(), server.Config{}) },
		"New":   func() { server.New(server.Config{}) },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				r := recover()
				if r == nil {
					t.Fatalf("%s(Config{}) did not panic — a nil schema mounts cleanly and then panics on every request instead", name)
				}
				if msg, _ := r.(string); !strings.Contains(msg, "Config.Schema") {
					t.Errorf("panic = %v, want it to name Config.Schema", r)
				}
			}()
			call()
		})
	}
}

// TestRunReturnsOnContext @notice Pins that Run binds, drains and returns — and never hangs.
//
// @dev The deadlock this exists for is a race, which is why the middle case runs a loop rather
// than once. fasthttp's Shutdown returns nil having closed nothing when Serve has not appended the
// listener yet (it checks s.ln == nil), so a cancellation landing in the first microseconds of
// startup shuts down a server that is not yet listening — and the listener then serves forever
// with nothing left to stop it. Run closes the listener itself for exactly that window. Against an
// implementation that only calls ShutdownWithContext, this test hangs rather than fails.
//
// @param t the test handle
func TestRunReturnsOnContext(t *testing.T) {
	t.Run("an already-cancelled context returns without binding", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		// The port is held, so a Run that got as far as net.Listen would come back with a bind
		// error. nil is the proof that it never tried.
		if err := server.Run(ctx, ln.Addr().String(), server.Config{Schema: newStubSchema()}); err != nil {
			t.Errorf("Run(cancelled) = %v, want nil — it should return before binding", err)
		}
	})

	t.Run("cancelling during startup still drains and returns", func(t *testing.T) {
		for i := range 20 {
			ctx, cancel := context.WithCancel(context.Background())

			done := make(chan error, 1)
			go func() {
				done <- server.Run(ctx, "127.0.0.1:0", server.Config{Schema: newStubSchema()})
			}()

			// A different point in startup each round, so the race window is actually visited
			// rather than assumed.
			time.Sleep(time.Duration(i) * 50 * time.Microsecond)
			cancel()

			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("round %d: Run = %v, want nil after a clean drain", i, err)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("round %d: Run did not return within 5s of cancellation — the shutdown raced the listener", i)
			}
		}
	})

	// Owning the listener is what makes this synchronous. Handed to app.Listen inside the
	// goroutine instead, a bind failure would have to be raced back out through the channel.
	t.Run("a bind failure is returned", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		if err := server.Run(ctx, ln.Addr().String(), server.Config{Schema: newStubSchema()}); err == nil {
			t.Error("Run on a held port = nil, want a bind error")
		}
	})
}

// TestHealth @notice Pins the liveness path: 200 up, 503 broken, and 503 rather than a hang.
//
// @dev The last case is the one worth the goroutine in Mount. A check that never reads the context
// it is handed cannot be cancelled, so a probe that only sets a deadline still blocks — and a
// liveness probe that hangs reads to a load balancer as a slow server rather than a broken one,
// which is the difference between being rotated out and being left in.
//
// @param t the test handle
func TestHealth(t *testing.T) {
	get := func(t *testing.T, cfg server.Config, timeout time.Duration) *http.Response {
		t.Helper()

		res, err := server.New(cfg).Test(
			httptest.NewRequest(http.MethodGet, "/healthz", nil),
			fiber.TestConfig{Timeout: timeout, FailOnTimeout: true},
		)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { res.Body.Close() })
		return res
	}

	t.Run("an empty Health registers no route", func(t *testing.T) {
		res := get(t, server.Config{Schema: newStubSchema()}, time.Second)
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("GET /healthz = %d, want 404 — Health is off by default", res.StatusCode)
		}
	})

	t.Run("a nil check is 200 whenever the process is up", func(t *testing.T) {
		res := get(t, server.Config{Schema: newStubSchema(), Health: "/healthz"}, time.Second)
		if res.StatusCode != http.StatusOK {
			t.Errorf("GET /healthz = %d, want 200", res.StatusCode)
		}
	})

	t.Run("a passing check is 200", func(t *testing.T) {
		res := get(t, server.Config{
			Schema:      newStubSchema(),
			Health:      "/healthz",
			HealthCheck: func(context.Context) error { return nil },
		}, time.Second)

		if res.StatusCode != http.StatusOK {
			t.Errorf("GET /healthz = %d, want 200", res.StatusCode)
		}
	})

	t.Run("a failing check is 503 and does not echo the error", func(t *testing.T) {
		res := get(t, server.Config{
			Schema:      newStubSchema(),
			Health:      "/healthz",
			HealthCheck: func(context.Context) error { return errors.New("dial tcp 10.0.0.7:5432: connect: refused") },
		}, time.Second)

		if res.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("GET /healthz = %d, want 503", res.StatusCode)
		}

		body, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		// Liveness paths are commonly reachable from further out than the API is. The check's
		// error names a host and a port; the response must not.
		if strings.Contains(string(body), "10.0.0.7") {
			t.Errorf("body = %q, want the check's error kept server-side", body)
		}
	})

	t.Run("a check that ignores its context is 503, not a hang", func(t *testing.T) {
		start := time.Now()
		res := get(t, server.Config{
			Schema: newStubSchema(),
			Health: "/healthz",
			// Deliberately does not select on ctx.Done(): cancelling a context stops nothing
			// unless someone is reading it, and half the checks people write are like this.
			HealthCheck: func(context.Context) error { time.Sleep(3 * time.Second); return nil },
		}, 10*time.Second)

		if res.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("GET /healthz = %d, want 503 at the deadline", res.StatusCode)
		}
		if elapsed := time.Since(start); elapsed > 2500*time.Millisecond {
			t.Errorf("GET /healthz took %v, want it answered at the 2s deadline", elapsed)
		}
	})
}

// assertBody @notice Runs req against app and asserts the status code and a substring of the body.
//
// @param t      the test handle; marked as a helper so failures report the caller's line
// @param app    the Fiber app under test
// @param req    the request to dispatch through app.Test
// @param status the expected HTTP status code
// @param want   a substring the response body must contain
func assertBody(t *testing.T, app *fiber.App, req *http.Request, status int, want string) {
	t.Helper()

	res, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != status {
		t.Errorf("%s %s = %d, want %d", req.Method, req.URL, res.StatusCode, status)
	}

	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()

	if body := string(b); !strings.Contains(body, want) {
		t.Errorf("%s %s body = %q, want it to contain %q", req.Method, req.URL, body, want)
	}
}
