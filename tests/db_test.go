package tests

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	neturl "net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/ulas96/luima/db"
	"github.com/ulas96/luima/luimaerr"
)

// TestConnectRedactsDSN @notice Asserts a malformed connection string does not put the password
// in the returned error.
//
// @dev url.Parse fails here on the invalid port, and its *url.Error embeds the raw URL —
// fmt.Sprintf("%s %q: %s", e.Op, e.URL, e.Err), no redaction. Wrapping that with %w and calling
// log.Fatal on it, which is what the quickstart does, writes a live credential to stderr and into
// whatever ships stderr onward. The window is narrow (a syntactically valid DSN never reaches this
// branch) and the blast radius is a production password in a log store.
//
// @param t the test handle
func TestConnectRedactsDSN(t *testing.T) {
	_, err := db.Connect("postgres://app:hunter2@db.internal:not-a-port/app")
	if err == nil {
		t.Fatal("Connect on a malformed DSN returned no error")
	}

	if got := err.Error(); strings.Contains(got, "hunter2") {
		t.Errorf("Connect error contains the password: %q", got)
	}
	// The diagnosis has to survive the redaction, or the fix trades a leak for an unfixable
	// startup failure.
	if got := err.Error(); !strings.Contains(got, "port") {
		t.Errorf("Connect error = %q, want it to still say what was wrong", got)
	}
}

// TestConnectNeverPanicsOrLeaks @notice Asserts a DSN pgdriver cannot use comes back from Connect
// as an error — never as a panic, and never with the password in its text.
//
// @dev Needs no database: every case is refused while the DSN is parsed, before anything dials,
// and the "parse database url: " prefix is asserted to hold Connect to that. pgdriver's parse is
// unsafe to hand these to directly, in four ways:
//
//   - pgdriver.WithDSN panics on any error from parseDSN, so each case here would crash the
//     process from inside Connect.
//   - pgdriver.NewDriver().OpenConnector returns that error instead — except for an empty
//     username, which panics anyway. parseDSN calls pgdriver.WithUser whenever the URL has a
//     userinfo section, and WithUser panics on "". postgres://:pw@host/db and postgres://@host/db
//     both take that path, so the function whose job is to return parse errors crashes on them.
//   - Some of the errors it does return quote the DSN, or part of it. A unix:// DSN with no socket
//     path is refused with the entire DSN formatted into the message, and a malformed URL comes
//     back as url.Parse's *url.Error, which quotes the raw URL. Keeping only that error's Op and
//     Err does not fix it: Err quotes the text url.Parse rejected, and an unescaped / ? or # in a
//     password ends the URL's authority early, so the "port" it rejects is the password's first
//     part — invalid port ":hunter2" after host.
//   - And some DSNs it does not refuse at all. When what is left of a cut-short authority still
//     reads as a host and port, url.Parse accepts it and pgdriver dials it: the password's digits
//     as the port, or the part of a password after an @ as the host. The dial error names both.
//     The three cut-short cases dial 127.0.0.1:1 when that gets through, so a regression fails on
//     the prefix, fast, with no DNS lookup, instead of reaching whatever a real host would answer.
//
// The quickstart log.Fatals Connect's error, so any of these writes a live password, or the start
// of one, to stderr. Against a Connect that passes OpenConnector's result straight through, the
// two empty-username cases panic, the unix, malformed-port and three reserved-character cases
// leak, and the three cut-short cases dial; against one that keeps Op and Err as they are, the
// three reserved-character cases still leak. Against one that calls WithDSN directly, the ten
// cases parseDSN refuses panic — the unsupported sslmode, the foreign scheme and the empty URL,
// whose messages carry no DSN, are here for that regression.
//
// The empty URL is also what os.Getenv returns for an unset DATABASE_URL, and the redaction's first
// edge: replacing the empty string wherever it occurs puts a marker between every character.
//
// The short password is the other edge. A password short enough to occur in pgdriver's own
// wording has to be withheld with the message, not marked wherever it occurs: a marker at every a
// would turn "requires a path" into "requires [redacted] p[redacted]th", which spells the password
// out. It is also the one way to reach the password check at all — no error parseDSN builds today
// carries the decoded password — so the withheld text is asserted, not just the marker count.
//
// The recover runs per case, so a panic fails as that case instead of taking the test binary down
// with every case after it unreported.
//
// @param t the test handle
func TestConnectNeverPanicsOrLeaks(t *testing.T) {
	for _, tc := range []struct{ name, dsn string }{
		{"empty username", "postgres://:hunter2@h:5432/d"},
		{"empty userinfo", "postgres://@h:5432/d"},
		{"unix socket with no path", "unix://u:hunter2@h"},
		{"malformed port", "postgres://u:hunter2@h:not-a-port/d"},
		{"password with a slash", "postgres://u:hunter2/x@h:5432/d"},
		{"password with a hash", "postgres://u:hunter2#x@h:5432/d"},
		{"password with a question mark", "postgres://u:hunter2?x@h:5432/d"},
		{"cut short at a slash, after digits", "postgres://127.0.0.1:1/hunter2@h:5432/d"},
		{"cut short at a hash, after digits", "postgres://127.0.0.1:1#hunter2@h:5432/d"},
		{"cut short at a question mark, after an @", "postgres://u:hunter2@127.0.0.1:1?x@h:5432/d"},
		{"unsupported sslmode", "postgres://u:hunter2@h:5432/d?sslmode=bogus"},
		{"foreign scheme", "mysql://u:hunter2@h/d"},
		{"empty url", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("Connect panicked: %v", r)
				}
			}()

			conn, err := db.Connect(tc.dsn)
			if err == nil {
				conn.Close()
				t.Fatal("Connect returned no error")
			}
			got := err.Error()
			if !strings.HasPrefix(got, "parse database url: ") {
				t.Errorf("Connect error = %q — want it refused while parsing, before anything dials", got)
			}
			if strings.Contains(got, "hunter2") {
				t.Errorf("Connect error contains the password: %q", got)
			}
			if strings.Count(got, "[redacted]") > 1 {
				t.Errorf("Connect error = %q — one marker at most, or the markers spell out what they replace", got)
			}
		})
	}

	t.Run("short password", func(t *testing.T) {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("Connect panicked: %v", r)
			}
		}()

		conn, err := db.Connect("unix://u:a@")
		if err == nil {
			conn.Close()
			t.Fatal("Connect returned no error")
		}
		got := err.Error()
		if strings.Count(got, "[redacted]") > 1 {
			t.Errorf("Connect error = %q — a marker wherever the password occurs spells it out", got)
		}
		if strings.Contains(got, "unix socket") {
			t.Errorf("Connect error = %q — pgdriver's text contains the password a, so it has to be withheld", got)
		}
	})
}

