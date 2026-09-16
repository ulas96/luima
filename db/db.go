// Package db @notice Opens the bun handle over a pgdriver pool that luima's resolvers query
// through.
//
// @dev Connecting is the only part of the data layer that is the same in every application.
// Everything else is your models and the crud package. Connect is that sameness in one argument;
// ConnectWith is the same function with one more, for the configuration that has to be code
// rather than connection-string text.
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	neturl "net/url"
	"os"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
)

// Connect @notice Opens the pool the resolvers query through and proves it works.
//
// @dev pgdriver parses the connection string (parseDSN), and three things about how it does that
// decide what a DSN can say:
//
//   - Parameters. It reads host, sslmode, sslrootcert, sslcert, sslkey, application_name, timeout,
//     dial_timeout, connect_timeout, read_timeout and write_timeout. Every other parameter becomes
//     a Config.ConnParams entry, sent as SET name TO 'value' on every new connection. So
//     ?statement_timeout=5s works, and a misspelled parameter is not a parse error: it fails the
//     boot round trip instead — 42704 for an unknown name, 42601 for a key such as a-b — or, for a
//     dotted name such as app.x, which Postgres takes as a custom setting, never fails at all.
//     Two names are refused while parsing instead, password and sslpassword: sent as SET, their
//     value would reach the server log in the statement Postgres rejects (see connector). sslcert
//     and sslkey are read only alongside sslmode or sslrootcert; without either they are sent as
//     SET and fail with 42704. ?host= is dialed as written, with no port added. A timeout written
//     as an integer <= 0, which queryOptions.duration turns into -1ns — a deadline that has
//     already passed — is read as unset, and keeps pgdriver's default, as 0.5.0 read
//     ?connect_timeout=0.
//   - TLS. With sslmode absent, allow or prefer, the connection is TLS with InsecureSkipVerify: a
//     managed Postgres connects with nothing to configure, and nothing is verified, sslrootcert or
//     not. require is just as unverified, unless sslrootcert is given too, when it acts as
//     verify-ca. verify-ca and verify-full both verify the certificate chain, against sslrootcert
//     or the system roots, and the host name, against a ServerName pgdriver sets from the host in
//     the URL's authority, without its port. That is 0.5.0's verify-ca, not libpq's: pgdriver
//     checks verify-ca's chain alone, and connector hands it back to crypto/tls's full check. A
//     host given only as ?host= leaves ServerName empty, and crypto/tls then refuses every
//     verify-ca and verify-full handshake before sending it; set c.TLSConfig.ServerName in a
//     ConnectWith tune for that, or a VerifyConnection of your own for a chain-only check. disable
//     is plaintext. In every mode but disable a server that refuses TLS is an error, never a
//     plaintext fallback, so a Postgres without TLS — a CI container, a unix socket — needs
//     ?sslmode=disable.
//   - Defaults. A DSN with no host dials $PGHOST and $PGPORT, then localhost and 5432. A host name
//     or IPv4 address with no port gets 5432, whatever $PGPORT says, but an IPv6 literal gets no
//     port at all — parseDSN adds one only to a host without a colon — so [::1] fails to dial with
//     "missing port in address" until it is written [::1]:5432. No user is $PGUSER, then postgres;
//     no database is $PGDATABASE, then postgres, and that one is silent: a DSN that forgot its
//     database connects to postgres. No password is $PGPASSWORD, which pgdriver does not read and
//     connector does, as 0.5.0 did.
//
// postgres://, postgresql:// and unix:///path/to/socket all parse.
//
// Connect keeps pgdriver's socket timeouts — DialTimeout 5s, ReadTimeout 10s, WriteTimeout 5s
// (newDefaultConfig) — because they are the only bound on the I/O pgdriver does without the
// caller's context: the rows after a query's row description (rows.next reads with
// context.TODO()), the drain in rows.Close, COMMIT and ROLLBACK (tx.Commit and tx.Rollback run on
// context.Background()), and the whole startup of every connection that database/sql's
// connectionOpener dials in the background, on a context with no deadline. Zero them and a server
// that stops answering holds a connection forever — or the pool's one opener goroutine, which
// every later background dial queues behind. What they cost is a ceiling: a statement whose
// response takes longer than ReadTimeout fails client-side with an i/o timeout, whatever
// RequestTimeout allows. Raise it with ?read_timeout=60s, or with ConnectWith and
// func(c *pgdriver.Config) { c.ReadTimeout = 60 * time.Second }.
//
// Query timeouts: server.Config.RequestTimeout puts a deadline on the resolver's context, and
// pgdriver makes the earlier of that deadline and now+WriteTimeout the socket deadline for writing
// each query, and the earlier of it and now+ReadTimeout the one for reading the reply
// ((*Conn).deadline) — all of it for an Exec (readQuery), and up to the row description for a
// query (readQueryData), whose rows are then read without the context. That stops the client
// waiting, and nothing else: pgdriver never asks Postgres to cancel a statement, so the statement
// runs on until it finishes. StatementTimeout, or ?statement_timeout= in the DSN, is the bound
// Postgres enforces itself.
//
// The pool is database/sql's, sized by ConnectWith. Resize it on the returned handle: *bun.DB
// embeds *sql.DB through its state struct, so SetMaxOpenConns, SetMaxIdleConns, SetConnMaxIdleTime
// and Stats are promoted to it. Query hooks go on the handle as well: db.WithQueryHook(h) returns a
// copy over the same pool that runs h, so hand the copy to your resolvers (AddQueryHook is
// deprecated).
//
// Connect takes a URL and nothing else, deliberately — a one-argument default that is correct is
// why this package is one call. What has to be code goes through ConnectWith, which is this
// function with one more parameter.
//
// Unlike the server this was lifted from, Connect returns its error rather than calling
// log.Fatal, and takes the URL rather than reading os.Getenv: a library must not kill the
// caller's process, choose their logging, or read configuration behind their back. Write
// log.Fatal(err) at the call site if that is what you want. Connect's errors never contain the
// DSN or its password, so logging one does not leak the credential — a DSN whose user info an
// unescaped / ? or # cut short is refused before it can dial part of the password as a host or a
// port, and name it in the dial error (see connector) — and a DSN pgdriver would panic on comes
// back as an error too.
//
// @param url     a postgres:// or postgresql:// connection string
// @return *bun.DB the bun handle over a live pool, already proven with a round trip; nil on error
// @return error   a parse failure, or the ping failure with the pool already closed
func Connect(url string) (*bun.DB, error) {
	return ConnectWith(url, nil)
}

