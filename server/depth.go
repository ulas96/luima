package server

import (
	"context"

	"github.com/99designs/gqlgen/graphql"
	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/gqlerror"
)

// maxDepth @notice The gqlgen extension behind Config.MaxDepth.
//
// @dev An OperationContextMutator rather than an AroundOperations interceptor, and that choice
// removes a hazard rather than expressing a preference. gqlgen's own source warns that an
// interceptor short-circuiting without graphql.OneShot makes streaming transports loop forever —
// and after 0.3.0 this is a library where a streaming transport can actually be registered. A
// mutator cannot make that mistake: its error is returned from CreateOperationContext, and every
// transport turns that into exactly one response through DispatchError.
//
// It is also the shape of its sibling. extension.FixedComplexityLimit is a mutator, so both limits
// reject the same way — HTTP 200 with a code in extensions — and being consistent with the limit
// next to it beats being more correct than it. srv.Use composes, so a consumer can still add
// their own rules; SetValidationRulesFn, the other candidate, replaces the rule set and would take
// a seat that is not luima's to take.
type maxDepth struct {
	limit int
}

// ExtensionName @notice Names the extension in gqlgen's stats and logs.
//
// @return string the extension name
func (maxDepth) ExtensionName() string { return "LuimaMaxDepth" }

// Validate @notice Reports nothing to check against the schema.
//
// @dev The limit is a property of the document, not of the schema, so there is nothing here that
// could be wrong at mount time.
//
// @return error always nil
func (maxDepth) Validate(graphql.ExecutableSchema) error { return nil }

// MutateOperationContext @notice Rejects an operation nested deeper than the limit.
//
// @dev Runs after parse and validation, so opCtx.Doc is a document gqlparser has already blessed
// and opCtx.Operation is never nil.
//
// @param ctx    the request context
// @param opCtx  the parsed operation
// @return *gqlerror.Error the rejection, or nil to let the operation run
func (m maxDepth) MutateOperationContext(_ context.Context, opCtx *graphql.OperationContext) *gqlerror.Error {
	d := selectionDepth(opCtx.Operation.SelectionSet, opCtx.Doc.Fragments, map[string]bool{})
	if d <= m.limit {
		return nil
	}

	err := gqlerror.Errorf("operation has depth %d, which exceeds the limit of %d", d, m.limit)
	// The same shape gqlgen puts on its own limit rejections, so a client already branching on
	// extensions.code needs nothing new to handle this one.
	err.Extensions = map[string]any{"code": "DEPTH_LIMIT_EXCEEDED"}
	return err
}

// selectionDepth @notice Reports the deepest branch of a selection set.
//
// @dev The three cases are not symmetric, and each asymmetry is load-bearing:
//
//   - A field is the only thing that spends a level.
//   - An inline fragment is a type condition. `... on User { name }` nests nothing, and counting it
//     makes an interface-heavy schema unusable — every polymorphic selection would pay a level the
//     document does not actually have.
//   - A spread carries no SelectionSet at all, so it has to be resolved out of frags. Skip this
//     case and every named fragment reads as a leaf: a 40-deep document behind `...F` measures 1
//     and passes. That is a two-line change to the attacking query, which makes it the whole
//     reason this walker is not four lines long.
//
// @param set      the selections to walk
// @param frags    the document's fragment definitions
// @param visiting the spreads currently on the stack
// @return int     the depth of the deepest branch
func selectionDepth(set ast.SelectionSet, frags ast.FragmentDefinitionList, visiting map[string]bool) int {
	depth := 0
	for _, sel := range set {
		var d int
		switch s := sel.(type) {
		case *ast.Field:
			d = 1 + selectionDepth(s.SelectionSet, frags, visiting)
		case *ast.InlineFragment:
			d = selectionDepth(s.SelectionSet, frags, visiting)
		case *ast.FragmentSpread:
			// Belt and braces, not the primary defence: gqlparser's NoFragmentCycles rejects a
			// cyclic document at 422 before any extension runs, so in production this never
			// fires. It stays because an unguarded walk on a cycle exhausts the goroutine
			// stack, and that is the one failure recover() cannot catch — a dead process
			// rather than a red test.
			if visiting[s.Name] {
				continue
			}
			def := frags.ForName(s.Name)
			if def == nil {
				continue // an undefined spread; the validator has its own complaint about it
			}
			visiting[s.Name] = true
			d = selectionDepth(def.SelectionSet, frags, visiting)
			delete(visiting, s.Name)
		}
		if d > depth {
			depth = d
		}
	}
	return depth
}

