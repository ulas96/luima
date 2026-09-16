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
// @dev With sslmode absent, TLS is on but the certificate is not verified, and a server that
// declines TLS is an error rather than a quiet fallback to plaintext — ?sslmode=disable is how to
// ask for that. verify-full is what verifies it: the chain and the host name, with pgdriver taking
// ServerName from the host written before the path, as here — a host given only as ?host= leaves
// it empty, and verify-full cannot connect. verify-ca checks the chain only, and require checks
// nothing unless sslrootcert is set, which pgdriver then reads as verify-ca.
//
// pgdriver's parseDSN reads sslmode, sslrootcert, application_name, host and the timeout
// parameters itself, and sslcert and sslkey only alongside an sslmode or sslrootcert. Every other
// query parameter is sent to the server as SET on each new connection, so a misspelled one is not
// a parse error: it usually fails at the boot round trip, as SQLSTATE 42704 for an unknown name.
// db.Connect lists the exceptions.
func ExampleConnect_sslmode() {
	conn, err := db.Connect("postgres://user:pass@db.example.com:5432/postgres?sslmode=verify-full")
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()
}

// ExampleConnectWith @notice A bound Postgres enforces itself.
//
// @dev RequestTimeout puts a deadline on the resolver's context, and pgdriver turns that into a
// socket deadline — which stops the client waiting and does nothing to the server. pgdriver never
// asks Postgres to cancel a statement, so a query whose request has timed out runs on, holding its
// locks, on a backend whose client is already gone. statement_timeout is the bound that does not
// depend on the client still being there.
//
// A DSN can carry it too: pgdriver sends any query parameter it does not know as SET on every new
// connection, so ?statement_timeout=30s works, and StatementTimeout overrides it when both are set.
// ConnectWith is for what a DSN cannot express — a *tls.Config or a Dialer of your own, a value
// computed at run time — and it keeps the bounded boot round trip a hand-rolled constructor loses.
//
// pgdriver's socket timeouts are a client-side bound of their own. Connect keeps its defaults, 10s
// to read and 5s to write, so a response slower than 10s fails with an i/o timeout whatever
// statement_timeout allows; raise ReadTimeout with a tune or ?read_timeout= when a statement is
// allowed to run long.
//
// That is why the bound here is 5s. A query that exceeds it comes back as SQLSTATE 57014, which
// luimaerr.SQLState reads, only because 5s is under both client-side bounds — ReadTimeout's 10s
// and RequestTimeout's 15s default. A 30s bound on the same defaults still stops the statement in
// Postgres at 30s, but by then the caller has had an i/o timeout, with no SQLSTATE, for 20s.
func ExampleConnectWith() {
	conn, err := db.ConnectWith(os.Getenv("DATABASE_URL"), db.StatementTimeout(5*time.Second))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	// tune sees pgdriver's parsed *Config after the DSN is applied and before anything dials, so
	// every connection the pool opens gets what it sets — here, room for a statement allowed a
	// minute, which also needs a RequestTimeout above it:
	//
	//	db.ConnectWith(url, func(c *pgdriver.Config) {
	//	    db.StatementTimeout(time.Minute)(c)
	//	    c.ReadTimeout = 65 * time.Second
	//	})
	//
	// Pool sizing is not in pgdriver.Config at all:
	//
	//	conn.SetMaxOpenConns(40) // pool sizing is on the returned *bun.DB
}