// TestConnectVerifyFull @notice Asserts ?sslmode=verify-full gets as far as a TLS handshake.
//
// @dev crypto/tls refuses a handshake whose tls.Config sets neither ServerName nor
// InsecureSkipVerify — "tls: either ServerName or InsecureSkipVerify must be specified in the
// tls.Config" — and verify-full, the one sslmode that checks the host name as well as the chain,
// has to leave InsecureSkipVerify off. pgdriver's parseDSN fills ServerName in from the host in the
// URL's authority, without its port, for verify-full as for require and verify-ca; luima used to
// fill it itself, for a driver that did not. So this test now guards pgdriver, and luima's parse
// path in front of it: a pgdriver release that drops the fill, or a Connect that swaps in a
// tls.Config of its own, and verify-full cannot connect at all while every sslmode=disable test
// stays green. TestConnectVerifyCA pins what the handshake then verifies.
//
// This cannot be covered by TestCRUD: CI's postgres:16 service container speaks plain TCP and the
// suite connects with sslmode=disable. So the server here is a stub that does exactly the two
// steps needed to observe a ClientHello — read the 8-byte SSLRequest, answer 'S' — and then hangs
// up. A certificate failure afterwards is fine and expected; what is asserted is that the client
// got far enough to send a handshake at all.
//
// @param t the test handle
func TestConnectVerifyFull(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	hello := make(chan byte, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		// The SSLRequest packet is exactly 8 bytes: int32 length, int32 magic 80877103.
		if _, err := io.ReadFull(conn, make([]byte, 8)); err != nil {
			return
		}
		if _, err := conn.Write([]byte("S")); err != nil { // "yes, proceed with TLS"
			return
		}

		first := make([]byte, 1)
		if _, err := io.ReadFull(conn, first); err != nil {
			return
		}
		hello <- first[0]
	}()

	_, err = db.Connect("postgres://u:p@" + ln.Addr().String() + "/d?sslmode=verify-full&connect_timeout=5")
	if err == nil {
		t.Fatal("Connect against a stub that hangs up returned no error")
	}
	if got := err.Error(); strings.Contains(got, "ServerName or InsecureSkipVerify") {
		t.Fatalf("crypto/tls refused before dialling: %q — ServerName was never set", got)
	}

	select {
	case b := <-hello:
		// 0x16 is the TLS record type for a handshake. Anything else means the client sent
		// something other than a ClientHello.
		if b != 0x16 {
			t.Errorf("first byte after the SSLRequest was %#x, want 0x16 (TLS handshake)", b)
		}
	case <-time.After(5 * time.Second):
		t.Error("no TLS handshake arrived — the client never got past its own tls.Config check")
	}
}