// ConnectWith @notice Connect, plus the pgdriver.Config tuning that has to be code.
//
// @dev A function rather than a struct of promoted fields: pgdriver.Config has sixteen of them and
// a struct would cover four, so every option luima did not think of would send the consumer back
// to re-implementing this function — which means re-implementing the parts of it that are easy
// to get wrong: a DSN that must neither panic nor reach a log, the pool sizing, and the bounded
// boot round trip.
//
// tune is called with the *pgdriver.Config the connector dials with — (*Connector).Config returns
// the connector's own pointer, not a copy — after the DSN has been applied and before anything has
// dialed, the one point at which a change reaches every connection. So tune wins over the DSN,
// and it runs before the boot round trip reads DialTimeout, so raising DialTimeout raises that
// bound with it.
//
// A DSN carries statement_timeout, read_timeout and application_name itself (see Connect). What
// is left for tune is what text cannot say: a *tls.Config with an in-memory CA pool or its own
// VerifyConnection, a Dialer, a ConnParams entry merged over whatever the DSN set —
// StatementTimeout is that case pre-written — or a value computed at startup, such as a password
// assigned as it is, with none of the percent-encoding a DSN needs. Raise ReadTimeout here for
// slow statements; do not zero it (see Connect).
//
// Naming *pgdriver.Config costs a consumer an import line and nothing in their module graph:
// luima already requires pgdriver to connect at all.
//
// @param url     a postgres:// or postgresql:// connection string, as Connect takes
// @param tune    called with the parsed config; nil is exactly Connect
// @return *bun.DB the bun handle over a live pool, already proven with a round trip; nil on error
// @return error   a parse failure, or the ping failure with the pool already closed
func ConnectWith(url string, tune func(*pgdriver.Config)) (*bun.DB, error) {
	c, err := connector(url)
	if err != nil {
		return nil, err
	}

	cfg := c.Config()
	if tune != nil {
		tune(cfg)
	}

	sqldb := sql.OpenDB(c)
	// database/sql's defaults are wrong for a server. No limit on open connections turns a burst
	// of requests into a burst of connections, and past max_connections into Postgres's 53300
	// too_many_connections for every client of that server. Two idle connections
	// (defaultMaxIdleConns) mean every burst past two concurrent queries closes the extras as they
	// are released, then pays a dial, a TLS handshake, a startup and the SETs to reopen them.
	//
	// The idle time is what makes the raised idle limit safe. database/sql closes an idle
	// connection for its age only when an idle time or a lifetime is set — startCleanerLocked
	// starts no cleaner otherwise — so without it every connection a burst opened stays open for
	// the life of the process, holding a Postgres backend and a max_connections slot each, up to n
	// of them. Five minutes and ten per CPU are the pool 0.5.0 ran with, its driver's defaults.
	n := 10 * runtime.NumCPU()
	sqldb.SetMaxOpenConns(n)
	sqldb.SetMaxIdleConns(n)
	sqldb.SetConnMaxIdleTime(5 * time.Minute)
	// WithDiscardUnknownColumns, because crud.Create and crud.Update read RETURNING * into the
	// model. Without it a column the model does not declare — one a migration added before the
	// deploy that maps it — fails the scan with "bun: T does not have column" after the statement
	// has already committed, and the client's retry of a stored row gets a CONFLICT. bun reads the
	// flag from the DB alone: go-pg's per-model discard_unknown_columns tag only logs a WARN now,
	// and the flag cannot be set on a handle this function has already built.
	db := bun.NewDB(sqldb, pgdialect.New(), bun.WithDiscardUnknownColumns())

	// sql.OpenDB and bun.NewDB dial nothing — pgdialect's Init is empty. This round trip is what
	// proves the host, the TLS mode, the credentials and every ConnParams SET. Without it a bad
	// credential, or a misspelled DSN parameter, surfaces one failed request at a time in
	// production instead of once, loudly, at boot.
	//
	// ExecContext, not Exec, and the deadline is not decoration. The dial, the TLS handshake, the
	// startup, the SETs and the select all run on this context, and pgdriver's socket deadline is
	// the earlier of its deadline and now+ReadTimeout ((*Conn).deadline). At the 5s default that
	// is this context's, under the 10s ReadTimeout, so it is one bound on the whole round trip —
	// what ?connect_timeout=N reads as if it means. Raised past ReadTimeout, it still bounds the
	// total while ReadTimeout caps each read. A DialTimeout of 0 or less, which only tune can set —
	// connector turns the DSN's back into the default — falls back to 5s here; a negative one still
	// fails the dial at once, because net.Dialer's deadline method adds a negative Timeout too.
	timeout := cfg.DialTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second // pgdriver's own DialTimeout default, in newDefaultConfig
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if _, err := db.ExecContext(ctx, "select 1"); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return db, nil
}

