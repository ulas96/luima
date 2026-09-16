package tests

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/99designs/gqlgen/graphql"
	"github.com/uptrace/bun"

	"github.com/ulas96/luima/crud"
	"github.com/ulas96/luima/db"
	"github.com/ulas96/luima/luimaerr"
)

// testUser @notice The model TestCRUD round-trips through Postgres.
//
// @dev The embedded bun.BaseModel is what names the table. bun's (*Table).processFields skips an
// unexported field that is not embedded, so an unexported table-name field would be dropped
// without a word and every query would go to test_users, the pluralized type name.
//
// The ,array tag is load-bearing too: it is what makes pgdialect's onField give the field an array
// appender. Without it a []string falls back to schema.AppendJSONValue, the first Create sends
// '["apollo"]', and Postgres rejects that for a text[] column.
type testUser struct {
	bun.BaseModel `bun:"table:luima_test_users"`

	PersonalID string `bun:"personal_id,pk"`
	Name       string `bun:"name"`

	// Projects @notice The text[] column, NOT NULL DEFAULT '{}' in the table.
	//
	// @dev Every fixture that writes it sets it non-nil — []string{} where the value is beside the
	// point — except testNilSlice's. pgdialect's appendStringSlice writes a nil slice as NULL
	// rather than letting the column default apply, so a nil here is a 23502 — which testNilSlice
	// pins, and which anywhere else would mask the failure under test.
	Projects []string `bun:"projects,array"`

	// Owner @notice Stands in for whatever column your authorization actually keys on.
	//
	// @dev Deliberately without ,nullzero, so a zero Owner is written as the empty string on every
	// full replace — silently, since the empty string is not NULL. testPartialUpdate pins that. The
	// column is nullable with no default so the way out can be observed too: nullzeroUser's tag
	// writes DEFAULT, which is NULL only for a column with no default.
	Owner string `bun:"owner"`
}

// nullzeroUser @notice testUser over the same table, with ,nullzero on Owner.
//
// @dev The tag that turns the blanking testPartialUpdate pins back into NULL. For a zero field
// tagged ,nullzero, bun's (*Field).appendValue writes DEFAULT instead of the value — pgdialect
// declares feature.DefaultPlaceholder, so appendSetStruct asks for DEFAULT rather than NULL — and
// DEFAULT is NULL here because owner has no default. On a column with one, the same tag restores
// the default instead.
type nullzeroUser struct {
	bun.BaseModel `bun:"table:luima_test_users"`

	PersonalID string   `bun:"personal_id,pk"`
	Name       string   `bun:"name"`
	Projects   []string `bun:"projects,array"`
	Owner      string   `bun:"owner,nullzero"`
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

	// Pins the table, not the tag. A missing ,array tag never reaches this line — the Create above
	// fails first, on JSON that text[] rejects — so what this catches is a luima_test_users this
	// test did not create, with some other projects type, which `create table if not exists` keeps.
	var storedType string
	if err := conn.NewRaw("select pg_typeof(projects)::text from luima_test_users limit 1").Scan(ctx, &storedType); err != nil {
		t.Fatal(err)
	}
	if storedType != "text[]" {
		t.Errorf("projects is stored as %q, want %q — luima_test_users is not the table this test creates", storedType, "text[]")
	}

	// The duplicate must reach the client as a message, and must not carry driver text with
	// it. This is PresentError's redaction asserted end to end.
	//
	// Projects has to be non-nil here in particular: Postgres checks NOT NULL before the unique
	// index, so a nil slice turns this into a 23502 and the 23505 is never raised.
	_, err = crud.Create(ctx, conn, &testUser{PersonalID: id, Name: "Ada", Projects: []string{}}, "user "+id)
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

	// Update is a full replace, so an empty slice has to be able to clear the column. pgdialect's
	// appendStringSlice writes []string{} as '{}' (nil would be NULL, and a 23502 here). OmitZero
	// would clear it too: bun counts a slice as zero only when it is nil (schema's zeroChecker).
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

	// Absence on Update has one signal: RowsAffected() == 0, with no error. Through Exec with no
	// dest, (*UpdateQuery).scanOrExec scans RETURNING * into the model with hasDest false, and
	// (*baseQuery)._scan turns zero rows into sql.ErrNoRows only when hasDest is true — for Scan,
	// or Exec handed a dest. So nothing fails when the WHERE matches nothing; without that check,
	// this reports success and echoes the struct back.
	//
	// Projects is set so that a WHERE that did match a row fails here as "succeeded", which is what
	// it would be, rather than as a 23502 from writing NULL over the row's projects.
	_, err = crud.Update(ctx, conn, &testUser{PersonalID: "no-such-id", Name: "x", Projects: []string{}}, "user no-such-id")
	if err == nil {
		t.Fatal("Update of an absent row succeeded")
	}
	if msg := luimaerr.PresentError(ctx, err).Message; !strings.Contains(msg, "not found") {
		t.Errorf("Update of an absent row presented as %q, want it to mention %q", msg, "not found")
	}

	rows, err := crud.List[testUser](ctx, conn, func(q *bun.SelectQuery) *bun.SelectQuery { return q.Order("personal_id") })
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
		t.Errorf("Get after Delete returned an error: %v — sql.ErrNoRows must become (nil, nil)", err)
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

	// Every helper takes bun.IDB, which bun.Tx satisfies, so the same calls work inside a
	// transaction unchanged. RunInTx rolls back when the function returns an error and returns that
	// error as it is, so getting the sentinel back means the Create inside succeeded first.
	errRollback := errors.New("rollback")
	if err := conn.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := crud.Create(ctx, tx, &testUser{PersonalID: "L-tx", Name: "Tx", Projects: []string{}}, "user L-tx"); err != nil {
			return err
		}
		return errRollback
	}); !errors.Is(err, errRollback) {
		t.Errorf("RunInTx = %v, want the rollback sentinel", err)
	}
	if after, err := crud.Get(ctx, conn, &testUser{PersonalID: "L-tx"}); err != nil || after != nil {
		t.Errorf("row survived a rolled-back transaction: %+v, %v", after, err)
	}

	t.Run("ownership predicate", func(t *testing.T) { testOwnership(ctx, t, conn) })
	t.Run("partial update", func(t *testing.T) { testPartialUpdate(ctx, t, conn) })
	t.Run("suppressed conflict", func(t *testing.T) { testSuppressedConflict(ctx, t, conn) })
	t.Run("swallowed by trigger", func(t *testing.T) { testSwallowedByTrigger(ctx, t, conn) })
	t.Run("nil slice", func(t *testing.T) { testNilSlice(ctx, t, conn) })
	t.Run("unknown column", func(t *testing.T) { testUnknownColumn(ctx, t, conn) })
}

