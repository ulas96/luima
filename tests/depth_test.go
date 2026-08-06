package tests

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"

	"github.com/ulas96/luima/server"
)

// askDeep @notice POSTs a document to a fresh app mounted at the given depth limit.
//
// @param t        the test handle
// @param maxDepth the Config.MaxDepth to mount with
// @param query    the GraphQL document
// @return int     the HTTP status
// @return string  the response body
func askDeep(t *testing.T, maxDepth int, query string) (int, string) {
	t.Helper()

	app := fiber.New()
	server.Mount(app, server.Config{
		DisablePlayground: true,
		MaxDepth:          maxDepth,
		Schema:            newDeepStubSchema(),
	})

	res, err := app.Test(postJSON(query))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, string(b)
}

// assertRejected @notice Asserts a document was refused by the depth limiter specifically.
//
// @dev The code, not the status: a depth rejection is HTTP 200 with an errors array, matching how
// gqlgen's own ComplexityLimit behaves. Asserting on the status alone would pass for a 422 the
// validator produced for an unrelated reason, which is how a depth test rots into a test of
// gqlparser.
//
// @param t     the test handle
// @param body  the response body
func assertRejected(t *testing.T, body string) {
	t.Helper()

	if !strings.Contains(body, "DEPTH_LIMIT_EXCEEDED") {
		t.Errorf("response %s carries no DEPTH_LIMIT_EXCEEDED — the document was executed", body)
	}
}

// assertServed @notice Asserts a document reached the resolver.
//
// @param t     the test handle
// @param body  the response body
func assertServed(t *testing.T, body string) {
	t.Helper()

	if strings.Contains(body, "DEPTH_LIMIT_EXCEEDED") {
		t.Errorf("response %s was rejected — a legitimate document hit the depth limit", body)
	}
	if strings.Contains(body, `"errors"`) {
		t.Errorf("response %s carries errors", body)
	}
}

// TestMaxDepth @notice Pins Config.MaxDepth against the four documents that distinguish a working
// depth limiter from one that looks like it works.
//
// @dev Every case below was measured before it was written; the two fragment ones are why the
// walker resolves doc.Fragments rather than walking Doc.Operations alone. A spread node carries no
// SelectionSet of its own, so a naive walker reads every named fragment as a leaf and a 40-deep
// document passes at depth 1 — a two-line change to the attacking query.
//
// @param t the test handle
func TestMaxDepth(t *testing.T) {
	t.Run("a shallow query passes the default", func(t *testing.T) {
		_, body := askDeep(t, 0, "{ node { name } }")
		assertServed(t, body)
	})

	t.Run("plain nesting is rejected", func(t *testing.T) {
		_, body := askDeep(t, 0, "{ node { "+nest(40, "name")+" } }")
		assertRejected(t, body)
	})

	// The one a naive walker gets wrong. Identical depth to the case above, expressed through a
	// named fragment.
	t.Run("fragment spread", func(t *testing.T) {
		_, body := askDeep(t, 0, "{ node { ...F } } fragment F on Node { "+nest(40, "name")+" }")
		assertRejected(t, body)
	})

	// And the same again through two hops, because a walker that resolves one level of spread but
	// not the spreads inside it is a real way to half-fix this.
	t.Run("chained fragment spreads", func(t *testing.T) {
		_, body := askDeep(t, 0,
			"{ node { ...A } } "+
				"fragment A on Node { child { ...B } } "+
				"fragment B on Node { "+nest(40, "name")+" }")
		assertRejected(t, body)
	})

	// An inline fragment is a type condition, not a level. Counting it makes an interface-heavy
	// schema unusable: every ... on Foo would spend a level the query does not actually nest.
	t.Run("an inline fragment is not a level", func(t *testing.T) {
		_, body := askDeep(t, 2, "{ node { ... on Node { name } } }")
		assertServed(t, body)
	})

	// gqlparser's NoFragmentCycles rejects this before any extension runs, so the walker's cycle
	// guard never fires here. What this pins is that it terminates at all — an unguarded walk on a
	// cycle exhausts the goroutine stack, which recover() cannot catch, so the failure mode is a
	// dead process rather than a red test.
	t.Run("a cyclic fragment terminates", func(t *testing.T) {
		status, body := askDeep(t, 0,
			"{ node { ...A } } "+
				"fragment A on Node { child { ...B } } "+
				"fragment B on Node { child { ...A } }")
		if status == http.StatusOK && !strings.Contains(body, `"errors"`) {
			t.Errorf("cyclic document executed: %s", body)
		}
	})

	t.Run("negative disables it", func(t *testing.T) {
		_, body := askDeep(t, -1, "{ node { "+nest(40, "name")+" } }")
		assertServed(t, body)
	})

	t.Run("a limit above the document admits it", func(t *testing.T) {
		_, body := askDeep(t, 41, "{ node { "+nest(40, "name")+" } }")
		assertServed(t, body)
	})
}

// TestMaxDepthAdmitsIntrospection @notice Pins that the default limit does not reject the
// playground's own introspection query.
//
// @dev The risk this feature actually carries. A zero-valued Config has to be the good
// configuration — that is the rule QueryCache and ComplexityLimit are written to, and a default
// MaxDepth that rejects introspection would break the playground on a fresh mount with no error
// anyone could act on. The standard introspection document nests ofType seven deep inside
// types.fields.type, which is the deepest thing a default install serves.
//
// Measured: this document first passes at MaxDepth 13, so the default of 15 clears it by two. That
// margin is the reason the default is not lower.
//
// @param t the test handle
func TestMaxDepthAdmitsIntrospection(t *testing.T) {
	app := fiber.New()
	server.Mount(app, server.Config{DisablePlayground: true, Schema: newStubSchema()})

	res, err := app.Test(postJSON(introspectionQuery))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "DEPTH_LIMIT_EXCEEDED") {
		t.Errorf("the default MaxDepth rejects introspection, so the playground is blind: %s", b)
	}
}

// introspectionQuery @notice The document graphiql sends on load, trimmed to its deepest branch.
//
// @dev Kept verbatim rather than generated: the point is the real nesting depth of the real query,
// and a paraphrase that happens to be shallower would pass while the playground failed.
const introspectionQuery = `
query IntrospectionQuery {
  __schema {
    queryType { name }
    types { ...FullType }
    directives { name locations args { ...InputValue } }
  }
}
fragment FullType on __Type {
  kind
  name
  fields(includeDeprecated: true) {
    name
    args { ...InputValue }
    type { ...TypeRef }
  }
  inputFields { ...InputValue }
  interfaces { ...TypeRef }
  enumValues(includeDeprecated: true) { name }
  possibleTypes { ...TypeRef }
}
fragment InputValue on __InputValue { name type { ...TypeRef } defaultValue }
fragment TypeRef on __Type {
  kind
  name
  ofType {
    kind
    name
    ofType {
      kind
      name
      ofType {
        kind
        name
        ofType { kind name ofType { kind name ofType { kind name ofType { kind name } } } }
      }
    }
  }
}`
