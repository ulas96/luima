package server

import (
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// CORSConfig @notice Which browser origins may read the GraphQL response.
type CORSConfig struct {
	// Origins @notice Exact origins, e.g. "https://app.example.com". A single "*" allows any.
	//
	// @dev Matched exactly and echoed back one at a time, because the header takes one origin or
	// the literal "*" and nothing else — a comma-separated list is not a value browsers parse.
	Origins []string

	// Headers @notice Request headers to allow, added to Content-Type and Authorization.
	Headers []string

	// MaxAge @notice How long a browser may cache the preflight. Default 10m. Negative sends 0,
	// which tells the browser not to cache it at all.
	MaxAge time.Duration
}

// CORS @notice Cross-origin access as portable net/http middleware, for Config.HTTPMiddleware.
//
// @dev Sets Vary: Origin on every response it touches, and that line is the whole reason to
// prefer this over four hand-written ones. The allowed origin is echoed from the request, so the
// response varies by a request header; without Vary, any shared cache — a CDN, a corporate proxy,
// the browser's own store — serves the first caller's Access-Control-Allow-Origin to the second,
// and the symptom is a CORS failure that reproduces for one user and no one else.
//
// Methods are fixed at GET, POST and OPTIONS: those are the only ones luima's routes answer, and a
// consumer serving others is not serving them from here.
//
// There is deliberately no Credentials knob. Access-Control-Allow-Credentials with a wildcard
// origin is the classic misconfiguration, and the case it serves — cookie-authenticated
// cross-origin requests — needs an auth layer luima does not ship. Leaving the field out makes the
// combination unrepresentable, which is stronger than validating it. Use rs/cors in
// HTTPMiddleware if you need it; that needs no Fiber import either.
//
// One wart, documented rather than fixed. HTTPMiddleware runs inside the adaptor and Fiber's
// timeout middleware wraps outside it, so a request that hits RequestTimeout is answered by that
// middleware and its 408 carries no CORS headers — the browser then reports a CORS error rather
// than a timeout. Wrapping the timeout path to fix it would move CORS outside the adaptor and cost
// the portability that is the point of this being net/http middleware.
//
// @param c the origins and headers to allow
// @return func(http.Handler) http.Handler middleware, outermost-first in HTTPMiddleware
func CORS(c CORSConfig) func(http.Handler) http.Handler {
	allowed := make(map[string]bool, len(c.Origins))
	wildcard := false
	for _, o := range c.Origins {
		if o == "*" {
			wildcard = true
		}
		allowed[o] = true
	}

	// Content-Type is what makes a POST of application/json non-simple and therefore preflighted
	// at all, so omitting it would fail every request this exists to allow. Authorization is the
	// one every consumer adds next.
	headers := strings.Join(append([]string{"Content-Type", "Authorization"}, c.Headers...), ", ")

	maxAge := c.MaxAge
	if maxAge == 0 {
		maxAge = 10 * time.Minute
	}
	age := "0"
	if maxAge > 0 {
		age = strconv.Itoa(int(maxAge.Seconds()))
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Before the origin check, not after: a response with no Access-Control-Allow-Origin
			// is still a response that depends on Origin, and caching *that* under a key that
			// ignores Origin is how an allowed caller gets served a refusal.
			w.Header().Add("Vary", "Origin")

			switch origin := r.Header.Get("Origin"); {
			case origin == "":
				// Not a cross-origin browser request. Nothing to allow.
			case wildcard:
				allowOrigin(w, "*", headers, age)
			case allowed[origin]:
				allowOrigin(w, origin, headers, age)
			}

			next.ServeHTTP(w, r)
		})
	}
}

// allowOrigin @notice Writes the four headers that make one origin's request readable.
//
// @dev Written before next.ServeHTTP so they survive: gqlgen's transports call WriteHeader, and
// a header set after that is dropped. The preflight itself is answered by transport.Options, not
// here — this only supplies what that 200 is missing.
//
// @param w       the response
// @param origin  the value for Access-Control-Allow-Origin: one origin, or "*"
// @param headers the joined Access-Control-Allow-Headers value
// @param age     the Access-Control-Max-Age value, in seconds
func allowOrigin(w http.ResponseWriter, origin, headers, age string) {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", origin)
	h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	h.Set("Access-Control-Allow-Headers", headers)
	h.Set("Access-Control-Max-Age", age)
}

// RateLimit @notice Fixed-window limiter as portable net/http middleware, for
// Config.HTTPMiddleware. Over the limit is 429 with Retry-After.
//
// @dev Fixed window, not sliding, and the difference is a real ceiling rather than a rounding
// error: a caller who spends n at the end of one window and n at the start of the next lands 2n
// requests inside one window's width. That is the price of holding one integer per key instead of
// a timestamp ring, and for the thing this exists to stop — an unbounded { users { id } } in a
// loop — a factor of two does not change the answer. Reach for a token bucket when it does.
//
// The counters are dropped wholesale at each window rollover, and that sweep is the memory bound:
// the map holds one entry per distinct key seen in the current window and can never accumulate
// across windows. A per-key map with no eviction is an unbounded allocation driven by an
// unauthenticated header — a memory exhaustion bug inside the feature that exists to prevent one.
// There is no goroutine; the rollover is checked on the request path.
//
// This is per process. Two replicas behind a load balancer enforce 2n; a shared store is the
// upgrade, and it belongs in the consumer's middleware rather than here.
//
// luima already documents the hole this plugs: crud.List notes that an unbounded list field is
// reachable by anyone who can send { users { id } }, and that ComplexityLimit cannot see it
// because row count is not an input to the complexity calculation.
//
// @param n   requests allowed per window
// @param per the window
// @param key what to bucket on; nil means r.RemoteAddr. Read a header here when you are behind a
// proxy — RemoteAddr is then the proxy's address, one bucket for every caller, and a limiter that
// limits nothing. Trust that header only from a proxy you control, or a caller sets it.
// @return func(http.Handler) http.Handler middleware, outermost-first in HTTPMiddleware
func RateLimit(n int, per time.Duration, key func(*http.Request) string) func(http.Handler) http.Handler {
	var (
		mu        sync.Mutex
		hits      = map[string]int{}
		windowEnd time.Time
	)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			k := r.RemoteAddr
			if key != nil {
				k = key(r)
			}

			mu.Lock()
			now := time.Now()
			if now.After(windowEnd) {
				hits = map[string]int{} // the sweep: see above
				windowEnd = now.Add(per)
			}
			hits[k]++
			over, retry := hits[k] > n, windowEnd.Sub(now)
			mu.Unlock()

			if over {
				// +1 so the value is never 0, which a client reads as "retry immediately" and
				// spends its next request arriving inside the same window.
				w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
				http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
