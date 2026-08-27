package luimagen

import (
	"cmp"
	"fmt"
	"go/ast"
	"go/format"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	gqlparser "github.com/vektah/gqlparser/v2"
	gqlast "github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"
)

// column is one field of the table Generate builds, after a Field has been validated and its
// GraphQL scalar resolved.
type column struct {
	field  string // Go field name, e.g. "PersonalID"
	goType string // Go type as given in Field.Type, e.g. "string", "[]string" — written back out verbatim into the generated model struct
	sql    string // pg column name, derived from field via snakeCase
	gql    string // GraphQL scalar name: String, Int, Float, or Boolean
	array  bool   // true when goType has a "[]" prefix
}

type modelTable struct {
	typeName string // Go and GraphQL type name, e.g. "User"
	pk       column
	cols     []column // non-pk columns, in Field declaration order
}

// tableFromFields validates fields and resolves each one's GraphQL scalar and SQL column name.
// Exactly one Field must have PK set — everything downstream (Get/Update/Delete's WherePK)
// depends on there being exactly one — and it must be a scalar, not a slice: luimagen derives a
// single-column SDL argument and model tag from the PK, so an array PK is out of scope, not
// unrepresentable. At least one non-PK column must exist too: it is what the XInput object
// carries for create/update, and a GraphQL input type must declare at least one field.
//
// Two fields may also not collide on either derived name. The exact-duplicate case is the likeliest
// CLI typo — a repeated -field flag — and it emits the struct field twice, so gqlgen fails at stage
// 3 with both files already on disk. The two silent cases are worse: URLValue and UrlValue are
// distinct Go identifiers that snakeCase both maps to url_value, which binds two struct fields to
// one column, and ID and Id both lowerFirst to the GraphQL field id. The check is here rather than
// downstream because "validation happens before any file is written" is the guarantee.
func tableFromFields(typeName string, fields []Field) (*modelTable, error) {
	if err := checkIdent("type name", typeName); err != nil {
		return nil, err
	}
	t := &modelTable{typeName: typeName}
	seenCol, seenGQL := map[string]string{}, map[string]string{}
	for _, f := range fields {
		if err := checkIdent("field name", f.Name); err != nil {
			return nil, err
		}
		gql, array, err := scalarType(f.Type)
		if err != nil {
			return nil, fmt.Errorf("field %s: %w", f.Name, err)
		}
		col := column{field: f.Name, goType: f.Type, sql: cmp.Or(f.Column, snakeCase(f.Name)), gql: gql, array: array}
		// The same tag rules as Options.Table, because it is written into the same `pg:"…"`.
		if err := checkTag("column name", col.sql); err != nil {
			return nil, fmt.Errorf("field %s: %w", f.Name, err)
		}
		// Before the PK branch, so the primary key collides with the rest like any other field.
		if prev, dup := seenCol[col.sql]; dup {
			return nil, fmt.Errorf("fields %s and %s both map to column %q — the column name derives from the field name (snakeCase) unless Field.Column overrides it, and go-pg would bind two struct fields to one column", prev, f.Name, col.sql)
		}
		seenCol[col.sql] = f.Name
		if prev, dup := seenGQL[lowerFirst(f.Name)]; dup {
			return nil, fmt.Errorf("fields %s and %s both map to GraphQL field %q — the field name derives from the Go field name (lowerFirst), and a GraphQL type cannot declare one name twice", prev, f.Name, lowerFirst(f.Name))
		}
		seenGQL[lowerFirst(f.Name)] = f.Name
		if f.PK {
			if array {
				return nil, fmt.Errorf("%s has PK field %s of array type %q — luimagen supports a single-column scalar primary key only", typeName, f.Name, f.Type)
			}
			if t.pk.field != "" {
				return nil, fmt.Errorf("%s has two PK fields: %s and %s — luimagen supports a single-column primary key only", typeName, t.pk.field, f.Name)
			}
			t.pk = col
			continue
		}
		t.cols = append(t.cols, col)
	}
	if t.pk.field == "" {
		return nil, fmt.Errorf("%s has no field with PK: true — Get/Update/Delete all call WherePK (crud.go)", typeName)
	}
	if len(t.cols) == 0 {
		return nil, fmt.Errorf("%s has no non-PK fields — its %sInput would be empty, and a GraphQL input type must declare at least one field", typeName, typeName)
	}
	return t, nil
}

