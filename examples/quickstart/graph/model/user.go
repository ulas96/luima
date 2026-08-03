package model

// User @notice The hand-written, autobound model behind the GraphQL User type.
//
// @dev Three things here are load-bearing. tableName names the table, the ,pk tag is mandatory
// because Get, Update and Delete all call WherePK, and ,array is what keeps Projects from being
// encoded as JSONB that a text[] column rejects. Every field is exported because gqlgen, go-pg
// and encoding/json all read them by reflection.
//
// gqlgen.yml binds this type by autobind, so it is not regenerated — edit it freely.
type User struct {
	tableName  struct{} `pg:"app_users"`
	PersonalID string   `pg:"personal_id,pk"`
	Name       string   `pg:"name"`
	Company    string   `pg:"company"`
	Projects   []string `pg:"projects,array"`
}