// connector @notice Parses url into the pgdriver connector ConnectWith opens its pool over, with
// an error where pgdriver would panic, neither url nor its password in any error, and the DSN read
// the way 0.5.0 read it where pgdriver reads it otherwise.
//
// @dev Guards first, and pgdriver forces all of them:
//
//   - The cut-short user info. An unescaped / ? or # in a password ends the URL's authority early,
//     and url.Parse accepts what is left whenever it still reads as a host and port.
//     postgres://127.0.0.1:2024#x@db/app is user info cut short at the #: its user becomes the
//     host, and 2024, the start of the password, the port. postgres://app:p@ss/x@db/app keeps a
//     user and a password p, and ss becomes the host. pgdriver dials it — it defaults a missing
//     database, where 0.5.0's parser refused the # and ? shapes for having none — and the dial
//     error that ConnectWith returns names the host and port it dialed, which is to say part of
//     the password: "dial tcp 127.0.0.1:2024: connect: connection refused", "lookup ss: no such
//     host". What every such DSN has, and a well-formed one almost never does, is the @ that should
//     have ended the user info outside the authority, in the path, query or fragment; one that
//     means it can write it as %40. This check, and the next, run outside openConnector because
//     their errors quote nothing, and redact's password check would withhold them whenever the
//     password is short enough to occur in their wording.
//   - ?password= and ?sslpassword=. libpq reads both; pgdriver reads neither, so each becomes a
//     ConnParams entry that newConn sends as SET password TO 'value' once authentication has
//     succeeded without it — trust, peer, a certificate, or the same password in the user info.
//     Postgres refuses the SET with 42704 and, at the default log_min_error_statement, writes the
//     statement to the server log, value and all, where luima's redaction never reaches. The key
//     is compared as Postgres compares a setting name: without case, and without quotes.
//   - The recover and the redaction: see openConnector.
//
// Then three corrections, each where pgdriver departs from what a DSN meant under 0.5.0:
//
//   - verify-ca checks the host name. pgdriver implements it as libpq does, a VerifyPeerCertificate
//     callback over the chain with InsecureSkipVerify set, and uses the same callback for require
//     with an sslrootcert; that callback is the only one parseDSN installs. Clearing both hands
//     the connection back to crypto/tls, which verifies the same RootCAs and the ServerName
//     parseDSN already set. 0.5.0's verify-ca was that check, so a DSN carried over is not quietly
//     weaker; a chain-only check is still a tune away, as a VerifyConnection of your own.
//   - A negative timeout is unset. queryOptions.duration answers -1 for an integer <= 0, meaning
//     "disabled", and pgdriver then adds it to now: every dial, read or write gets a deadline
//     already passed. pgdriver's own defaults go back in, which is what 0.5.0 made of
//     ?connect_timeout=0, and what luima's zero-is-unset rule makes of it. Zero itself — no
//     deadline — stays reachable from tune, where Connect's warning about it is.
//   - $PGPASSWORD fills an empty password. go-pg read it; pgdriver reads $PGHOST, $PGPORT, $PGUSER
//     and $PGDATABASE, and not it.
//
// Before tune, all of it, so tune still wins.
//
// @param url                  the connection string, as ConnectWith takes it
// @return *pgdriver.Connector the connector, holding the live *pgdriver.Config; nil on error
// @return error               "parse database url: ...", with url and its password redacted
func connector(url string) (*pgdriver.Connector, error) {
	u, err := neturl.Parse(url)
	if err == nil && strings.Contains(u.Opaque+u.EscapedPath()+u.RawQuery+u.EscapedFragment(), "@") {
		return nil, errors.New("parse database url: the user info ends before its @ " +
			"(percent-encode any / ? # or % in the user or password, and write any @ past the host as %40)")
	}
	if err == nil {
		for k := range u.Query() {
			if n := strings.ToLower(strings.Trim(k, `"`)); n == "password" || n == "sslpassword" {
				return nil, errors.New("parse database url: ?" + n + "= is not read by pgdriver, which would send it " +
					"to the server as a SET (put the password in the user info, or set it in a ConnectWith tune)")
			}
		}
	}

	c, err := openConnector(url)
	if err != nil {
		return nil, err
	}
	cfg := c.Config()

	if t := cfg.TLSConfig; t != nil && t.VerifyPeerCertificate != nil {
		t.InsecureSkipVerify = false
		t.VerifyPeerCertificate = nil
	}

	def := pgdriver.NewConnector().Config()
	if cfg.DialTimeout < 0 {
		cfg.DialTimeout = def.DialTimeout
	}
	if cfg.ReadTimeout < 0 {
		cfg.ReadTimeout = def.ReadTimeout
	}
	if cfg.WriteTimeout < 0 {
		cfg.WriteTimeout = def.WriteTimeout
	}

	if cfg.Password == "" {
		cfg.Password = os.Getenv("PGPASSWORD")
	}
	return c, nil
}

