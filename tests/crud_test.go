package tests

import (
	"context"
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
		projects    text[] not null default '{}'
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
	}); err != pg.ErrTxDone {
		t.Errorf("RunInTransaction = %v, want the rollback sentinel", err)
	}
	if after, err := crud.Get(ctx, conn, &testUser{PersonalID: "L-tx"}); err != nil || after != nil {
		t.Errorf("row survived a rolled-back transaction: %+v, %v", after, err)
	}
}
