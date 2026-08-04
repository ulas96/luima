// Package db @notice Opens the Postgres pool luima's resolvers query through.
//
// @dev One function, because connecting is the only part of the data layer that is the same in
// every application. Everything else is your models and the crud package.
package db

import (
	"context"
	"errors"
	"fmt"
	"net"
	neturl "net/url"
	"time"

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
//     ?sslmode=verify-full is what verifies it, and Connect fills in the ServerName that
//     pg.ParseURL leaves empty so that mode can actually complete a handshake.
//
// Both postgres:// and postgresql:// schemes parse. ?connect_timeout=N bounds the whole boot
// round trip here, not just the dial.
//
// Connect takes a URL and nothing else, deliberately. It is not the only way in: everything it
// does is three exported calls, so tuning that pg.Options exposes and a DSN cannot express —
// ReadTimeout, PoolSize, an OnConnect that sets statement_timeout — is a matter of calling
// pg.ParseURL, editing the *pg.Options and calling pg.Connect yourself. Query timeouts do not
// need that: server.Config.RequestTimeout puts a deadline on the resolver's context, and go-pg
// turns that into both the socket deadline and a Postgres CancelRequest.
//
// Unlike the server this was lifted from, Connect returns its error rather than calling
// log.Fatal, and takes the URL rather than reading os.Getenv: a library must not kill the
// caller's process, choose their logging, or read configuration behind their back. Write
// log.Fatal(err) at the call site if that is what you want. Connect's errors never contain the
// DSN, so logging one does not leak the password.
//
// @param url    a postgres:// or postgresql:// connection string
// @return *pg.DB a live pool, already proven with a round trip; nil on error
// @return error  a parse failure, or the ping failure with the pool already closed
func Connect(url string) (*pg.DB, error) {
	opt, err := pg.ParseURL(url)
	if err != nil {
		// url.Error's Error() is fmt.Sprintf("%s %q: %s", e.Op, e.URL, e.Err) — it embeds the
		// raw DSN, password and all, with no redaction. The call site logs this (the
		// quickstart calls log.Fatal on it), so wrapping it verbatim writes a live credential
		// into a log store with different retention and different access control from your
		// secret manager. Keep the diagnosis, drop the URL.
		//
		// Only this branch needs it: pg.ParseURL's own errors — the unsupported-option and
		// missing-database ones — do not include the URL.
		var ue *neturl.Error
		if errors.As(err, &ue) {
			return nil, fmt.Errorf("parse database url: %s: %w", ue.Op, ue.Err)
		}
		return nil, fmt.Errorf("parse database url: %w", err)
	}

	// pg.ParseURL builds the tls.Config for verify-ca and verify-full but never fills in
	// ServerName, and the only tls.Client call in the driver passes no host either. crypto/tls
	// refuses a handshake with neither ServerName nor InsecureSkipVerify set, so without these
	// lines ?sslmode=verify-full — the one mode that verifies anything — cannot connect at all,
	// and the pressure is to fall back to ?sslmode=require, which is InsecureSkipVerify: true.
	if t := opt.TLSConfig; t != nil && !t.InsecureSkipVerify && t.ServerName == "" {
		host, _, err := net.SplitHostPort(opt.Addr)
		if err != nil {
			return nil, fmt.Errorf("parse database url: host %q: %w", opt.Addr, err)
		}
		t.ServerName = host
	}

	db := pg.Connect(opt)

	// pg.Connect is lazy — it dials nothing. This is the round trip that proves the
	// credentials and the host. Without it a bad credential surfaces one failed request at a
	// time in production instead of once, loudly, at boot.
	//
	// ExecContext, not Exec, and the deadline is not decoration. DialTimeout bounds the dial
	// and nothing else: go-pg defaults ReadTimeout and WriteTimeout to zero, which it maps to
	// no socket deadline at all. So a host that completes the TCP handshake and then stalls —
	// a black-holed firewall rule, a hung proxy, a paused container — leaves a plain Exec
	// blocked forever, and the process never finishes booting. This bounds the whole round
	// trip, which is what ?connect_timeout=N reads as if it means.
	timeout := opt.DialTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second // go-pg's own DialTimeout default, applied inside pg.Connect
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if _, err := db.ExecContext(ctx, "select 1"); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return db, nil
}