// TestConnectVerifyCA @notice Asserts what the two verifying sslmodes check: verify-ca the
// certificate chain and not the host name, verify-full the host name too.
//
// @dev Needs no database. The stub answers the SSLRequest with 'S' and completes a real TLS
// handshake, in the first two cases as a server whose certificate chains to the CA the DSN names in
// sslrootcert but is valid only for 192.0.2.1, while the DSN dials 127.0.0.1. What is asserted is
// whether the client let that handshake finish; the stub hangs up afterwards, so Connect fails
// either way.
//
// verify-full has to refuse it, with an x509.HostnameError: pgdriver leaves the check to
// crypto/tls with ServerName set from the host (parseDSN). The error type is asserted, not just
// the failure, because a verify-full that fails for any other reason fails the stub's handshake
// too — a ServerName that never got set is refused by crypto/tls before it sends a ClientHello —
// and would pass a test that only wanted a failure. A Connect that verified verify-full as
// verify-ca would accept a certificate its CA signed for any host, and nothing else in this suite
// would notice.
//
// verify-ca has to accept it, and that half pins a change rather than a guarantee. pgdriver checks
// verify-ca's chain in a VerifyPeerCertificate callback, with InsecureSkipVerify set and no host
// name (parseDSN) — libpq's meaning of verify-ca. 0.5.0 checked the host name under verify-ca as
// well. When this half fails, pgdriver has started checking it again, and db.Connect's TLS bullet
// is out of date.
//
// Accepting one certificate is not the same claim as checking the chain, so the third case is the
// one that pins the chain: a leaf valid for the address being dialed, signed by a second CA that
// sslrootcert does not name. verify-ca has to refuse that, with an x509.UnknownAuthorityError.
// Without it, a verify-ca that dropped the callback and kept InsecureSkipVerify — accepting any
// certificate at all, which is what "require" does — would pass both other cases.
//
// @param t the test handle
func TestConnectVerifyCA(t *testing.T) {
	caPEM, leaf := testServerCertificate(t, net.IPv4(192, 0, 2, 1))
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	// A second CA, and its PEM is thrown away: nothing the client trusts signed this leaf. Valid
	// for the address the DSN dials, so a refusal can only be the chain check.
	_, untrusted := testServerCertificate(t, net.IPv4(127, 0, 0, 1))

	for _, tc := range []struct {
		name                 string
		mode                 string
		leaf                 tls.Certificate
		accepted             bool
		wantHostname         bool
		wantUnknownAuthority bool
	}{
		{name: "verify-ca", mode: "verify-ca", leaf: leaf, accepted: true},
		{name: "verify-full", mode: "verify-full", leaf: leaf, wantHostname: true},
		{name: "verify-ca untrusted chain", mode: "verify-ca", leaf: untrusted, wantUnknownAuthority: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()

			handshake := make(chan error, 1)
			go func() {
				conn, err := ln.Accept()
				if err != nil {
					handshake <- err
					return
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

				// The SSLRequest packet is exactly 8 bytes: int32 length, int32 magic 80877103.
				if _, err := io.ReadFull(conn, make([]byte, 8)); err != nil {
					handshake <- err
					return
				}
				if _, err := conn.Write([]byte("S")); err != nil {
					handshake <- err
					return
				}
				handshake <- tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{tc.leaf}}).Handshake()
			}()

			_, connErr := db.Connect("postgres://u:p@" + ln.Addr().String() + "/d?sslmode=" + tc.mode +
				"&sslrootcert=" + neturl.QueryEscape(caFile) + "&connect_timeout=5")
			if connErr == nil {
				t.Fatal("Connect against a stub that hangs up returned no error")
			}

			select {
			case err := <-handshake:
				if tc.accepted && err != nil {
					t.Errorf("%s refused a certificate its CA signed for another host: the stub's handshake = %v, Connect = %v", tc.name, err, connErr)
				}
				if !tc.accepted && err == nil {
					t.Errorf("%s completed a handshake it had to refuse (Connect = %v)", tc.name, connErr)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the stub never got as far as a handshake")
			}

			if _, ok := errors.AsType[x509.HostnameError](connErr); ok != tc.wantHostname {
				t.Errorf("Connect = %v — want an x509.HostnameError only from verify-full", connErr)
			}
			if _, ok := errors.AsType[x509.UnknownAuthorityError](connErr); ok != tc.wantUnknownAuthority {
				t.Errorf("Connect = %v — want an x509.UnknownAuthorityError only from the untrusted chain", connErr)
			}
		})
	}
}