// testUnknownColumn @notice Asserts Create and Update succeed against a table holding a column the
// model does not declare.
//
// @dev That table is the deploy order every additive migration uses: the column first, the code
// that maps it after. In between, RETURNING * hands back a column testUser has no field for, and
// bun's (*structTableModel).ScanColumn refuses it — "bun: testUser does not have column" — unless
// the DB was built WithDiscardUnknownColumns. The statement has already committed by then, so
// Create answers a stored row with a redacted error, and the client's retry gets a CONFLICT. bun
// reads the flag from the DB alone — go-pg's per-model discard_unknown_columns tag only logs a WARN
// — so ConnectWith is the one place it can be set.
//
// Last in TestCRUD, because the column stays until the cleanup drops it.
//
// @param ctx  the query context
// @param t    the test handle
// @param conn the live pool
func testUnknownColumn(ctx context.Context, t *testing.T, conn *bun.DB) {
	t.Helper()

	if _, err := conn.ExecContext(ctx, "alter table luima_test_users add column added_later text not null default 'x'"); err != nil {
		t.Fatal(err)
	}
	const id = "unknown-column-1"
	t.Cleanup(func() {
		if _, err := crud.Delete(ctx, conn, &testUser{PersonalID: id}); err != nil {
			t.Errorf("cleanup: %v", err)
		}
		if _, err := conn.ExecContext(ctx, "alter table luima_test_users drop column added_later"); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})

	created, err := crud.Create(ctx, conn, &testUser{PersonalID: id, Name: "Ada", Projects: []string{}}, "user "+id)
	if err != nil || created == nil {
		t.Fatalf("Create against a table with a column the model lacks = %+v, %v — the row was stored either way", created, err)
	}
	updated, err := crud.Update(ctx, conn, &testUser{PersonalID: id, Name: "Grace", Projects: []string{}}, "user "+id)
	if err != nil || updated == nil || updated.Name != "Grace" {
		t.Fatalf("Update against a table with a column the model lacks = %+v, %v", updated, err)
	}
}

