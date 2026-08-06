// Package tests @notice Holds every test and runnable example in the module.
//
// @dev The tests live here rather than beside their packages, so each one exercises luima across
// a real module boundary — exactly as a consumer does. Nothing in this directory can reach an
// unexported symbol, which means a test passing here proves the exported surface is sufficient.
//
// The cost, stated plainly: the Example functions no longer render on pkg.go.dev under the
// symbols they demonstrate, because godoc binds an example to a symbol by directory. They still
// compile and their Output blocks still run, so they cannot rot — they are simply not published.
package tests

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/99designs/gqlgen/graphql"
	"github.com/vektah/gqlparser/v2"
	"github.com/vektah/gqlparser/v2/ast"
)

// stubSchema @notice A minimal graphql.ExecutableSchema that answers every query with
// {"ping":"pong"}.
//
// @dev Enough to route against, and no more: these tests assert wiring, not execution. A real
// generated schema would drag gqlgen codegen into the test module for no added coverage.
type stubSchema struct {
	schema *ast.Schema

	// exec @notice Overrides the fixed payload, so a test can read the resolver's context.
	//
	// @dev This is the only seam that sees what a real resolver sees. The context propagation,
	// timeout and cancellation tests all assert from in here, because that is the one place
	// where "the deadline reached the resolver" is distinguishable from "the deadline existed
	// somewhere in the middleware chain".
	exec func(context.Context) *graphql.Response
}

// Schema @notice Returns the parsed AST the handler validates queries against.
//
// @return *ast.Schema the single-field schema built by newStubSchema
func (s stubSchema) Schema() *ast.Schema { return s.schema }

// Complexity @notice Reports no custom complexity for any field.
//
// @dev The (0, false) pair tells gqlgen to fall back to its default cost of 1 per field, which
// is what the ComplexityLimit extension then measures against.
//
// @return int  the field's cost, unused here
// @return bool whether a custom cost was supplied — always false
func (s stubSchema) Complexity(context.Context, string, string, int, map[string]any) (int, bool) {
	return 0, false
}

// Exec @notice Returns a handler that answers with a fixed payload, or with s.exec when set.
//
// @dev The ctx passed to the returned handler is the one gqlgen built from the *http.Request, so
// it is exactly what a generated resolver receives.
//
// @return graphql.ResponseHandler a function yielding {"ping":"pong"} for every operation
func (s stubSchema) Exec(context.Context) graphql.ResponseHandler {
	return func(ctx context.Context) *graphql.Response {
		if s.exec != nil {
			return s.exec(ctx)
		}
		return &graphql.Response{Data: []byte(`{"ping":"pong"}`)}
	}
}

// newStubSchema @notice Builds the stub schema used by every server and root-package test.
//
// @dev MustLoadSchema panics on a malformed document. That is correct here: the input is a
// constant, so a panic means this file is broken, not the code under test.
//
// @return graphql.ExecutableSchema a schema with the single field `Query.ping: String`
func newStubSchema() graphql.ExecutableSchema {
	return newStubSchemaFunc(nil)
}

// newStubSchemaFunc @notice The same stub, answering through exec so a test can read the
// resolver's context.
//
// @param exec  the resolver body, or nil for the fixed {"ping":"pong"}
// @return graphql.ExecutableSchema a schema with the single field `Query.ping: String`
func newStubSchemaFunc(exec func(context.Context) *graphql.Response) graphql.ExecutableSchema {
	return stubSchema{
		schema: gqlparser.MustLoadSchema(&ast.Source{
			Name:  "test",
			Input: "type Query { ping: String }",
		}),
		exec: exec,
	}
}

// newDeepStubSchema @notice The same stub over a self-referential type, so a document can nest
// without bound.
//
// @dev Node.child: Node is the shape MaxDepth exists for — the cyclic schema SECURITY.md names,
// where 40 levels of nesting costs about 40 against a complexity limit of 1000 and multiplies into
// a resolver call per node per level. gqlgen validates every document against Schema(), so a
// 40-deep query needs a schema that admits one; the single-field stub cannot express the attack.
//
// @return graphql.ExecutableSchema a schema whose Node type contains itself
func newDeepStubSchema() graphql.ExecutableSchema {
	return stubSchema{
		schema: gqlparser.MustLoadSchema(&ast.Source{
			Name:  "deep",
			Input: "type Query { node: Node } type Node { name: String child: Node }",
		}),
	}
}

// postJSON @notice Builds a POST of an arbitrary GraphQL document.
//
// @dev postQuery is hardcoded to {ping}, which is every wiring test's whole payload. The depth
// tests each need a different document, and json.Marshal rather than string concatenation because
// those documents carry newlines.
//
// @param query          the GraphQL document
// @return *http.Request a request transport.POST will serve
func postJSON(query string) *http.Request {
	body, err := json.Marshal(map[string]string{"query": query})
	if err != nil {
		panic(err) // a constant in the caller, so this is a broken test file, not a failure
	}
	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// nest @notice Builds a document nested to depth levels through Node.child.
//
// @param depth      how many levels of child to emit
// @param leaf       the innermost selection
// @return string    the selection set, without the enclosing braces
func nest(depth int, leaf string) string {
	if depth <= 1 {
		return leaf
	}
	return "child { " + nest(depth-1, leaf) + " }"
}