// testServerCertificate @notice Builds a throwaway CA, and a server certificate it signs that is
// valid for one IP address.
//
// @dev ECDSA P-256 because it is fast. ServerAuth because x509.Verify asks for that usage when
// none is given, and pgdriver's verify-ca callback gives none. An IP address and no DNS names, so
// crypto/tls's host name check fails against any other address with an x509.HostnameError.
//
// @param t   the test handle
// @param ip  the one address the server certificate is valid for
// @return []byte          the CA certificate, PEM-encoded, for sslrootcert
// @return tls.Certificate the server certificate and its key, for tls.Server
func testServerCertificate(t *testing.T, ip net.IP) ([]byte, tls.Certificate) {
	t.Helper()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "luima test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "luima test server"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{ip},
	}, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// TestConnectPingTimeout @notice Asserts Connect gives up on a host that accepts the connection
// and then says nothing.
//
// @dev DialTimeout bounds the dial and nothing else: pgdriver hands it to net.Dialer, and the
// startup exchange after the dial reads under the earlier of the context's deadline and pgdriver's
// ReadTimeout ((*pgdriver.Conn).deadline). A hung proxy and a paused container present exactly
// like this stub — the TCP handshake completes, then silence — so without a deadline of Connect's
// own, boot would sit out the whole 10s ReadTimeout on every attempt. (A black-holed firewall rule
// is the other case: it drops the SYN, so the dial itself stalls, and DialTimeout already bounds
// that.) Connect runs its boot round trip under a context bounded by DialTimeout, and
// ?connect_timeout=1 is a DialTimeout of 1s, so that context expires well before the ReadTimeout
// and ends the startup read here after one second.
//
// The limit below is 5s rather than 10s because 10s is where the ReadTimeout would end the wait
// with no help from Connect: a limit at or past it races that timeout instead of catching a
// Connect whose boot round trip lost its own deadline.
//
// @param t the test handle
func TestConnectPingTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		// Accept and stall. Never close: closing would surface as EOF, which is the failure
		// mode that already worked.
		defer conn.Close()
		time.Sleep(30 * time.Second)
	}()

	done := make(chan error, 1)
	go func() {
		_, err := db.Connect("postgres://u:p@" + ln.Addr().String() + "/d?sslmode=disable&connect_timeout=1")
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Connect to a stalled peer returned no error")
		}
		// The expected failure is the startup read hitting its socket deadline, which is
		// os.ErrDeadlineExceeded ("i/o timeout"). Anything else is still bounded, so it is logged
		// rather than failed.
		if !errors.Is(err, os.ErrDeadlineExceeded) && !errors.Is(err, context.DeadlineExceeded) {
			t.Logf("Connect failed with %v — bounded, which is what matters", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Connect did not return within 5s — the boot round trip is not bounded by connect_timeout")
	}
}