// checkIdent rejects a name that cannot become an exported Go struct field. An unexported name
// (`-field name:string:pk` — a natural CLI typo) generates a struct field gqlgen cannot autobind
// and that patch.go's composite literal cannot reference from package graph; a name that is not
// an identifier at all (a hyphen, a space) generates source that fails format.Source. Both
// failures would otherwise surface only after gqlgen has already rewritten the consumer's
// generated code, which is the state docs/luimagen.md §2.4 exists to recover from.
//
// '_' is the third rejection and the least obvious: it *is* a legal Go identifier rune, so
// token.IsIdentifier passes it, but templates.ToGo treats it as a word delimiter and drops it —
// Owner_Name comes back as OwnerName. That breaks the one assumption every name lookup in this
// package rests on ("ToGo only re-cases a delimiter-free name"): findFunc's EqualFold cannot
// match a collapsed method name, and targets' inputField looks up "owner_name" against a map
// keyed "ownername", misses, and splices input.Owner_Name — a field that does not exist. Both
// land after gqlgen has rewritten everything, and the run reports success.
func checkIdent(what, name string) error {
	if !token.IsIdentifier(name) {
		return fmt.Errorf("%s %q is not a Go identifier", what, name)
	}
	// A Go identifier may hold any unicode letter; a GraphQL Name is ASCII only —
	// /[_A-Za-z][_0-9A-Za-z]*/ (spec §2.1.9). So an accented field name is a legal struct field
	// whose derived SDL field gqlparser cannot parse, and checkComposes cannot be relied on to
	// catch it: it ignores a source set that does not already validate, which is every schema
	// using gqlgen's injected @goModel/@goField. This is also what lets snakeCase and lowerFirst
	// treat their rune handling as belt-and-braces rather than as the thing keeping SDL valid.
	for _, r := range name {
		if r > unicode.MaxASCII {
			return fmt.Errorf("%s %q must be ASCII — a GraphQL Name is /[_A-Za-z][_0-9A-Za-z]*/, so the SDL field derived from it would not parse", what, name)
		}
	}
	if strings.Contains(name, "_") {
		return fmt.Errorf("%s %q must not contain '_' — gqlgen's templates.ToGo drops it (%s becomes %s), so the generated resolver would reference a field that does not exist", what, name, name, strings.ReplaceAll(name, "_", ""))
	}
	if !ast.IsExported(name) {
		return fmt.Errorf("%s %q must be exported — capitalize it (%s)", what, name, upperFirst(name))
	}
	return nil
}

// checkTag rejects a SQL name that would not survive being written into a raw-string struct tag.
// Everything else on the way in is a checked Go identifier; Options.Table and Field.Column are the
// two values that go into `pg:"…"` verbatim. A backtick terminates the tag literal early
// (format.Source then fails at stage 2, after planSDL has already passed), and everything %q
// escapes — a double quote, a backslash, a tab, any control character — becomes a literal
// backslash sequence inside the raw string, so go-pg silently reads a name nobody wrote. Testing
// against strconv.Quote catches the whole escaped set rather than the two characters that were
// obvious. Deliberately not a full identifier check: a schema-qualified "tenant.users" is valid.
func checkTag(what, v string) error {
	if strings.Contains(v, "`") || strconv.Quote(v) != `"`+v+`"` {
		return fmt.Errorf("%s %q must not contain a backtick or any character %%q escapes (a double quote, a backslash, a tab, a control character) — it is written verbatim into the model's `pg:\"…\"` tag", what, v)
	}
	return nil
}

// scalarType maps a Go type name to a GraphQL scalar. luimagen maps exactly the Go types that
// round-trip through gqlgen's default bindings — GraphQL Int becomes Go int and GraphQL Float
// becomes Go float64 in the generated resolver signatures — so string, int, float64 and bool
// (and slices of them) are the only shapes whose model field, SDL field and resolver parameter
// all agree. Other integer widths (int8..int64, uint*) and float32 would generate resolver code
// that does not compile, so they are a clear error rather than a guessed-wrong SDL field.
func scalarType(goType string) (gql string, array bool, err error) {
	base, array := strings.CutPrefix(goType, "[]")
	switch base {
	case "string":
		return "String", array, nil
	case "int":
		return "Int", array, nil
	case "float64":
		return "Float", array, nil
	case "bool":
		return "Boolean", array, nil
	default:
		return "", false, fmt.Errorf("unsupported type %q — gqlgen binds GraphQL Int to Go int and Float to float64, so luimagen maps string/int/float64/bool and slices of them; %s would generate resolver code that does not compile", goType, base)
	}
}

