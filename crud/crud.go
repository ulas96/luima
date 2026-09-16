// Package crud @notice Five generic helpers for the bodies of bun-backed gqlgen resolvers.
//
// @dev The point of them is the error classification, not the query. Writing
// db.NewInsert().Model(u).Exec(ctx) was never hard; knowing that a duplicate key must become a
// *luimaerr.CustomError or [luimaerr.PresentError] will redact it to "internal server error" is
// the part that takes a production incident to learn.
//
// Every helper takes bun.IDB rather than *bun.DB, so *bun.DB, bun.Conn and bun.Tx all satisfy
// it — pass the tx inside db.RunInTx(...) and nothing else changes.
//
// Each helper's opts take its own statement's query: *bun.SelectQuery for Get and List, then
// *bun.InsertQuery, *bun.UpdateQuery and *bun.DeleteQuery — the closure type bun's own Apply
// methods take. bun builds each statement with its own type, and the builder interface some of
// them share, bun.QueryBuilder, covers only the WHERE clause of a select, update or delete — so
// one option type for all five would have to be a wrapper.
//
// Your model carries the schema in its bun tags, and four things about it are load-bearing:
//
//	type User struct {
//	    bun.BaseModel `bun:"table:app_users"` // names the table — see below
//
//	    PersonalID string   `bun:"personal_id,pk"` // a pk is mandatory: Get/Update/Delete call WherePK
//	    Name       string   `bun:"name"`           // exported: gqlgen, bun and encoding/json all read it
//	    Projects   []string `bun:"projects,array"` // ,array or bun writes it as JSON and text[] rejects it
//	}
//
// The table name is the one that fails quietly. bun reads it from an embedded bun.BaseModel, never
// from an ordinary field. It skips every unexported field that is not embedded, without a word — so
// a model ported from a mapper that named the table in an unexported field still compiles — and
// names the table after the type instead, underscored and pluralized: the User above without its
// BaseModel queries "users" ((*schema.Table).init and processFields). Where no such table exists
// every helper fails with a redacted error; where one does, the helpers can succeed against the
// wrong table.
package crud

import (
	"context"
	"database/sql"
	"errors"

	"github.com/uptrace/bun"

	"github.com/ulas96/luima/luimaerr"
)

// Get @notice Selects one row by primary key.
//
// @dev A missing row is (nil, nil), not an error, so a nullable GraphQL field renders as null.
// That translation exists because a select scanned into a single struct reports zero rows as
// sql.ErrNoRows — (*bun.SelectQuery).scanResult always passes hasDest, and (*baseQuery)._scan
// raises the error for a single-row model. Scanned into a slice it just comes back empty, which
// is why List never needs it.
//
// The opts are what make an ownership predicate expressible — see [Delete].
//
// @param ctx   the resolver context
// @param db    bun.IDB — *bun.DB, bun.Conn and bun.Tx all satisfy it
// @param key   a model with only its primary key populated; it is filled in and returned
// @param opts  query modifiers applied left to right, after WherePK
// @return *T   the stored row, or nil when no row matched
// @return error any driver error other than sql.ErrNoRows
func Get[T any](ctx context.Context, db bun.IDB, key *T, opts ...func(*bun.SelectQuery) *bun.SelectQuery) (*T, error) {
	q := db.NewSelect().Model(key).WherePK()
	for _, opt := range opts {
		q = opt(q)
	}
	if err := q.Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return key, nil
}

