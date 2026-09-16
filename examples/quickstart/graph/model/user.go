package model

import "github.com/uptrace/bun"

// User @notice The hand-written, autobound model behind the GraphQL User type.
//
// @dev Three things here are load-bearing. The embedded bun.BaseModel names the table, the ,pk tag
// is mandatory because Get, Update and Delete all call WherePK, and ,array is what keeps Projects
// from being written as JSON, which a text[] column rejects with 22P02 — bun's appender table maps
// reflect.Slice to AppendJSONValue (schema/append_value.go), and it is that tag that swaps in
// pgdialect's array appender and scanner instead (dialect.go, onField). Every field is exported
// because bun and encoding/json read them by reflection, and gqlgen's generated code reads them
// from another package — it binds this type at codegen time and emits obj.PersonalID, not a
// reflect call.
//
// The table name is the one that fails quietly. bun reads it from an embedded bun.BaseModel and
// from nowhere else: it skips every unexported field that is not embedded without a word
// ((*schema.Table).processFields), so a model that names its table in an unexported `tableName`
// field — the spelling other Postgres mappers use — still compiles here and is ignored, and bun
// names the table after the type instead, underscored and pluralized: "users", not "app_users".
// Deleting the embedded line below does not break the build, only the queries.
//
// One more thing the tags decide: bun writes a nil ,array slice as NULL, not as DEFAULT, so a
// Projects left nil is 23502 against the README's `projects text[] not null default '{}'`. This
// model never sees one — projects is [String!]! in the schema, and gqlgen's unmarshaller for a
// non-null list is make([]string, len(vSlice)), non-nil even when the list is empty — so only a
// User built in Go, in a seed script or a test, can reach it.
//
// gqlgen.yml binds this type by autobind, so it is not regenerated — edit it freely.
type User struct {
	bun.BaseModel `bun:"table:app_users"`

	PersonalID string   `bun:"personal_id,pk"`
	Name       string   `bun:"name"`
	Company    string   `bun:"company"`
	Projects   []string `bun:"projects,array"`
}