// snakeCase derives a pg column name from a Go field name, matching go-pg's own default
// column-naming convention (internal.Underscore, v10.15.1): an upper-case rune gets a '_'
// before it when either neighbour is lower-case. The next-rune clause is what handles an
// initialism followed by a word — URLValue becomes url_value, not urlvalue. checkIdent rejects
// non-ASCII names before this runs, so the rune loop is belt-and-braces rather than load-bearing;
// it costs nothing and stays honest if that guard ever moves. What the rule cannot derive is a
// column with no lower-case neighbour at all — URLID is go-pg's own urlid where the table almost
// certainly says url_id — and Field.Column is the override for exactly that; docs/luimagen.md §2.1.
func snakeCase(s string) string {
	rs := []rune(s)
	var b strings.Builder
	for i, r := range rs {
		if unicode.IsUpper(r) {
			// i > 0 gates both clauses, exactly as in go-pg: a leading upper-case rune never
			// takes an underscore (Name -> name, not _name).
			if i > 0 && i+1 < len(rs) && (unicode.IsLower(rs[i-1]) || unicode.IsLower(rs[i+1])) {
				b.WriteByte('_')
			}
			r = unicode.ToLower(r)
		}
		b.WriteRune(r)
	}
	return b.String()
}

func gqlFieldType(c column) string {
	if c.array {
		return "[" + c.gql + "!]!"
	}
	return c.gql + "!"
}

// lowerFirst derives a GraphQL field/arg name from a Go field name: Name -> name, PersonalID ->
// personalId. Go's exported-field convention capitalizes a whole initialism (ID, URL, ...);
// GraphQL's lowerCamelCase convention does not, so a trailing "ID" is folded to "Id" before the
// leading rune is lowercased. Other embedded initialisms (a field ending in URL or API) aren't
// handled — same class of known ceiling as the table-name pluralization in docs/luimagen.md §2.1; add a case here
// the day a real field needs it.
func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	if strings.HasSuffix(s, "ID") {
		s = s[:len(s)-1] + "d"
	}
	// Decode the first rune rather than slicing s[:1], for the same reason snakeCase works on
	// runes: a byte slice would split a multi-byte leading rune mid-sequence. checkIdent rejects
	// non-ASCII before this runs, so this is defensive, not the guard.
	r, size := utf8.DecodeRuneInString(s)
	return string(unicode.ToLower(r)) + s[size:]
}

// upperFirst is lowerFirst's inverse for the leading rune only, used to suggest a fix in
// checkIdent's error message.
func upperFirst(s string) string {
	if s == "" {
		return s
	}
	r, size := utf8.DecodeRuneInString(s)
	return string(unicode.ToUpper(r)) + s[size:]
}

// sdl builds the SDL fragment for t. declareQuery/declareMutation choose "type Query { }" for
// the first table appended to a schema file versus "extend type Query { }" for every table
// after — see docs/luimagen.md §2.3.
func sdl(t *modelTable, declareQuery, declareMutation bool) string {
	var b strings.Builder

	fmt.Fprintf(&b, "type %s {\n  %s: %s!\n", t.typeName, lowerFirst(t.pk.field), t.pk.gql)
	for _, c := range t.cols {
		fmt.Fprintf(&b, "  %s: %s\n", lowerFirst(c.field), gqlFieldType(c))
	}
	b.WriteString("}\n\n")

	fmt.Fprintf(&b, "input %sInput {\n", t.typeName)
	for _, c := range t.cols {
		fmt.Fprintf(&b, "  %s: %s\n", lowerFirst(c.field), gqlFieldType(c))
	}
	b.WriteString("}\n\n")

	queryKw, mutationKw := "extend type", "extend type"
	if declareQuery {
		queryKw = "type"
	}
	if declareMutation {
		mutationKw = "type"
	}

	lower := lowerFirst(t.typeName)
	pk := lowerFirst(t.pk.field)
	fmt.Fprintf(&b, "%s Query {\n  %s(%s: %s!): %s\n  %ss: [%s!]!\n}\n\n",
		queryKw, lower, pk, t.pk.gql, t.typeName, lower, t.typeName)
	fmt.Fprintf(&b, "%s Mutation {\n  create%s(%s: %s!, input: %sInput!): %s!\n  update%s(%s: %s!, input: %sInput!): %s!\n  delete%s(%s: %s!): Boolean!\n}\n",
		mutationKw,
		t.typeName, pk, t.pk.gql, t.typeName, t.typeName,
		t.typeName, pk, t.pk.gql, t.typeName, t.typeName,
		t.typeName, pk, t.pk.gql)
	return b.String()
}

