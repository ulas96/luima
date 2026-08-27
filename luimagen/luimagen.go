// Package luimagen @notice Generates a table's CRUD layer: given the table's fields it writes the
// Go model struct, appends the matching GraphQL SDL to a schema file, runs the consumer's own
// `go tool gqlgen generate`, and fills in the five resulting resolver stubs with calls to
// luima.Get/List/Create/Update/Delete.
//
// @dev A separate package from the module root, not re-exported through luima.go — importing
// github.com/ulas96/luima never pulls this in, and Config/Mount/Run gain no new surface. See
// docs/luimagen.md §1.
package luimagen

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Field @notice One column of the table Generate builds.
//
// @dev Name is where the GraphQL field name comes from (lowerFirst), and where the SQL column
// name comes from unless Column overrides it. Exactly one Field in Options.Fields must have PK
// set. There is no GraphQL field-name override; see docs/luimagen.md §4.
type Field struct {
	Name string // Go field name, e.g. "PersonalID"
	Type string // Go type as it should appear in the generated struct: string, int, float64, bool, or a slice of one of those, e.g. "[]string"
	PK   bool   // true for exactly one field
	// Column is the SQL column name; default snakeCase(Name), which is go-pg's own convention.
	// Set it when the table's column is not what that derives — most often a run of capitals with
	// no lower-case neighbour, where go-pg's rule inserts no separator at all: URLID becomes
	// urlid, and the column is almost certainly url_id. luimagen does not create the table, so a
	// derived name that disagrees with it compiles fine and fails on the first query.
	Column string
}

// Options @notice Configures one Generate call for one table.
//
// @dev Every path field defaults to the layout docs/gqlgen-contract.md documents, matching
// Config's own "zero means unset, not off" convention (CLAUDE.md) — Options{Type: ..., Fields:
// ...} is enough for a consumer whose module follows that layout.
type Options struct {
	Type         string  // Go/GraphQL type name, e.g. "User" — required
	Fields       []Field // the table's columns, in declaration order — required, exactly one with PK: true
	Table        string  // SQL table name; default snake_case(Type)+"s", e.g. "users" — set explicitly for anything irregular (a prefix, non-English pluralization)
	Dir          string  // the consumer module's root: where `go tool gqlgen generate` runs, and what ModelDir/SchemaFile/ResolverFile are relative to; default "."
	ModelDir     string  // directory to write the model struct into, relative to Dir; default "graph/model"
	SchemaFile   string  // schema file to append the generated SDL to, relative to Dir; default "graph/schema.graphqls"
	ResolverFile string  // resolver file gqlgen writes stubs into, relative to Dir; default: SchemaFile with its extension replaced by ".resolvers.go"
	ModelPkg     string  // import name of the model package as used in resolver code, e.g. "model1" when gqlgen aliased it; default "model". Not the generated file's package clause — that is read from ModelDir
}

// withDefaults @notice Fills every unset Options field with the layout docs/gqlgen-contract.md
// documents, then rebases the three paths onto Dir.
//
// @dev Value receiver: the caller's Options is left alone, and Generate works on the returned copy.
// Order matters twice — Table derives from Type, and ResolverFile from SchemaFile — so neither can
// move above what it reads.
// @return Options a copy with no empty field left to interpret downstream
func (o Options) withDefaults() Options {
	if o.Table == "" {
		o.Table = snakeCase(o.Type) + "s"
	}
	if o.Dir == "" {
		o.Dir = "."
	}
	if o.ModelDir == "" {
		o.ModelDir = "graph/model"
	}
	if o.SchemaFile == "" {
		o.SchemaFile = "graph/schema.graphqls"
	}
	if o.ResolverFile == "" {
		// Derived from SchemaFile, not a fixed graph/schema.resolvers.go: gqlgen's documented
		// layout (resolver.layout: follow-schema) writes each schema file's stubs beside it, so
		// -schema graph/widget.graphqls means graph/widget.resolvers.go. A fixed default is right
		// only for the default -schema and silently patches the wrong file for any other — and the
		// miss lands at stage 4, after gqlgen has already regenerated the module.
		o.ResolverFile = strings.TrimSuffix(o.SchemaFile, filepath.Ext(o.SchemaFile)) + ".resolvers.go"
	}
	if o.ModelPkg == "" {
		o.ModelPkg = "model"
	}
	// Dir means one thing — the module root — and everything else hangs off it. It used to mean
	// only "where the gqlgen subprocess runs" while the three paths stayed relative to the
	// process's working directory, which is four coupled knobs where one will do: set Dir alone
	// and gqlgen rewrites one module's generated code while luimagen reads and appends to
	// another's. An absolute path is left alone, so a caller that computed one still gets it.
	o.ModelDir = rebase(o.Dir, o.ModelDir)
	o.SchemaFile = rebase(o.Dir, o.SchemaFile)
	o.ResolverFile = rebase(o.Dir, o.ResolverFile)
	return o
}

