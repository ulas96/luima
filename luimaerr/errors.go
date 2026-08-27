// Package luimaerr @notice luima's error contract: the one place that decides what a resolver
// error is allowed to tell a client.
//
// @dev It is named luimaerr rather than errors because a package called errors shadows the
// standard library in every file that imports both — and this file calls into it three times
// (errors.AsType in PresentError and in SQLState, errors.New for the redacted message).
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
	// UserMessage @notice The text the client receives verbatim. Assume it is public, and
	// assume it is untrusted.
	//
	// @dev Public, so never build it from another error: &CustomError{UserMessage: err.Error()}
	// undoes the redaction in one line that reads like careful error handling, because
	// PresentError returns this field as-is. Untrusted, because the usual way to build it is
	// from client input — crud.Create's label is documented as "user "+id. The response is
	// JSON, so there is no injection at luima's layer; a client that renders error messages
	// into the DOM inherits the sink.
	UserMessage string

	// InternalError @notice The cause, kept for the log and for errors.Is/As.
	//
	// @dev gqlgen's own reference server stores a string here; keeping the error means the
	// underlying pg.Error stays reachable, which is what makes SQLState work on a wrapped
	// error.
	InternalError error

	// Code @notice A machine-readable code for the client, e.g. "CONFLICT". Optional.
	//
	// @dev Clients should branch on this, never on UserMessage — the message is built from
	// caller-supplied text (see crud.Create's label) and is not a stable contract. Empty means
	// no extensions object is emitted, so a zero CustomError is unchanged on the wire.
	//
	// Nothing here is auth-shaped on purpose. CONFLICT and NOT_FOUND describe rows; a library
	// that ships no auth has no business defining UNAUTHENTICATED.
	Code string
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
// It is not, however, the only path to the wire. A transport-level failure — a malformed JSON body,
// an unsupported content type — is written by gqlgen's transport before an executor exists, so it
// never reaches this function and is not redacted. See the note on Config.Fiber in the server
// package. Errors gqlgen generates and hands here keep their own extensions.code
// (GRAPHQL_PARSE_FAILED, GRAPHQL_VALIDATION_FAILED, COMPLEXITY_LIMIT_EXCEEDED) through the
// pass-through branch below.
//
// @param ctx  the resolver context, read only for graphql.GetPath
// @param err  the error a resolver returned
// @return *gqlerror.Error the message the client receives, with the field path attached
func PresentError(ctx context.Context, err error) *gqlerror.Error {
	if ce, ok := errors.AsType[*CustomError](err); ok {
		out := &gqlerror.Error{Message: ce.UserMessage, Path: graphql.GetPath(ctx)}
		// Only when set, so a zero CustomError is byte-identical on the wire to what 0.2.1
		// sent. An empty code would be worse than none: a client branching on
		// extensions.code == "" has no way to tell "this server does not send codes" from
		// "this error has no code".
		if ce.Code != "" {
			out.Extensions = map[string]any{"code": ce.Code}
		}
		return out
	}
	// Parse and validation errors are gqlgen's own text about the query the client just sent,
	// so they disclose nothing about the server. They pass through unchanged — without this,
	// every schema typo would read as "internal server error" and debugging a client would be
	// impossible.
	//
	// A type assertion, not errors.As/errors.AsType, and that distinction is the whole redaction
	// contract. Those walk the chain, so any error that *wraps* a *gqlerror.Error anywhere inside
	// it would be returned whole — and one line of ordinary-looking error handling,
	// fmt.Errorf("insert into %s failed for tenant %d: %w", table, tenantID, gqlErr), would
	// then ship the table name and the tenant id to the client. Unwrapping here makes redaction
	// opt-*out*. gqlgen hands its own parse and validation errors to the presenter unwrapped,
	// so the branch this exists for is unaffected.
	if ge, ok := err.(*gqlerror.Error); ok { //nolint:errorlint // deliberate; see above
		return ge
	}
	// log.Printf on purpose. A Config.Logger field would be a second way to do what
	// Config.ErrorPresenter already does: wrap this function, log however you like, and return
	// what it returns:
	//
	//	cfg.ErrorPresenter = func(ctx context.Context, err error) *gqlerror.Error {
	//	    slog.ErrorContext(ctx, "resolver error", "err", err)
	//	    return luimaerr.PresentError(ctx, err)
	//	}
	//
	// %q, not %v: err routinely carries attacker-controlled text — a GraphQL variable echoed
	// back by a constraint message, or the label passed to crud.Create. %v writes newlines
	// literally, so a caller sending "x\nresolver error: all clear" forges a second log line
	// and anything parsing these logs per line can be lied to. %q escapes them and makes the
	// boundary of the untrusted string visible.
	//
	// Note what this line is not: redaction happens on the wire, not here. A Postgres error's
	// DETAIL field carries the offending row's values — Key (email)=(victim@example.com) — so
	// the data withheld from the client above is written to stdout in full. That is deliberate,
	// it is what makes an incident debuggable, and it means your log store inherits the
	// database's confidentiality requirements. See SECURITY.md.
	log.Printf("resolver error: %q", err)
	redacted := gqlerror.WrapPath(graphql.GetPath(ctx), errors.New("internal server error"))
	// Unconditional here, unlike the CustomError branch: this is the one error whose class the
	// client can be told for free. The message says nothing, so the code says nothing either —
	// it just spares every Apollo-shaped client a string comparison against "internal server
	// error", which is the string this function most wants freedom to change.
	redacted.Extensions = map[string]any{"code": "INTERNAL_SERVER_ERROR"}
	return redacted
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
// pointer and not pgx's *pgconn.PgError — so the type argument here is pg.Error itself, and the
// pre-1.27 errors.As spelling of the same thing declares `var pgErr pg.Error` with no `*` and
// still passes `&pgErr` — errors.As always takes a pointer, and passing pgErr itself panics at the
// first driver error that reaches it. Getting this
// wrong is the most common bug when porting error handling between the two drivers: it fails to
// compile in one direction and silently never matches in the other.
//
// @param err     any error, including nil and wrapped chains
// @return string the five-character SQLSTATE, or "" when no pg.Error is in the chain
func SQLState(err error) string {
	if pgErr, ok := errors.AsType[pg.Error](err); ok {
		return pgErr.Field('C')
	}
	return ""
}
