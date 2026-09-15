package tests

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-pg/pg/v10"

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

// TestConnectVerifyFull @notice Asserts ?sslmode=verify-full gets as far as a TLS handshake.
//
// @dev pg.ParseURL maps verify-ca and verify-full to a bare &tls.Config{} and never sets
// ServerName, and the driver's only tls.Client call passes no host either. crypto/tls then refuses
// outright — "tls: either ServerName or InsecureSkipVerify must be specified in the tls.Config" —
// so before Connect filled ServerName in, the one sslmode that verifies anything could not connect
// at all, and the documented advice in SECURITY.md was unreachable.
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

// TestConnectPingTimeout @notice Asserts Connect gives up on a host that accepts the connection
// and then says nothing.
//
// @dev DialTimeout bounds the dial and nothing else. go-pg leaves ReadTimeout and WriteTimeout at
// zero, which it maps to no socket deadline at all, so a plain Exec("select 1") against a stalled
// peer blocks forever and the process never finishes booting. A black-holed firewall rule, a hung
// proxy and a paused container all present exactly this way.
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
		if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "deadline") {
			t.Logf("Connect failed with %v — bounded, which is what matters", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Connect never returned — the boot-time ping has no deadline")
	}
}

// TestStatementTimeout @notice Proves the bound is enforced by Postgres, not by the client.
//
// @dev SKIPS without DATABASE_URL, like TestCRUD, and for the same reason: this is the only test
// that can tell the two bounds apart. A test that merely asserted OnConnect was set would pass
// against an implementation that never reaches the server.
//
// The assertion is the SQLSTATE. 57014 is query_canceled, which only the server sends — a
// client-side context deadline surfaces as context.DeadlineExceeded with no SQLSTATE at all, and
// that is precisely the failure mode RequestTimeout already has and this exists to fix.
//
// 50ms rather than something tighter: OnConnect runs on the same connection ConnectWith then
// proves with `select 1`, so a bound short enough to catch that round trip makes the test flaky
// rather than strict.
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

	// No deadline on this context, deliberately: with one, a pass would prove nothing about
	// where the cancellation came from.
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
// Without the clamp, -1s reaches Postgres as "SET statement_timeout = -1000". The server rejects
// that with SQLSTATE 22023 (invalid_parameter_value), OnConnect returns it, and go-pg removes the
// connection it was initialising and hands the error to whatever query asked for one
// (baseDB.getConn). The first such query is ConnectWith's own boot ping, so the call returns that
// error instead of a pool. The first assertion is what fails when the clamp is removed.
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
	if _, err := conn.QueryOne(pg.Scan(&got), "show statement_timeout"); err != nil {
		t.Fatal(err)
	}
	if got != "0" {
		t.Errorf("show statement_timeout = %q, want \"0\" — negative is documented as disabled", got)
	}
}
