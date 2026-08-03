// Package crud @notice Five generic helpers for the bodies of go-pg-backed gqlgen resolvers.
//
// @dev The point of them is the error classification, not the query. Writing
// db.ModelContext(ctx, u).Insert() was never hard; knowing that a duplicate key must become a
// *luimaerr.CustomError or [luimaerr.PresentError] will redact it to "internal server error" is
// the part that takes a production incident to learn.
//
// Every helper takes orm.DB rather than *pg.DB, so *pg.DB, *pg.Conn and *pg.Tx all satisfy it —
// pass the tx inside db.RunInTransaction(...) and nothing else changes.
//
// Your model carries the schema in its go-pg tags, and three things about it are load-bearing:
//
//	type User struct {
//	    tableName  struct{} `pg:"app_users"`
//	    PersonalID string   `pg:"personal_id,pk"`  // a pk is mandatory: Get/Update/Delete call WherePK
//	    Name       string   `pg:"name"`            // exported: gqlgen, go-pg and encoding/json all read it
//	    Projects   []string `pg:"projects,array"`  // ,array or go-pg encodes it as JSONB and text[] rejects it
//	}
package crud

import (
	"context"
	"errors"

	"github.com/go-pg/pg/v10"
	"github.com/go-pg/pg/v10/orm"

	"github.com/ulas96/luima/luimaerr"
)

// Get @notice Selects one row by primary key.
//
// @dev A missing row is (nil, nil), not an error, so a nullable GraphQL field renders as null.
// That translation exists because a single-row Select reports absence as pg.ErrNoRows — a list
// Select just comes back empty, which is why List never needs it.
//
// @param ctx   the resolver context
// @param db    orm.DB — *pg.DB, *pg.Conn and *pg.Tx all satisfy it
// @param key   a model with only its primary key populated; it is filled in and returned
// @return *T   the stored row, or nil when no row matched
// @return error any driver error other than pg.ErrNoRows
func Get[T any](ctx context.Context, db orm.DB, key *T) (*T, error) {
	if err := db.ModelContext(ctx, key).WherePK().Select(); err != nil {
		if errors.Is(err, pg.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return key, nil
}

// List @notice Selects rows, applying each opt to the query in order.
//
//	crud.List[model.User](ctx, r.DB, func(q *orm.Query) *orm.Query {
//	    return q.Order("personal_id")
//	})
//
// @dev Do order your lists. Postgres gives no stable row order without ORDER BY, so an unordered
// List produces intermittently reordered GraphQL responses that look like a caching bug.
//
// One closure against go-pg's own documented API covers Where, Relation, Column, Limit, Offset
// and everything else, so the library ships no wrapper zoo of named options.
//
// @param ctx   the resolver context
// @param db    orm.DB — *pg.DB, *pg.Conn and *pg.Tx all satisfy it
// @param opts  query modifiers applied left to right; none means "select every row"
// @return []*T the rows, never nil — an empty table yields an empty slice
// @return error any driver error
func List[T any](ctx context.Context, db orm.DB, opts ...func(*orm.Query) *orm.Query) ([]*T, error) {
	// Seeded non-nil so an empty table marshals as [] rather than null, which a non-null
	// [T!]! field would reject.
	rows := []*T{}
	q := db.ModelContext(ctx, &rows)
	for _, opt := range opts {
		q = opt(q)
	}
	if err := q.Select(); err != nil {
		return nil, err
	}
	return rows, nil
}

// Create @notice Inserts m and returns the stored row.
//
// @dev A unique violation (23505) becomes a *CustomError, which is what makes it reach the
// client at all; every other driver error is returned bare and PresentError redacts it.
//
// RETURNING * is deliberate, and is the one place this differs from the server it was lifted
// from. That server could skip it after auditing its single table for defaults, triggers and
// generated columns. luima serves tables it has never seen: without RETURNING, a table with a
// DEFAULT now(), a BEFORE INSERT trigger, an identity column or a generated column makes this
// answer with the value the client *sent* rather than the value Postgres *stored* — silently,
// forever, with a passing test suite. It is the same statement and the same round trip.
//
// @param ctx    the resolver context
// @param db     orm.DB — *pg.DB, *pg.Conn and *pg.Tx all satisfy it
// @param m      the model to insert; it is overwritten with the stored row and returned
// @param label  names the thing in the conflict message — Create(ctx, db, u, "user "+id)
// yields "user E-1042 already exists"
// @return *T    the stored row, with defaults, triggers and generated columns applied
// @return error a *luimaerr.CustomError on 23505, the bare driver error otherwise
func Create[T any](ctx context.Context, db orm.DB, m *T, label string) (*T, error) {
	if _, err := db.ModelContext(ctx, m).Returning("*").Insert(); err != nil {
		if luimaerr.SQLState(err) == "23505" { // unique_violation
			return nil, &luimaerr.CustomError{UserMessage: label + " already exists", InternalError: err}
		}
		return nil, err
	}
	return m, nil
}

// Update @notice Replaces every column of the row with m's primary key, and returns the stored row.
//
// @dev A full replace: there are no partial updates. Update, not UpdateNotZero — UpdateNotZero
// skips zero-valued fields, so an empty []string could not clear an array column and an empty
// string could not clear a text column. That is not a partial-update feature, it is a silent
// data-retention bug. Real partial updates need nullable input fields and a Column allowlist,
// and are out of scope for v1.
//
// Absence has two spellings here and which one you get depends on the RETURNING clause, so
// both are checked:
//
//   - A plain UPDATE *succeeds* with res.RowsAffected() == 0 when the WHERE matched nothing.
//     Checking only errors.Is(err, pg.ErrNoRows) is a bug that stays invisible until someone
//     updates a row that does not exist.
//   - With RETURNING *, go-pg is scanning a result set back into m, so zero rows comes back as
//     pg.ErrNoRows instead. Checking only RowsAffected() would let that surface as a redacted
//     "internal server error".
//
// See Create for why RETURNING * is here.
//
// @param ctx    the resolver context
// @param db     orm.DB — *pg.DB, *pg.Conn and *pg.Tx all satisfy it
// @param m      the complete model, primary key included; every column is written
// @param label  names the thing in the not-found message
// @return *T    the stored row
// @return error a *luimaerr.CustomError when no row matched, the bare driver error otherwise
func Update[T any](ctx context.Context, db orm.DB, m *T, label string) (*T, error) {
	res, err := db.ModelContext(ctx, m).WherePK().Returning("*").Update()
	if err != nil {
		if errors.Is(err, pg.ErrNoRows) {
			return nil, &luimaerr.CustomError{UserMessage: label + " not found"}
		}
		return nil, err
	}
	if res.RowsAffected() == 0 {
		return nil, &luimaerr.CustomError{UserMessage: label + " not found"}
	}
	return m, nil
}

// Delete @notice Removes the row with key's primary key, reporting whether one was there.
//
// @dev Nothing to classify: absence is false, not an error.
//
// @param ctx    the resolver context
// @param db     orm.DB — *pg.DB, *pg.Conn and *pg.Tx all satisfy it
// @param key    a model with only its primary key populated
// @return bool  true when a row was deleted, false when none matched
// @return error any driver error
func Delete[T any](ctx context.Context, db orm.DB, key *T) (bool, error) {
	res, err := db.ModelContext(ctx, key).WherePK().Delete()
	if err != nil {
		return false, err
	}
	return res.RowsAffected() > 0, nil
}