// testSuppressedConflict @notice Asserts an insert the caller asked Postgres to skip comes back as
// absence, not as an error.
//
// @dev ON CONFLICT DO NOTHING is the reason Create needed the variadic at all — it is the cheap way
// to attempt an insert inside a transaction without risking the whole thing, because a suppressed
// conflict does not abort the transaction where a real 23505 does. The other way is a savepoint,
// which a tx.RunInTx nested inside the transaction opens ((bun.Tx).RunInTx), for two more
// statements.
//
// The trap it opens is a quiet one. Create issues RETURNING * through Exec with no dest, so
// (*InsertQuery).scanOrExec scans with hasDest false, and a suppressed insert comes back with no
// row, no error and RowsAffected() == 0 — (*baseQuery)._scan raises sql.ErrNoRows only when
// hasDest is true. Nothing fails, so the RowsAffected check is all that stops Create answering
// (m, nil): handing back the values the client sent, here a row named "second", as if Postgres had
// stored them.
//
// @param ctx  the query context
// @param t    the test handle
// @param conn the live pool
func testSuppressedConflict(ctx context.Context, t *testing.T, conn *bun.DB) {
	t.Helper()

	ignoreConflict := func(q *bun.InsertQuery) *bun.InsertQuery { return q.On("CONFLICT DO NOTHING") }
	const id = "conflict-1"
	t.Cleanup(func() {
		if _, err := crud.Delete(ctx, conn, &testUser{PersonalID: id}); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})

	first, err := crud.Create(ctx, conn, &testUser{PersonalID: id, Name: "first", Projects: []string{}}, "user "+id, ignoreConflict)
	if err != nil || first == nil {
		t.Fatalf("first Create = %+v, %v — want the stored row", first, err)
	}

	second, err := crud.Create(ctx, conn, &testUser{PersonalID: id, Name: "second", Projects: []string{}}, "user "+id, ignoreConflict)
	if err != nil {
		t.Errorf("suppressed Create = %v, want nil — the insert was skipped, not broken", err)
	}
	if second != nil {
		t.Errorf("suppressed Create returned %+v, want nil", second)
	}

	// The first row is still the one in the table: DO NOTHING means nothing, not an upsert.
	stored, err := crud.Get(ctx, conn, &testUser{PersonalID: id})
	if err != nil {
		t.Fatal(err)
	}
	if stored == nil || stored.Name != "first" {
		t.Errorf("stored row = %+v, want the first insert untouched", stored)
	}
}