// List @notice Selects rows, applying each opt to the query in order.
//
//	crud.List[model.User](ctx, r.DB, func(q *bun.SelectQuery) *bun.SelectQuery {
//	    return q.Order("personal_id")
//	})
//
// @dev Do order your lists. Postgres gives no stable row order without ORDER BY, so an unordered
// List produces intermittently reordered GraphQL responses that look like a caching bug.
//
// One closure against bun's own documented query builder covers Where, Relation, Column, Limit,
// Offset and everything else, so the library ships no wrapper zoo of named options.
//
// Do bound them, too. Passing no options selects every row in the table, and that is a denial of
// service rather than a default: the rows are materialized into []*T, gqlgen marshals the whole
// response into memory, and the fasthttp adaptor buffers it once more before writing — three
// copies, no ceiling, reachable by anyone who can send `{ users { id } }`. ComplexityLimit does
// not help, because the row count is not an input to the complexity calculation; a list field
// costs the same whether it returns one row or ten million. Pagination is out of scope, so a
// q.Limit(n) in the resolver is what stands in for it.
//
// @param ctx   the resolver context
// @param db    bun.IDB — *bun.DB, bun.Conn and bun.Tx all satisfy it
// @param opts  query modifiers applied left to right; none means "select every row" — see above
// @return []*T the rows, never nil — an empty table yields an empty slice
// @return error any driver error
func List[T any](ctx context.Context, db bun.IDB, opts ...func(*bun.SelectQuery) *bun.SelectQuery) ([]*T, error) {
	// Seeded non-nil so an empty table marshals as [] rather than null, which a non-null
	// [T!]! field would reject. With no rows bun leaves the slice it was handed as it was
	// ((*sliceTableModel).ScanRows), so the seed is what comes back.
	rows := []*T{}
	q := db.NewSelect().Model(&rows)
	for _, opt := range opts {
		q = opt(q)
	}
	if err := q.Scan(ctx); err != nil {
		return nil, err
	}
	return rows, nil
}

