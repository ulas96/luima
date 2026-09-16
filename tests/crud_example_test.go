package tests

import (
	"context"
	"fmt"
	"log"

	"github.com/uptrace/bun"

	"github.com/ulas96/luima/crud"
	"github.com/ulas96/luima/luimaerr"
)

// User @notice The consumer's hand-written, autobound model.
//
// @dev Its bun tags are the schema. The embedded bun.BaseModel names the table: bun's
// (*Table).processFields skips an unexported field that is not embedded, so an unexported
// table-name field is ignored and every query goes to users, the pluralized type name. A pk is
// mandatory because Get, Update and Delete all call WherePK, and ,array is what keeps a []string
// from being encoded as JSON that a text[] column rejects.
//
// Built in Go, Projects wants []string{}, not nil: pgdialect's appendStringSlice writes a nil slice
// as NULL, which the documented `projects text[] not null default '{}'` rejects with 23502 rather
// than filling in the default.
type User struct {
	bun.BaseModel `bun:"table:app_users"`

	PersonalID string   `bun:"personal_id,pk"`
	Name       string   `bun:"name"`
	Company    string   `bun:"company"`
	Projects   []string `bun:"projects,array"`
}

// ExampleCreate @notice The body of a createUser resolver.
//
// @dev The 23505 classification is what makes the conflict reach the client at all — returning a
// bare error would show the caller "internal server error".
func ExampleCreate() {
	var db *bun.DB // in a resolver this is r.DB
	ctx := context.Background()

	u := &User{PersonalID: "E-1042", Name: "Ada", Projects: []string{"apollo"}}

	created, err := crud.Create(ctx, db, u, "user "+u.PersonalID)
	if err != nil {
		// On a duplicate, err.Error() is "user E-1042 already exists".
		log.Fatal(err)
	}
	fmt.Println(created.PersonalID)
}

// ExampleCreate_suppressedConflict @notice Attempting an insert that may already be there.
//
// @dev The opts are an *bun.InsertQuery closure, and this is what they are for: a suppressed
// conflict does not abort the surrounding transaction, where a real 23505 does — so inside
// db.RunInTx this is the cheap way to attempt an insert without risking the whole thing.
//
// Check the result. An insert Postgres declined to perform is absence, not an error: (nil, nil),
// the same convention as Get, and reached the same way it is in Update — RowsAffected() == 0, with
// no error. A caller who assumes non-nil nil-dereferences the second time the same key is sent.
func ExampleCreate_suppressedConflict() {
	var db *bun.DB
	ctx := context.Background()

	u := &User{PersonalID: "E-1042", Name: "Ada", Projects: []string{}}

	created, err := crud.Create(ctx, db, u, "user "+u.PersonalID, func(q *bun.InsertQuery) *bun.InsertQuery {
		return q.On("CONFLICT DO NOTHING")
	})
	if err != nil {
		log.Fatal(err)
	}
	if created == nil {
		fmt.Println("already there")
		return
	}
	fmt.Println(created.PersonalID)
}