// testSwallowedByTrigger @notice The same absence, reached with no options at all.
//
// @dev This is the one that made 0.3.0 a bug fix rather than a feature. The caller is not the only
// thing that can suppress an insert: a BEFORE INSERT trigger returning NULL does it too, and that
// is the ordinary way to write a soft-ignore or an audit filter. Against 0.2.1, which ran on go-pg,
// this call returned pg.ErrNoRows bare and the client was told the server is broken.
//
// On bun the statement itself reports nothing — the RowsAffected() == 0 that testSuppressedConflict
// describes. sql.ErrNoRows is still asserted against: it is what the same insert returns through
// Scan instead of Exec ((*InsertQuery).Scan passes hasDest true to (*baseQuery)._scan), and
// PresentError would redact it just as it did in 0.2.1.
//
// The trigger is conditional on the id prefix and dropped on the way out, so the surrounding script
// and the other subtests never see it.
//
// @param ctx  the query context
// @param t    the test handle
// @param conn the live pool
func testSwallowedByTrigger(ctx context.Context, t *testing.T, conn *bun.DB) {
	t.Helper()

	if _, err := conn.Exec(`create or replace function luima_test_swallow() returns trigger as $$
		begin
			if new.personal_id like 'swallow-%' then return null; end if;
			return new;
		end;
	$$ language plpgsql`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`create or replace trigger luima_test_swallow_trg
		before insert on luima_test_users
		for each row execute function luima_test_swallow()`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := conn.Exec("drop trigger if exists luima_test_swallow_trg on luima_test_users"); err != nil {
			t.Errorf("cleanup: %v", err)
		}
		if _, err := conn.Exec("drop function if exists luima_test_swallow()"); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})

	// Projects is not what makes this pass: the trigger returns NULL before Postgres checks NOT
	// NULL, so a nil slice is swallowed just the same. It is set so that a trigger that did not
	// fire fails below as a stored row rather than as a 23502.
	got, err := crud.Create(ctx, conn, &testUser{PersonalID: "swallow-1", Name: "gone", Projects: []string{}}, "user swallow-1")
	if err != nil {
		t.Errorf("Create = %v, want nil — the trigger swallowed the row deliberately", err)
	}
	if got != nil {
		t.Errorf("Create returned %+v, want nil", got)
	}
	if errors.Is(err, sql.ErrNoRows) {
		t.Error("Create leaked sql.ErrNoRows, which PresentError redacts to \"internal server error\" — a mutation that did what it was told would read as a broken server")
	}

	if stored, err := crud.Get(ctx, conn, &testUser{PersonalID: "swallow-1"}); err != nil || stored != nil {
		t.Errorf("row %+v reached the table, so the trigger did not fire: %v", stored, err)
	}
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
func testOwnership(ctx context.Context, t *testing.T, conn *bun.DB) {
	t.Helper()

	// The same predicate once per statement type, because each helper takes its own closure type.
	aliceGet := func(q *bun.SelectQuery) *bun.SelectQuery { return q.Where("owner = ?", "alice") }
	aliceUpdate := func(q *bun.UpdateQuery) *bun.UpdateQuery { return q.Where("owner = ?", "alice") }
	aliceDelete := func(q *bun.DeleteQuery) *bun.DeleteQuery { return q.Where("owner = ?", "alice") }
	bobDelete := func(q *bun.DeleteQuery) *bun.DeleteQuery { return q.Where("owner = ?", "bob") }

	for _, u := range []*testUser{
		{PersonalID: "own-a", Name: "A", Owner: "alice", Projects: []string{}},
		{PersonalID: "own-b", Name: "B", Owner: "bob", Projects: []string{}},
	} {
		if _, err := crud.Create(ctx, conn, u, "user "+u.PersonalID); err != nil {
			t.Fatalf("Create %s: %v", u.PersonalID, err)
		}
	}

	// Alice asking for Bob's row gets the same answer as asking for a row that does not exist,
	// which is the correct answer to give an unauthorized caller — it leaks no existence.
	if got, err := crud.Get(ctx, conn, &testUser{PersonalID: "own-b"}, aliceGet); err != nil || got != nil {
		t.Errorf("scoped Get of another owner's row = %+v, %v — want (nil, nil)", got, err)
	}
	if got, err := crud.Get(ctx, conn, &testUser{PersonalID: "own-a"}, aliceGet); err != nil || got == nil {
		t.Errorf("scoped Get of your own row = %+v, %v — want the row", got, err)
	}

	// Projects is set so that a predicate that stopped scoping fails as "succeeded" — a hijack —
	// rather than as a 23502 from writing NULL over Bob's projects.
	_, err := crud.Update(ctx, conn, &testUser{PersonalID: "own-b", Name: "hijacked", Projects: []string{}}, "user own-b", aliceUpdate)
	if err == nil {
		t.Error("scoped Update across owners succeeded")
	} else if msg := luimaerr.PresentError(ctx, err).Message; !strings.Contains(msg, "not found") {
		t.Errorf("scoped Update across owners presented as %q, want %q", msg, "not found")
	}

	if deleted, err := crud.Delete(ctx, conn, &testUser{PersonalID: "own-b"}, aliceDelete); err != nil || deleted {
		t.Errorf("scoped Delete across owners = %v, %v — want false", deleted, err)
	}
	// The row has to still be there. A Delete that reported false while deleting anyway would
	// pass the assertion above and lose the data.
	if survived, err := crud.Get(ctx, conn, &testUser{PersonalID: "own-b"}); err != nil || survived == nil {
		t.Fatalf("Bob's row did not survive Alice's scoped Delete: %+v, %v", survived, err)
	}

	if deleted, err := crud.Delete(ctx, conn, &testUser{PersonalID: "own-b"}, bobDelete); err != nil || !deleted {
		t.Errorf("scoped Delete by the real owner = %v, %v — want true", deleted, err)
	}
	if _, err := crud.Delete(ctx, conn, &testUser{PersonalID: "own-a"}); err != nil {
		t.Errorf("cleanup Delete: %v", err)
	}
}

