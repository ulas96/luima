package graph

import (
	"github.com/go-pg/pg/v10"

	"github.com/ulas96/luima/examples/quickstart/graph/model"
)

// Resolver @notice gqlgen's dependency-injection root.
//
// @dev Whatever a resolver needs goes here — the pool, and in a real application a logger and
// your auth context. gqlgen constructs it once and hands it to every resolver.
type Resolver struct{ DB *pg.DB }

// newUser @notice Builds a model.User from the GraphQL input and the primary key.
//
// @dev It lives here, not in schema.resolvers.go: codegen sweeps non-resolver declarations out
// of that file into a /* !!! WARNING !!! */ block.
//
// @param personalID   the primary key, passed as its own GraphQL argument
// @param in           the mutation's input object
// @return *model.User a complete model ready for Create or Update — both write every column
func newUser(personalID string, in model.UserInput) *model.User {
	return &model.User{
		PersonalID: personalID,
		Name:       in.Name,
		Company:    in.Company,
		Projects:   in.Projects,
	}
}
