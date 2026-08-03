package tests

import (
	"log"
	"os"

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
