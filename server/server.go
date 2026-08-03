// Package server @notice Mounts a gqlgen handler on Fiber v3, correctly.
//
// @dev "Correctly" is doing real work in that sentence. Every line of Mount exists because
// leaving it out fails silently: r.All rather than r.Post (or browser clients 405 on the CORS
// preflight, with nothing in the log), SetQueryCache (or every request re-parses and
// re-validates), extension.Introspection (or the playground's docs pane is blind).
//
// This package cannot replace gqlgen codegen, and no package could. gqlgen generates code into
// your module: generated.NewExecutableSchema is a symbol only your own `go tool gqlgen generate`
// run produces, from your own schema.graphqls. So you still own gqlgen.yml, schema.graphqls,
// graph/resolver.go and the codegen step — see docs/gqlgen-contract.md, because getting it wrong
// produces runtime panics that `go build` does not catch.
package server

import (
	"github.com/99designs/gqlgen/graphql"
	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/handler/extension"
	"github.com/99designs/gqlgen/graphql/handler/lru"
	"github.com/99designs/gqlgen/graphql/handler/transport"
	"github.com/99designs/gqlgen/graphql/playground"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"
	"github.com/vektah/gqlparser/v2/ast"

	"github.com/ulas96/luima/luimaerr"
)

// Config @notice Assembles the server.
//
// @dev Only Schema is required; every zero value has a working default, so Config{Schema: ...}
// is the good configuration rather than the pathological one.
type Config struct {
	// Schema @notice Your generated executable schema, e.g.
	//   generated.NewExecutableSchema(generated.Config{Resolvers: &graph.Resolver{DB: db}})
	Schema graphql.ExecutableSchema

	// Endpoint @notice The GraphQL path. Default "/graphql".
	Endpoint string

	// Playground @notice The GraphiQL path. Default "/".
	//
	// @dev Fiber matches it exactly, where net/http's Handle("/", ...) was a prefix match — an
	// unknown path is a plain 404 here.
	Playground string

	// DisablePlayground @notice Turns GraphiQL off entirely. Do this in production.
	//
	// @dev A separate bool rather than Playground == "": the zero Config must be the good
	// configuration, so "" has to mean "unset", which leaves no spelling for "off".
	DisablePlayground bool

	// PlaygroundTitle @notice The browser tab title. Default "graphql".
	PlaygroundTitle string

	// QueryCache @notice The parsed-query LRU size. Default 1000. Negative disables it.
	//
	// @dev Zero means "unset", not "off", because a zero-valued Config must not be the
	// pathological one: handler.New starts at graphql.NoCache, and with no cache every
	// request re-parses and re-validates the whole query document. There is no good reason to
	// pass a negative here.
	QueryCache int

	// ComplexityLimit @notice Caps query complexity. Default 1000. Negative disables it.
	//
	// @dev Zero means "unset", as with QueryCache.
	ComplexityLimit int

	// ErrorPresenter @notice The error contract. Default [luimaerr.PresentError].
	ErrorPresenter graphql.ErrorPresenterFunc

	// Fiber @notice Passed through to fiber.New. Ignored by Mount.
	//
	// @dev Fiber's BodyLimit defaults to 4 MB where net/http had none — irrelevant to
	// queries, and the thing to raise here the day you add the multipart transport.
	//
	// Setting Fiber.ErrorHandler does nothing useful: adaptor.HTTPHandler always returns nil,
	// so no resolver error ever becomes a Fiber error. ErrorPresenter is the only error
	// contract there is.
	Fiber fiber.Config
}

// New @notice Builds a *fiber.App with the GraphQL endpoint and playground mounted.
//
// @param cfg        the server configuration; only Schema is required
// @return *fiber.App an app ready for Listen, with cfg.Fiber applied
func New(cfg Config) *fiber.App {
	app := fiber.New(cfg.Fiber)
	Mount(app, cfg)
	return app
}

// Mount @notice Registers the same routes on a router you already have — an existing app, or a
// group.
//
// @dev cfg.Fiber is ignored here: the app already exists, so its configuration is not this
// function's to set.
//
// @param r   a *fiber.App or the result of app.Group(prefix)
// @param cfg the server configuration; only Schema is required
func Mount(r fiber.Router, cfg Config) {
	endpoint := cfg.Endpoint
	if endpoint == "" {
		endpoint = "/graphql"
	}

	// Not handler.NewDefaultServer: deprecated in v0.17.94, and it gives no way to set an
	// error presenter, which makes luimaerr.PresentError impossible. Everything it would have added is
	// spelled out below.
	srv := handler.New(cfg.Schema)

	srv.AddTransport(transport.Options{}) // CORS preflight, for browser clients
	srv.AddTransport(transport.GET{})
	srv.AddTransport(transport.POST{})

	if n := cfg.QueryCache; n >= 0 {
		if n == 0 {
			n = 1000
		}
		srv.SetQueryCache(lru.New[*ast.QueryDocument](n))
	}

	// handler.New adds no extensions; without this the playground's docs pane is blind.
	srv.Use(extension.Introspection{})
	if n := cfg.ComplexityLimit; n >= 0 {
		if n == 0 {
			n = 1000
		}
		srv.Use(extension.FixedComplexityLimit(n))
	}

	presenter := cfg.ErrorPresenter
	if presenter == nil {
		presenter = luimaerr.PresentError
	}
	srv.SetErrorPresenter(presenter)

	// gqlgen is an http.Handler; adaptor converts fasthttp's RequestCtx into an *http.Request
	// per request. So Fiber buys routing, middleware and its ecosystem here — not speed.
	//
	// All, never Post: gqlgen's transports dispatch on the method themselves, so GET, POST and
	// the OPTIONS preflight all have to reach srv. Register with Post and every browser client
	// 405s on the preflight, with nothing in the server log because the request never reached
	// gqlgen. This is the single most important line in the library; TestMountRoutes pins it.
	r.All(endpoint, adaptor.HTTPHandler(srv))

	if !cfg.DisablePlayground {
		path := cfg.Playground
		if path == "" {
			path = "/"
		}
		title := cfg.PlaygroundTitle
		if title == "" {
			title = "graphql"
		}
		r.Get(path, adaptor.HTTPHandler(playground.Handler(title, endpoint)))
	}
}