// openConnector @notice pgdriver's own DSN parse, with an error where it would panic and neither
// url nor its password in the error.
//
// @dev OpenConnector returns parseDSN's error instead of panicking with it, but parseDSN builds its
// options as it goes, and WithUser panics on an empty name — so postgres://:secret@host/db and
// postgres://@host/db panic out of the very call that validates. (WithAddr, WithDatabase and
// WithNetwork panic on "" too; parseDSN never passes them one.) Connect must not panic: a panic
// skips the caller's error handling, and a crash prints the panic's value before any redaction.
//
// The connector OpenConnector returns is the one to keep. It is NewConnector(opts...) behind the
// driver.Connector interface, so the assertion cannot fail, and keeping it parses the DSN once:
// building a second one with NewConnector(WithDSN(url)) would parse it again, read the sslrootcert,
// sslcert and sslkey files again, and panic inside WithDSN if one of them changed in between.
// WithDSN's options alone, too, with no WithReadTimeout(0) or WithWriteTimeout(0): pgdriver's 10s
// and 5s are the only deadline on the reads and writes it does without a context. See Connect.
//
// @param url                  the connection string, as ConnectWith takes it
// @return *pgdriver.Connector the connector parseDSN's options built; nil on error
// @return error               "parse database url: ...", from redact
func openConnector(url string) (c *pgdriver.Connector, err error) {
	defer func() {
		if r := recover(); r != nil {
			perr, ok := r.(error)
			if !ok {
				perr = fmt.Errorf("%v", r)
			}
			c, err = nil, perr
		}
		if err != nil {
			err = redact(url, err)
		}
	}()

	dc, err := pgdriver.NewDriver().OpenConnector(url)
	if err != nil {
		return nil, err
	}
	return dc.(*pgdriver.Connector), nil
}