// ExampleList @notice The body of a users resolver.
//
// @dev Order your lists: Postgres gives no stable row order without ORDER BY, so an unordered
// List produces intermittently reordered GraphQL responses that look like a caching bug.
func ExampleList() {
	var db *bun.DB
	ctx := context.Background()

	users, err := crud.List[User](ctx, db, func(q *bun.SelectQuery) *bun.SelectQuery {
		return q.Order("personal_id")
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(len(users))
}

// ExampleList_filtered @notice Filtering and pagination through the same closure.
//
// @dev The closure is handed bun's whole *bun.SelectQuery, so this package ships no wrapper zoo of
// named options.
func ExampleList_filtered() {
	var db *bun.DB
	ctx := context.Background()

	users, err := crud.List[User](ctx, db, func(q *bun.SelectQuery) *bun.SelectQuery {
		return q.
			Where("company = ?", "Acme").
			Order("personal_id").
			Limit(20).
			Offset(40)
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(len(users))
}

// ExampleGet @notice The body of a user(personalId:) resolver.
//
// @dev Get returns (nil, nil) for a missing row, which is exactly what a nullable GraphQL field
// needs: the response is null rather than an error.
func ExampleGet() {
	var db *bun.DB
	ctx := context.Background()

	u, err := crud.Get(ctx, db, &User{PersonalID: "E-1042"})
	if err != nil {
		log.Fatal(err)
	}
	if u == nil {
		fmt.Println("no such user") // renders as null
		return
	}
	fmt.Println(u.Name)
}

// ExampleUpdate @notice A full-object replace.
//
// @dev Update replaces every column, so an empty slice clears the array. Send the whole object:
// there are no partial updates without opts — see ExampleUpdate_columns for the one that is.
func ExampleUpdate() {
	var db *bun.DB
	ctx := context.Background()

	u := &User{PersonalID: "E-1042", Name: "Grace", Company: "Acme", Projects: []string{}}

	updated, err := crud.Update(ctx, db, u, "user "+u.PersonalID)
	if err != nil {
		// When no row matched, err.Error() is "user E-1042 not found".
		log.Fatal(err)
	}
	fmt.Println(updated.Name)
}

// ExampleDelete @notice Deleting a row.
//
// @dev Absence is false, not an error.
func ExampleDelete() {
	var db *bun.DB
	ctx := context.Background()

	deleted, err := crud.Delete(ctx, db, &User{PersonalID: "E-1042"})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(deleted)
}

// ExampleDelete_scoped @notice Deleting only if the caller owns the row.
//
// @dev Get, Update and Delete take query modifiers just as List does, and this is what they are
// for. Without a second predicate the statement is WHERE personal_id = $1 alone, so any caller who
// can reach the port can delete any row — and the alternative was dropping to raw bun and
// hand-rolling the SQLSTATE classification these helpers exist to provide.
//
// A row that exists but is not the caller's comes back false, exactly as a row that does not exist
// does. That is the right answer to give an unauthorized caller: it discloses no existence.
func ExampleDelete_scoped() {
	var db *bun.DB
	ctx := context.Background()
	caller := "u-42" // from your auth middleware; see SECURITY.md

	deleted, err := crud.Delete(ctx, db, &User{PersonalID: "E-1042"}, func(q *bun.DeleteQuery) *bun.DeleteQuery {
		return q.Where("owner_id = ?", caller)
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(deleted)
}

// ExampleUpdate_columns @notice Writing only the columns you name.
//
// @dev Update is a full replace, so a column on the struct that your input mapper does not set is
// written anyway, as whatever the struct holds: bun's (*Field).appendValue writes a string the
// mapper never set as the empty string, and only a nil pointer or a zero field tagged ,nullzero as
// DEFAULT. Add a Role column, forget to touch the mapper, and every update blanks it — with no
// error, because the empty string satisfies NOT NULL. q.Column narrows the SET clause to the named
// columns, which is the partial update the full replace otherwise rules out.
func ExampleUpdate_columns() {
	var db *bun.DB
	ctx := context.Background()

	updated, err := crud.Update(ctx, db, &User{PersonalID: "E-1042", Name: "Ada"}, "user E-1042",
		func(q *bun.UpdateQuery) *bun.UpdateQuery {
			return q.Column("name") // SET name = ? — company, projects and role untouched
		})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(updated.Name)
}

// ExampleCreate_transaction @notice Composing several helpers into one transaction.
//
// @dev Every helper takes bun.IDB, which *bun.DB, bun.Conn and bun.Tx all satisfy — so passing the
// tx instead of the pool is the only change. RunInTx rolls back when the function returns an error,
// and returns that error.
func ExampleCreate_transaction() {
	var db *bun.DB
	ctx := context.Background()

	err := db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := crud.Create(ctx, tx, &User{PersonalID: "E-1", Name: "Ada", Projects: []string{}}, "user E-1"); err != nil {
			return err
		}
		_, err := crud.Create(ctx, tx, &User{PersonalID: "E-2", Name: "Grace", Projects: []string{}}, "user E-2")
		return err
	})
	if err != nil {
		log.Fatal(err)
	}
}

// ExampleCreate_customClassification @notice Classifying further when the default message is not
// the right one.
//
// @dev Anything that is not a *luimaerr.CustomError is redacted before it reaches the client, so
// a code luima does not classify has to be wrapped by the resolver to be heard.
func ExampleCreate_customClassification() {
	var db *bun.DB
	ctx := context.Background()

	u := &User{PersonalID: "E-1042", Name: "Ada", Company: "no-such-company", Projects: []string{}}

	created, err := crud.Create(ctx, db, u, "user "+u.PersonalID)
	if err != nil {
		if luimaerr.SQLState(err) == "23503" { // foreign_key_violation
			err = &luimaerr.CustomError{
				UserMessage:   "that company does not exist",
				InternalError: err,
			}
		}
		log.Fatal(err)
	}
	fmt.Println(created.PersonalID)
}
