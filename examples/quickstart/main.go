package main

import (
	"log"
	"os"

	"github.com/ulas96/luima"

	"github.com/ulas96/luima/examples/quickstart/graph"
	"github.com/ulas96/luima/examples/quickstart/graph/generated"
)

// main @notice The whole server: connect, build the app, listen.
//
// @dev generated.NewExecutableSchema is the seam luima cannot cross — it is produced by your own
// `go tool gqlgen generate` run from your own schema.graphqls, so it exists only in this module.
// Everything after it is luima's.
//
// log.Fatal is the call site's choice, not the library's: Connect returns its error rather than
// exiting, so a server that wants to retry or fall back still can.
func main() {
	db, err := luima.Connect(os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	app := luima.New(luima.Config{
		Schema: generated.NewExecutableSchema(generated.Config{
			Resolvers: &graph.Resolver{DB: db},
		}),
	})

	log.Fatal(app.Listen(":8080"))
}
