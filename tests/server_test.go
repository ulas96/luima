package tests

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