// testPartialUpdate @notice Pins both halves of the full-replace trade-off, and the tag that
// changes the second.
//
// @dev Update writes every column, so a column present on the struct but not set by your input
// mapper is written on every call — add `Role string` to a model, forget to touch the mapper, and
// every update clobbers that user's role.
//
// It is clobbered with the zero value, not NULL. For a field without ,nullzero, bun's
// (*Field).appendValue hands the value to the field's appender, so the SET clause writes owner as
// the empty string; only a nil pointer or a zero ,nullzero field becomes DEFAULT. And the empty
// string is not NULL, so a NOT NULL text column no longer turns the forgotten field into a loud
// 23502 — it is blanked, silently, whatever the column allows. The second assertion documents that
// in code, because a footgun nobody executes is still a footgun. It fails against go-pg, which
// wrote NULL there, and that is the point: this is the stored-data change an upgrading consumer
// inherits.
//
// The third assertion is the tag that changes it: through nullzeroUser, the same zero Owner is
// written as DEFAULT, which is NULL because owner has no default.
//
// The first assertion is the escape hatch: q.Column(...) narrows the SET clause, which is what
// makes a real partial update expressible through the same helper. It rests on bun's
// (*UpdateQuery).mustAppendSet taking its fields from the column list (getDataFields) rather than
// from every data field, so it is worth pinning rather than assuming.
//
// @param ctx  the query context
// @param t    the test handle
// @param conn the live pool
func testPartialUpdate(ctx context.Context, t *testing.T, conn *bun.DB) {
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
	cols := func(q *bun.UpdateQuery) *bun.UpdateQuery { return q.Column("name") }
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
	// survive the scan: a NULL text column reads back into a Go string as "" too.
	var isNull bool
	var owner string
	if err := conn.NewRaw("select owner is null, owner from luima_test_users where personal_id = ?", id).Scan(ctx, &isNull, &owner); err != nil {
		t.Fatal(err)
	}
	if isNull || owner != "" {
		t.Errorf("owner after a plain Update = %q (null: %v), want \"\" — bun writes the zero value, not NULL, for a field without ,nullzero", owner, isNull)
	}

	// And the tag that changes it. The same zero Owner, through owner,nullzero.
	if _, err := crud.Update(ctx, conn, &nullzeroUser{PersonalID: id, Name: "after", Projects: []string{}}, "user "+id); err != nil {
		t.Fatalf("Update through owner,nullzero: %v", err)
	}
	if err := conn.NewRaw("select owner is null from luima_test_users where personal_id = ?", id).Scan(ctx, &isNull); err != nil {
		t.Fatal(err)
	}
	if !isNull {
		t.Error("owner is not NULL after an Update through owner,nullzero — bun writes DEFAULT for a zero ,nullzero field, and owner has no default")
	}
}

// testNilSlice @notice Asserts a nil slice reaches Postgres as NULL, and that the 23502 it causes
// is redacted like any other driver error.
//
// @dev An insert go-pg accepted. go-pg wrote DEFAULT for a zero field without ,use_zero
// ((*InsertQuery).appendValues), so a nil Projects stored the column's '{}'. bun writes DEFAULT
// only for a nil pointer, or a zero field tagged ,nullzero or default:
// ((*InsertQuery).marshalsToDefault), so a nil ,array slice goes to pgdialect's
// appendStringSlice, which writes it as NULL — and projects is NOT NULL.
//
// A resolver fed a non-null list never gets here: gqlgen's generated unmarshaller builds the slice
// with make for a list such as the quickstart's projects: [String!]!, and returns nil only for a
// nullable one (codegen/type.gotpl). So it takes a model built in Go, or a nullable list argument
// mapped onto a NOT NULL column.
//
// luima does not classify it — a nil where the column needs a value is a bug in the caller's model,
// not something to tell a client — so what is pinned is the redaction, in both shapes PresentError
// is handed: bare, and wrapped the way graphql.AddError wraps every error before presenting it,
// with graphql.ErrorOnPath. That wrapper went out verbatim through 0.5.0.
//
// @param ctx  the query context
// @param t    the test handle
// @param conn the live pool
func testNilSlice(ctx context.Context, t *testing.T, conn *bun.DB) {
	t.Helper()

	const id = "nil-slice-1"
	t.Cleanup(func() {
		if _, err := crud.Delete(ctx, conn, &testUser{PersonalID: id}); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})

	got, err := crud.Create(ctx, conn, &testUser{PersonalID: id, Name: "nil", Projects: nil}, "user "+id)
	if err == nil {
		t.Fatalf("Create with a nil slice = %+v, want an error — bun writes nil as NULL and projects is NOT NULL", got)
	}
	if state := luimaerr.SQLState(err); state != "23502" {
		t.Errorf("SQLState(%v) = %q, want 23502 (not_null_violation)", err, state)
	}
	if msg := luimaerr.PresentError(ctx, err).Message; msg != "internal server error" {
		t.Errorf("a 23502 presented as %q, want \"internal server error\"", msg)
	}
	if msg := luimaerr.PresentError(ctx, graphql.ErrorOnPath(ctx, err)).Message; msg != "internal server error" {
		t.Errorf("a 23502 wrapped by graphql.ErrorOnPath presented as %q, want \"internal server error\"", msg)
	}
}
