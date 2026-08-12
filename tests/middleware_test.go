package tests

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/ulas96/luima/server"
)

// corsApp @notice Builds a server with one CORS middleware in HTTPMiddleware.
//
// @dev Through the real stack rather than against a bare handler, because that is where the risk
// is: HTTPMiddleware runs inside the adaptor, so every header it sets has to survive fasthttp's
// header conversion on the way back out. A test against httptest.NewRecorder would pass whether or
// not it does.
//
// @param origins the allowed origins
// @return *fiber.App an app answering /graphql
func corsApp(origins ...string) *fiber.App {
	return server.New(server.Config{
		Schema: newStubSchema(),
		HTTPMiddleware: []func(http.Handler) http.Handler{
			server.CORS(server.CORSConfig{Origins: origins}),
		},
	})
}

// TestCORS @notice Pins the two headers a cross-origin GraphQL response needs, and Vary.
//
// @dev The Vary assertions are the ones that catch a rewrite. The allowed origin is echoed from
// the request, so the response varies by a request header; without Vary: Origin any shared cache —
// a CDN, a corporate proxy, the browser's own store — serves the first caller's
// Access-Control-Allow-Origin to the second, and the symptom is a CORS failure that reproduces for
// one user and no one else. It is set on the refusal too, for the same reason in reverse: a cached
// refusal served to an allowed origin is the same bug.
//
// @param t the test handle
func TestCORS(t *testing.T) {
	const allowed = "https://app.example.com"

	t.Run("an allowed origin is echoed back", func(t *testing.T) {
		req := postJSON(`{ping}`)
		req.Header.Set("Origin", allowed)

		res, err := corsApp(allowed).Test(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()

		if got := res.Header.Get("Access-Control-Allow-Origin"); got != allowed {
			t.Errorf("Access-Control-Allow-Origin = %q, want %q", got, allowed)
		}
		if got := res.Header.Get("Vary"); got != "Origin" {
			t.Errorf("Vary = %q, want Origin — without it a shared cache serves one origin's grant to another", got)
		}
	})

	t.Run("an unlisted origin gets no grant, but still varies", func(t *testing.T) {
		req := postJSON(`{ping}`)
		req.Header.Set("Origin", "https://evil.example.com")

		res, err := corsApp(allowed).Test(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()

		if got := res.Header.Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("Access-Control-Allow-Origin = %q, want empty for an unlisted origin", got)
		}
		if got := res.Header.Get("Vary"); got != "Origin" {
			t.Errorf("Vary = %q, want Origin — a cached refusal served to an allowed origin is the same bug", got)
		}
	})

	t.Run("no middleware means no header", func(t *testing.T) {
		req := postJSON(`{ping}`)
		req.Header.Set("Origin", allowed)

		res, err := server.New(server.Config{Schema: newStubSchema()}).Test(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()

		if got := res.Header.Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("Access-Control-Allow-Origin = %q, want empty — luima sets no CORS header of its own", got)
		}
		if got := res.Header.Get("Vary"); got != "" {
			t.Errorf("Vary = %q, want empty", got)
		}
	})

	t.Run("a single star allows any origin", func(t *testing.T) {
		req := postJSON(`{ping}`)
		req.Header.Set("Origin", "https://anything.example.com")

		res, err := corsApp("*").Test(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()

		// The literal star, not the echoed origin: it is what makes the response cacheable, and
		// it is safe only because there is no Credentials knob to combine it with.
		if got := res.Header.Get("Access-Control-Allow-Origin"); got != "*" {
			t.Errorf("Access-Control-Allow-Origin = %q, want *", got)
		}
	})

	// The whole point of the feature. transport.Options already answers the preflight with Allow
	// and a 200; the browser refuses the response anyway until these three arrive with it.
	t.Run("the preflight carries the grant alongside gqlgen's Allow", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodOptions, "/graphql", nil)
		req.Header.Set("Origin", allowed)
		req.Header.Set("Access-Control-Request-Method", http.MethodPost)

		res, err := corsApp(allowed).Test(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()

		if res.StatusCode != http.StatusOK {
			t.Fatalf("OPTIONS /graphql = %d, want 200", res.StatusCode)
		}
		for header, want := range map[string]string{
			"Access-Control-Allow-Origin":  allowed,
			"Access-Control-Allow-Methods": "GET, POST, OPTIONS",
			"Access-Control-Max-Age":       "600",
		} {
			if got := res.Header.Get(header); got != want {
				t.Errorf("%s = %q, want %q", header, got, want)
			}
		}
		// Content-Type is what makes an application/json POST preflighted in the first place, so
		// omitting it would fail every request this exists to allow.
		if got := res.Header.Get("Access-Control-Allow-Headers"); got != "Content-Type, Authorization" {
			t.Errorf("Access-Control-Allow-Headers = %q, want Content-Type, Authorization", got)
		}
		if res.Header.Get("Allow") == "" {
			t.Error("no Allow header — CORS must not stop the preflight reaching gqlgen's Options transport")
		}
	})
}

// TestRateLimit @notice Pins the 429, its Retry-After, and the window rollover.
//
// @dev The rollover case is the one shaped by the implementation rather than the contract: the
// counters are dropped wholesale at each window boundary, because a per-key map with no eviction
// is an unbounded allocation driven by an unauthenticated header — a memory exhaustion bug inside
// the feature that exists to prevent one. That the map does not grow is not observable from
// outside the package; that the key is served again in the next window is, and it is the same
// line of code.
//
// @param t the test handle
func TestRateLimit(t *testing.T) {
	// Against a recorder rather than through the app for everything but the last case: the window
	// has to be short enough to roll over inside a test, and a 50ms RequestTimeout race is not
	// what is under test here.
	send := func(mw func(http.Handler) http.Handler, remote string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/graphql", nil)
		req.RemoteAddr = remote

		rec := httptest.NewRecorder()
		mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})).ServeHTTP(rec, req)
		return rec
	}

	t.Run("the n+1st request in a window is 429 with Retry-After", func(t *testing.T) {
		mw := server.RateLimit(2, time.Minute, nil)

		for i := range 2 {
			if code := send(mw, "10.0.0.1:1111").Code; code != http.StatusOK {
				t.Fatalf("request %d = %d, want 200 — it is inside the limit", i+1, code)
			}
		}

		rec := send(mw, "10.0.0.1:1111")
		if rec.Code != http.StatusTooManyRequests {
			t.Errorf("request 3 = %d, want 429", rec.Code)
		}
		// Never 0: a client reads that as "retry immediately" and spends its next request
		// arriving inside the same window.
		if got := rec.Header().Get("Retry-After"); got == "" || got == "0" {
			t.Errorf("Retry-After = %q, want a positive number of seconds", got)
		}
	})

	t.Run("buckets are per key", func(t *testing.T) {
		mw := server.RateLimit(1, time.Minute, nil)

		if code := send(mw, "10.0.0.1:1111").Code; code != http.StatusOK {
			t.Fatalf("first caller = %d, want 200", code)
		}
		if code := send(mw, "10.0.0.2:2222").Code; code != http.StatusOK {
			t.Errorf("second caller = %d, want 200 — one caller must not spend another's budget", code)
		}
	})

	t.Run("a custom key overrides RemoteAddr", func(t *testing.T) {
		// The case behind the doc comment: behind a proxy, RemoteAddr is the proxy's address for
		// every caller, one bucket for all of them, and a limiter that limits nothing.
		mw := server.RateLimit(1, time.Minute, func(r *http.Request) string {
			return r.Header.Get("X-Tenant")
		})

		one := httptest.NewRequest(http.MethodGet, "/graphql", nil)
		one.Header.Set("X-Tenant", "acme")
		one.RemoteAddr = "10.0.0.1:1111"

		two := httptest.NewRequest(http.MethodGet, "/graphql", nil)
		two.Header.Set("X-Tenant", "acme")
		two.RemoteAddr = "10.0.0.2:2222" // a different address, the same bucket

		ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

		first, second := httptest.NewRecorder(), httptest.NewRecorder()
		mw(ok).ServeHTTP(first, one)
		mw(ok).ServeHTTP(second, two)

		if first.Code != http.StatusOK {
			t.Fatalf("first = %d, want 200", first.Code)
		}
		if second.Code != http.StatusTooManyRequests {
			t.Errorf("second = %d, want 429 — the key, not the address, is the bucket", second.Code)
		}
	})

	t.Run("the window rolls over and the key is served again", func(t *testing.T) {
		mw := server.RateLimit(1, 50*time.Millisecond, nil)

		if code := send(mw, "10.0.0.1:1111").Code; code != http.StatusOK {
			t.Fatalf("first = %d, want 200", code)
		}
		if code := send(mw, "10.0.0.1:1111").Code; code != http.StatusTooManyRequests {
			t.Fatalf("second = %d, want 429 inside the window", code)
		}

		time.Sleep(60 * time.Millisecond)

		if code := send(mw, "10.0.0.1:1111").Code; code != http.StatusOK {
			t.Errorf("after the window = %d, want 200 — the counters are dropped at the rollover", code)
		}
	})

	// One case through the real stack, because a 429 written by net/http middleware inside the
	// adaptor still has to come back out of fasthttp as a 429 with its header intact.
	t.Run("the 429 survives the adaptor", func(t *testing.T) {
		app := server.New(server.Config{
			Schema:         newStubSchema(),
			HTTPMiddleware: []func(http.Handler) http.Handler{server.RateLimit(1, time.Minute, nil)},
		})

		first, err := app.Test(postJSON(`{ping}`))
		if err != nil {
			t.Fatal(err)
		}
		first.Body.Close()
		if first.StatusCode != http.StatusOK {
			t.Fatalf("first = %d, want 200", first.StatusCode)
		}

		second, err := app.Test(postJSON(`{ping}`))
		if err != nil {
			t.Fatal(err)
		}
		defer second.Body.Close()

		if second.StatusCode != http.StatusTooManyRequests {
			t.Errorf("second = %d, want 429", second.StatusCode)
		}
		if second.Header.Get("Retry-After") == "" {
			t.Error("no Retry-After — the header did not survive the adaptor")
		}
	})
}