// TestConnectKeepsSocketTimeouts @notice Asserts tune is handed pgdriver's socket timeouts
// untouched and the DSN already applied, and that what tune sets is what the connector dials with.
//
// @dev Needs no database. tune installs a Dialer that refuses, so the boot round trip fails at its
// first dial, just after tune has seen the config.
//
// The three timeouts are pgdriver's defaults (newDefaultConfig), and Connect keeps them on purpose.
// ReadTimeout and WriteTimeout are the only deadline on the I/O pgdriver does without the caller's
// context: the rows after a query's first response (rows.next reads under context.TODO()), COMMIT
// and ROLLBACK (tx.Commit and tx.Rollback run on context.Background()), and the startup of every
// connection database/sql's connectionOpener dials in the background. WithReadTimeout(0) and
// WithWriteTimeout(0) ahead of WithDSN are the obvious way to stop ReadTimeout capping
// RequestTimeout, and they would let a server that stops answering hold a connection forever;
// this test is what fails when they are added.
//
// statement_timeout pins the order. tune has to see the DSN's "10s", or it ran before WithDSN
// applied it; the config tune changed has to keep tune's int64, or the DSN was applied again over
// it. Either way StatementTimeout would lose to a ?statement_timeout= in the URL, which it
// overrides.
//
// The Dialer pins the rest: that the connector dials with that config at all. Reading the config
// back through tune's pointer cannot show it — a ConnectWith that built a fresh connector from the
// DSN after tune passes every other assertion here while dialing with none of what tune set,
// StatementTimeout included, and only TestStatementTimeout would notice, with DATABASE_URL set.
// That connector dials the DSN's own host, a unix socket that does not exist, so it fails as fast
// and never calls tune's Dialer.
//
// @param t the test handle
func TestConnectKeepsSocketTimeouts(t *testing.T) {
	var (
		seen    pgdriver.Config  // what tune was handed, copied before it changes anything
		fromDSN any              // statement_timeout as tune first saw it
		live    *pgdriver.Config // the config tune changed
		dialed  atomic.Bool      // set only by a connector dialing with that config
	)
	_, err := db.ConnectWith(
		"postgres://u:p@/d?host=/nonexistent/luima.sock&sslmode=disable&statement_timeout=10s",
		func(c *pgdriver.Config) {
			seen, fromDSN, live = *c, c.ConnParams["statement_timeout"], c
			db.StatementTimeout(1500 * time.Millisecond)(c)
			c.Dialer = func(context.Context, string, string) (net.Conn, error) {
				dialed.Store(true)
				return nil, errors.New("tune's dialer refuses every dial")
			}
		},
	)
	if err == nil {
		t.Fatal("ConnectWith returned no error with a Dialer that refuses every dial")
	}
	if live == nil {
		t.Fatal("tune was never called")
	}
	if !dialed.Load() {
		t.Errorf("the boot round trip never called tune's Dialer (ConnectWith = %v) — the connector dials with a config other than the one tune changed", err)
	}

	if seen.DialTimeout != 5*time.Second || seen.ReadTimeout != 10*time.Second || seen.WriteTimeout != 5*time.Second {
		t.Errorf("tune saw DialTimeout %v, ReadTimeout %v, WriteTimeout %v — want pgdriver's 5s, 10s and 5s, never zeroed",
			seen.DialTimeout, seen.ReadTimeout, seen.WriteTimeout)
	}
	if fromDSN != "10s" {
		t.Errorf("tune saw statement_timeout = %#v, want the DSN's \"10s\" — tune has to run after the DSN is applied", fromDSN)
	}
	if got := live.ConnParams["statement_timeout"]; got != any(int64(1500)) {
		t.Errorf("the config tune changed ends with statement_timeout = %#v, want tune's int64(1500) — nothing may apply the DSN over it", got)
	}
}

