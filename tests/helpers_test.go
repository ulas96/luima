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

	"github.com/99designs/gqlgen/graphql"
	"github.com/vektah/gqlparser/v2"
	"github.com/vektah/gqlparser/v2/ast"
)

// stubSchema @notice A minimal graphql.ExecutableSchema that answers every query with
// {"ping":"pong"}.
//
// @dev Enough to route against, and no more: these tests assert wiring, not execution. A real
// generated schema would drag gqlgen codegen into the test module for no added coverage.
type stubSchema struct{ schema *ast.Schema }

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

// Exec @notice Returns a handler that answers with a fixed payload.
//
// @return graphql.ResponseHandler a function yielding {"ping":"pong"} for every operation
func (s stubSchema) Exec(context.Context) graphql.ResponseHandler {
	return func(context.Context) *graphql.Response {
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
	return stubSchema{gqlparser.MustLoadSchema(&ast.Source{
		Name:  "test",
		Input: "type Query { ping: String }",
	})}
}
