// Package db @notice Opens the Postgres pool luima's resolvers query through.
//
// @dev Connecting is the only part of the data layer that is the same in every application.
// Everything else is your models and the crud package. Connect is that sameness in one argument;
// ConnectWith is the same function with the one seam a DSN cannot reach.
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
// Connect takes a URL and nothing else, deliberately — a one-argument default that is correct is
// why this package is one call. For tuning that pg.Options exposes and a DSN cannot express —
// ReadTimeout, PoolSize, an OnConnect that sets statement_timeout — use ConnectWith, which is this
// function with one more parameter. Query timeouts do not need either: server.Config.RequestTimeout
// puts a deadline on the resolver's context, and go-pg turns that into both the socket deadline and
// a Postgres CancelRequest.
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
	return ConnectWith(url, nil)
}

// ConnectWith @notice Connect, plus the pg.Options tuning a DSN cannot express.
//
// @dev A function rather than a struct of promoted fields: pg.Options has around twenty of them
// and a struct would cover four, so every option luima did not think of would send the consumer
// back to re-implementing this function — which means re-implementing the two things in it that
// are easy to get wrong, the TLS ServerName fill and the bounded boot ping. tune sees the parsed
// options after pg.ParseURL and before pg.Connect, which is the only point where any of them can
// still be changed.
//
// It runs after the ServerName fill, so a consumer can override that too, and before the ping
// reads opt.DialTimeout, so raising DialTimeout raises the boot round trip with it.
//
// A QueryHook does not need this — pg.DB.AddQueryHook takes one on the returned pool
// (go-pg/hook.go:75). statement_timeout does, and StatementTimeout is that case pre-written.
//
// This costs no import a consumer does not already pay: the resolver root names *pg.DB.
//
// @param url   a postgres:// or postgresql:// connection string, as Connect takes
// @param tune  called with the parsed options; nil is exactly Connect
// @return *pg.DB a live pool, already proven with a round trip; nil on error
// @return error  a parse failure, or the ping failure with the pool already closed
func ConnectWith(url string, tune func(*pg.Options)) (*pg.DB, error) {
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
		if ue, ok := errors.AsType[*neturl.Error](err); ok {
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

	if tune != nil {
		tune(opt)
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

// StatementTimeout @notice A tune func for ConnectWith that bounds every query server-side.
//
// @dev The one piece of pg.Options tuning worth wrapping, because getting it right is four fiddly
// lines rather than an assignment: it has to run on every new pooled connection, the duration has
// to reach Postgres as an integer number of milliseconds, and SET takes no bind parameter — so the
// value is formatted into the statement, which is safe only because it is an int64.
//
// This is the bound RequestTimeout cannot be. A context deadline reaches Postgres as a
// CancelRequest, and that is best-effort: go-pg dials a second connection to send it and only logs
// a failure, so a query can outlive its own cancellation and hold a pooled connection that other
// requests are queued behind. statement_timeout is enforced by the server whether or not the
// client is still there. See SECURITY.md.
//
// The round-up is not tidiness. Milliseconds truncates, so any d under 1ms would reach Postgres as
// "SET statement_timeout = 0" — which is Postgres for *no timeout at all*, turning a bound into
// its own opposite silently. A zero or negative d still disables it, because that is what the
// caller asked for.
//
// Overwrites any OnConnect already set; set yours after this one if you have both.
//
// @param d      the bound; Postgres cancels a statement that exceeds it (SQLSTATE 57014,
// readable with luimaerr.SQLState). Zero or negative disables it.
// @return func(*pg.Options) a tune func for ConnectWith
func StatementTimeout(d time.Duration) func(*pg.Options) {
	ms := d.Milliseconds()
	if d > 0 && ms == 0 {
		ms = 1
	}
	return func(o *pg.Options) {
		o.OnConnect = func(ctx context.Context, cn *pg.Conn) error {
			_, err := cn.ExecContext(ctx, fmt.Sprintf("SET statement_timeout = %d", ms))
			return err
		}
	}
}