// noIntrospection @notice The gqlgen extension behind Config.DisableIntrospection.
//
// @dev gqlgen's own gate is inside field resolution: the generated introspectSchema and
// introspectType return a plain errors.New("introspection disabled"), which gqlgen wraps like a
// resolver's error, so PresentError redacts it to INTERNAL_SERVER_ERROR and logs it — and any
// unauthenticated scanner then drives your error-rate alerts with one query. Rejecting the
// operation here, the way maxDepth does, answers before any field runs, through DispatchError, with
// a cause-less error PresentError passes through: HTTP 200, INTROSPECTION_DISABLED, no log line.
//
// gqlgen's gate stays behind it. opCtx.DisableIntrospection is still true, because Mount does not
// register extension.Introspection, so a document this walk misses is still refused — redacted.
type noIntrospection struct{}

// ExtensionName @notice Names the extension in gqlgen's stats and logs.
//
// @return string the extension name
func (noIntrospection) ExtensionName() string { return "LuimaNoIntrospection" }

// Validate @notice Reports nothing to check against the schema.
//
// @return error always nil
func (noIntrospection) Validate(graphql.ExecutableSchema) error { return nil }

// MutateOperationContext @notice Rejects an operation that selects __schema or __type.
//
// @param ctx    the request context
// @param opCtx  the parsed operation
// @return *gqlerror.Error the rejection, or nil to let the operation run
func (noIntrospection) MutateOperationContext(_ context.Context, opCtx *graphql.OperationContext) *gqlerror.Error {
	if !introspects(opCtx.Operation.SelectionSet, opCtx.Doc.Fragments, map[string]bool{}) {
		return nil
	}
	err := gqlerror.Errorf("introspection is disabled")
	err.Extensions = map[string]any{"code": "INTROSPECTION_DISABLED"}
	return err
}

// introspects @notice Reports whether a selection set selects __schema or __type anywhere.
//
// @dev Anywhere, not only at the root: gqlparser adds both to the query type's own field list
// (validator/schema.go), so a field that returns the query type reaches them nested. A spread is
// resolved through frags, as in selectionDepth, or `{...F}` hides the whole query. __typename is
// not introspection and is left alone, as gqlgen's own gate leaves it.
//
// seen is marked on entry and never cleared. That is the cycle guard, and it is also a memo: a
// fragment is walked once, because the walk stops at the first match, so a fragment seen before
// either is being walked or matched nothing. Clearing it, as selectionDepth does, lets a chain of
// fragments that each spread the next twice cost 2^n.
//
// @param set    the selections to walk
// @param frags  the document's fragment definitions
// @param seen   the spreads already walked
// @return bool  true when an introspection field is selected
func introspects(set ast.SelectionSet, frags ast.FragmentDefinitionList, seen map[string]bool) bool {
	for _, sel := range set {
		switch s := sel.(type) {
		case *ast.Field:
			if s.Name == "__schema" || s.Name == "__type" || introspects(s.SelectionSet, frags, seen) {
				return true
			}
		case *ast.InlineFragment:
			if introspects(s.SelectionSet, frags, seen) {
				return true
			}
		case *ast.FragmentSpread:
			if seen[s.Name] {
				continue
			}
			seen[s.Name] = true
			if def := frags.ForName(s.Name); def != nil && introspects(def.SelectionSet, frags, seen) {
				return true
			}
		}
	}
	return false
}