// planSDL refuses to run twice for the same type and returns the fragment to append. It writes
// nothing: Generate calls it *before* writeModel so a schema that already declares the type fails
// before a stray model file is left on disk to redeclare a hand-written one — the "validation
// happens before any file is written" the CHANGELOG claims.
//
// Both checks read every schema file beside path, not just path itself, because that is what
// gqlparser will see: the documented gqlgen layout globs the directory
// (examples/quickstart/gqlgen.yml: `schema: - graph/*.graphqls`). Reading path alone makes a
// `type Query` in a sibling invisible, so the fragment declares its own and gqlparser rejects the
// redeclare at stage 3 — after the model file and the fragment are both on disk. Only the checks
// widen; appendSDL still writes to path alone, so Options.SchemaFile still targets exactly one
// file. Per file rather than concatenated, so the duplicate error names the file that actually
// declares the type — docs/luimagen.md §2.4's recovery procedure needs that to be true.
//
// Both questions — "is Query already declared" and "is this type name taken" — are answered off
// gqlparser's own parse of each file. Parsing, not LoadSchema: a schema using gqlgen's injected
// @goModel/@goField does not validate standalone (which is why checkComposes below has to tolerate
// failure), but it always parses, and both questions are about declarations rather than about a
// valid schema. A line-anchored regex over the text answered them before, and needed its own
// description stripper to stop `type Query is the root of every read path` inside a """ block from
// counting as a declaration — a second, weaker mechanism beside the parser already in the file.
func planSDL(path string, t *modelTable) (string, error) {
	// Cleaned so the slices.Contains guard below means what it says: filepath.Glob returns cleaned
	// paths, so an uncleaned SchemaFile ("./graph/schema.graphqls") would never match its own glob
	// entry and get appended a second time.
	path = filepath.Clean(path)
	if _, err := os.Stat(path); err != nil {
		return "", err
	}
	// The extension comes from path rather than being hardcoded to ".graphqls": gqlgen.yml globs
	// whatever the consumer named their schema files, and ".graphql" is the other common spelling
	// — under which a hardcoded glob makes every sibling invisible, so a schema whose `type Query`
	// lives in a peer gets a second declaration appended. A path with no extension globs nothing,
	// deliberately: "*" would sweep the directory's .go files into the duplicate guard.
	peers := []string{path}
	if ext := filepath.Ext(path); ext != "" {
		matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), "*"+ext))
		if err != nil {
			return "", err
		}
		peers = matches
		if !slices.Contains(peers, path) { // a SchemaFile named something other than *<ext>
			peers = append(peers, path)
		}
	}

	var hasQuery, hasMutation bool
	var sources []*gqlast.Source
	for _, p := range peers {
		b, err := os.ReadFile(p)
		if err != nil {
			// A peer that cannot be read is skipped: nothing is written to it, and failing the run
			// over it would be worse than the gqlparser error the check exists to pre-empt. path
			// itself is not skippable — it is the file being appended to, and checkComposes splices
			// the fragment into the source *named* path, so dropping that entry would leave it
			// comparing two identical schemas and passing vacuously.
			if p == path {
				return "", err
			}
			continue
		}
		src := &gqlast.Source{Name: p, Input: string(b)}
		doc, err := parser.ParseSchema(src)
		if err != nil {
			if p == path {
				return "", err
			}
			continue // not SDL luimagen can read; if gqlgen globs it, gqlgen will say so
		}
		sources = append(sources, src)
		// Extensions are `extend type X`: extending a type this schema does not declare still means
		// the name is taken, but only a Definition answers the declare-vs-extend question. Both
		// lists cover every kind — an `input User` or a `scalar User` takes the name too.
		for _, d := range doc.Extensions {
			if d.Name == t.typeName {
				return "", fmt.Errorf("%s already extends type %s — remove it first or edit that file by hand", p, t.typeName)
			}
		}
		for _, d := range doc.Definitions {
			if d.Name == t.typeName {
				return "", fmt.Errorf("%s already declares type %s — remove it first or edit that file by hand", p, t.typeName)
			}
			hasQuery = hasQuery || d.Name == "Query"
			hasMutation = hasMutation || d.Name == "Mutation"
		}
	}
	frag := sdl(t, !hasQuery, !hasMutation)
	if err := checkComposes(sources, path, frag); err != nil {
		return "", err
	}
	return frag, nil
}

