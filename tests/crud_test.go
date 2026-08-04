package tests

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/go-pg/pg/v10"
	"github.com/go-pg/pg/v10/orm"

	"github.com/ulas96/luima/crud"
	"github.com/ulas96/luima/db"
	"github.com/ulas96/luima/luimaerr"
)

// testUser @notice The model TestCRUD round-trips through Postgres.
//
// @dev The ,array tag is load-bearing: without it go-pg falls through to pgTypeJSONB and Postgres
// rejects the JSON for a text[] column. The pg_typeof assertion in TestCRUD pins it.
type testUser struct {
	tableName  struct{} `pg:"luima_test_users"` //nolint:unused // go-pg reads it by reflection
	PersonalID string   `pg:"personal_id,pk"`
	Name       string   `pg:"name"`
	Projects   []string `pg:"projects,array"`

	// Owner @notice Stands in for whatever column your authorization actually keys on.
	//
	// @dev Deliberately nullable in the table and deliberately without ,use_zero, because that
	// pair is the shape the full replace corrupts silently — see testPartialUpdate.
	Owner string `pg:"owner"`
}

// TestCRUD @notice The round trip against a real Postgres.
//
// @dev SKIPS without DATABASE_URL — a green `go test ./...` proves less than it looks, so run
// with -v and confirm this test *ran*. CI runs it against a postgres service container, so it is
// not allowed to rot.
//
// @param t the test handle
func TestCRUD(t *testing.T) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set — skipping the round trip (this is the test that proves anything)")
	}

	conn, err := db.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })

	if _, err := conn.Exec(`create table if not exists luima_test_users (
		personal_id text primary key,
		name        text not null,
		projects    text[] not null default '{}',
		owner       text
	)`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := conn.Exec("drop table if exists luima_test_users"); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})

	ctx := context.Background()
	const id = "L-1"

	created, err := crud.Create(ctx, conn, &testUser{PersonalID: id, Name: "Ada", Projects: []string{"apollo"}}, "user "+id)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.Name != "Ada" {
		t.Errorf("Create returned name %q, want %q", created.Name, "Ada")
	}

	// Pins the ,array tag directly, so a failure reports the actual cause rather than an
	// insert error three layers down.
	var storedType string
	if _, err := conn.QueryOne(pg.Scan(&storedType), "select pg_typeof(projects)::text from luima_test_users limit 1"); err != nil {
		t.Fatal(err)
	}
	if storedType != "text[]" {
		t.Errorf("projects is stored as %q, want %q — the ,array tag is missing or ignored", storedType, "text[]")
	}

	// The duplicate must reach the client as a message, and must not carry driver text with
	// it. This is PresentError's redaction asserted end to end.
	_, err = crud.Create(ctx, conn, &testUser{PersonalID: id, Name: "Ada", Projects: nil}, "user "+id)
	if err == nil {
		t.Fatal("duplicate Create succeeded")
	}
	if got := luimaerr.SQLState(err); got != "23505" {
		t.Errorf("SQLState of a duplicate = %q, want %q", got, "23505")
	}
	msg := luimaerr.PresentError(ctx, err).Message
	if !strings.Contains(msg, "already exists") {
		t.Errorf("duplicate Create presented as %q, want it to mention %q", msg, "already exists")
	}
	if strings.Contains(msg, "SQLSTATE") || strings.Contains(msg, "luima_test_users") {
		t.Errorf("duplicate Create leaked driver detail to the client: %q", msg)
	}

	got, err := crud.Get(ctx, conn, &testUser{PersonalID: id})
	if err != nil || got == nil {
		t.Fatalf("Get: %v, %v", got, err)
	}
	if got.Name != "Ada" || len(got.Projects) != 1 || got.Projects[0] != "apollo" {
		t.Errorf("Get = %+v, want Ada with [apollo]", got)
	}

	// Update is a full replace, so an empty slice has to be able to clear the column. This is
	// what UpdateNotZero would silently fail to do.
	if _, err := crud.Update(ctx, conn, &testUser{PersonalID: id, Name: "Grace", Projects: []string{}}, "user "+id); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// Not redundant: without reading the row back, this test would pass even if the UPDATE
	// matched zero rows, because Update returns the struct it was handed.
	got, err = crud.Get(ctx, conn, &testUser{PersonalID: id})
	if err != nil || got == nil {
		t.Fatalf("Get after Update: %v, %v", got, err)
	}
	if got.Name != "Grace" {
		t.Errorf("name after Update = %q, want %q — the UPDATE did not reach the row", got.Name, "Grace")
	}
	if len(got.Projects) != 0 {
		t.Errorf("projects after Update = %v, want empty — a full replace must be able to clear the column", got.Projects)
	}

	// Absence on Update is RowsAffected() == 0 for a plain UPDATE and pg.ErrNoRows once
	// RETURNING is in the statement. Checking only one is a bug invisible until someone
	// updates a row that does not exist.
	_, err = crud.Update(ctx, conn, &testUser{PersonalID: "no-such-id", Name: "x"}, "user no-such-id")
	if err == nil {
		t.Fatal("Update of an absent row succeeded")
	}
	if msg := luimaerr.PresentError(ctx, err).Message; !strings.Contains(msg, "not found") {
		t.Errorf("Update of an absent row presented as %q, want it to mention %q", msg, "not found")
	}

	rows, err := crud.List[testUser](ctx, conn, func(q *orm.Query) *orm.Query { return q.Order("personal_id") })
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 || rows[0].PersonalID != id {
		t.Errorf("List = %+v, want one row %q", rows, id)
	}

	deleted, err := crud.Delete(ctx, conn, &testUser{PersonalID: id})
	if err != nil || !deleted {
		t.Fatalf("Delete = %v, %v", deleted, err)
	}
	if again, err := crud.Delete(ctx, conn, &testUser{PersonalID: id}); err != nil || again {
		t.Errorf("second Delete = %v, %v — absence is false, not an error", again, err)
	}

	// A missing row is (nil, nil) so a nullable field renders as null rather than an error.
	gone, err := crud.Get(ctx, conn, &testUser{PersonalID: id})
	if err != nil {
		t.Errorf("Get after Delete returned an error: %v — pg.ErrNoRows must become (nil, nil)", err)
	}
	if gone != nil {
		t.Errorf("Get after Delete = %+v, want nil", gone)
	}

	// The empty table still marshals as [], not null, which is what a non-null [T!]! needs.
	empty, err := crud.List[testUser](ctx, conn)
	if err != nil {
		t.Fatalf("List on empty table: %v", err)
	}
	if empty == nil {
		t.Error("List returned a nil slice — it must be seeded so [T!]! marshals as []")
	}

	// Every helper takes orm.DB, so the same calls work inside a transaction unchanged.
	if err := conn.RunInTransaction(ctx, func(tx *pg.Tx) error {
		if _, err := crud.Create(ctx, tx, &testUser{PersonalID: "L-tx", Name: "Tx"}, "user L-tx"); err != nil {
			return err
		}
		return pg.ErrTxDone // roll back
	}); !errors.Is(err, pg.ErrTxDone) {
		t.Errorf("RunInTransaction = %v, want the rollback sentinel", err)
	}
	if after, err := crud.Get(ctx, conn, &testUser{PersonalID: "L-tx"}); err != nil || after != nil {
		t.Errorf("row survived a rolled-back transaction: %+v, %v", after, err)
	}

	t.Run("ownership predicate", func(t *testing.T) { testOwnership(ctx, t, conn) })
	t.Run("partial update", func(t *testing.T) { testPartialUpdate(ctx, t, conn) })
}

