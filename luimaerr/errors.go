// Package luimaerr @notice luima's error contract: the one place that decides what a resolver
// error is allowed to tell a client.
//
// @dev It is named luimaerr rather than errors because a package called errors shadows the
// standard library in every file that imports both — and this file calls into it twice
// (errors.AsType, in PresentError and in SQLState).
//
// It imports nothing else in luima, so a package that must not pull in Fiber or gqlgen's handler
// can still return a *CustomError.
package luimaerr

import (
	"context"
	"errors"
	"log"
	"slices"

	"github.com/99designs/gqlgen/graphql"
	"github.com/uptrace/bun/driver/pgdriver"
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
	// underlying pgdriver.Error stays reachable, which is what makes SQLState work on a wrapped
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
// unauthenticated client raw driver strings ("... (SQLSTATE=23505)") and with them the table's
// column and constraint names. luima ships no auth, so this redaction is the only thing between
// a caller and the schema.
//
// Resolvers have to opt in to being heard: return a bare errors.New("user already exists") and
// the client sees "internal server error". That is the design, and it is most of why the CRUD
// helpers in the crud package exist — they do the classification so a resolver cannot forget it.
// A *CustomError is the only way in for anything reported while a field resolves: a resolver's
// error, a directive's, graphql.AddError and graphql.AddErrorf, an argument's unmarshalling, a
// recovered panic.
//
// err is almost never the error the resolver produced. graphql.ResolveField passes a returned
// error through graphql.AddFieldLocationToError, and graphql.AddError passes whatever it is given
// through graphql.ErrorOnPath. Both use errors.As, so an error with a *gqlerror.Error anywhere in
// its chain comes back as it went in, and any other is wrapped with gqlerror.WrapPath: Message is
// err.Error() verbatim, and Unwrap returns err. So a resolver's plain error arrives with the same
// type as a parse error, and the pass-through below cannot go by type alone.
//
// Nor does everything on the wire come through here, and the boundary is not the obvious one. A
// request no transport accepts, such as one with an unsupported content type, is answered
// "transport not supported" by handler.Server.ServeHTTP and never presented, and transport.GET
// writes its own refusals. A malformed JSON body is presented: transport.POST reports it through
// Executor.DispatchError as a cause-less *gqlerror.Error quoting the body back, so it passes
// through. That text is the caller's own bytes.
//
// @param ctx  the request context, read for the field context and the path
// @param err  the error gqlgen reports; a resolver's arrives already wrapped (see above)
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
	// Parse, validation and limit errors are gqlgen's own text about the request the client just
	// sent, so they disclose nothing about the server. They pass through unchanged — without
	// this, every schema typo would read as "internal server error" and debugging a client would
	// be impossible. Three checks recognise them, and weakening any one of them leaks.
	//
	// A type assertion, not errors.As/errors.AsType. Those walk the chain, so any error that
	// *wraps* a *gqlerror.Error anywhere inside it would be returned whole — and one line of
	// ordinary-looking error handling, fmt.Errorf("insert into %s failed for tenant %d: %w",
	// table, tenantID, gqlErr), would then ship the table name and the tenant id to the client.
	// Unwrapping here makes redaction opt-*out*.
	//
	// No cause (Err, which Unwrap returns). gqlparser's formatting constructors — Errorf,
	// ErrorPathf, ErrorPosf, ErrorLocf — and the validator's rule errors leave it nil; Wrap,
	// WrapPath and WrapIfUnwrapped copy Message from err.Error() and set it. Drop this check and
	// every driver error a resolver returns goes out verbatim, with no code and no log line, which
	// is what luima did through 0.5.0. It keys on the cause, not on who wrote Message: a
	// *gqlerror.Error with Err set is redacted even with a hand-written Message and its own
	// extensions.code. The one error about the document that gqlparser wraps is redacted with it —
	// a variable default that does not parse for a custom scalar ($t: Time = 99999999999999999999,
	// validator.VariableValues) — and keeping its GRAPHQL_VALIDATION_FAILED code heard would mean
	// trusting a code, which any extension registered through Configure can set.
	//
	// No field context. gqlgen reports everything it has to say about the request itself — parse,
	// validation, variable coercion, complexity, luima's depth limit, a malformed body — through
	// Executor.DispatchError, before any field runs. Everything presented with a field context in
	// ctx came from resolving a field, and a missing cause proves nothing there: a gqlerror.List a
	// resolver decoded from an upstream GraphQL response has none (Err is tagged json:"-"), nor
	// does gqlerror.Errorf("%v", err), nor what a recover function returns for a resolver's panic.
	// Drop this check and those go out verbatim, extensions and all. gqlgen's own text from that
	// stage — "must not be null", "introspection disabled", a built-in scalar's unmarshalling
	// error — is wrapped, so the cause check alone redacts it.
	ge, isGQL := err.(*gqlerror.Error) //nolint:errorlint // deliberate; see above
	if isGQL && ge.Unwrap() == nil && graphql.GetFieldContext(ctx) == nil {
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
	// Note what this line is not: redaction happens on the wire, not here. What is withheld from
	// the client above is written to stderr in full — your table, column and constraint names, and
	// whatever of the client's own input Postgres quoted back into the message. Not the row's
	// values: (pgdriver.Error).Error stops after the SQLSTATE, so the DETAIL field that carries
	// them — Key (email)=(victim@example.com) — is reachable with Field('D') and is nowhere in this
	// line. That is deliberate, it is what makes an incident debuggable, and it means your log
	// store inherits your schema, if not your rows. See SECURITY.md.
	//
	// When err is gqlgen's wrapper, %q renders it through (*gqlerror.Error).Error, which puts
	// gqlparser's position in the client's document first and pgdriver's rendering of the driver
	// error after it — (pgdriver.Error).Error is "%s: %s (SQLSTATE=%s)" over the severity, message
	// and code fields — so the line reads "input:1:2: ping ERROR: duplicate key value violates
	// unique constraint ... (SQLSTATE=23505)". "input" is gqlparser's default source name, not a
	// file of yours.
	log.Printf("resolver error: %q", err)
	redacted := &gqlerror.Error{
		Message: "internal server error",
		Path:    graphql.GetPath(ctx),
		// Unconditional here, unlike the CustomError branch: this is the one error whose class the
		// client can be told for free. The message says nothing, so the code says nothing
		// either — it just spares every Apollo-shaped client a string comparison against
		// "internal server error", which is the string this function most wants freedom to change.
		Extensions: map[string]any{"code": "INTERNAL_SERVER_ERROR"},
	}
	// Redaction removes what went wrong, never where, and gqlgen has usually recorded where more
	// precisely than ctx can: an argument that fails to unmarshal is wrapped on ping.at while ctx
	// still reads ping, and only the wrapper carries locations. Both point into the client's own
	// document, so they disclose nothing — provided they are this request's. ErrorOnPath and
	// AddFieldLocationToError write them into an error in place the first time they report it, so
	// an error value shared across requests arrives carrying the first request's alias and
	// position. A path that does not extend ctx's is not this field's, and copying it could answer
	// this client with another client's field names.
	if isGQL && len(ge.Path) >= len(redacted.Path) &&
		slices.Equal(ge.Path[:len(redacted.Path)], redacted.Path) {
		redacted.Path = ge.Path
		redacted.Locations = ge.Locations
	}
	return redacted
}

// SQLState @notice Returns the Postgres SQLSTATE of err, or "" if err is not a driver error.
//
//	if luima.SQLState(err) == "23505" { // unique_violation
//
// @dev Codes worth classifying: 23505 unique_violation, 23503 foreign_key_violation,
// 23502 not_null_violation, 23514 check_violation, 57014 query_canceled. pgdriver.Error also has
// IntegrityViolation() bool, a switch over the class-23 codes, if one branch for the whole class
// is enough, and StatementTimeout() bool for 57014.
//
// pgdriver.Error is a struct *value*: readError builds it as Error{m: m}, nothing in pgdriver takes
// its address, and Field, IntegrityViolation, StatementTimeout and Error all have value receivers.
// So the type argument here is pgdriver.Error with no `*`, and the pre-1.26 errors.As spelling of
// the same thing declares `var pgErr pgdriver.Error` and passes `&pgErr` — errors.As always takes
// a pointer, and passing pgErr itself panics on the first non-nil error.
//
// The `*` is the trap, and it is silent. *pgdriver.Error has the value's methods too, so it is an
// error: errors.AsType[*pgdriver.Error](err) compiles, and never matches, because the chain holds
// the value and never a pointer to it — every SQLSTATE branch behind it is dead code that reads as
// correct. That is the inverse of the trap an interface error type sets, where the same `*` fails
// to compile, and it is exactly the spelling someone porting from pgx arrives with, because
// pgx's *pgconn.PgError is a pointer.
//
// @param err     any error, including nil and wrapped chains
// @return string the five-character SQLSTATE, or "" when no pgdriver.Error is in the chain
func SQLState(err error) string {
	if pgErr, ok := errors.AsType[pgdriver.Error](err); ok {
		return pgErr.Field('C')
	}
	return ""
}
