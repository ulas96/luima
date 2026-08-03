package tests

import (
	"context"
	"errors"
	"fmt"

	"github.com/ulas96/luima/luimaerr"
)

// ExampleCustomError @notice Resolvers opt in to being heard.
//
// @dev A bare error is infrastructure detail as far as PresentError is concerned; only a
// *CustomError carries a message the client is allowed to see.
func ExampleCustomError() {
	ctx := context.Background()

	redacted := errors.New(`ERROR: duplicate key value violates unique constraint "app_users_pkey"`)
	heard := &luimaerr.CustomError{
		UserMessage:   "user E-1042 already exists",
		InternalError: redacted,
	}

	fmt.Println(luimaerr.PresentError(ctx, redacted).Message)
	fmt.Println(luimaerr.PresentError(ctx, heard).Message)

	// The cause stays reachable, so classification still works on a wrapped error.
	fmt.Println(errors.Is(heard, redacted))

	// Output:
	// internal server error
	// user E-1042 already exists
	// true
}

// ExampleSQLState @notice Branching on a Postgres SQLSTATE.
//
// @dev Replaces an errors.As dance whose target type is easy to get wrong: pg.Error is an
// interface, not a struct pointer, and not pgx's *pgconn.PgError.
func ExampleSQLState() {
	err := errors.New("not a driver error")

	switch luimaerr.SQLState(err) {
	case "23505":
		fmt.Println("already exists")
	case "23503":
		fmt.Println("referenced row does not exist")
	case "":
		fmt.Println("not a driver error")
	}

	// Output: not a driver error
}