// redact @notice Builds ConnectWith's parse error from pgdriver's, without the DSN or its password.
//
// @dev The call site logs this error — the quickstart calls log.Fatal on it — so text copied from
// the DSN writes a live credential into a log store with different retention and different access
// control from your secret manager. Keep the diagnosis, drop the secret. Two sources put it there:
//
//   - url.Parse, which parseDSN calls first. Its *url.Error renders as fmt.Sprintf("%s %q: %s",
//     e.Op, e.URL, e.Err) — the raw DSN, password and all — so only Op and Err are kept. Err is not
//     safe either: it quotes the text it rejected, and a password holding an unescaped / ? or #
//     ends the authority early, so postgres://app:Xk9/Q@db/app fails with invalid port ":Xk9"
//     after host, which is the password's first part. Every fragment of input url.Parse puts in
//     an error is quoted — %q, or strconv.Quote in EscapeError, InvalidHostError and netip's
//     ParseAddr error — so every quoted string goes, and the message then names the usual cause.
//   - pgdriver. parseDSN's unix-scheme error is fmt.Errorf("unix socket DSN requires a path: %s",
//     dsn), the DSN again. url.Parse succeeded to get that far, so the DSN is known exactly and is
//     replaced wherever it appears. The decoded password is checked for as well, and that check is
//     for the pgdriver release to come: no error parseDSN builds in v1.2.18 carries it — the DSN it
//     quotes is the raw one, replaced above, and the rest quote a scheme, an sslmode, a file path
//     or a duration — so today it fires only on a password that happens to occur in that text. It
//     withholds the whole text rather than being replaced, because a short password also occurs in
//     pgdriver's own words, and a marker at every place it occurs spells it out — a password of a
//     would turn "requires a path" into "requires [redacted] p[redacted]th".
//
// Neither branch wraps the original with %w: anything that walks the chain would print the text
// this removed.
//
// @param url     the connection string, as given
// @param err     the parse failure, or a recovered panic
// @return error  "parse database url: ...", with url and its password redacted
func redact(url string, err error) error {
	const redacted = "[redacted]"

	if ue, ok := errors.AsType[*neturl.Error](err); ok {
		reason := ue.Err.Error()
		msg := regexp.MustCompile(`"(?:[^"\\]|\\.)*"`).ReplaceAllLiteralString(reason, redacted)
		if msg != reason {
			msg += " (percent-encode any / ? # or % in the user or password)"
		}
		return fmt.Errorf("parse database url: %s: %s", ue.Op, msg)
	}

	msg := err.Error()
	// Not for an empty url: strings.ReplaceAll with an empty old string inserts the marker between
	// every rune.
	if url != "" {
		msg = strings.ReplaceAll(msg, url, redacted)
	}
	if u, perr := neturl.Parse(url); perr == nil {
		if pw, ok := u.User.Password(); ok && pw != "" && strings.Contains(msg, pw) {
			msg = "pgdriver's error is withheld because it contains the password"
		}
	}
	return errors.New("parse database url: " + msg)
}