// Create @notice Inserts m and returns the stored row.
//
// @dev A unique violation (23505) becomes a *CustomError, which is what makes it reach the
// client at all; every other driver error is returned bare and PresentError redacts it.
//
// Absence is the exception, and it is the one this used to get wrong — see below.
//
// RETURNING * is deliberate, and is the one place this differs from the server it was lifted
// from. That server could skip it after auditing its single table for defaults, triggers and
// generated columns. luima serves tables it has never seen: without RETURNING, a BEFORE INSERT
// trigger, or a column Postgres fills in, makes this answer with the value the client *sent*
// rather than the value Postgres *stored* — silently, forever, with a passing test suite. It is
// the same statement and the same round trip.
//
// RETURNING only reports what Postgres stored, though; whether Postgres fills a column in at all
// is the model's tag. bun writes a zero field as its value — the empty string, 0, FALSE, the
// zero time — and sends DEFAULT only for a nil pointer, or for a zero field tagged ,nullzero or
// default: ((*InsertQuery).marshalsToDefault). So a column with a DEFAULT, such as now(), wants
// ,nullzero or a pointer, or it stores the zero value. An identity or serial key wants
// ,autoincrement — ,identity alone still sends 0 — or the first Create stores 0 and the second
// fails with 23505, which reaches the client as a CONFLICT over a key it never chose, while a
// GENERATED ALWAYS identity refuses every Create with 428C9. A generated column wants ,scanonly,
// which keeps it out of the INSERT while RETURNING * still fills it in — or every Create is
// refused with 428C9. Both 428C9s are redacted. That tag reads back here and only here: a scanonly
// field is registered in the table's FieldMap, so a row that arrives with the column scans into
// it, but it never joins Fields ((*schema.Table).addField returns before that append), and [Get]
// and [List] select Fields rather than *, so through them the column stays zero.
//
// An insert the database declined to perform is absence, not an error: (nil, nil), the same
// convention as [Get]. Two things reach it. q.On("CONFLICT DO NOTHING") is the one the caller asks
// for — and the reason the opts exist here at all, since a suppressed conflict does not abort the
// surrounding transaction where a real 23505 does. That makes it the cheap way to attempt an
// insert inside one without risking the whole thing; the other is a savepoint, which a tx.RunInTx
// nested inside the transaction opens, for two more statements ((bun.Tx).RunInTx). A BEFORE
// INSERT trigger returning NULL is the one the caller does not ask for: it is the ordinary way to
// write a soft-ignore, and it needs no options.
//
// Both arrive as a row count of zero, not as an error: bun turns an empty result into
// sql.ErrNoRows only for Scan or for an Exec handed a destination, and this is an Exec without
// one. So there is one absence signal, and one check.
//
// Check the result. A caller who assumes non-nil nil-dereferences the second time the same key is
// inserted:
//
//	u, err := luima.Create(ctx, db, user, "user "+id, func(q *bun.InsertQuery) *bun.InsertQuery {
//	    return q.On("CONFLICT DO NOTHING")
//	})
//	if err != nil { return nil, err }
//	if u == nil { /* it was already there */ }
//
// @param ctx    the resolver context
// @param db     bun.IDB — *bun.DB, bun.Conn and bun.Tx all satisfy it
// @param m      the model to insert; it is overwritten with the stored row and returned
// @param label  names the thing in the conflict message — Create(ctx, db, u, "user "+id)
// yields "user E-1042 already exists"
// @param opts   query modifiers applied left to right, after Returning("*")
// @return *T    the stored row as RETURNING * read it back, or nil if the insert was suppressed
// @return error a *luimaerr.CustomError with Code "CONFLICT" on 23505, nil on a suppressed
// insert, the bare driver error otherwise
func Create[T any](ctx context.Context, db bun.IDB, m *T, label string, opts ...func(*bun.InsertQuery) *bun.InsertQuery) (*T, error) {
	q := db.NewInsert().Model(m).Returning("*")
	for _, opt := range opts {
		q = opt(q)
	}

	// Exec, not Scan. Both read the RETURNING row back into m, but (*InsertQuery).Exec passes
	// hasDest only when handed a dest, Scan passes it always, and (*baseQuery)._scan reports an
	// empty result as sql.ErrNoRows only when it is set. With Scan here a suppressed insert would
	// come back as sql.ErrNoRows, returned bare below — PresentError redacts it, and a mutation
	// that did exactly what it was told reads as a broken server.
	res, err := q.Exec(ctx)
	if err != nil {
		if luimaerr.SQLState(err) == "23505" { // unique_violation
			return nil, &luimaerr.CustomError{UserMessage: label + " already exists", InternalError: err, Code: "CONFLICT"}
		}
		return nil, err
	}

	// After the early return, never before it: res is nil whenever err is non-nil, so this check
	// written above the block is a nil dereference rather than a bug. It is the whole absence
	// check, with or without RETURNING: an option can only add to bun's RETURNING list, and one
	// that returns a query of its own without the clause reports the same zero either way — as a
	// plain exec's command tag, or, when bun sends any field as DEFAULT, from the RETURNING it adds
	// for those columns ((*InsertQuery).appendStructValues), scanned with no dest. The insert that
	// lets Postgres choose the key is always that second shape: ,autoincrement sets NullZero itself
	// ((*schema.Table).newField), so a zero key marshals to DEFAULT.
	n, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, nil
	}
	return m, nil
}

