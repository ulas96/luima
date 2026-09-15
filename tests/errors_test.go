package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"slices"
	"strings"
	"testing"

	"github.com/99designs/gqlgen/graphql"
	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/gqlerror"

	"github.com/ulas96/luima/luimaerr"
	"github.com/ulas96/luima/server"
)

// TestPresentError @notice Pins the three branches of the error contract.
//
// @dev Dropping the second branch is what makes every schema typo read as "internal server
// error"; dropping the third is what leaks the table's columns to an unauthenticated caller. So
// does widening the second to every *gqlerror.Error, because that is the type gqlgen wraps each
// resolver error in before the presenter sees it — or to every cause-less one, because a resolver
// can return one it did not write.
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

	// The same shape as the validation error above, presented while a field resolves. gqlgen
	// reports nothing about the document from there, so a resolver, a directive or a recover
	// function built it, and its missing cause proves nothing about its text.
	inField := graphql.WithFieldContext(ctx, &graphql.FieldContext{Object: "Query"})
	if got := luimaerr.PresentError(inField, gqlerror.Errorf("relation %q does not exist", "app_users")).Message; got != "internal server error" {
		t.Errorf("cause-less *gqlerror.Error from a resolving field presented as %q — it must not reach the client", got)
	}

	// The gqlerror branch must match only what it claims to match. Assert with errors.As
	// instead of a type assertion and this case passes the wrapper straight through, message
	// and all — which turns redaction from opt-in into opt-out, since any resolver that wraps a
	// gqlerror while adding context is then exempt from it.
	wrapped := fmt.Errorf("insert into app_users failed for tenant 42: %w", gqlerror.Errorf("inner"))
	if got := luimaerr.PresentError(ctx, wrapped).Message; got != "internal server error" {
		t.Errorf("error wrapping a gqlerror presented as %q — it bypassed redaction", got)
	}

	// The inverse shape, and the one gqlgen actually sends: a *gqlerror.Error wrapping the driver
	// error, built by gqlerror.WrapPath exactly as graphql.ErrorOnPath builds it, with Message
	// copied from err.Error(). ctx carries no path here, so the second assertion can only pass by
	// keeping the wrapper's own.
	onPath := luimaerr.PresentError(ctx, gqlerror.WrapPath(ast.Path{ast.PathName("user")}, leak))
	if onPath.Message != "internal server error" {
		t.Errorf("gqlgen-wrapped driver error presented as %q — it must not reach the client", onPath.Message)
	}
	if got := onPath.Path.String(); got != "user" {
		t.Errorf("redacted path = %q, want the wrapper's %q", got, "user")
	}

	// A wrapper whose path does not extend ctx's is not this field's — gqlgen fills Path in place,
	// so an error value shared across requests carries another request's — and the redacted error
	// falls back to ctx's. Shorter than ctx's is also the case that would slice out of range.
	nested := graphql.WithPathContext(ctx, graphql.NewPathWithField("user"))
	nested = graphql.WithPathContext(nested, graphql.NewPathWithField("name"))
	stale := luimaerr.PresentError(nested, gqlerror.WrapPath(ast.Path{ast.PathName("ping")}, leak))
	if got := stale.Path.String(); got != "user.name" {
		t.Errorf("redacted path = %q, want ctx's %q — another field's path must not be copied", got, "user.name")
	}
}