// StatementTimeout @notice A tune func for ConnectWith that bounds every query server-side.
//
// @dev A DSN can say ?statement_timeout=30s as well; this is the same bound for a duration that
// lives in Go rather than in the connection string, and it wins over the DSN's. pgdriver has no
// OnConnect hook. What it has is Config.ConnParams, which newConn sends as one SET per key on
// every connection it opens, before the connection is used. So this merges one key into that map
// rather than replacing it: a ?statement_timeout= in the DSN is overridden, and every other DSN
// parameter still goes out. To combine it with tuning of your own, call it inside your tune func:
// db.StatementTimeout(d)(c).
//
// ms stays an int64 because the value is formatted into the statement, not bound: newConn sends
// "SET statement_timeout TO $1", and formatQuery's appendArg writes an int64 there bare — SET
// statement_timeout TO 1500, which Postgres reads as milliseconds. appendArg refuses every type it
// does not list, int and time.Duration among them, with "pgdriver: unexpected arg", and that would
// fail every dial.
//
// This is the bound RequestTimeout cannot be. pgdriver never asks Postgres to cancel anything — it
// reads the backend key at startup and never uses it — so a context deadline stops only the
// client: (*Conn).deadline turns it into a socket deadline, the read fails, the connection is
// closed, and the statement runs on in Postgres until it finishes, holding its backend and its
// locks. statement_timeout is enforced by the server whether or not the client is still there.
// See SECURITY.md.
//
// When it fires, Postgres answers SQLSTATE 57014, and pgdriver returns that error as it is —
// checkBadConn always returns the original — so luimaerr.SQLState reads it, provided the client is
// still reading. The read waiting for the answer has a socket deadline of its own, the earlier of
// ReadTimeout (10s unless raised) and the request context's deadline, so a d at or past either one
// still bounds the statement in Postgres, but the caller gets an i/o timeout with no SQLSTATE
// first, on a connection pgdriver then closes. Keep d under both, or raise them with it.
//
// And the connection may not survive a 57014 that does arrive. checkBadConn counts 57014 as a bad
// connection and closes it, but it runs only in ExecContext and QueryContext: a 57014 read there —
// by an Exec, or by a query before its row description arrives — costs the pool that connection,
// and the next one pays a dial and the SETs again, while one read later, in rows.next, leaves the
// connection pooled. Inside a transaction, a closed connection fails every later statement, COMMIT
// included, with driver.ErrBadConn.
//
// The round-up is not tidiness. Milliseconds truncates, so any d under 1ms would reach Postgres as
// "SET statement_timeout TO 0" — which is Postgres for *no timeout at all*, turning a bound into
// its own opposite silently. A zero or negative d still disables it, because that is what the
// caller asked for. The clamp to 0 is not tidiness either: Postgres accepts statement_timeout only
// in 0 ms .. 2147483647 ms, so an unclamped -1s would reach it as "SET statement_timeout TO -1000"
// and be refused with SQLSTATE 22023. newConn returns that error in place of the connection, and
// database/sql fails the query that asked for one with it — it retries only driver.ErrBadConn
// (DB.retry). Every new connection sends the SET, so every one would fail — ConnectWith's own boot
// ping first, which turns the documented way to disable the bound into a ConnectWith that can only
// return an error. Only the lower end is clamped: a d above 2147483647ms, about 24.8 days, is
// refused the same way.
//
// @param d      the bound; Postgres cancels a statement that exceeds it (SQLSTATE 57014, readable
// with luimaerr.SQLState while d is under ReadTimeout and the request deadline). Zero or negative
// disables it.
// @return func(*pgdriver.Config) a tune func for ConnectWith
func StatementTimeout(d time.Duration) func(*pgdriver.Config) {
	ms := max(d.Milliseconds(), 0)
	if d > 0 && ms == 0 {
		ms = 1
	}
	return func(c *pgdriver.Config) {
		// A DSN without extra parameters leaves ConnParams nil, and writing to a nil map panics.
		if c.ConnParams == nil {
			c.ConnParams = map[string]any{}
		}
		c.ConnParams["statement_timeout"] = ms
	}
}
