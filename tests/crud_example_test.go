package tests

import (
	"context"
	"fmt"
	"log"

	"github.com/go-pg/pg/v10"
	"github.com/go-pg/pg/v10/orm"

	"github.com/ulas96/luima/crud"
	"github.com/ulas96/luima/luimaerr"
)

// User @notice The consumer's hand-written, autobound model.
//
// @dev Its go-pg tags are the schema: a pk is mandatory because Get, Update and Delete all call
// WherePK, and ,array is what keeps a []string from being encoded as JSON that a text[] column
// rejects.
type User struct {
	tableName  struct{} `pg:"app_users"` //nolint:unused // go-pg reads it by reflection
	PersonalID string   `pg:"personal_id,pk"`
	Name       string   `pg:"name"`
	Company    string   `pg:"company"`
	Projects   []string `pg:"projects,array"`
}

// ExampleCreate @notice The body of a createUser resolver.
//
// @dev The 23505 classification is what makes the conflict reach the client at all — returning a
// bare error would show the caller "internal server error".
func ExampleCreate() {
	var db *pg.DB // in a resolver this is r.DB
	ctx := context.Background()

	u := &User{PersonalID: "E-1042", Name: "Ada", Projects: []string{"apollo"}}

	created, err := crud.Create(ctx, db, u, "user "+u.PersonalID)
	if err != nil {
		// On a duplicate, err.Error() is "user E-1042 already exists".
		log.Fatal(err)
	}
	fmt.Println(created.PersonalID)
}

// ExampleList @notice The body of a users resolver.
//
// @dev Order your lists: Postgres gives no stable row order without ORDER BY, so an unordered
// List produces intermittently reordered GraphQL responses that look like a caching bug.
func ExampleList() {
	var db *pg.DB
	ctx := context.Background()

	users, err := crud.List[User](ctx, db, func(q *orm.Query) *orm.Query {
		return q.Order("personal_id")
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(len(users))
}

// ExampleList_filtered @notice Filtering and pagination through the same closure.
//
// @dev The closure is handed the whole go-pg query API, so this package ships no wrapper zoo of
// named options.
func ExampleList_filtered() {
	var db *pg.DB
	ctx := context.Background()

	users, err := crud.List[User](ctx, db, func(q *orm.Query) *orm.Query {
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
	var db *pg.DB
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
// @dev Update replaces every column, so an empty slice clears the array. There are no partial
// updates: send the whole object.
func ExampleUpdate() {
	var db *pg.DB
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
	var db *pg.DB
	ctx := context.Background()

	deleted, err := crud.Delete(ctx, db, &User{PersonalID: "E-1042"})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(deleted)
}

// ExampleDelete_scoped @notice Deleting only if the caller owns the row.
//
// @dev Get, Update and Delete take the same query modifiers List does, and this is what they are
// for. Without a second predicate the statement is WHERE personal_id = $1 alone, so any caller who
// can reach the port can delete any row — and the alternative was dropping to raw go-pg and
// hand-rolling the SQLSTATE classification these helpers exist to provide.
//
// A row that exists but is not the caller's comes back false, exactly as a row that does not exist
// does. That is the right answer to give an unauthorized caller: it discloses no existence.
func ExampleDelete_scoped() {
	var db *pg.DB
	ctx := context.Background()
	caller := "u-42" // from your auth middleware; see SECURITY.md

	deleted, err := crud.Delete(ctx, db, &User{PersonalID: "E-1042"}, func(q *orm.Query) *orm.Query {
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
// written anyway — and go-pg writes NULL, not the zero value, for any field without ,use_zero. Add
// a Role column, forget to touch the mapper, and every update clears it. q.Column narrows the SET
// clause to the named columns, which is the partial update the full replace otherwise rules out.
func ExampleUpdate_columns() {
	var db *pg.DB
	ctx := context.Background()

	updated, err := crud.Update(ctx, db, &User{PersonalID: "E-1042", Name: "Ada"}, "user E-1042",
		func(q *orm.Query) *orm.Query {
			return q.Column("name") // SET name = ? — company, projects and role untouched
		})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(updated.Name)
}

// ExampleCreate_transaction @notice Composing several helpers into one transaction.
//
// @dev Every helper takes orm.DB, which *pg.DB, *pg.Conn and *pg.Tx all satisfy — so passing the
// tx instead of the pool is the only change.
func ExampleCreate_transaction() {
	var db *pg.DB
	ctx := context.Background()

	err := db.RunInTransaction(ctx, func(tx *pg.Tx) error {
		if _, err := crud.Create(ctx, tx, &User{PersonalID: "E-1", Name: "Ada"}, "user E-1"); err != nil {
			return err
		}
		_, err := crud.Create(ctx, tx, &User{PersonalID: "E-2", Name: "Grace"}, "user E-2")
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
	var db *pg.DB
	ctx := context.Background()

	u := &User{PersonalID: "E-1042", Name: "Ada", Company: "no-such-company"}

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
