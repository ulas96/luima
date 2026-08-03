package tests

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/vektah/gqlparser/v2/gqlerror"

	"github.com/ulas96/luima/luimaerr"
)

// TestPresentError @notice Pins the three branches of the error contract.
//
// @dev Dropping the second branch is what makes every schema typo read as "internal server
// error"; dropping the third is what leaks the table's columns to an unauthenticated caller.
//
// @param t the test handle
func TestPresentError(t *testing.T) {
	ctx := context.Background()
	leak := errors.New(`ERROR: duplicate key value violates unique constraint "app_users_pkey" (SQLSTATE 23505)`)

	if got := luimaerr.PresentError(ctx, &luimaerr.CustomError{UserMessage: "nope", InternalError: leak}).Message; got != "nope" {
		t.Errorf("CustomError presented as %q", got)
	}
	if got := luimaerr.PresentError(ctx, gqlerror.Errorf("Cannot query field %q", "x")).Message; !strings.HasPrefix(got, "Cannot query field") {
		t.Errorf("validation error presented as %q — client/schema drift must stay debuggable", got)
	}
	if got := luimaerr.PresentError(ctx, leak).Message; got != "internal server error" {
		t.Errorf("raw driver error presented as %q — it must not reach the client", got)
	}
}

// TestCustomErrorUnwrap @notice Asserts the cause stays reachable through errors.Is/As.
//
// @dev This is what makes SQLState work on a CustomError, and it is the whole reason
// InternalError is typed error rather than string as in gqlgen's own reference server.
//
// @param t the test handle
func TestCustomErrorUnwrap(t *testing.T) {
	cause := errors.New("boom")
	ce := &luimaerr.CustomError{UserMessage: "user X already exists", InternalError: cause}

	if !errors.Is(ce, cause) {
		t.Error("errors.Is could not reach InternalError")
	}
	if got := ce.Error(); got != "user X already exists: boom" {
		t.Errorf("Error() = %q", got)
	}
	if got := (&luimaerr.CustomError{UserMessage: "alone"}).Error(); got != "alone" {
		t.Errorf("Error() with no cause = %q, want %q", got, "alone")
	}
}

// TestSQLState @notice Covers the non-driver case.
//
// @dev The positive case needs a real Postgres error and is asserted end to end by TestCRUD, via
// the duplicate Create.
//
// @param t the test handle
func TestSQLState(t *testing.T) {
	if got := luimaerr.SQLState(errors.New("not a driver error")); got != "" {
		t.Errorf("SQLState(non-driver) = %q, want %q", got, "")
	}
	if got := luimaerr.SQLState(nil); got != "" {
		t.Errorf("SQLState(nil) = %q, want %q", got, "")
	}
}
