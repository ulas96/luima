// Package luimaerr @notice luima's error contract: the one place that decides what a resolver
// error is allowed to tell a client.
//
// @dev It is named luimaerr rather than errors because a package called errors shadows the
// standard library in every file that imports both — and PresentError itself calls errors.As
// twice.
//
// It imports nothing else in luima, so a package that must not pull in Fiber or gqlgen's handler
// can still return a *CustomError.
package luimaerr

import (
	"context"
	"errors"
	"log"

	"github.com/99designs/gqlgen/graphql"
	"github.com/go-pg/pg/v10"
	"github.com/vektah/gqlparser/v2/gqlerror"
)

// CustomError @notice Carries a message the client is allowed to see.
//
// @dev Any resolver error that is not a *CustomError is infrastructure detail as far as
// PresentError is concerned, and is redacted.
type CustomError struct {
	// UserMessage @notice The text the client receives verbatim. Assume it is public.
	UserMessage string

	// InternalError @notice The cause, kept for the log and for errors.Is/As.
	//
	// @dev gqlgen's own reference server stores a string here; keeping the error means the
	// underlying pg.Error stays reachable, which is what makes SQLState work on a wrapped
	// error.
	InternalError error
}

// Error @notice Renders the message and, when there is one, the cause.
//
// @dev The cause is included because this string goes to the log, never to the client —
// PresentError reads UserMessage directly.
//
// @return string "user X already exists: <cause>", or just the message when there is no cause
func (e *CustomError) Error() string {
	if e.InternalError == nil {
		return e.UserMessage
	}
	return e.UserMessage + ": " + e.InternalError.Error()
}

// Unwrap @notice Exposes the cause to errors.Is and errors.As.
//
// @return error InternalError, which may be nil
func (e *CustomError) Unwrap() error { return e.InternalError }

// PresentError @notice The server's error contract, and Config.ErrorPresenter's default.
//
// @dev gqlgen's default presenter forwards err.Error() verbatim. That would hand an
// unauthenticated client raw driver strings ("... SQLSTATE 23505") and with them the table's
// column and constraint names. luima ships no auth, so this redaction is the only thing between
// a caller and the schema.
//
// Resolvers have to opt in to being heard: return a bare errors.New("user already exists") and
// the client sees "internal server error". That is the design, and it is most of why the CRUD
// helpers in the crud package exist — they do the classification so a resolver cannot forget it.
//
// @param ctx  the resolver context, read only for graphql.GetPath
// @param err  the error a resolver returned
// @return *gqlerror.Error the message the client receives, with the field path attached
func PresentError(ctx context.Context, err error) *gqlerror.Error {
	var ce *CustomError
	if errors.As(err, &ce) {
		return &gqlerror.Error{Message: ce.UserMessage, Path: graphql.GetPath(ctx)}
	}
	// Parse and validation errors are gqlgen's own text about the query the client just sent,
	// so they disclose nothing about the server. They pass through unchanged — without this,
	// every schema typo would read as "internal server error" and debugging a client would be
	// impossible.
	var ge *gqlerror.Error
	if errors.As(err, &ge) {
		return ge
	}
	// log.Printf on purpose. A Config.Logger field would be a second way to do what
	// Config.ErrorPresenter already does: wrap this function, log however you like, and return
	// what it returns.
	log.Printf("resolver error: %v", err)
	return gqlerror.WrapPath(graphql.GetPath(ctx), errors.New("internal server error"))
}

// SQLState @notice Returns the Postgres SQLSTATE of err, or "" if err is not a driver error.
//
//	if luima.SQLState(err) == "23505" { // unique_violation
//
// @dev Codes worth classifying: 23505 unique_violation, 23503 foreign_key_violation,
// 23502 not_null_violation, 23514 check_violation. pg.Error also has IntegrityViolation() bool
// if one branch for the whole 23xxx class is enough.
//
// pg.Error is an *interface* (error + Field(byte) + IntegrityViolation()), not a struct
// pointer and not pgx's *pgconn.PgError — so the errors.As target is `var pgErr pg.Error`.
// Getting this wrong is the most common bug when porting error handling between the two
// drivers: it fails to compile in one direction and silently never matches in the other.
//
// @param err     any error, including nil and wrapped chains
// @return string the five-character SQLSTATE, or "" when no pg.Error is in the chain
func SQLState(err error) string {
	var pgErr pg.Error
	if errors.As(err, &pgErr) {
		return pgErr.Field('C')
	}
	return ""
}