// TestPresentErrorOverHTTP @notice Pins the error contract on the wire, against the errors gqlgen
// really hands the presenter.
//
// @dev Every other test in this file calls PresentError directly, and that is how the contract
// stayed broken in every release through 0.5.0. gqlgen never passes a resolver's error along as
// returned: graphql.AddFieldLocationToError and graphql.ErrorOnPath wrap it in a *gqlerror.Error
// whose Message is err.Error() verbatim, and a presenter that passed every top-level
// *gqlerror.Error through sent the driver's text to the client, with no code and no log line,
// while each direct call here passed. Queried over HTTP through server.New, nothing between the
// resolver and the client is assumed.
//
// @param t the test handle
func TestPresentErrorOverHTTP(t *testing.T) {
	const secret = "app_users_pkey"
	leak := errors.New(`ERROR #23505 duplicate key value violates unique constraint "` + secret + `"`)

	// One case per route by which gqlgen hands the presenter an error it cannot vouch for. hidden is
	// what went wrong: it must be absent from the body and present in the log.
	for _, tc := range []struct {
		name      string
		cfg       server.Config
		query     string // "{ping}" when empty
		resolve   func(context.Context) (any, error)
		path      string
		locations []gqlerror.Location
		hidden    string
	}{
		{
			// Wrapped by graphql.AddFieldLocationToError, the only route that adds locations.
			name:      "an error returned by a resolver",
			resolve:   func(context.Context) (any, error) { return nil, leak },
			path:      "ping",
			locations: []gqlerror.Location{{Line: 1, Column: 2}},
			hidden:    secret,
		},
		{
			// Wrapped by graphql.ErrorOnPath.
			name: "an error added with graphql.AddError",
			resolve: func(ctx context.Context) (any, error) {
				graphql.AddError(ctx, leak)
				return "pong", nil
			},
			path:   "ping",
			hidden: secret,
		},
		{
			// Not wrapped at all. Err is tagged json:"-", so a decoded error has no cause and only the
			// field context tells it from a validation error. OperationContext.Error splits the list
			// and presents each element on its own.
			name: "a gqlerror.List a resolver decoded from an upstream GraphQL response",
			resolve: func(context.Context) (any, error) {
				raw, err := json.Marshal(map[string]any{"errors": []map[string]string{{"message": leak.Error()}}})
				if err != nil {
					return nil, err
				}
				var upstream graphql.Response
				if err := json.Unmarshal(raw, &upstream); err != nil {
					return nil, err
				}
				return nil, upstream.Errors
			},
			path:      "ping",
			locations: []gqlerror.Location{{Line: 1, Column: 2}},
			hidden:    secret,
		},
		{
			// Recovered by ResolveField and reported through OperationContext.Recover. The recover
			// function returns a cause-less *gqlerror.Error, the shape gqlgen's own docs give
			// SetRecoverFunc, with the panic value formatted in.
			name: "a resolver panic",
			cfg: server.Config{Configure: func(srv *handler.Server) {
				srv.SetRecoverFunc(func(_ context.Context, r any) error { return gqlerror.Errorf("panic: %v", r) })
			}},
			resolve: func(context.Context) (any, error) { panic(leak) },
			path:    "ping",
			hidden:  secret,
		},
		{
			// gqlgen's own text, but returned as a plain error while __schema resolves
			// (ExecutionContextState.IntrospectSchema, called here as generated code calls it), so
			// the presenter cannot tell it from a resolver's. Redacted and logged, deliberately.
			name:  "gqlgen's introspection-disabled error",
			cfg:   server.Config{DisableIntrospection: true},
			query: "{__schema{queryType{name}}}",
			resolve: func(ctx context.Context) (any, error) {
				schema, err := graphql.NewExecutionContextState[any, any, any](
					graphql.GetOperationContext(ctx), &graphql.ExecutableSchemaState[any, any, any]{}, nil, nil,
				).IntrospectSchema()
				return schema, err
			},
			path:      "__schema",
			locations: []gqlerror.Location{{Line: 1, Column: 2}},
			hidden:    "introspection disabled",
		},
		{
			// A client's bad value for gqlgen's built-in Time. gqlparser does not check custom
			// scalars, so graphql.UnmarshalTime's text is the only report, wrapped on the argument's
			// path — which the redacted error keeps, because ctx only knows the field's.
			name:    "gqlgen's error for a bad scalar argument",
			query:   `{ping(at: "yesterday")}`,
			resolve: func(context.Context) (any, error) { return "pong", nil },
			path:    "ping.at",
			hidden:  "RFC3339Nano",
		},
		{
			// Presented before any field runs, like a validation error, and with its code — but
			// validator.VariableValues wraps the strconv error, so its cause redacts it.
			name:    "gqlgen's error for a variable default that does not parse",
			query:   "query($t: Time = 99999999999999999999) { ping(at: $t) }",
			resolve: func(context.Context) (any, error) { return "pong", nil },
			path:    "variable.t",
			hidden:  "value out of range",
		},
	} {
		t.Run(tc.name+" is redacted", func(t *testing.T) {
			logged := captureLog(t)
			cfg := tc.cfg
			cfg.Schema = newResolverStubSchema(tc.resolve)
			query := tc.query
			if query == "" {
				query = "{ping}"
			}
			errs, body := presentOverHTTP(t, cfg, query)

			if strings.Contains(body, tc.hidden) {
				t.Errorf("response %s carries %q — an error the presenter cannot vouch for reached the client", body, tc.hidden)
			}
			if len(errs) != 1 {
				t.Fatalf("response %s carries %d errors, want 1", body, len(errs))
			}
			got := errs[0]
			if got.Message != "internal server error" {
				t.Errorf("message = %q, want %q", got.Message, "internal server error")
			}
			if code := got.Extensions["code"]; code != "INTERNAL_SERVER_ERROR" {
				t.Errorf("extensions.code = %v, want INTERNAL_SERVER_ERROR", code)
			}
			// Redaction removes what went wrong, never where: without the path a client cannot tell
			// which field of its query failed.
			if p := got.Path.String(); p != tc.path {
				t.Errorf("path = %q, want %q", p, tc.path)
			}
			if !slices.Equal(got.Locations, tc.locations) {
				t.Errorf("locations = %v, want %v", got.Locations, tc.locations)
			}
			// The other half of the contract. Redacted on the wire and absent from the log is not
			// redaction, it is an incident nobody can reconstruct.
			if !strings.Contains(logged.String(), tc.hidden) {
				t.Errorf("log %q does not carry the redacted error", logged)
			}
		})
	}

	t.Run("an error value shared across requests is answered on each request's own path", func(t *testing.T) {
		captureLog(t)
		// gqlgen writes Path and Locations into a *gqlerror.Error in place the first time it
		// reports one (graphql.ErrorOnPath, graphql.AddFieldLocationToError), so a package-level
		// error value carries the first request's alias and position into every later request.
		shared := gqlerror.Errorf("not found")
		cfg := server.Config{Schema: newResolverStubSchema(func(context.Context) (any, error) { return nil, shared })}

		first, _ := presentOverHTTP(t, cfg, "query {\n  acmePayrollExport: ping\n}")
		if len(first) != 1 || first[0].Path.String() != "acmePayrollExport" {
			t.Fatalf("first request answered %v, want one error on its own alias", first)
		}
		errs, body := presentOverHTTP(t, cfg, "{ping}")

		if strings.Contains(body, "acmePayrollExport") {
			t.Errorf("response %s carries another request's alias", body)
		}
		if len(errs) != 1 {
			t.Fatalf("response %s carries %d errors, want 1", body, len(errs))
		}
		if p := errs[0].Path.String(); p != "ping" {
			t.Errorf("path = %q, want this request's %q", p, "ping")
		}
		if slices.Contains(errs[0].Locations, gqlerror.Location{Line: 2, Column: 3}) {
			t.Errorf("locations = %v carry another request's position", errs[0].Locations)
		}
	})

	t.Run("a CustomError returned by a resolver is still heard", func(t *testing.T) {
		heard := &luimaerr.CustomError{UserMessage: "user E-1042 already exists", InternalError: leak, Code: "CONFLICT"}
		errs, body := presentOverHTTP(t, server.Config{
			Schema: newResolverStubSchema(func(context.Context) (any, error) { return nil, heard }),
		}, "{ping}")

		if strings.Contains(body, secret) {
			t.Errorf("response %s carries InternalError's text", body)
		}
		if len(errs) != 1 {
			t.Fatalf("response %s carries %d errors, want 1", body, len(errs))
		}
		got := errs[0]
		if got.Message != "user E-1042 already exists" {
			t.Errorf("message = %q — a CustomError must survive gqlgen's wrapping", got.Message)
		}
		if code := got.Extensions["code"]; code != "CONFLICT" {
			t.Errorf("extensions.code = %v, want CONFLICT", code)
		}
		if p := got.Path.String(); p != "ping" {
			t.Errorf("path = %q, want %q", p, "ping")
		}
	})

	// gqlgen's own errors about the document the client sent. Each is built with no cause and
	// presented before any field resolves, which is what the pass-through keys on, so a gqlgen
	// upgrade that starts wrapping one, or reports one from inside a field, fails here rather than in
	// front of a client that can no longer see its own typo.
	for _, tc := range []struct {
		name, query, message, code string
		complexity                 int
	}{
		{
			name: "a parse error", query: "{",
			message: "Expected Name", code: "GRAPHQL_PARSE_FAILED",
		},
		{
			name: "a validation error", query: "{nope}",
			message: "Cannot query field", code: "GRAPHQL_VALIDATION_FAILED",
		},
		{
			name: "a complexity rejection", query: "{a: ping b: ping}", complexity: 1,
			message: "operation has complexity 2", code: "COMPLEXITY_LIMIT_EXCEEDED",
		},
	} {
		t.Run(tc.name+" passes through", func(t *testing.T) {
			errs, body := presentOverHTTP(t, server.Config{
				Schema:          newResolverStubSchema(func(context.Context) (any, error) { return "pong", nil }),
				ComplexityLimit: tc.complexity,
			}, tc.query)

			if len(errs) != 1 {
				t.Fatalf("response %s carries %d errors, want 1", body, len(errs))
			}
			if got := errs[0].Message; !strings.HasPrefix(got, tc.message) {
				t.Errorf("message = %q, want gqlgen's own %q… — client/schema drift must stay debuggable", got, tc.message)
			}
			if code := errs[0].Extensions["code"]; code != tc.code {
				t.Errorf("extensions.code = %v, want %s", code, tc.code)
			}
		})
	}
}

