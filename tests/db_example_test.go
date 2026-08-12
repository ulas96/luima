package tests

import (
	"log"
	"os"
	"time"

	"github.com/ulas96/luima/db"
)

// ExampleConnect @notice Opening the pool at boot.
//
// @dev Connect returns its error rather than calling log.Fatal, so the decision to exit is the
// caller's. The eager round trip inside means a bad credential fails here, at boot, instead of
// one request at a time in production.
func ExampleConnect() {
	conn, err := db.Connect(os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()
}

// ExampleConnect_sslmode @notice Verifying the server certificate.
//
// @dev pg.ParseURL accepts only sslmode, application_name and connect_timeout — anything else is
// a startup failure, not a no-op. With sslmode absent, TLS is on but the certificate is not
// verified; verify-full is what verifies it.
func ExampleConnect_sslmode() {
	conn, err := db.Connect("postgres://user:pass@db.example.com:5432/postgres?sslmode=verify-full")
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()
}

// ExampleConnectWith @notice A bound Postgres enforces itself.
//
// @dev RequestTimeout puts a deadline on the resolver's context, and go-pg turns that into a
// Postgres CancelRequest — but that cancel is best-effort: it dials a second connection to send it
// and only logs a failure, so a query can outlive its own cancellation and hold a pooled
// connection that every other request is queued behind. statement_timeout is the bound that does
// not depend on the client still being there.
//
// It is unreachable through a DSN: pg.ParseURL accepts only sslmode, application_name and
// connect_timeout, and rejects everything else outright. ConnectWith is the seam, and it keeps the
// two things a hand-rolled constructor loses — the TLS ServerName that makes ?sslmode=verify-full
// connect at all, and the bounded boot round trip.
//
// A query that exceeds the bound comes back as SQLSTATE 57014; luimaerr.SQLState reads it.
func ExampleConnectWith() {
	db, err := db.ConnectWith(os.Getenv("DATABASE_URL"), db.StatementTimeout(30*time.Second))
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	// Anything pg.Options exposes is reachable the same way; tune is called with the parsed
	// options, so this is the place for PoolSize, ReadTimeout or an application_name.
	//
	//	db.ConnectWith(url, func(o *pg.Options) { o.PoolSize = 40 })
}