// testOwnership @notice Asserts the opts variadic actually scopes Get, Update and Delete.
//
// @dev This is the whole point of the variadic. Without it Get, Update and Delete are hard-wired
// to WherePK alone, so `WHERE personal_id = $1 AND owner_id = $2` — the query authorization
// actually needs — is inexpressible, and every helper is an IDOR by construction. luima ships no
// auth by design; being unable to *write* the predicate is a different thing, and a gap in the
// API rather than in the scope.
//
// @param ctx  the query context
// @param t    the test handle
// @param conn the live pool
func testOwnership(ctx context.Context, t *testing.T, conn *pg.DB) {
	t.Helper()

	ownedBy := func(who string) func(*orm.Query) *orm.Query {
		return func(q *orm.Query) *orm.Query { return q.Where("owner = ?", who) }
	}

	for _, u := range []*testUser{
		{PersonalID: "own-a", Name: "A", Owner: "alice"},
		{PersonalID: "own-b", Name: "B", Owner: "bob"},
	} {
		if _, err := crud.Create(ctx, conn, u, "user "+u.PersonalID); err != nil {
			t.Fatalf("Create %s: %v", u.PersonalID, err)
		}
	}

	// Alice asking for Bob's row gets the same answer as asking for a row that does not exist,
	// which is the correct answer to give an unauthorized caller — it leaks no existence.
	if got, err := crud.Get(ctx, conn, &testUser{PersonalID: "own-b"}, ownedBy("alice")); err != nil || got != nil {
		t.Errorf("scoped Get of another owner's row = %+v, %v — want (nil, nil)", got, err)
	}
	if got, err := crud.Get(ctx, conn, &testUser{PersonalID: "own-a"}, ownedBy("alice")); err != nil || got == nil {
		t.Errorf("scoped Get of your own row = %+v, %v — want the row", got, err)
	}

	_, err := crud.Update(ctx, conn, &testUser{PersonalID: "own-b", Name: "hijacked"}, "user own-b", ownedBy("alice"))
	if err == nil {
		t.Error("scoped Update across owners succeeded")
	} else if msg := luimaerr.PresentError(ctx, err).Message; !strings.Contains(msg, "not found") {
		t.Errorf("scoped Update across owners presented as %q, want %q", msg, "not found")
	}

	if deleted, err := crud.Delete(ctx, conn, &testUser{PersonalID: "own-b"}, ownedBy("alice")); err != nil || deleted {
		t.Errorf("scoped Delete across owners = %v, %v — want false", deleted, err)
	}
	// The row has to still be there. A Delete that reported false while deleting anyway would
	// pass the assertion above and lose the data.
	if survived, err := crud.Get(ctx, conn, &testUser{PersonalID: "own-b"}); err != nil || survived == nil {
		t.Fatalf("Bob's row did not survive Alice's scoped Delete: %+v, %v", survived, err)
	}

	if deleted, err := crud.Delete(ctx, conn, &testUser{PersonalID: "own-b"}, ownedBy("bob")); err != nil || !deleted {
		t.Errorf("scoped Delete by the real owner = %v, %v — want true", deleted, err)
	}
	if _, err := crud.Delete(ctx, conn, &testUser{PersonalID: "own-a"}); err != nil {
		t.Errorf("cleanup Delete: %v", err)
	}
}