// presentOverHTTP @notice POSTs a document to a server built from cfg and decodes the errors it
// answers with.
//
// @dev Decoded into gqlgen's own graphql.Response, so path, locations and extensions come back as
// the types PresentError produced rather than through a hand-written mirror of the wire format.
//
// @param t      the test handle
// @param cfg    the server configuration; the playground is switched off
// @param query  the GraphQL document
// @return gqlerror.List the errors the client received
// @return string        the raw body, for asserting what never reached it
func presentOverHTTP(t *testing.T, cfg server.Config, query string) (gqlerror.List, string) {
	t.Helper()

	cfg.DisablePlayground = true
	res, err := server.New(cfg).Test(postJSON(query))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	var resp graphql.Response
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("response %s is not a GraphQL response: %v", body, err)
	}
	return resp.Errors, string(body)
}

// captureLog @notice Sends the standard logger to a buffer until the test ends.
//
// @dev PresentError logs with log.Printf, so this is the only place the server-side half of a
// redaction is visible. Swapping a global is safe only because nothing in this package calls
// t.Parallel.
//
// @param t the test handle
// @return *bytes.Buffer everything logged from now until the test's cleanup
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()

	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

// TestPresentErrorCode @notice Pins what reaches extensions.code on each branch.
//
// @dev A client is supposed to branch on the code rather than on the message, and crud.Create
// builds its message from caller-supplied text — so the code is the only part of a luima error
// response that is a contract. Three properties, and the third is the compatibility one: a
// CustomError with no Code must put nothing on the wire, or every client written against 0.2.1
// starts seeing an extensions object it did not have before.
//
// @param t the test handle
func TestPresentErrorCode(t *testing.T) {
	ctx := context.Background()

	coded := luimaerr.PresentError(ctx, &luimaerr.CustomError{UserMessage: "user X already exists", Code: "CONFLICT"})
	if got := coded.Extensions["code"]; got != "CONFLICT" {
		t.Errorf("extensions.code = %v, want CONFLICT", got)
	}

	redacted := luimaerr.PresentError(ctx, errors.New("constraint app_users_pkey"))
	if got := redacted.Extensions["code"]; got != "INTERNAL_SERVER_ERROR" {
		t.Errorf("redacted extensions.code = %v, want INTERNAL_SERVER_ERROR", got)
	}
	if redacted.Message != "internal server error" {
		t.Errorf("the code changed the message to %q", redacted.Message)
	}

	// The zero CustomError is what 0.2.1 callers already construct, and it must stay
	// byte-identical on the wire.
	bare := luimaerr.PresentError(ctx, &luimaerr.CustomError{UserMessage: "nope"})
	if bare.Extensions != nil {
		t.Errorf("a CustomError with no Code emitted extensions %v", bare.Extensions)
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
