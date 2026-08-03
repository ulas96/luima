// Package db @notice Opens the Postgres pool luima's resolvers query through.
//
// @dev One function, because connecting is the only part of the data layer that is the same in
// every application. Everything else is your models and the crud package.
package db

import (
	"fmt"

	"github.com/go-pg/pg/v10"
)

// Connect @notice Opens the pool the resolvers query through and proves it works.
//
// @dev Two things about pg.ParseURL that the connection string has to respect:
//
//   - It rejects any query parameter other than sslmode, application_name and connect_timeout,
//     with "pg: options other than ... are not supported". pgx-style tuning such as
//     ?default_query_exec_mode=simple_protocol is therefore a startup failure here, not a
//     no-op.
//   - With sslmode absent it returns &tls.Config{InsecureSkipVerify: true}, so a managed
//     Postgres connects over TLS with nothing to configure — and nothing verified.
//     ?sslmode=verify-full is what verifies it.
//
// Both postgres:// and postgresql:// schemes parse.
//
// Unlike the server this was lifted from, Connect returns its error rather than calling
// log.Fatal, and takes the URL rather than reading os.Getenv: a library must not kill the
// caller's process, choose their logging, or read configuration behind their back. Write
// log.Fatal(err) at the call site if that is what you want.
//
// @param url    a postgres:// or postgresql:// connection string
// @return *pg.DB a live pool, already proven with a round trip; nil on error
// @return error  a parse failure, or the ping failure with the pool already closed
func Connect(url string) (*pg.DB, error) {
	opt, err := pg.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}

	db := pg.Connect(opt)
	// pg.Connect is lazy — it dials nothing. This is the round trip that proves the
	// credentials and the host. Without it a bad credential surfaces one failed request at a
	// time in production instead of once, loudly, at boot.
	if _, err := db.Exec("select 1"); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return db, nil
}