// checkComposes parses the schema luimagen is about to create — every source planSDL read, plus
// the fragment appended to path — with the same parser gqlgen runs at stage 3.
//
// The line-anchored guards above only ever see a *type name*, and that is not what collides in
// practice. A schema that hand-declares `Query.settings` and no `type Setting` passes
// mentionsType, and then the appended `settings: [Setting!]!` is a duplicate field; so is a
// pre-existing `input SettingInput`; so are two generated types whose naive +"s" plurals meet
// (User and Users both claim `Query.users`). Every one of those is a gqlparser error at stage 3,
// with the model file and the fragment already on disk — the state "validation happens before any
// file is written" promises cannot happen.
//
// A failure on the sources *alone* is ignored rather than reported. planSDL only reads the files
// beside path, while gqlgen.yml may glob several directories, so an unresolved reference in that
// partial view is not luimagen's to reject — nor is an undeclared directive, nor a first-run
// schema with no root type yet. Only a set that parses clean before and dirty after is the
// fragment's fault, which is the only claim this function is entitled to make.
func checkComposes(sources []*gqlast.Source, path, frag string) error {
	if _, err := gqlparser.LoadSchema(sources...); err != nil {
		return nil
	}
	withFrag := make([]*gqlast.Source, len(sources))
	for i, s := range sources {
		withFrag[i] = s
		if s.Name == path {
			withFrag[i] = &gqlast.Source{Name: s.Name, Input: s.Input + "\n" + frag}
		}
	}
	if _, err := gqlparser.LoadSchema(withFrag...); err != nil {
		return fmt.Errorf("the generated SDL does not compose with the existing schema: %w", err)
	}
	return nil
}

// appendSDL appends the fragment planSDL built. Split from it so every check is read-only and
// runs first.
func appendSDL(path, frag string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = f.WriteString("\n" + frag)
	return err
}

// writeModel writes t's Go struct into <dir>/<lower(typeName)>.go, refusing to overwrite an
// existing file for the same reason appendSDL refuses to double-declare — a second run should
// fail loud, not silently clobber a file that may since have been hand-edited.
func writeModel(dir, pkg string, t *modelTable, table string) error {
	// The directory is the consumer's, and on a first run — a module with a schema but no gqlgen
	// output yet — it does not exist. writeModel runs before gqlgen, so nothing else can create it,
	// and the raw "no such file or directory" it used to fail with is indistinguishable from the
	// wrong ModelDir.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, strings.ToLower(t.typeName)+".go")
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s already exists — remove it first or edit the model by hand", path)
	}
	src, err := modelSource(packageClause(dir, pkg), t, table)
	if err != nil {
		return err
	}
	return os.WriteFile(path, src, 0o644)
}

// modelSource builds the model struct's source text and runs it through format.Source — there
// is no existing file to preserve here, unlike patch.go's splice, so this is plain templating.
func modelSource(pkg string, t *modelTable, table string) ([]byte, error) {
	var b strings.Builder
	// The doc comment is not decoration: .golangci.yml enables revive's `exported` rule, and this
	// file lands in the consumer's module, so a bare `type User struct` lints as an error on code
	// they did not write. It also carries the three details the quickstart's hand-written model
	// explains, which are the three a generated model most needs to explain.
	fmt.Fprintf(&b, "package %s\n\n// %s @notice The autobound model behind the GraphQL %s type, generated by luimagen.\n//\n// @dev Three things here are load-bearing. tableName names the table, the ,pk tag is mandatory\n// because Get, Update and Delete all call WherePK, and ,array is what keeps a slice column from\n// being encoded as JSONB that an array column rejects. Every field is exported because gqlgen,\n// go-pg and encoding/json all read them by reflection.\n//\n// gqlgen autobinds this type rather than regenerating it — edit it freely.\ntype %s struct {\n",
		pkg, t.typeName, t.typeName, t.typeName)
	fmt.Fprintf(&b, "\ttableName struct{} `pg:%q`\n", table)
	fmt.Fprintf(&b, "\t%s %s `pg:\"%s,pk\"`\n", t.pk.field, t.pk.goType, t.pk.sql)
	for _, c := range t.cols {
		tag := c.sql
		if c.array {
			tag += ",array"
		}
		fmt.Fprintf(&b, "\t%s %s `pg:\"%s\"`\n", c.field, c.goType, tag)
	}
	b.WriteString("}\n")
	return format.Source([]byte(b.String()))
}