// testPartialUpdate @notice Pins both halves of the full-replace trade-off.
//
// @dev Update writes every column, so a column present on the struct but not set by your input
// mapper is written on every call — add `Role string` to a model, forget to touch the mapper, and
// every update clobbers that user's role.
//
// And it writes NULL, not the zero value. go-pg's Field.AppendValue emits NULL for any zero-valued
// field whose tag lacks ,use_zero (NullZero() is !hasFlag(UseZeroFlag)), so what a forgotten mapper
// field actually does depends on the column: NOT NULL turns it into a loud 23502, which is the
// lucky case, and a nullable column takes the NULL silently. `pg:"owner,use_zero"` is the tag that
// changes it to "". The second assertion here documents that in code, because a footgun nobody
// executes is still a footgun.
//
// The first assertion is the escape hatch: q.Column(...) narrows the SET clause, which is what
// makes a real partial update expressible through the same helper. It rests on go-pg honouring
// the column list in appendSetStruct rather than only for the SELECT, so it is worth pinning
// rather than assuming.
//
// @param ctx  the query context
// @param t    the test handle
// @param conn the live pool
func testPartialUpdate(ctx context.Context, t *testing.T, conn *pg.DB) {
	t.Helper()

	const id = "partial-1"
	if _, err := crud.Create(ctx, conn, &testUser{PersonalID: id, Name: "before", Owner: "alice", Projects: []string{}}, "user "+id); err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() {
		if _, err := crud.Delete(ctx, conn, &testUser{PersonalID: id}); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})

	// Owner is deliberately left zero on the model, exactly as a forgetful mapper would.
	cols := func(q *orm.Query) *orm.Query { return q.Column("name") }
	if _, err := crud.Update(ctx, conn, &testUser{PersonalID: id, Name: "after", Projects: []string{}}, "user "+id, cols); err != nil {
		t.Fatalf("Update with Column: %v", err)
	}

	got, err := crud.Get(ctx, conn, &testUser{PersonalID: id})
	if err != nil || got == nil {
		t.Fatalf("Get: %+v, %v", got, err)
	}
	if got.Name != "after" {
		t.Errorf("name = %q, want %q — Column(\"name\") must still write the column it names", got.Name, "after")
	}
	if got.Owner != "alice" {
		t.Errorf("owner = %q, want %q — Column(...) must not touch columns it does not name", got.Owner, "alice")
	}

	// And the other half: a plain Update clobbers it. This is not a bug report, it is the
	// documented full replace — but it becomes a security one the moment the clobbered column is
	// an authorization column, so it is asserted rather than described.
	if _, err := crud.Update(ctx, conn, &testUser{PersonalID: id, Name: "after", Projects: []string{}}, "user "+id); err != nil {
		t.Fatalf("plain Update: %v", err)
	}

	// Asserted against the database rather than through Get, because the distinction does not
	// survive the scan: a NULL text column reads back into a Go string as "". NULL is what makes
	// this sharper than "the column is blanked" — a NOT NULL column raises 23502 instead, so the
	// constraint you did not think of as a safety feature is the only thing that makes this loud.
	var isNull bool
	if _, err := conn.QueryOne(pg.Scan(&isNull), "select owner is null from luima_test_users where personal_id = ?", id); err != nil {
		t.Fatal(err)
	}
	if !isNull {
		t.Error("owner survived a plain Update — go-pg writes NULL for a zero-valued field without ,use_zero, and this test is what documents that")
	}
}
