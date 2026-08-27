package luimagen

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTableFromFields(t *testing.T) {
	tbl, err := tableFromFields("User", []Field{
		{Name: "PersonalID", Type: "string", PK: true},
		{Name: "Name", Type: "string"},
		{Name: "Projects", Type: "[]string"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if tbl.pk.field != "PersonalID" || tbl.pk.sql != "personal_id" || tbl.pk.gql != "String" {
		t.Errorf("pk = %+v", tbl.pk)
	}
	if len(tbl.cols) != 2 {
		t.Fatalf("cols = %+v", tbl.cols)
	}
	if tbl.cols[1].field != "Projects" || !tbl.cols[1].array || tbl.cols[1].goType != "[]string" {
		t.Errorf("Projects column = %+v", tbl.cols[1])
	}
}

func TestTableFromFieldsRejectsTwoOrZeroPKs(t *testing.T) {
	if _, err := tableFromFields("User", []Field{{Name: "A", Type: "string"}}); err == nil {
		t.Error("expected an error with no PK field")
	}
	if _, err := tableFromFields("User", []Field{
		{Name: "A", Type: "string", PK: true},
		{Name: "B", Type: "string", PK: true},
	}); err == nil {
		t.Error("expected an error with two PK fields")
	}
}

func TestTableFromFieldsRejectsArrayPK(t *testing.T) {
	if _, err := tableFromFields("Weird", []Field{
		{Name: "Tags", Type: "[]string", PK: true},
		{Name: "Name", Type: "string"},
	}); err == nil {
		t.Error("expected an error for an array-typed PK field")
	}
}

func TestTableFromFieldsRejectsPKOnly(t *testing.T) {
	if _, err := tableFromFields("Note", []Field{{Name: "Body", Type: "string", PK: true}}); err == nil {
		t.Error("expected an error for a table with no non-PK fields")
	}
}

func TestSnakeCase(t *testing.T) {
	for in, want := range map[string]string{
		"PersonalID": "personal_id",
		"Name":       "name",
		"ID":         "id",
		"Company":    "company",
		"URLValue":   "url_value",
		"HTTPStatus": "http_status",
		"HTTPServer": "http_server",
	} {
		if got := snakeCase(in); got != want {
			t.Errorf("snakeCase(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLowerFirst(t *testing.T) {
	for in, want := range map[string]string{
		"PersonalID": "personalId",
		"Name":       "name",
		"ID":         "id",
		"Company":    "company",
	} {
		if got := lowerFirst(in); got != want {
			t.Errorf("lowerFirst(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTableFromFieldsAcceptsRoundTripScalars(t *testing.T) {
	tbl, err := tableFromFields("Product", []Field{
		{Name: "SKU", Type: "int", PK: true},
		{Name: "Price", Type: "float64"},
		{Name: "InStock", Type: "bool"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if tbl.pk.gql != "Int" || tbl.cols[0].gql != "Float" || tbl.cols[1].gql != "Boolean" {
		t.Errorf("resolved scalars = %q %q %q", tbl.pk.gql, tbl.cols[0].gql, tbl.cols[1].gql)
	}
}

// TestTableFromFieldsRejectsNonRoundTripTypes pins the type-compatibility finding: gqlgen's
// default bindings turn GraphQL Int into Go int and Float into float64, so any other width
// would generate resolver code that does not compile — reject it before any file is written.
func TestTableFromFieldsRejectsNonRoundTripTypes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fields []Field
	}{
		{"int64 PK", []Field{{Name: "ID", Type: "int64", PK: true}, {Name: "Name", Type: "string"}}},
		{"int64 column", []Field{{Name: "ID", Type: "string", PK: true}, {Name: "N", Type: "int64"}}},
		{"int32 column", []Field{{Name: "ID", Type: "string", PK: true}, {Name: "N", Type: "int32"}}},
		{"uint column", []Field{{Name: "ID", Type: "string", PK: true}, {Name: "N", Type: "uint"}}},
		{"float32 column", []Field{{Name: "ID", Type: "string", PK: true}, {Name: "N", Type: "float32"}}},
		{"[]int64 column", []Field{{Name: "ID", Type: "string", PK: true}, {Name: "Ns", Type: "[]int64"}}},
	} {
		if _, err := tableFromFields("T", tc.fields); err == nil {
			t.Errorf("%s: expected an error for a type that does not round-trip through gqlgen's bindings", tc.name)
		}
	}
}

func TestSDLMatchesTheQuickstartShape(t *testing.T) {
	tbl := &modelTable{
		typeName: "User",
		pk:       column{field: "PersonalID", sql: "personal_id", gql: "String"},
		cols:     []column{{field: "Name", sql: "name", gql: "String"}},
	}
	out := sdl(tbl, true, true)
	for _, want := range []string{
		"type User {\n  personalId: String!\n  name: String!\n}",
		"input UserInput {\n  name: String!\n}",
		"type Query {\n  user(personalId: String!): User\n  users: [User!]!\n}",
		"type Mutation {\n  createUser(personalId: String!, input: UserInput!): User!",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("sdl() missing %q, got:\n%s", want, out)
		}
	}

	ext := sdl(tbl, false, false)
	if !strings.Contains(ext, "extend type Query {") || !strings.Contains(ext, "extend type Mutation {") {
		t.Errorf("sdl(false, false) did not extend, got:\n%s", ext)
	}
}

func TestModelSource(t *testing.T) {
	tbl := &modelTable{
		typeName: "User",
		pk:       column{field: "PersonalID", goType: "string", sql: "personal_id"},
		cols: []column{
			{field: "Name", goType: "string", sql: "name"},
			{field: "Projects", goType: "[]string", sql: "projects", array: true},
		},
	}
	src, err := modelSource("model", tbl, "app_users")
	if err != nil {
		t.Fatal(err)
	}
	out := string(src)
	// format.Source column-aligns adjacent struct fields (e.g. "tableName  struct{}" gets an
	// extra space to line up with the longer "PersonalID string"), so compare against
	// whitespace-normalized output rather than hardcoding a specific gofmt column width.
	norm := strings.Join(strings.Fields(out), " ")
	for _, want := range []string{
		"package model",
		"type User struct {",
		"tableName struct{} `pg:\"app_users\"`",
		"PersonalID string `pg:\"personal_id,pk\"`",
		"Projects []string `pg:\"projects,array\"`",
	} {
		if !strings.Contains(norm, want) {
			t.Errorf("modelSource() missing %q, got:\n%s", want, out)
		}
	}
}

func TestPatchSourceFillsAllFiveAndIsIdempotent(t *testing.T) {
	tbl := &modelTable{
		typeName: "User",
		pk:       column{field: "PersonalID", goType: "string", sql: "personal_id", gql: "String"},
		cols:     []column{{field: "Name", sql: "name", gql: "String"}},
	}

	src := []byte(`package graph

import (
	"context"
	"fmt"

	"github.com/ulas96/luima/examples/quickstart/graph/generated"
	"github.com/ulas96/luima/examples/quickstart/graph/model"
)

func (r *mutationResolver) CreateUser(ctx context.Context, personalID string, input model.UserInput) (*model.User, error) {
	panic(fmt.Errorf("not implemented: CreateUser - createUser"))
}

func (r *mutationResolver) UpdateUser(ctx context.Context, personalID string, input model.UserInput) (*model.User, error) {
	panic(fmt.Errorf("not implemented: UpdateUser - updateUser"))
}

func (r *mutationResolver) DeleteUser(ctx context.Context, personalID string) (bool, error) {
	panic(fmt.Errorf("not implemented: DeleteUser - deleteUser"))
}

func (r *queryResolver) Users(ctx context.Context) ([]*model.User, error) {
	panic(fmt.Errorf("not implemented: Users - users"))
}

func (r *queryResolver) User(ctx context.Context, personalID string) (*model.User, error) {
	panic(fmt.Errorf("not implemented: User - user"))
}
`)

	out, changed, err := patchSource(src, tbl, "model", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected changed = true on first pass")
	}
	text := string(out)
	for _, want := range []string{
		`luima.Get(ctx, r.DB, &model.User{PersonalID: personalID})`,
		`luima.List[model.User](ctx, r.DB, func(q *orm.Query) *orm.Query {`,
		`return luima.Create(ctx, r.DB, &model.User{PersonalID: personalID, Name: input.Name}, "user "+personalID)`,
		`return luima.Delete(ctx, r.DB, &model.User{PersonalID: personalID})`,
		`"github.com/ulas96/luima"`,
		`"github.com/go-pg/pg/v10/orm"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("patched source missing %q, got:\n%s", want, text)
		}
	}
	// Every stub in this fixture uses fmt.Errorf, and patching replaces all five, so fmt is
	// dead afterward — format.Source doesn't catch an unused import (that's a build-time check,
	// not a formatting one), so this has to be asserted explicitly.
	if strings.Contains(text, `"fmt"`) {
		t.Errorf("patched source still imports \"fmt\" after every fmt.Errorf stub was replaced, got:\n%s", text)
	}

	// The import block must be goimports-canonical: the stdlib group first, a blank line, then
	// the third-party group sorted by path — the two added imports inserted into their right
	// places, not textually prepended.
	wantImports := `import (
	"context"

	"github.com/go-pg/pg/v10/orm"
	"github.com/ulas96/luima"
	"github.com/ulas96/luima/examples/quickstart/graph/generated"
	"github.com/ulas96/luima/examples/quickstart/graph/model"
)`
	if !strings.Contains(text, wantImports) {
		t.Errorf("import block is not sorted and grouped, got:\n%s", text)
	}

	if _, changed, err = patchSource(out, tbl, "model", nil); err != nil {
		t.Fatal(err)
	} else if changed {
		t.Error("second pass over already-patched source should find no stubs left")
	}
}

func TestPatchSourcePrunesFmtByASTNotByText(t *testing.T) {
	tbl := &modelTable{
		typeName: "User",
		pk:       column{field: "PersonalID", goType: "string", sql: "personal_id", gql: "String"},
		cols:     []column{{field: "Name", sql: "name", gql: "String"}},
	}

	src := []byte(`package graph

import (
	"context"
	"fmt"
)

// This comment mentions fmt.Errorf but is not code — it must not keep the import alive.
func (r *queryResolver) User(ctx context.Context, personalID string) (*model.User, error) {
	panic(fmt.Errorf("not implemented: User - user"))
}
`)

	out, changed, err := patchSource(src, tbl, "model", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected changed = true")
	}
	text := string(out)
	// The textual pruneUnusedFmt this replaces checked for "fmt." anywhere in the file, so this
	// comment alone would have kept a dead import and broken the consumer's build.
	if strings.Contains(text, `"fmt"`) {
		t.Errorf("fmt import survived a comment mentioning fmt.Errorf, got:\n%s", text)
	}
	if !strings.Contains(text, "luima.Get(ctx, r.DB, &model.User{PersonalID: personalID})") {
		t.Errorf("stub was not filled, got:\n%s", text)
	}
}

func TestPatchSourceNonStringPKUsesFmtLabel(t *testing.T) {
	tbl := &modelTable{
		typeName: "Counter",
		pk:       column{field: "ID", goType: "int", sql: "id", gql: "Int"},
		cols:     []column{{field: "Name", sql: "name", gql: "String"}},
	}

	src := []byte(`package graph

import (
	"context"
	"fmt"
)

func (r *mutationResolver) CreateCounter(ctx context.Context, id int, input model.CounterInput) (*model.Counter, error) {
	panic(fmt.Errorf("not implemented: CreateCounter - createCounter"))
}

func (r *queryResolver) Counter(ctx context.Context, id int) (*model.Counter, error) {
	panic(fmt.Errorf("not implemented: Counter - counter"))
}
`)

	out, changed, err := patchSource(src, tbl, "model", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected changed = true")
	}
	text := string(out)
	// A non-string PK cannot ride the "counter "+id concat — that does not compile. The label
	// goes through fmt.Sprintf instead, and the fmt import must survive for it.
	for _, want := range []string{
		`return luima.Create(ctx, r.DB, &model.Counter{ID: id, Name: input.Name}, fmt.Sprintf("counter %v", id))`,
		`luima.Get(ctx, r.DB, &model.Counter{ID: id})`,
		`"fmt"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("patched source missing %q, got:\n%s", want, text)
		}
	}
	if strings.Contains(text, `"counter "+id`) {
		t.Errorf("patched source still concatenates the non-string PK, got:\n%s", text)
	}
	// The fixture patches only Create and Get — no List, so no spliced body references orm and
	// the import must not be added (see TestPatchSourceSkipsOrmWhenListIsHandWritten for the
	// rule), while fmt survives for the label and luima for every body.
	wantImports := "import (\n\t\"context\"\n\t\"fmt\"\n\n\t\"github.com/ulas96/luima\"\n)"
	if !strings.Contains(text, wantImports) {
		t.Errorf("import block not canonical, got:\n%s", text)
	}
}

// TestPatchSourceSkipsOrmWhenListIsHandWritten pins the import-fix rule for a pre-filled List:
// gqlgen preserves a non-stub body on its next generate, so a hand-written Users survives while
// the other four stubs get spliced. None of the four spliced bodies references orm — only List's
// q.Order closure does — so the import fixer must not add it, or the consumer's build breaks on
// an unused import.
func TestPatchSourceSkipsOrmWhenListIsHandWritten(t *testing.T) {
	tbl := &modelTable{
		typeName: "User",
		pk:       column{field: "PersonalID", goType: "string", sql: "personal_id", gql: "String"},
		cols:     []column{{field: "Name", sql: "name", gql: "String"}},
	}

	src := []byte(`package graph

import (
	"context"
	"fmt"
)

func (r *mutationResolver) CreateUser(ctx context.Context, personalID string, input model.UserInput) (*model.User, error) {
	panic(fmt.Errorf("not implemented: CreateUser - createUser"))
}

func (r *mutationResolver) UpdateUser(ctx context.Context, personalID string, input model.UserInput) (*model.User, error) {
	panic(fmt.Errorf("not implemented: UpdateUser - updateUser"))
}

func (r *mutationResolver) DeleteUser(ctx context.Context, personalID string) (bool, error) {
	panic(fmt.Errorf("not implemented: DeleteUser - deleteUser"))
}

func (r *queryResolver) Users(ctx context.Context) ([]*model.User, error) {
	return []*model.User{}, nil
}

func (r *queryResolver) User(ctx context.Context, personalID string) (*model.User, error) {
	panic(fmt.Errorf("not implemented: User - user"))
}
`)

	out, changed, err := patchSource(src, tbl, "model", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected changed = true with four stubs left to patch")
	}
	text := string(out)
	for _, want := range []string{
		`luima.Get(ctx, r.DB, &model.User{PersonalID: personalID})`,
		`return luima.Create(ctx, r.DB, &model.User{PersonalID: personalID, Name: input.Name}, "user "+personalID)`,
		`"github.com/ulas96/luima"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("patched source missing %q, got:\n%s", want, text)
		}
	}
	// The hand-written Users body survives untouched and no spliced body mentions orm, so neither
	// the identifier nor the import may appear anywhere.
	if strings.Contains(text, "orm") {
		t.Errorf("patched source references orm although List was left hand-written, got:\n%s", text)
	}
	// All four remaining fmt.Errorf stubs were spliced and nothing else uses fmt, so the import
	// must be gone too — same rule as TestPatchSourceFillsAllFiveAndIsIdempotent.
	if strings.Contains(text, `"fmt"`) {
		t.Errorf("patched source still imports \"fmt\" after every fmt.Errorf stub was replaced, got:\n%s", text)
	}
}

func TestTableFromFieldsRejectsBadIdentifiers(t *testing.T) {
	for _, tc := range []struct {
		name   string
		typ    string
		fields []Field
	}{
		{"unexported field", "User", []Field{{Name: "id", Type: "string", PK: true}, {Name: "Name", Type: "string"}}},
		{"unexported type", "user", []Field{{Name: "ID", Type: "string", PK: true}, {Name: "Name", Type: "string"}}},
		{"spaced field", "User", []Field{{Name: "ID", Type: "string", PK: true}, {Name: "Full Name", Type: "string"}}},
		{"hyphenated field", "User", []Field{{Name: "ID", Type: "string", PK: true}, {Name: "Full-Name", Type: "string"}}},
		// '_' is a legal Go identifier rune, so token.IsIdentifier passes it — but templates.ToGo
		// treats it as a word delimiter and drops it, so Owner_Name comes back as OwnerName and
		// the spliced body references input.Owner_Name, which does not exist. Measured: the run
		// exits 0 and `go build ./...` then fails in the consumer's module.
		{"underscored field", "User", []Field{{Name: "ID", Type: "string", PK: true}, {Name: "Owner_Name", Type: "string"}}},
		{"underscored type", "My_Type", []Field{{Name: "ID", Type: "string", PK: true}, {Name: "Name", Type: "string"}}},
	} {
		if _, err := tableFromFields(tc.typ, tc.fields); err == nil {
			t.Errorf("%s: expected an error — an unexported or non-identifier name generates a struct field gqlgen cannot bind and code that does not compile", tc.name)
		}
	}
}

// TestLowerFirstNonASCII pins the rune fix: slicing s[:1] splits a multi-byte leading rune
// mid-sequence, and the U+FFFD that results is an SDL name gqlgen rejects — after the model file
// is already on disk.
func TestLowerFirstNonASCII(t *testing.T) {
	if got := lowerFirst("Ünvan"); got != "ünvan" {
		t.Errorf("lowerFirst(%q) = %q, want %q", "Ünvan", got, "ünvan")
	}
}

// TestPlanSDLDeclareProbeReadsDeclarations pins the declare-vs-extend probe against the shapes a
// text search gets wrong: `type MutationResponse` (a common convention) is not a Mutation, a name
// inside a comment is not a declaration, and `extend type Query` is not one either — a fragment
// that declares a second `type Query`, or extends a base type nobody declared, is a gqlparser
// error at stage 3 with the model file and the fragment already on disk.
func TestPlanSDLDeclareProbeReadsDeclarations(t *testing.T) {
	for _, tc := range []struct {
		name    string
		schema  string
		extends bool // want `extend type Query`, i.e. the probe saw a declaration
	}{
		{"declared", "type Query {\n  noop: Boolean!\n}\n", true},
		{"prefix only", "type QueryResult {\n  ok: Boolean!\n}\n", false},
		{"comment only", "# type Query is declared elsewhere\nscalar Void\n", false},
		{"extension only", "extend type Query {\n  a: Int!\n}\n", false},
		{"directive on the declaration", "directive @dir on OBJECT\ntype Query @dir {\n  a: Int!\n}\n", true},
		{"description block", "\"\"\"\ntype Query is the root of every read path\n\"\"\"\nscalar Void\n", false},
	} {
		frag, err := planSDL(schemaFile(t, tc.schema), testTable())
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := strings.Contains(frag, "extend type Query {"); got != tc.extends {
			t.Errorf("%s: extend type Query = %v, want %v, got:\n%s", tc.name, got, tc.extends, frag)
		}
	}
}

func schemaFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "schema.graphqls")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func testTable() *modelTable {
	return &modelTable{
		typeName: "User",
		pk:       column{field: "PersonalID", goType: "string", sql: "personal_id", gql: "String"},
		cols:     []column{{field: "Name", goType: "string", sql: "name", gql: "String"}},
	}
}

// TestPlanSDLDeclaresThenExtends covers the declare-vs-extend decision end to end — the branch
// TestSDLMatchesTheQuickstartShape only exercises by passing the booleans in by hand.
func TestPlanSDLDeclaresThenExtends(t *testing.T) {
	path := schemaFile(t, "scalar Void\n")

	frag, err := planSDL(path, testTable())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(frag, "type Query {") || strings.Contains(frag, "extend type Query {") {
		t.Errorf("first table should declare, got:\n%s", frag)
	}
	if err := appendSDL(path, frag); err != nil {
		t.Fatal(err)
	}

	second := testTable()
	second.typeName = "Note"
	frag, err = planSDL(path, second)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(frag, "extend type Query {") || !strings.Contains(frag, "extend type Mutation {") {
		t.Errorf("second table should extend, got:\n%s", frag)
	}
}

func TestPlanSDLDeclaresMutationAlongsideMutationResponse(t *testing.T) {
	path := schemaFile(t, "type MutationResponse {\n  ok: Boolean!\n}\n")
	frag, err := planSDL(path, testTable())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(frag, "extend type Mutation {") {
		t.Errorf("`type MutationResponse` is not a Mutation declaration — extending it produces a schema gqlparser rejects, got:\n%s", frag)
	}
}

func TestPlanSDLRefusesDuplicate(t *testing.T) {
	for _, existing := range []string{"type User {\n  id: ID!\n}\n", "extend type User {\n  id: ID!\n}\n"} {
		if _, err := planSDL(schemaFile(t, existing), testTable()); err == nil {
			t.Errorf("expected a duplicate error for a schema containing %q", existing)
		}
	}
}

func TestWriteModelRefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	if err := writeModel(dir, "model", testTable(), "users"); err != nil {
		t.Fatal(err)
	}
	if err := writeModel(dir, "model", testTable(), "users"); err == nil {
		t.Error("expected an error rather than clobbering a model file that may have been hand-edited")
	}
}

// TestGenerateValidatesSchemaBeforeWritingModel pins the ordering: a schema that already declares
// the type must fail before the model file exists, or the consumer's model package ends up with
// two declarations of the same struct and stops building.
func TestGenerateValidatesSchemaBeforeWritingModel(t *testing.T) {
	dir := t.TempDir()
	modelDir := filepath.Join(dir, "model")
	if err := os.Mkdir(modelDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "schema.graphqls")
	if err := os.WriteFile(path, []byte("type User {\n  id: ID!\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := Generate(Options{
		Type:       "User",
		Fields:     []Field{{Name: "PersonalID", Type: "string", PK: true}, {Name: "Name", Type: "string"}},
		ModelDir:   modelDir,
		SchemaFile: path,
	})
	if err == nil {
		t.Fatal("expected a duplicate-type error")
	}
	if _, statErr := os.Stat(filepath.Join(modelDir, "user.go")); statErr == nil {
		t.Error("model file was written despite the schema check failing — it now redeclares User in the consumer's model package")
	}
}

func TestWithDefaults(t *testing.T) {
	got := Options{Type: "User"}.withDefaults()
	for _, tc := range [][2]string{
		{got.Table, "users"},
		{got.Dir, "."},
		{got.ModelDir, "graph/model"},
		{got.SchemaFile, "graph/schema.graphqls"},
		{got.ResolverFile, "graph/schema.resolvers.go"},
		{got.ModelPkg, "model"},
	} {
		if tc[0] != tc[1] {
			t.Errorf("default = %q, want %q", tc[0], tc[1])
		}
	}
	// snakeCase runs before the "s", so a multi-word type pluralizes its last word only.
	if got := (Options{Type: "APIKey"}).withDefaults().Table; got != "api_keys" {
		t.Errorf("default table for APIKey = %q, want %q", got, "api_keys")
	}
	// An explicit Table wins — the quickstart's app_users is what no default could produce.
	if got := (Options{Type: "User", Table: "app_users"}).withDefaults().Table; got != "app_users" {
		t.Errorf("explicit table = %q, want app_users", got)
	}
}

func TestInputFieldNames(t *testing.T) {
	dir := t.TempDir()
	// gqlgen names the file through model.filename in gqlgen.yml, so the lookup globs the whole
	// directory rather than assuming models_gen.go.
	if err := os.WriteFile(filepath.Join(dir, "user.go"), []byte("package model\n\ntype User struct{ OwnerId string }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "generated_models.go"), []byte("package model\n\ntype UserInput struct {\n\tOwnerID    string\n\tProfileURL string\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := inputFieldNames(dir, "UserInput")
	if err != nil {
		t.Fatal(err)
	}
	if got["ownerid"] != "OwnerID" || got["profileurl"] != "ProfileURL" {
		t.Errorf("inputFieldNames = %v", got)
	}
	if _, err := inputFieldNames(dir, "NoteInput"); err == nil {
		t.Error("expected an error when the generated input struct is nowhere in the model directory")
	}
}

// TestPatchSourceUsesGeneratedNames is the regression test for the whole "predict vs read" fix.
// gqlgen runs SDL names through templates.ToGo, which re-capitalizes common initialisms: the SDL
// field profileUrl becomes ProfileURL on the generated input, and the type URL's SDL query field
// uRL becomes the method URl. Both differ from what luimagen would have predicted, and both must
// still produce code that compiles.
func TestPatchSourceUsesGeneratedNames(t *testing.T) {
	tbl := &modelTable{
		typeName: "URL",
		pk:       column{field: "OwnerId", goType: "string", sql: "owner_id", gql: "String"},
		cols:     []column{{field: "ProfileUrl", goType: "string", sql: "profile_url", gql: "String"}},
	}
	inputFields := map[string]string{"profileurl": "ProfileURL"}

	src := []byte(`package graph

import (
	"context"
	"fmt"
)

func (r *mutationResolver) CreateURL(ctx context.Context, ownerID string, input model.URLInput) (*model.URL, error) {
	panic(fmt.Errorf("not implemented: CreateURL - createURL"))
}

func (r *mutationResolver) UpdateURL(ctx context.Context, ownerID string, input model.URLInput) (*model.URL, error) {
	panic(fmt.Errorf("not implemented: UpdateURL - updateURL"))
}

func (r *mutationResolver) DeleteURL(ctx context.Context, ownerID string) (bool, error) {
	panic(fmt.Errorf("not implemented: DeleteURL - deleteURL"))
}

func (r *queryResolver) URLs(ctx context.Context) ([]*model.URL, error) {
	panic(fmt.Errorf("not implemented: URLs - uRLs"))
}

func (r *queryResolver) URl(ctx context.Context, ownerID string) (*model.URL, error) {
	panic(fmt.Errorf("not implemented: URl - uRL"))
}
`)

	out, changed, err := patchSource(src, tbl, "model", inputFields)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected changed = true")
	}
	text := string(out)
	for _, want := range []string{
		// URl matched URL case-insensitively — an exact match would have skipped it and left the
		// stub behind.
		`return luima.Get(ctx, r.DB, &model.URL{OwnerId: ownerID})`,
		// The key is luimagen's own model field; the value is the name gqlgen actually generated.
		`ProfileUrl: input.ProfileURL`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("patched source missing %q, got:\n%s", want, text)
		}
	}
	if strings.Contains(text, "input.ProfileUrl") {
		t.Errorf("patched source predicted the input field name instead of reading it, got:\n%s", text)
	}
	if strings.Contains(text, "not implemented") {
		t.Errorf("a stub survived the patch, got:\n%s", text)
	}
}

// TestCheckCompleteCatchesAPartialPatch pins the partial-patch check: a half-patched file still
// compiles, so a surviving panic("not implemented") only fires on the first query that reaches it.
func TestCheckCompleteCatchesAPartialPatch(t *testing.T) {
	src := []byte(`package graph

import (
	"context"
	"fmt"
)

func (r *queryResolver) User(ctx context.Context, personalID string) (*model.User, error) {
	panic(fmt.Errorf("not implemented: User - user"))
}
`)
	if err := checkComplete(src, testTable()); err == nil {
		t.Error("expected an error naming the four methods gqlgen did not generate")
	}
	// A file with all five present passes, whether or not they are still stubs.
	full, _, err := patchSource(fullStubFile, testTable(), "model", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkComplete(full, testTable()); err != nil {
		t.Errorf("a fully patched file should be complete: %v", err)
	}
}

// fullStubFile is gqlgen's output for the quickstart's User: all five stubs, nothing else.
var fullStubFile = []byte(`package graph

import (
	"context"
	"fmt"
)

func (r *mutationResolver) CreateUser(ctx context.Context, personalID string, input model.UserInput) (*model.User, error) {
	panic(fmt.Errorf("not implemented: CreateUser - createUser"))
}

func (r *mutationResolver) UpdateUser(ctx context.Context, personalID string, input model.UserInput) (*model.User, error) {
	panic(fmt.Errorf("not implemented: UpdateUser - updateUser"))
}

func (r *mutationResolver) DeleteUser(ctx context.Context, personalID string) (bool, error) {
	panic(fmt.Errorf("not implemented: DeleteUser - deleteUser"))
}

func (r *queryResolver) Users(ctx context.Context) ([]*model.User, error) {
	panic(fmt.Errorf("not implemented: Users - users"))
}

func (r *queryResolver) User(ctx context.Context, personalID string) (*model.User, error) {
	panic(fmt.Errorf("not implemented: User - user"))
}
`)

// TestIsStubLeavesHandWrittenPanicAlone is the "a method already filled in by hand is left alone"
// guarantee. Overwriting a deliberate placeholder with an unscoped luima.Delete is silent damage —
// isStub must look at the panic argument, not just at the fact that the body is a panic.
func TestIsStubLeavesHandWrittenPanicAlone(t *testing.T) {
	tbl := testTable()
	src := []byte(`package graph

import (
	"context"
	"fmt"
)

func (r *mutationResolver) CreateUser(ctx context.Context, personalID string, input model.UserInput) (*model.User, error) {
	panic(fmt.Errorf("not implemented: CreateUser - createUser"))
}

func (r *mutationResolver) UpdateUser(ctx context.Context, personalID string, input model.UserInput) (*model.User, error) {
	panic(fmt.Errorf("not implemented: UpdateUser - updateUser"))
}

func (r *mutationResolver) DeleteUser(ctx context.Context, personalID string) (bool, error) {
	panic("TODO: needs an ownership predicate before this ships")
}

func (r *queryResolver) Users(ctx context.Context) ([]*model.User, error) {
	panic(fmt.Errorf("not implemented: Users - users"))
}

func (r *queryResolver) User(ctx context.Context, personalID string) (*model.User, error) {
	panic(fmt.Errorf("not implemented: User - user"))
}
`)
	out, changed, err := patchSource(src, tbl, "model", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected the other four stubs to be patched")
	}
	text := string(out)
	if !strings.Contains(text, `panic("TODO: needs an ownership predicate before this ships")`) {
		t.Errorf("a hand-written placeholder was overwritten with an unscoped delete, got:\n%s", text)
	}
	if strings.Contains(text, "luima.Delete") {
		t.Errorf("DeleteUser was patched although its body was not gqlgen's stub, got:\n%s", text)
	}
	if !strings.Contains(text, "luima.Get(ctx, r.DB, &model.User{PersonalID: personalID})") {
		t.Errorf("the real stubs were not patched, got:\n%s", text)
	}
}

// TestImportEditsSeeEverySpec pins the multi-block case: an import declared in a second block is
// still a declaration, and appending luima to the first block while the second already has it is a
// redeclare — a compile error in the consumer's module.
func TestImportEditsSeeEverySpec(t *testing.T) {
	src := []byte(`package graph

import "context"

import (
	"fmt"

	"github.com/ulas96/luima"
)

func (r *queryResolver) User(ctx context.Context, personalID string) (*model.User, error) {
	panic(fmt.Errorf("not implemented: User - user"))
}
`)
	out, changed, err := patchSource(src, testTable(), "model", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected changed = true")
	}
	if n := strings.Count(string(out), `"github.com/ulas96/luima"`); n != 1 {
		t.Errorf("luima is imported %d times — a second declaration is a compile error, got:\n%s", n, out)
	}
}

// TestPatchSourceAddsAliasedFmt pins the add branch's alias-awareness: the drop branch leaves an
// aliased fmt alone, so an add branch keyed on the path alone adds nothing while the non-string
// PK label calls fmt.Sprintf — undefined: fmt.
func TestPatchSourceAddsAliasedFmt(t *testing.T) {
	tbl := &modelTable{
		typeName: "Counter",
		pk:       column{field: "ID", goType: "int", sql: "id", gql: "Int"},
		cols:     []column{{field: "Name", goType: "string", sql: "name", gql: "String"}},
	}
	src := []byte(`package graph

import (
	"context"
	f "fmt"
)

func (r *mutationResolver) CreateCounter(ctx context.Context, id int, input model.CounterInput) (*model.Counter, error) {
	panic(f.Errorf("not implemented: CreateCounter - createCounter"))
}
`)
	out, changed, err := patchSource(src, tbl, "model", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected changed = true")
	}
	if !strings.Contains(string(out), "\t\"fmt\"") {
		t.Errorf("the spliced fmt.Sprintf label has no unaliased fmt import, got:\n%s", out)
	}
	// The other half, and the one that actually breaks the build: the splice replaced the only
	// f.Errorf in the file, so `f` is now referenced nowhere. Leaving `f "fmt"` beside the added
	// "fmt" is legal to parse and legal to format — format.Source does not typecheck — and fails
	// the consumer's build with `f imported and not used`.
	if strings.Contains(string(out), `f "fmt"`) {
		t.Errorf("the dead fmt alias survived the splice; `f` is unused and the build fails, got:\n%s", out)
	}
}

// TestPatchStubsRejectsAPartialFile pins the wiring: checkComplete has to run inside patchStubs,
// where the file is, or cmd/luimagen prints "filled Get/List/Create/Update/Delete" over a file
// that still panics at query time.
func TestPatchStubsRejectsAPartialFile(t *testing.T) {
	dir := t.TempDir()
	full := filepath.Join(dir, "full.resolvers.go")
	if err := os.WriteFile(full, fullStubFile, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := patchStubs(full, testTable(), "model", nil); err != nil {
		t.Fatalf("a complete file should patch cleanly: %v", err)
	}

	partial := filepath.Join(dir, "partial.resolvers.go")
	if err := os.WriteFile(partial, []byte(`package graph

import (
	"context"
	"fmt"
)

func (r *queryResolver) User(ctx context.Context, personalID string) (*model.User, error) {
	panic(fmt.Errorf("not implemented: User - user"))
}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := patchStubs(partial, testTable(), "model", nil); err == nil {
		t.Error("expected patchStubs to refuse a file missing four of the five methods")
	}
}

// TestPatchSourceRejectsAWrongSignature pins the arity guard: the body funcs index their
// parameters, so a signature that is not the one luimagen generated SDL for has to be an error
// naming the method, not an index-out-of-range panic out of Generate — which would land after
// gqlgen has already rewritten the consumer's generated code.
func TestPatchSourceRejectsAWrongSignature(t *testing.T) {
	src := []byte(`package graph

import (
	"context"
	"fmt"
)

func (r *queryResolver) User(ctx context.Context) (*model.User, error) {
	panic(fmt.Errorf("not implemented: User - user"))
}
`)
	_, _, err := patchSource(src, testTable(), "model", nil)
	if err == nil {
		t.Fatal("expected an error for a resolver taking no key argument")
	}
	if !strings.Contains(err.Error(), "User") {
		t.Errorf("the error should name the method, got: %v", err)
	}

	// Go forbids mixing named and unnamed parameters, so the only unnamed signature is one with
	// no names at all — paramNames returns nothing and the guard, not an index, reports it.
	unnamed := []byte(`package graph

import (
	"context"
	"fmt"
)

func (r *queryResolver) User(context.Context, string) (*model.User, error) {
	panic(fmt.Errorf("not implemented: User - user"))
}
`)
	if _, _, err := patchSource(unnamed, testTable(), "model", nil); err == nil {
		t.Error("expected an error for a resolver with unnamed parameters")
	}
}

// TestInputFieldNamesRecasedStruct is the other half of TestPatchSourceUsesGeneratedNames' rule.
// Generate asks for Options.Type+"Input", but gqlgen names the struct through
// templates.ToGoModelName, which re-capitalizes the common initialisms: SDL `input ApiKeyInput`
// comes back as `type APIKeyInput struct`. An exact match misses it and Generate dies at stage 4,
// with the model file and the SDL fragment already written and gqlgen's output already rewritten.
func TestInputFieldNamesRecasedStruct(t *testing.T) {
	dir := t.TempDir()
	src := "package model\n\ntype APIKeyInput struct {\n\tOwnerID string\n\tLabel   string\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "models_gen.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := inputFieldNames(dir, "ApiKeyInput")
	if err != nil {
		t.Fatal(err)
	}
	if got["ownerid"] != "OwnerID" || got["label"] != "Label" {
		t.Errorf("inputFieldNames = %v", got)
	}
}

// TestTableFromFieldsRejectsColliding pins the duplicate guard. The exact-duplicate row is the
// likeliest CLI typo — a repeated -field flag — and fails at gqlgen with both files on disk. The
// other two are the silent ones: two distinct Go identifiers derived down onto one column name, or
// onto one GraphQL field name.
func TestTableFromFieldsRejectsColliding(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fields []Field
	}{
		{"an exact duplicate", []Field{
			{Name: "PersonalID", Type: "string", PK: true},
			{Name: "Name", Type: "string"},
			{Name: "Name", Type: "string"},
		}},
		{"two field names snakeCase maps to one column", []Field{
			{Name: "PersonalID", Type: "string", PK: true},
			{Name: "URLValue", Type: "string"},
			{Name: "UrlValue", Type: "string"},
		}},
		{"two field names lowerFirst maps to one GraphQL field", []Field{
			{Name: "ID", Type: "string", PK: true},
			{Name: "Id", Type: "string"},
			{Name: "Name", Type: "string"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tableFromFields("User", tc.fields); err == nil {
				t.Error("expected a collision error before anything is written")
			}
		})
	}
}

// TestPatchSourceRejectsExtraParams pins the arity guard's *upper* bound. A field carrying an
// argument luimagen's SDL never declared gets a resolver parameter the generated body never reads,
// and splicing anyway discards a value the client sent, silently, at runtime.
func TestPatchSourceRejectsExtraParams(t *testing.T) {
	src := []byte(`package graph

import (
	"context"
	"fmt"
)

func (r *queryResolver) Users(ctx context.Context, filter *string) ([]*model.User, error) {
	panic(fmt.Errorf("not implemented: Users - users"))
}
`)
	if _, _, err := patchSource(src, testTable(), "model", nil); err == nil {
		t.Error("expected an arity error: Users takes one parameter after ctx, luimagen's SDL generates none")
	}
}

// TestPlanSDLSeesSiblingSchemaFiles pins the widened probe. gqlgen's documented layout globs the
// schema directory (examples/quickstart/gqlgen.yml: `graph/*.graphqls`), so both checks have to read
// what gqlparser reads. Reading only SchemaFile emits a second `type Query` — a redeclare gqlparser
// rejects at stage 3, after the model file and the fragment are both on disk.
func TestPlanSDLSeesSiblingSchemaFiles(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "root.graphqls")
	path := filepath.Join(dir, "schema.graphqls")
	if err := os.WriteFile(root, []byte("type Query {\n  ok: Boolean!\n}\n\ntype Mutation {\n  noop: Boolean!\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("scalar Void\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	frag, err := planSDL(path, testTable())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(frag, "extend type Query {") || !strings.Contains(frag, "extend type Mutation {") {
		t.Errorf("Query and Mutation are declared in a sibling file, so this must extend, got:\n%s", frag)
	}

	// The duplicate guard has the mirror gap, and its message has to name the file that actually
	// declares the type — §2.4's recovery procedure is "delete the appended SDL block by hand".
	if err := os.WriteFile(root, []byte("type User {\n  id: ID!\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = planSDL(path, testTable())
	if err == nil {
		t.Fatal("expected a duplicate error for a type declared in a sibling file")
	}
	if !strings.Contains(err.Error(), "root.graphqls") {
		t.Errorf("the duplicate error must name the file that declares the type, got: %v", err)
	}
}

// TestCheckTagRejectsTagBreakers pins the two inputs that are not checked Go identifiers.
// Options.Table and Field.Column go verbatim into a backtick-delimited `pg:"…"` tag, so a backtick
// ends the tag literal early and everything %q escapes — a double quote, a backslash, a tab —
// becomes a literal backslash sequence inside the raw string that go-pg reads as part of the name.
// The backslash and tab cases are why the check is strconv.Quote and not two ContainsAny bytes.
func TestCheckTagRejectsTagBreakers(t *testing.T) {
	for _, bad := range []string{"my`table", `my"table`, `ten\ant.users`, "ten\tant"} {
		if err := checkTag("table name", bad); err == nil {
			t.Errorf("checkTag(%q) = nil, want an error before anything is written", bad)
		}
	}
	// A schema-qualified name is not an identifier and must still pass — this is not an ident check.
	if err := checkTag("table name", "tenant.users"); err != nil {
		t.Errorf("checkTag(\"tenant.users\") = %v, want nil", err)
	}
}

// TestCheckCompleteCatchesASurvivingPanic is TestIsStubLeavesHandWrittenPanicAlone's other half.
// Leaving the hand-written body alone is right; reporting the run as a success is not — the CLI
// then prints that all five resolve while DeleteUser is a live panic that fires on the first
// deleteUser mutation in production.
func TestCheckCompleteCatchesASurvivingPanic(t *testing.T) {
	src := []byte(`package graph

import (
	"context"
	"fmt"
)

func (r *mutationResolver) CreateUser(ctx context.Context, personalID string, input model.UserInput) (*model.User, error) {
	panic(fmt.Errorf("not implemented: CreateUser - createUser"))
}

func (r *mutationResolver) UpdateUser(ctx context.Context, personalID string, input model.UserInput) (*model.User, error) {
	panic(fmt.Errorf("not implemented: UpdateUser - updateUser"))
}

func (r *mutationResolver) DeleteUser(ctx context.Context, personalID string) (bool, error) {
	if personalID == "" {
		panic("TODO: needs an ownership predicate before this ships")
	}
	return false, nil
}

func (r *queryResolver) Users(ctx context.Context) ([]*model.User, error) {
	panic(fmt.Errorf("not implemented: Users - users"))
}

func (r *queryResolver) User(ctx context.Context, personalID string) (*model.User, error) {
	panic(fmt.Errorf("not implemented: User - user"))
}
`)
	out, _, err := patchSource(src, testTable(), "model", nil)
	if err != nil {
		t.Fatal(err)
	}
	err = checkComplete(out, testTable())
	if err == nil {
		t.Fatal("expected an error: DeleteUser still panics, so reporting the run as filled is a false success")
	}
	if !strings.Contains(err.Error(), "DeleteUser") {
		t.Errorf("the error must name the method still panicking, got %v", err)
	}
	// The nested panic is what makes this stricter than isStub: a guarded placeholder is not the
	// whole body, so a body-shape check misses it.
	if err := checkComplete(fullStubFile, testTable()); err == nil {
		t.Error("an unpatched stub file must fail too")
	}
}

// TestPlanSDLIgnoresBlockDescriptions pins the second SDL comment form. `#` is handled by the line
// anchor; a """ block is not — its prose lines start at column zero like any declaration, so
// "type Query is the root of every read path" inside one used to read as a Query declaration and
// emit `extend type Query` with no base type for gqlparser to reject at stage 3.
func TestPlanSDLIgnoresBlockDescriptions(t *testing.T) {
	path := schemaFile(t, "\"\"\"\ntype Query is the root of every read path\n\"\"\"\nscalar Void\n")
	frag, err := planSDL(path, testTable())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(frag, "extend type Query {") {
		t.Errorf("a Query mentioned only in a description block is not a declaration, got:\n%s", frag)
	}
}

// TestPlanSDLRefusesFieldCollision pins what the type-name guards cannot see. A schema that
// hand-declares Query.users and no `type User` passes mentionsType, and the appended
// `users: [User!]!` is then a duplicate field gqlparser rejects — at stage 3, with the model file
// and the fragment already on disk.
func TestPlanSDLRefusesFieldCollision(t *testing.T) {
	path := schemaFile(t, "type Query {\n  users: [String!]!\n}\n")
	if _, err := planSDL(path, testTable()); err == nil {
		t.Error("expected a pre-write error for a schema that already declares Query.users")
	}
	// A schema the partial view cannot resolve on its own is not luimagen's to reject: gqlgen.yml
	// may glob files planSDL never reads, so only a set that parses clean before and dirty after
	// counts against the fragment.
	broken := schemaFile(t, "type Query {\n  thing: Thing!\n}\n")
	if _, err := planSDL(broken, testTable()); err != nil {
		t.Errorf("an already-unresolvable schema must not fail the run: %v", err)
	}
}

// TestPatchSourceAddsUnaliasedLuima pins the import-add key. An aliased import satisfies "this
// path is imported" while binding no package name a spliced body can use, so keying the add on the
// path alone leaves every body calling luima.Get against `undefined: luima`.
func TestPatchSourceAddsUnaliasedLuima(t *testing.T) {
	src := []byte(`package graph

import (
	"context"

	l "github.com/ulas96/luima"
)

func (r *queryResolver) User(ctx context.Context, personalID string) (*model.User, error) {
	panic("not implemented")
}

func (r *queryResolver) Users(ctx context.Context) ([]*model.User, error) {
	panic("not implemented")
}

func present(err error) error { return l.PresentError(nil, err) }
`)
	out, _, err := patchSource(src, testTable(), "model", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "\n\t\"github.com/ulas96/luima\"") {
		t.Errorf("no unaliased luima import was added, so the spliced luima.Get does not compile:\n%s", out)
	}
	if !strings.Contains(string(out), "\n\t\"github.com/go-pg/pg/v10/orm\"") {
		t.Errorf("no unaliased orm import was added, so the spliced List closure does not compile:\n%s", out)
	}
}

// TestWithDefaultsRebasesUnderDir pins Dir's one meaning. It used to move only the gqlgen
// subprocess, leaving the three paths relative to the process's working directory — so a caller
// who set Dir alone had gqlgen rewrite one module's generated code while luimagen read and
// appended to another's.
func TestWithDefaultsRebasesUnderDir(t *testing.T) {
	got := Options{Type: "User", Dir: "svc"}.withDefaults()
	for _, tc := range [][2]string{
		{got.ModelDir, filepath.Join("svc", "graph", "model")},
		{got.SchemaFile, filepath.Join("svc", "graph", "schema.graphqls")},
		{got.ResolverFile, filepath.Join("svc", "graph", "schema.resolvers.go")},
	} {
		if tc[0] != tc[1] {
			t.Errorf("rebased path = %q, want %q", tc[0], tc[1])
		}
	}
	// An absolute path is a caller who already computed one — leave it.
	abs := filepath.Join(t.TempDir(), "models")
	if got := (Options{Type: "User", Dir: "svc", ModelDir: abs}).withDefaults().ModelDir; got != abs {
		t.Errorf("absolute ModelDir = %q, want it untouched (%q)", got, abs)
	}
}

// TestWithDefaultsResolverFollowsSchema pins the one default that is derived rather than fixed.
// gqlgen's documented layout (resolver.layout: follow-schema) writes each schema file's stubs
// beside it, so a consumer who moves -schema and nothing else had luimagen read
// graph/schema.resolvers.go, find none of the five methods, and fail *after* gqlgen had already
// regenerated the module — a path-default bug reported as "gqlgen generated no ...".
func TestWithDefaultsResolverFollowsSchema(t *testing.T) {
	got := Options{Type: "Widget", SchemaFile: "graph/widget.graphqls"}.withDefaults().ResolverFile
	if want := filepath.Join("graph", "widget.resolvers.go"); got != want {
		t.Errorf("ResolverFile = %q, want %q", got, want)
	}
}

// TestPlanSDLGlobsTheSchemaExtension pins that the sibling probe follows SchemaFile's own
// extension. ".graphql" is the other common spelling, and a hardcoded "*.graphqls" glob makes every
// peer invisible under it — so a `type Query` next door goes unseen and the fragment declares a
// second one, which gqlparser rejects at stage 3 with both files already written.
func TestPlanSDLGlobsTheSchemaExtension(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "base.graphql"), []byte("type Query {\n  noop: Boolean!\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "schema.graphql")
	if err := os.WriteFile(path, []byte("scalar Void\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	frag, err := planSDL(path, testTable())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(frag, "extend type Query {") {
		t.Errorf("a Query declared in a .graphql sibling has to be seen, got:\n%s", frag)
	}
}

// TestPlanSDLFailsOnAnUnreadablePath separates path from its peers. A peer that cannot be read is
// skipped — nothing is written to it — but path is the file being appended to, and checkComposes
// splices the fragment into the source *named* path: drop that entry and the compose check parses
// two identical schemas, agrees with itself, and passes having checked nothing.
func TestPlanSDLFailsOnAnUnreadablePath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "schema.graphqls")
	if err := os.Mkdir(path, 0o755); err != nil { // stat-able, not readable as a file
		t.Fatal(err)
	}
	if _, err := planSDL(path, testTable()); err == nil {
		t.Error("expected planSDL to fail when the file it appends to cannot be read")
	}
}

// TestWriteModelCreatesTheDirectory pins the first-run case: a module with a schema but no gqlgen
// output yet has no model directory, and writeModel runs before gqlgen, so nothing else can create
// it. Without the MkdirAll the run dies on a raw errno indistinguishable from a wrong ModelDir.
func TestWriteModelCreatesTheDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "graph", "model")
	if err := writeModel(dir, "model", testTable(), "users"); err != nil {
		t.Fatalf("writeModel into a missing directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "user.go")); err != nil {
		t.Error(err)
	}
}

// TestWriteModelJoinsTheExistingPackage pins that the generated file's package clause comes from
// the directory, not from Options.ModelPkg. ModelPkg is the identifier the resolver bodies prefix,
// and the only reason to set it is that gqlgen aliased the import (model1.User) — writing
// `package model1` beside gqlgen's `package model` breaks the model package outright.
func TestWriteModelJoinsTheExistingPackage(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "models_gen.go"), []byte("package model\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeModel(dir, "model1", testTable(), "users"); err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(filepath.Join(dir, "user.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(src), "package model\n") {
		t.Errorf("generated model must join the package already in the directory, got:\n%s", src)
	}
}

// TestPatchStubsKeepsTheWorkOnAnIncompleteFile pins the write order. checkComplete used to run
// before os.WriteFile, so one hand-written panic among the five threw away every body patchSource
// had correctly spliced — leaving the caller with the model file, the appended SDL and a
// regenerated module, and no way to redo the work: planSDL's duplicate guard rejects a re-run.
func TestPatchStubsKeepsTheWorkOnAnIncompleteFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schema.resolvers.go")
	src := strings.Replace(string(fullStubFile),
		`panic(fmt.Errorf("not implemented: DeleteUser - deleteUser"))`,
		`panic("TODO: needs an ownership predicate")`, 1)
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := patchStubs(path, testTable(), "model", nil); err == nil {
		t.Fatal("expected patchStubs to report the surviving panic")
	}
	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "luima.Get(") {
		t.Errorf("the four correct splices must be saved, not discarded:\n%s", out)
	}
	if !strings.Contains(string(out), `panic("TODO: needs an ownership predicate")`) {
		t.Error("the hand-written body must still be left alone")
	}
}

// TestFindStructTakesTheFirstMatch pins the short circuit. ast.Inspect returning false only stops
// the descent into the matched node — the walk carries on — so without the sentinel the *last*
// EqualFold hit won, and a model package holding both APIKeyInput and ApiKeyInput handed back the
// wrong field map for every spliced body.
func TestFindStructTakesTheFirstMatch(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "", `package model

type APIKeyInput struct{ OwnerID string }

type ApiKeyInput struct{ OwnerId string }
`, 0)
	if err != nil {
		t.Fatal(err)
	}
	st := findStruct(file, "ApiKeyInput")
	if st == nil {
		t.Fatal("no match")
	}
	if got := st.Fields.List[0].Names[0].Name; got != "OwnerID" {
		t.Errorf("first match wins: got %q, want OwnerID", got)
	}
}

// TestPatchSourceRefusesAConflictingBinding pins that an import add is keyed on the *name* being
// free, not on the path being absent. A resolver file that binds luima to a fork would otherwise
// get a second, unaliased github.com/ulas96/luima spliced in beside it — "luima redeclared in this
// block" in the consumer's module, which format.Source cannot catch because it does not typecheck.
func TestPatchSourceRefusesAConflictingBinding(t *testing.T) {
	src := []byte(`package graph

import (
	"context"
	"fmt"

	luima "github.com/acme/luima-fork"
)

func (r *queryResolver) User(ctx context.Context, personalID string) (*model.User, error) {
	panic(fmt.Errorf("not implemented: User - user"))
}

var _ = luima.Version
`)
	if _, _, err := patchSource(src, testTable(), "model", nil); err == nil {
		t.Error("expected an error naming the conflicting binding, not a redeclared import")
	}
}

// TestCheckIdentRejectsNonASCII pins the GraphQL half of the name rules. A Go identifier may hold
// any unicode letter; a GraphQL Name is /[_A-Za-z][_0-9A-Za-z]*/, so an accented field name is a
// legal struct field whose SDL field gqlparser cannot parse — and checkComposes cannot be relied on
// to catch it, since it ignores a source set that does not already validate standalone, which is
// every schema using gqlgen's injected @goModel/@goField.
func TestCheckIdentRejectsNonASCII(t *testing.T) {
	if err := checkIdent("field name", "Ürün"); err == nil {
		t.Error("expected a non-ASCII field name to be rejected before anything is written")
	}
}

// TestFieldColumnOverridesTheDerivedName pins the escape hatch snakeCase needs. go-pg's rule
// inserts no separator inside a run of capitals, so URLID becomes urlid while the DBA's column is
// almost certainly url_id — and luimagen does not create the table, so the mismatch compiles and
// fails on the first query.
func TestFieldColumnOverridesTheDerivedName(t *testing.T) {
	got, err := tableFromFields("URL", []Field{
		{Name: "URLID", Type: "string", PK: true, Column: "url_id"},
		{Name: "Ratio", Type: "float64"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.pk.sql != "url_id" {
		t.Errorf("pk column = %q, want url_id (snakeCase alone gives %q)", got.pk.sql, snakeCase("URLID"))
	}
}

// TestGenerateReportsABadTypeAsATypeError pins the validation order. The default table name derives
// from Type, so validating the table first reported `table name "foo"bars"` — a string the caller
// never supplied — and pointed them at -table when the problem is -type.
func TestGenerateReportsABadTypeAsATypeError(t *testing.T) {
	err := Generate(Options{Type: `Foo"Bar`, Fields: []Field{{Name: "ID", Type: "string", PK: true}, {Name: "Name", Type: "string"}}})
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "table name") {
		t.Errorf("an invalid Type must not be reported as a table-name error: %v", err)
	}
}
