// Package main @notice The luimagen CLI: scaffolds one table's CRUD layer — model struct, SDL,
// and the five resolver bodies — from its fields.
//
// @dev A thin flag-parsing wrapper over github.com/ulas96/luima/luimagen, which holds the actual
// implementation and the design behind it (docs/luimagen.md). Nothing in the library imports this
// package, so `github.com/ulas96/luima` gains no surface from it — the one exception to CLAUDE.md's
// "no scaffolding CLI", for the reason in docs/luimagen.md §1.
//
// Usage, from a gqlgen consumer module's root:
//
//	go run github.com/ulas96/luima/cmd/luimagen -type User \
//	  -field PersonalID:string:pk -field Name:string -field Projects:[]string
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/ulas96/luima/luimagen"
)

// fields @notice Collects repeated -field flags into []luimagen.Field.
//
// @dev A flag.Value rather than one comma-joined string: a Go slice type carries a comma of its
// own in some spellings, and repeating the flag keeps every field on its own line in a shell
// history that is likely to be re-run.
type fields []luimagen.Field

// String @notice Renders the collected fields, for flag's own error and default output.
//
// @dev Part of flag.Value; never the place the CLI reports anything itself.
// @return string the accumulated fields, in declaration order
func (f *fields) String() string { return fmt.Sprint([]luimagen.Field(*f)) }

// Set @notice Parses one -field value: "Name:Type", optionally followed by "pk" and/or
// "column=<sql name>" in either order.
//
// @dev Split on ':' rather than a struct tag or JSON, because this is typed by hand at a shell
// prompt; the two optional segments are named rather than positional so neither has to be given
// to reach the other. A column name holding a ':' is out of reach here — call luimagen.Generate
// directly for that.
// @param v      one -field value as typed
// @return error names the offending segment; flag prints it and exits
func (f *fields) Set(v string) error {
	parts := strings.Split(v, ":")
	if len(parts) < 2 {
		return fmt.Errorf("-field %q: want Name:Type[:pk][:column=<sql name>]", v)
	}
	field := luimagen.Field{Name: parts[0], Type: parts[1]}
	for _, opt := range parts[2:] {
		switch {
		case opt == "pk":
			field.PK = true
		case strings.HasPrefix(opt, "column="):
			field.Column = strings.TrimPrefix(opt, "column=")
		default:
			return fmt.Errorf("-field %q: %q is neither \"pk\" nor \"column=<sql name>\"", v, opt)
		}
	}
	*f = append(*f, field)
	return nil
}

// main @notice Parses the flags, calls luimagen.Generate, and reports what the run proved.
//
// @dev Every default is left empty and resolved inside Options.withDefaults, so the flag help and
// the library agree by construction instead of by two copies of the same string. The two required
// flags are checked here rather than left to Generate only so the message can name the flag.
func main() {
	opts := luimagen.Options{}
	var fs fields
	flag.StringVar(&opts.Type, "type", "", "Go/GraphQL type name, e.g. User (required)")
	flag.Var(&fs, "field", "one column, repeatable: Name:Type[:pk][:column=<sql name>], e.g. -field PersonalID:string:pk")
	flag.StringVar(&opts.Table, "table", "", "SQL table name (default snake_case(type)+\"s\")")
	flag.StringVar(&opts.Dir, "dir", "", "the consumer module's root, which every other path is relative to (default \".\")")
	flag.StringVar(&opts.ModelDir, "model-dir", "", "directory to write the model struct into (default graph/model)")
	flag.StringVar(&opts.SchemaFile, "schema", "", "schema file to append the generated SDL to (default graph/schema.graphqls)")
	flag.StringVar(&opts.ResolverFile, "resolvers", "", "resolver file gqlgen writes stubs into (default: -schema with its extension replaced by .resolvers.go)")
	flag.StringVar(&opts.ModelPkg, "model-pkg", "", "import name of the model package as used in resolver code, e.g. model1 when gqlgen aliased it (default model)")
	flag.Parse()
	opts.Fields = fs

	if opts.Type == "" {
		fmt.Fprintln(os.Stderr, "luimagen: -type is required, e.g. -type User")
		os.Exit(1)
	}
	if len(opts.Fields) == 0 {
		fmt.Fprintln(os.Stderr, "luimagen: at least one -field is required, e.g. -field PersonalID:string:pk")
		os.Exit(1)
	}

	if err := luimagen.Generate(opts); err != nil {
		fmt.Fprintln(os.Stderr, "luimagen:", err)
		os.Exit(1)
	}
	// Not "filled": a method an earlier run or a hand edit already implemented was left alone,
	// not spliced. What Generate actually proves is checkComplete's claim — all five exist and
	// none of them still panics — so that is what this says.
	fmt.Printf("luimagen: Get/List/Create/Update/Delete for %s all resolve\n", opts.Type)
}