// TestStatementTimeout @notice Proves the bound is enforced by Postgres, not by the client.
//
// @dev SKIPS without DATABASE_URL, like TestCRUD, and for the same reason: this is the only test
// that can tell the two bounds apart. A test that merely asserted StatementTimeout put
// statement_timeout into pgdriver's ConnParams would pass against an implementation whose SET never
// reaches the server.
//
// The assertion is the SQLSTATE. 57014 is query_canceled, which only the server sends. A
// client-side bound carries no SQLSTATE at all: a socket deadline — a context's, or pgdriver's own
// ReadTimeout — surfaces as a net.Error reading "i/o timeout", and a context that expires while
// waiting for a pooled connection surfaces as ctx.Err() from database/sql ((*sql.DB).conn).
// Neither stops the statement, and that is precisely the failure mode RequestTimeout has and this
// exists to fix.
//
// 50ms rather than something tighter: pgdriver sends the SET from newConn, on the connection
// database/sql dials for ConnectWith's own boot round trip, so that round trip already runs under
// the bound, and a bound short enough to catch it makes the test flaky rather than strict.
//
// @param t the test handle
func TestStatementTimeout(t *testing.T) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set — skipping the server-side timeout round trip")
	}

	conn, err := db.ConnectWith(url, db.StatementTimeout(50*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })

	// No deadline on this context, deliberately: the only client-side bounds left are pgdriver's
	// socket timeouts, 10s for the read by default, far past pg_sleep(1) — so whatever stops the
	// statement at 50ms has to be the server.
	_, err = conn.ExecContext(context.Background(), "select pg_sleep(1)")
	if err == nil {
		t.Fatal("select pg_sleep(1) succeeded under a 50ms statement_timeout")
	}
	if state := luimaerr.SQLState(err); state != "57014" {
		t.Errorf("SQLState(%v) = %q, want 57014 (query_canceled) — the bound has to be the server's", err, state)
	}
}

// TestStatementTimeoutNegativeDisables @notice Asserts a negative StatementTimeout disables the
// bound, as its @param says, rather than breaking every connection.
//
// @dev SKIPS without DATABASE_URL, like TestStatementTimeout: what goes wrong is Postgres refusing
// the SET, and only a server can refuse it.
//
// Without the clamp, -1s reaches Postgres as "SET statement_timeout TO -1000" — pgdriver formats
// the int64 into the statement client-side. The server rejects that with SQLSTATE 22023
// (invalid_parameter_value), and pgdriver's newConn returns the error in place of the connection
// it was opening, without closing the socket it dialed. database/sql hands that error to the query
// that asked for a connection ((*sql.DB).conn) and does not retry, since it is not
// driver.ErrBadConn ((*sql.DB).retry). The SET runs on every new connection, so every such query
// fails, and the first is ConnectWith's own boot round trip — the call returns that error instead
// of a pool. The first assertion is what fails when the clamp is removed.
//
// The second assertion is what "disables" means: SHOW reads 0, which is Postgres for no timeout.
// A clamp to any other value — folding a negative d into the 1ms round-up, say — passes the first
// assertion and fails this one.
//
// @param t the test handle
func TestStatementTimeoutNegativeDisables(t *testing.T) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set — skipping the negative statement_timeout round trip")
	}

	conn, err := db.ConnectWith(url, db.StatementTimeout(-time.Second))
	if err != nil {
		t.Fatalf("ConnectWith(StatementTimeout(-1s)) = %v, want a pool with the bound disabled", err)
	}
	t.Cleanup(func() { conn.Close() })

	var got string
	if err := conn.NewRaw("show statement_timeout").Scan(context.Background(), &got); err != nil {
		t.Fatal(err)
	}
	if got != "0" {
		t.Errorf("show statement_timeout = %q, want \"0\" — negative is documented as disabled", got)
	}
}