// Update @notice Replaces every column of the row with m's primary key, and returns the stored row.
//
// @dev A full replace: there are no partial updates. Deliberately without OmitZero — OmitZero
// skips zero-valued fields, so an empty string could not clear a text column, and false or 0 could
// never be written at all. That is not a partial-update feature, it is a silent data-retention
// bug. Real partial updates need nullable input fields and a Column allowlist, and are out of
// scope for v1.
//
// Each column gets what bun renders for its field. A zero string, number or bool is written as
// itself — an empty string, 0, FALSE — never as the column's default; a nil ,array slice as NULL,
// and an empty one as '{}', which clears the column; a nil pointer, or a zero field tagged
// ,nullzero, as DEFAULT, which is NULL only for a column that has no default of its own
// ((*schema.Field).appendValue, and pgdialect's appendStringSlice for the arrays).
//
// Absence has one spelling: RowsAffected() == 0, with no error, whether or not RETURNING * is in
// the statement — bun turns an empty result into sql.ErrNoRows only for Scan or for an Exec handed
// a destination, and this is an Exec without one. Checking for sql.ErrNoRows instead is a bug that
// stays invisible until someone updates a row that does not exist.
//
// See Create for why RETURNING * is here.
//
// The opts are the escape hatch from the full replace. q.Column(...) narrows the SET clause to
// the named columns, which is the partial update the full replace otherwise rules out — and it is
// the answer to the failure mode the full replace creates, where a column present on the struct
// but not set by your input mapper is overwritten on every update:
//
//	luima.Update(ctx, db, u, "user "+id, func(q *bun.UpdateQuery) *bun.UpdateQuery {
//	    return q.Column("name", "email") // SET name = ?, email = ? — nothing else touched
//	})
//
// The primary key stays out of SET even when it is named there ((*baseQuery).getDataFields).
// And a q.Where(...) is what scopes the update to rows the caller owns; see [Delete].
//
// @param ctx    the resolver context
// @param db     bun.IDB — *bun.DB, bun.Conn and bun.Tx all satisfy it
// @param m      the complete model, primary key included; every column is written
// @param label  names the thing in the not-found message
// @param opts   query modifiers applied left to right, after WherePK
// @return *T    the stored row
// @return error a *luimaerr.CustomError with Code "NOT_FOUND" when no row matched, the bare
// driver error otherwise
func Update[T any](ctx context.Context, db bun.IDB, m *T, label string, opts ...func(*bun.UpdateQuery) *bun.UpdateQuery) (*T, error) {
	q := db.NewUpdate().Model(m).WherePK().Returning("*")
	for _, opt := range opts {
		q = opt(q)
	}
	// Exec, not Scan, for the reason in Create: (*UpdateQuery).Scan would turn a row that is not
	// there into sql.ErrNoRows, returned bare and redacted instead of reaching the client as
	// NOT_FOUND.
	res, err := q.Exec(ctx)
	if err != nil {
		return nil, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, &luimaerr.CustomError{UserMessage: label + " not found", Code: "NOT_FOUND"}
	}
	return m, nil
}

// Delete @notice Removes the row with key's primary key, reporting whether one was there.
//
// @dev Nothing to classify: absence is false, not an error.
//
// The opts exist so authorization is expressible. luima ships no auth and does not intend to, but
// "no auth" and "you cannot write the WHERE clause auth needs" are different things — without a
// second predicate there is no way to say
//
//	DELETE FROM app_users WHERE personal_id = $1 AND owner_id = $2
//
// short of dropping to raw bun and hand-rolling the SQLSTATE classification that this package
// exists to provide. So the helpers' happy path was IDOR by construction:
//
//	luima.Delete(ctx, r.DB, &model.User{PersonalID: id}, func(q *bun.DeleteQuery) *bun.DeleteQuery {
//	    return q.Where("owner_id = ?", callerID(ctx))
//	})
//
// A row that exists but is not yours then reports as absent, which is the right answer to give an
// unauthorized caller anyway — it leaks no existence.
//
// The option types differ per statement, but the predicate need not be written three times: as a
// func(bun.QueryBuilder) bun.QueryBuilder it goes through ApplyQueryBuilder, which the select,
// update and delete queries all have.
//
// @param ctx    the resolver context
// @param db     bun.IDB — *bun.DB, bun.Conn and bun.Tx all satisfy it
// @param key    a model with only its primary key populated
// @param opts   query modifiers applied left to right, after WherePK
// @return bool  true when a row was deleted, false when none matched
// @return error any driver error
func Delete[T any](ctx context.Context, db bun.IDB, key *T, opts ...func(*bun.DeleteQuery) *bun.DeleteQuery) (bool, error) {
	q := db.NewDelete().Model(key).WherePK()
	for _, opt := range opts {
		q = opt(q)
	}
	res, err := q.Exec(ctx)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}