// rebase @notice Joins path onto dir, leaving an absolute path alone.
//
// @dev The absolute case is a caller who already computed the path they meant; joining Dir onto it
// would silently move it.
// @param dir  the consumer module's root
// @param path one of ModelDir/SchemaFile/ResolverFile
// @return string path relative to dir, or path itself when it is already absolute
func rebase(dir, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(dir, path)
}

// Generate @notice Builds the model struct for opts.Type from opts.Fields, appends its SDL to
// opts.SchemaFile, runs `go tool gqlgen generate` in opts.Dir, and patches the five stubs it
// writes to call luima.Get/List/Create/Update/Delete. See docs/luimagen.md §2 for why each
// step is shaped the way it is.
//
// @dev Silent on success and side-effecting only through the files it writes plus stdout/stderr
// inherited by the gqlgen subprocess — never fmt.Print. A library call has no business deciding
// whether its caller wants progress lines; cmd/luimagen prints them instead.
// @dev A failed call can leave the model file and/or appended SDL on disk — see
// docs/luimagen.md §2.4 ("Recovering from a failed run") for why that's deliberate and how to
// clean up before retrying.
// @param opts Options{Type: "User", Fields: [...]} is enough when the module follows the layout
// docs/gqlgen-contract.md documents; every other field overrides one default.
// @return error wraps whichever stage failed — building the table, writing the model, writing
// SDL, running gqlgen, or patching stubs — with enough context to tell them apart
func Generate(opts Options) error {
	if opts.Type == "" {
		return fmt.Errorf("luimagen: Options.Type is required")
	}
	// Before withDefaults, because the default table name is derived from Type: an invalid Type
	// otherwise fails checkTag first, reporting a table name the caller never supplied and pointing
	// them at -table when the problem is -type.
	if err := checkIdent("type name", opts.Type); err != nil {
		return fmt.Errorf("building %s: %w", opts.Type, err)
	}
	opts = opts.withDefaults()
	if err := checkTag("table name", opts.Table); err != nil {
		return fmt.Errorf("building %s: %w", opts.Type, err)
	}

	t, err := tableFromFields(opts.Type, opts.Fields)
	if err != nil {
		return fmt.Errorf("building %s: %w", opts.Type, err)
	}

	// planSDL before writeModel: it is the last read-only check, and running it after the model
	// file is written leaves a stray graph/model/<type>.go behind to redeclare a hand-written
	// struct whenever the schema already declares the type.
	frag, err := planSDL(opts.SchemaFile, t)
	if err != nil {
		// "checking", not "writing": planSDL only reads, and a missing schema file is the most
		// likely first-run failure. Reported as a write it sends the reader to §2.4's recovery
		// procedure for a run that touched nothing.
		return fmt.Errorf("checking SDL: %w", err)
	}

	if err := writeModel(opts.ModelDir, opts.ModelPkg, t, opts.Table); err != nil {
		return fmt.Errorf("writing model: %w", err)
	}

	if err := appendSDL(opts.SchemaFile, frag); err != nil {
		return fmt.Errorf("writing SDL: %w", err)
	}

	if err := runGqlgenGenerate(opts.Dir); err != nil {
		return fmt.Errorf("gqlgen generate: %w", err)
	}

	// The generated XInput's field names come from gqlgen, not from Field.Name — read them rather
	// than predict them, or a name whose initialism gqlgen re-capitalizes splices a body that does
	// not compile.
	inputFields, err := inputFieldNames(opts.ModelDir, opts.Type+"Input")
	if err != nil {
		return fmt.Errorf("reading generated %sInput: %w", opts.Type, err)
	}

	if err := patchStubs(opts.ResolverFile, t, opts.ModelPkg, inputFields); err != nil {
		return fmt.Errorf("patching %s: %w", opts.ResolverFile, err)
	}
	return nil
}