// TestStatementTimeoutConnParams @notice Asserts StatementTimeout merges one int64 of milliseconds
// into ConnParams — over a statement_timeout the DSN set, beside every other parameter.
//
// @dev Needs no database, so unlike the two tests above it runs in every `go test ./...` — and
// unlike TestStatementTimeout it proves nothing about the server, which is why both exist. Each
// assertion is a way to break every connection, or lose the bound quietly, that the round trips
// catch only with DATABASE_URL set, or not at all:
//
//   - The type. newConn sends the entry as "SET statement_timeout TO $1", and formatQuery's
//     appendArg writes an int64 there bare but refuses every type it does not list, int and
//     time.Duration among them, with "pgdriver: unexpected arg" — which fails every dial.
//   - The merge. The map also holds every DSN parameter pgdriver's parseDSN does not read itself;
//     replacing it would drop a ?search_path= from every connection without a word.
//   - The nil map. A DSN with no such parameter leaves ConnParams nil, and writing to it panics.
//   - The ends. 500µs has to round up to 1, because 0 is Postgres for no timeout at all, and -1s
//     has to clamp to 0, because Postgres refuses -1000 with SQLSTATE 22023 on every connection.
//
// @param t the test handle
func TestStatementTimeoutConnParams(t *testing.T) {
	c := &pgdriver.Config{ConnParams: map[string]any{"statement_timeout": "10s", "search_path": "public"}}
	db.StatementTimeout(1500 * time.Millisecond)(c)
	if got := c.ConnParams["statement_timeout"]; got != any(int64(1500)) {
		t.Errorf("statement_timeout = %#v, want int64(1500) in place of the DSN's \"10s\"", got)
	}
	if got := c.ConnParams["search_path"]; got != "public" {
		t.Errorf("search_path = %#v, want \"public\" — StatementTimeout has to merge into ConnParams, not replace it", got)
	}

	for _, tc := range []struct {
		d    time.Duration
		want int64
	}{
		{time.Second, 1000},
		{500 * time.Microsecond, 1},
		{0, 0},
		{-time.Second, 0},
	} {
		var c pgdriver.Config
		db.StatementTimeout(tc.d)(&c)
		if got := c.ConnParams["statement_timeout"]; got != any(tc.want) {
			t.Errorf("StatementTimeout(%v) wrote statement_timeout = %#v, want int64(%d)", tc.d, got, tc.want)
		}
	}
}

// TestConnectPoolBound @notice Asserts Connect caps the pool at ten connections per CPU rather
// than leaving database/sql's default, which is no cap at all, and keeps as many idle.
//
// @dev SKIPS without DATABASE_URL: Connect proves the pool with a round trip before it returns
// one, so there is no pool to inspect without a server.
//
// With maxOpen at zero, its default, database/sql dials a new connection whenever none is idle
// ((*sql.DB).conn), and pgdriver imposes no limit of its own. Uncapped, a load spike gives every
// concurrent query a connection until Postgres refuses the next one with SQLSTATE 53300
// (too_many_connections) — on whichever request happened to dial it, and on anything else that
// shares the server's connection slots. Capped, the excess waits for a pooled connection on its
// request context instead, and RequestTimeout bounds that wait.
//
// Stats reads the cap back from the *sql.DB that *bun.DB embeds, so this asserts the setting
// rather than trying to provoke the spike.
//
// The idle limit Connect raises alongside it has no such read-back, so that half is provoked.
// database/sql keeps two idle connections unless told otherwise (defaultMaxIdleConns): of three
// released together it closes one ((*sql.DB).putConnDBLocked counts it in MaxIdleClosed), and the
// next burst pays a dial, a startup and the SETs to open it again.
//
// The idle time that makes that limit safe is not asserted, and cannot be from here: Stats has no
// read-back for it either, and provoking it means waiting out the five minutes before
// database/sql's cleaner counts a connection in MaxIdleTimeClosed. The comment at
// SetConnMaxIdleTime in ConnectWith is its only guard.
//
// @param t the test handle
func TestConnectPoolBound(t *testing.T) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set — skipping the pool bound, which needs a pool Connect has proven")
	}

	conn, err := db.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })

	if got, want := conn.Stats().MaxOpenConnections, 10*runtime.NumCPU(); got != want {
		t.Errorf("Stats().MaxOpenConnections = %d, want %d (10 per CPU) — 0 is database/sql's unlimited default", got, want)
	}

	held := make([]io.Closer, 0, 3)
	for range 3 {
		c, err := conn.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, c)
	}
	for _, c := range held {
		c.Close()
	}
	if s := conn.Stats(); s.Idle != 3 || s.MaxIdleClosed != 0 {
		t.Errorf("3 connections released together left %d idle and closed %d — want all 3 kept, not database/sql's default of 2", s.Idle, s.MaxIdleClosed)
	}
}
