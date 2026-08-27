package luimagen

import (
	"cmp"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// runGqlgenGenerate shells out to the consumer's own codegen step — luima cannot replace it,
// and neither can luimagen; see docs/gqlgen-contract.md.
func runGqlgenGenerate(dir string) error {
	cmd := exec.Command("go", "tool", "gqlgen", "generate")
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// target names one gqlgen-generated stub method and how to fill it in, once the parameter
// names gqlgen chose (e.g. "personalID" from SDL's personalId) are known.
type target struct {
	recv  string
	name  string
	arity int // parameters after ctx that body indexes — checked for exact equality before body
	// runs, so a signature that is not the one luimagen generated SDL for is an error and not an
	// index-out-of-range panic out of Generate. Exact, not a minimum: too *many* parameters means
	// the field carries an argument luimagen's SDL never declared (a filter, a locale, one a
	// directive injected), and splicing anyway discards it silently at runtime.
	body func(params []string) string
}

func targets(t *modelTable, modelPkg string, inputFields map[string]string) []target {
	typeName := t.typeName

	// inputField maps one of luimagen's own model field names to the field gqlgen actually
	// generated on XInput. The two are not always the same string: gqlgen runs the SDL name
	// through templates.ToGo, which re-capitalizes common initialisms — ownerId becomes OwnerID,
	// profileUrl becomes ProfileURL — so `OwnerId: input.OwnerId` does not compile. The lookup is
	// keyed on the lower-cased name because ToGo only ever re-cases a delimiter-free name, never
	// changes its letters. A nil map falls back to the field's own name, which is what every
	// name that does round-trip already produces.
	inputField := func(name string) string {
		if got, ok := inputFields[strings.ToLower(name)]; ok {
			return got
		}
		return name
	}

	// The key side of each pair is luimagen's own model struct — written by writeModel, so its
	// names are Field.Name verbatim. Only the input side goes through inputField.
	assignments := func(inputParam string) string {
		parts := make([]string, len(t.cols))
		for i, c := range t.cols {
			parts[i] = fmt.Sprintf("%s: %s.%s", c.field, inputParam, inputField(c.field))
		}
		return strings.Join(parts, ", ")
	}

	// label builds the human-readable "type key" label luima.Create/Update take for error
	// messages. A string PK keeps the plain concat ("user "+personalID); any other scalar PK
	// goes through fmt.Sprintf, because "user "+key does not compile when key is an int — and
	// the import fix in patchSource keeps "fmt" alive for exactly that reason.
	label := func(p []string) string {
		if t.pk.goType == "string" {
			return fmt.Sprintf("%q+%s", strings.ToLower(typeName)+" ", p[0])
		}
		return fmt.Sprintf("fmt.Sprintf(%q, %s)", strings.ToLower(typeName)+" %v", p[0])
	}

	return []target{
		{"queryResolver", typeName, 1, func(p []string) string {
			return fmt.Sprintf("return luima.Get(ctx, r.DB, &%s.%s{%s: %s})", modelPkg, typeName, t.pk.field, p[0])
		}},
		{"queryResolver", typeName + "s", 0, func(p []string) string {
			// The Limit is commented in the generated file, not just here: luima ships no
			// pagination (CLAUDE.md, "out of scope, deliberately"), so an uncapped list is a
			// footgun on a growing table — and a silent cap is indistinguishable from a table
			// that really holds 100 rows.
			return fmt.Sprintf("// Capped at 100 rows: luima ships no pagination, so this bounds an\n\t// otherwise unbounded query. Raise or drop the Limit to suit the table.\n\treturn luima.List[%s.%s](ctx, r.DB, func(q *orm.Query) *orm.Query {\n\t\treturn q.Order(%q).Limit(100)\n\t})", modelPkg, typeName, t.pk.sql)
		}},
		{"mutationResolver", "Create" + typeName, 2, func(p []string) string {
			return fmt.Sprintf("return luima.Create(ctx, r.DB, &%s.%s{%s: %s, %s}, %s)",
				modelPkg, typeName, t.pk.field, p[0], assignments(p[1]), label(p))
		}},
		{"mutationResolver", "Update" + typeName, 2, func(p []string) string {
			return fmt.Sprintf("return luima.Update(ctx, r.DB, &%s.%s{%s: %s, %s}, %s)",
				modelPkg, typeName, t.pk.field, p[0], assignments(p[1]), label(p))
		}},
		{"mutationResolver", "Delete" + typeName, 1, func(p []string) string {
			return fmt.Sprintf("return luima.Delete(ctx, r.DB, &%s.%s{%s: %s})", modelPkg, typeName, t.pk.field, p[0])
		}},
	}
}

// inputFieldNames returns the fields gqlgen actually generated on structName, keyed by lower-cased
// name, by parsing the generated model package. gqlgen derives a generated input struct's field
// names from the SDL through templates.ToGo, so predicting them from Field.Name is what breaks;
// reading them is exact and costs no dependency (importing gqlgen's codegen packages would drag
// golang.org/x/tools into luima's module graph, which examples/quickstart/go.mod argues against).
// Every file in the directory is tried because the generated model's filename is a gqlgen.yml
// setting, not something luimagen chooses.
func inputFieldNames(modelDir, structName string) (map[string]string, error) {
	paths, err := filepath.Glob(filepath.Join(modelDir, "*.go"))
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	for _, path := range paths {
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			continue // a file that does not parse is not the one gqlgen just wrote
		}
		st := findStruct(file, structName)
		if st == nil {
			continue
		}
		names := make(map[string]string, len(st.Fields.List))
		for _, f := range st.Fields.List {
			for _, n := range f.Names {
				names[strings.ToLower(n.Name)] = n.Name
			}
		}
		return names, nil
	}
	return nil, fmt.Errorf("no struct %s in %s/*.go — gqlgen writes generated models to model.filename in gqlgen.yml; point Options.ModelDir at that directory", structName, modelDir)
}

// findStruct finds the struct type named name. Case-insensitive for the same reason findFunc is,
// and it is the same rule applied to the other half of the seam: Generate asks for
// Options.Type+"Input", but gqlgen names the generated struct templates.ToGoModelName(name)
// (plugin/modelgen/models.go), which re-capitalizes the 24 common initialisms — so SDL
// `input ApiKeyInput` comes back as `type APIKeyInput struct`. An exact match misses it, and the
// miss lands at stage 4, after the model file and the SDL fragment are already on disk.
func findStruct(file *ast.File, name string) *ast.StructType {
	var found *ast.StructType
	ast.Inspect(file, func(n ast.Node) bool {
		// Returning false at the match only stops the descent into that node — the walk carries on
		// over the rest of the file, so without this the *last* EqualFold hit wins. A model package
		// holding both gqlgen's APIKeyInput and a hand-written ApiKeyInput would then hand back the
		// wrong field map, and every spliced body would name fields the generated struct does not
		// have.
		if found != nil {
			return false
		}
		ts, ok := n.(*ast.TypeSpec)
		if !ok || !strings.EqualFold(ts.Name.Name, name) {
			return true
		}
		if st, ok := ts.Type.(*ast.StructType); ok {
			found = st
		}
		return false
	})
	return found
}

// packageClause returns the package name dir's existing Go files declare, falling back to fallback
// when it has none yet. The generated model has to join the package gqlgen already generates into,
// and that is not Options.ModelPkg: ModelPkg is the *identifier* the resolver bodies prefix, which
// differs from the package name exactly when gqlgen aliased the import (two packages named model in
// one resolver file, so the bodies say model1.User). Writing `package model1` beside gqlgen's
// `package model` breaks the model package — after gqlgen has already regenerated everything.
func packageClause(dir, fallback string) string {
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return fallback
	}
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue // may legally be package <name>_test
		}
		f, err := parser.ParseFile(token.NewFileSet(), p, nil, parser.PackageClauseOnly)
		if err == nil && f.Name != nil && f.Name.Name != "" {
			return f.Name.Name
		}
	}
	return fallback
}

// patchStubs replaces the panic body of each still-unimplemented CRUD stub gqlgen just wrote
// with a call into luima's generic helpers. It only ever touches a function whose body is still
// exactly the "not implemented" stub (see isStub) — a method already filled in, by an earlier
// luimagen run or by hand, is left alone, which is what makes a second run safe to attempt.
func patchStubs(path string, t *modelTable, modelPkg string, inputFields map[string]string) error {
	src, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	patched, changed, err := patchSource(src, t, modelPkg, inputFields)
	if err != nil {
		return err
	}
	// Written before checkComplete reports, not after. Every splice patchSource made is correct
	// whatever checkComplete then finds, and discarding them leaves the caller holding the model
	// file, the appended SDL and a fully regenerated module with none of the bodies filled — a
	// state no re-run can redo, because planSDL's duplicate guard now rejects the type. Keep the
	// work; report what is still missing.
	if changed {
		if err := os.WriteFile(path, patched, 0o644); err != nil {
			return err
		}
	}
	if err := checkComplete(patched, t); err != nil {
		return err
	}
	if !changed {
		return fmt.Errorf("found no unimplemented stub for %s in %s — already generated?", t.typeName, path)
	}
	return nil
}

// checkComplete reports the CRUD methods the patched file does not actually implement — missing
// outright, or present with a body that still panics. It lives here rather than in patchSource
// because it is a claim about the whole gqlgen-generated file — that all five are live — not
// about one splice: patchSource is happy to fill whatever subset it is given. Without it a partial
// patch is reported as success, and a partially patched file still *compiles* (a stub is valid
// Go), so the surviving panic only fires on the first query that reaches it, in production — the
// failure docs/gqlgen-contract.md calls the most important operational fact about this seam.
// inputFields is irrelevant to a name-only check.
//
// The still-panics half covers what isStub deliberately walks past. A hand-written
// panic("TODO: needs an ownership predicate before this ships") is a body somebody wrote, so
// patchSource must not overwrite it — but finishing the run silently means cmd/luimagen prints
// "filled Get/List/Create/Update/Delete" over a method that is still a live panic. Left alone is
// the right edit; reported as filled is not.
func checkComplete(src []byte, t *modelTable) error {
	file, err := parser.ParseFile(token.NewFileSet(), "", src, 0)
	if err != nil {
		return fmt.Errorf("re-parsing patched source: %w", err)
	}
	var missing, panics []string
	for _, tg := range targets(t, "model", nil) {
		fn := findFunc(file, tg.recv, tg.name)
		switch {
		case fn == nil:
			missing = append(missing, tg.recv+"."+tg.name)
		case panicsSomewhere(fn):
			panics = append(panics, tg.recv+"."+tg.name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("the resolver file declares no %s — either the SDL fragment landed in a file gqlgen.yml does not glob, or gqlgen wrote the stubs elsewhere (under resolver.layout: follow-schema they land beside the schema file, so Options.ResolverFile has to follow Options.SchemaFile); a missing method is a field that panics at query time", strings.Join(missing, ", "))
	}
	if len(panics) > 0 {
		return fmt.Errorf("%s still panic(s) — luimagen left the hand-written body alone (it is not gqlgen's \"not implemented\" stub), so that method compiles and panics at query time; every other stub is already patched and saved, so fill this one in by hand — a re-run would fail at the duplicate-type guard unless you first undo the whole run (docs/luimagen.md §2.4)", strings.Join(panics, ", "))
	}
	return nil
}

// panicsSomewhere reports whether fn's body contains a call to the builtin panic anywhere — not
// just as the whole body, which is all isStub looks at. A deliberate placeholder can be guarded,
// logged first, or nested in a switch; every shape of it is a method that compiles and dies at
// query time. Shadowing panic with a local of that name is not a thing anyone does in a generated
// resolver file, and the false positive would only be a loud error, never a silent splice.
func panicsSomewhere(fn *ast.FuncDecl) bool {
	if fn.Body == nil {
		return false
	}
	found := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "panic" {
			found = true
			return false
		}
		return true
	})
	return found
}

// edit is one byte-range splice: replace src[start:end] with text. Offsets are into the
// original source, so callers apply a sorted set back-to-front to keep earlier offsets valid.
type edit struct {
	start, end int
	text       string
}

func patchSource(src []byte, t *modelTable, modelPkg string, inputFields map[string]string) ([]byte, bool, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "", src, parser.ParseComments)
	if err != nil {
		return nil, false, err
	}

	var edits []edit

	for _, tg := range targets(t, modelPkg, inputFields) {
		fn := findFunc(file, tg.recv, tg.name)
		if fn == nil || !isStub(fn) {
			continue
		}
		params := paramNames(fn)
		if len(params) != tg.arity {
			return nil, false, fmt.Errorf("%s takes %d parameter(s) after ctx, want %d — that is not the signature luimagen's SDL generates", tg.name, len(params), tg.arity)
		}
		body := tg.body(params)
		edits = append(edits, edit{
			start: fset.Position(fn.Body.Lbrace).Offset + 1,
			end:   fset.Position(fn.Body.Rbrace).Offset,
			text:  "\n\t" + body + "\n",
		})
	}
	if len(edits) == 0 {
		return src, false, nil
	}

	slices.SortFunc(edits, func(a, b edit) int { return cmp.Compare(b.start, a.start) })
	out := string(src)
	for _, e := range edits { // back-to-front so earlier offsets stay valid after each splice
		out = out[:e.start] + e.text + out[e.end:]
	}

	// The import fix runs on a re-parse of the spliced source, not the original AST: the new
	// bodies are what actually reference fmt (a non-string PK label) or stop referencing it
	// (the replaced stubs), so the usage walk must see the post-splice tree.
	fset2 := token.NewFileSet()
	file2, err := parser.ParseFile(fset2, "", []byte(out), parser.ParseComments)
	if err != nil {
		return nil, false, fmt.Errorf("re-parsing patched source: %w", err)
	}
	ies, err := importEdits([]byte(out), fset2, file2)
	if err != nil {
		return nil, false, err
	}
	slices.SortFunc(ies, func(a, b edit) int { return cmp.Compare(b.start, a.start) })
	for _, ie := range ies { // back-to-front, same reason as the body splices above
		out = out[:ie.start] + ie.text + out[ie.end:]
	}

	formatted, err := format.Source([]byte(out))
	if err != nil {
		return nil, false, fmt.Errorf("formatting patched source: %w", err)
	}
	return formatted, true, nil
}

// findFunc finds the method named name on receiver recv. The comparison is case-insensitive
// because gqlgen derives the method name from the SDL field name through templates.ToGo, which
// re-cases common initialisms and nothing else — for a delimiter-free name wordWalker never
// inserts, drops or substitutes a rune, so two names differing only in case are the same method.
// An exact match misses (Options.Type "URL" -> SDL field uRL -> method URl) and silently patches
// only some of the five stubs.
func findFunc(file *ast.File, recv, name string) *ast.FuncDecl {
	for _, d := range file.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || !strings.EqualFold(fn.Name.Name, name) || fn.Recv == nil || len(fn.Recv.List) == 0 {
			continue
		}
		if recvTypeName(fn.Recv.List[0].Type) == recv {
			return fn
		}
	}
	return nil
}

func recvTypeName(e ast.Expr) string {
	if star, ok := e.(*ast.StarExpr); ok {
		e = star.X
	}
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

// isStub reports whether fn's body is still exactly gqlgen's
// panic(fmt.Errorf("not implemented: ...")) — docs/gqlgen-contract.md:21-27. The panic *argument*
// is what makes this a stub check rather than a panic check, and it is the whole "a method already
// filled in by hand is left alone" guarantee: a deliberate placeholder like
// panic("TODO: needs an ownership predicate before this ships") is a body somebody wrote, and
// replacing it with an unscoped luima.Delete is exactly the silent damage this must not do.
func isStub(fn *ast.FuncDecl) bool {
	if fn.Body == nil || len(fn.Body.List) != 1 {
		return false
	}
	es, ok := fn.Body.List[0].(*ast.ExprStmt)
	if !ok {
		return false
	}
	call, ok := es.X.(*ast.CallExpr)
	if !ok {
		return false
	}
	if id, ok := call.Fun.(*ast.Ident); !ok || id.Name != "panic" || len(call.Args) != 1 {
		return false
	}
	return strings.HasPrefix(panicMessage(call.Args[0]), "not implemented")
}

// panicMessage returns the string literal panic was called with: gqlgen writes
// panic(fmt.Errorf("not implemented: X - x")), so one level of wrapping call is unwrapped, and a
// plain panic("not implemented") counts too. Anything else — a variable, a concatenation, a
// non-literal — returns "" and is therefore not a stub.
func panicMessage(arg ast.Expr) string {
	if call, ok := arg.(*ast.CallExpr); ok {
		if len(call.Args) == 0 {
			return ""
		}
		arg = call.Args[0]
	}
	lit, ok := arg.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return ""
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		return ""
	}
	return s
}

// paramNames returns fn's parameter names in order, dropping the leading ctx — every gqlgen
// resolver method's own convention, not luimagen's. Dropping the first parameter *field* rather
// than the first *name* is the same thing for any signature Go accepts — named and unnamed
// parameters cannot be mixed — but it says what it means, and a signature with no names at all
// then yields nothing for patchSource's arity guard to catch, instead of silently dropping the
// first real argument.
func paramNames(fn *ast.FuncDecl) []string {
	var names []string
	for i, f := range fn.Type.Params.List {
		if i == 0 {
			continue // ctx
		}
		for _, n := range f.Names {
			names = append(names, n.Name)
		}
	}
	return names
}

// imp is one entry of the import-block rewrite: an optional alias (gqlgen writes none) and
// the import path.
type imp struct {
	name string
	path string
}

// importEdits computes the edits that rewrite the file's import declarations so that the two
// paths the patched bodies need are imported, a "fmt" import no surviving code uses is dropped
// (and one is added when the new bodies use it), and the result is goimports-canonical: one block,
// stdlib group, blank line, third-party group, each sorted by path. Every decision comes from the
// AST of the already-patched source — whether a path is already imported (compared on the quoted
// literal, not a substring), which group a path belongs to, and whether any identifier named fmt
// survives — while the replacement itself is text, spliced like every other edit. Import paths are
// *ast.BasicLit, not idents, so a comment or string literal mentioning fmt.Errorf can neither keep
// a dead import alive nor matter to the comparison.
//
// Specs are collected from every import declaration and merged into the first, and the rest are
// deleted. Reading only the first block makes an import declared in a second one invisible, so
// luima gets appended to the first while the second still declares it — "luima redeclared", a
// compile error in the consumer's module. Merging is also what prunes a dead "fmt" living in a
// later block.
func importEdits(src []byte, fset *token.FileSet, file *ast.File) ([]edit, error) {
	var decls []*ast.GenDecl
	var imports []imp
	for _, d := range file.Decls {
		g, ok := d.(*ast.GenDecl)
		if !ok || g.Tok != token.IMPORT {
			continue
		}
		decls = append(decls, g)
		for _, sp := range g.Specs {
			is, ok := sp.(*ast.ImportSpec)
			if !ok {
				continue
			}
			path, err := strconv.Unquote(is.Path.Value)
			if err != nil {
				continue
			}
			name := ""
			if is.Name != nil {
				name = is.Name.Name
			}
			imports = append(imports, imp{name: name, path: path})
		}
	}
	if len(decls) == 0 {
		return nil, nil
	}

	// Drop any "fmt" whose binding is dead after the splice — after a full patch pass nothing
	// uses gqlgen's, and an unused import breaks the consumer's build — then add an unaliased one
	// when the new bodies do use it: a non-string PK label goes through fmt.Sprintf. The drop is
	// keyed on the *binding* name, not the path, and that is what stops an aliased `f "fmt"` from
	// surviving unused beside a freshly added "fmt": two imports of one path is legal Go, an
	// unreferenced `f` is not. Backwards, so the delete does not skip the next entry.
	for i := len(imports) - 1; i >= 0; i-- {
		if imports[i].path != "fmt" {
			continue
		}
		name := imports[i].name
		if name == "" {
			name = "fmt"
		}
		if !usesIdent(file, name) {
			imports = append(imports[:i], imports[i+1:]...)
		}
	}
	// Each import is added only when a surviving identifier names its package: every body
	// references luima, but only List's q.Order closure references orm — so a hand-written List
	// left alone (a body gqlgen preserved) must not drag in an orm import nothing uses, or the
	// consumer's build breaks on an unused import. fmt rides the same loop, after the drop above.
	//
	// The add is keyed on the *name* being free, not only on the path being absent. A resolver file
	// that already binds luima (or orm, or fmt) to some other path — an alias a hand-written body
	// uses — would otherwise get a second, unaliased import of that name spliced in beside it, and
	// "luima redeclared in this block" is a build failure in the consumer's module that nothing
	// here would catch: format.Source does not typecheck.
	for _, need := range []struct{ path, ident string }{
		{"fmt", "fmt"},
		{"github.com/ulas96/luima", "luima"},
		{"github.com/go-pg/pg/v10/orm", "orm"},
	} {
		if hasUnaliasedImport(imports, need.path) || !usesIdent(file, need.ident) {
			continue
		}
		if other, bound := boundPath(imports, need.ident); bound {
			return nil, fmt.Errorf("the resolver file already binds %s to %q, so the generated bodies cannot reference %s under that name — alias the other import differently and re-run, or write these five bodies by hand", need.ident, other, need.path)
		}
		imports = append(imports, imp{path: need.path})
	}

	std, third := splitGroups(imports)
	repl := renderImportBlock(std, third)
	start, end := fset.Position(decls[0].Pos()).Offset, fset.Position(decls[0].End()).Offset

	var edits []edit
	if repl != string(src[start:end]) || len(decls) > 1 {
		edits = append(edits, edit{start: start, end: end, text: repl}) // else already canonical — don't churn the bytes
	}
	for _, d := range decls[1:] { // merged into the first above; leaving one would redeclare
		edits = append(edits, edit{start: fset.Position(d.Pos()).Offset, end: fset.Position(d.End()).Offset})
	}
	return edits, nil
}

// boundPath reports which import path binds name in this import block, if any. An unaliased import
// binds the last element of its path — a heuristic, since a package may be named something else
// entirely — but a wrong answer only ever turns an add into a clear error, or an error into the
// compile failure it was already going to be.
func boundPath(imports []imp, name string) (string, bool) {
	for _, im := range imports {
		n := im.name
		if n == "" {
			n = im.path
			if i := strings.LastIndex(im.path, "/"); i >= 0 {
				n = im.path[i+1:]
			}
		}
		if n == name {
			return im.path, true
		}
	}
	return "", false
}

// usesIdent reports whether any identifier named name appears anywhere in file. Import
// specs are skipped — their path literals are *ast.BasicLit anyway, but an aliased name inside
// one must not count as a use.
func usesIdent(file *ast.File, name string) bool {
	used := false
	ast.Inspect(file, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.ImportSpec:
			return false
		case *ast.Ident:
			if x.Name != name {
				return true
			}
			used = true
			return false
		}
		return true
	})
	return used
}

// hasUnaliasedImport reports whether path is imported under its own package name — the only form
// a spliced body can use. Keying an add on the path alone is the bug: a surviving `l
// "github.com/ulas96/luima"` or `f "fmt"` — one a hand-written body still references — satisfies
// "already imported", so nothing is added while every spliced body calls luima.Get or
// fmt.Sprintf, which are undefined: luima and undefined: fmt. Two imports of one path is legal
// Go; a body naming a package that is not bound is not.
func hasUnaliasedImport(imports []imp, path string) bool {
	for _, im := range imports {
		if im.path == path && im.name == "" {
			return true
		}
	}
	return false
}

// splitGroups partitions imports into stdlib (first path element contains no dot) and
// third-party, each sorted by path — the two groups goimports separates with a blank line.
func splitGroups(imports []imp) (std, third []imp) {
	for _, im := range imports {
		if isStdlib(im.path) {
			std = append(std, im)
		} else {
			third = append(third, im)
		}
	}
	slices.SortFunc(std, func(a, b imp) int { return cmp.Compare(a.path, b.path) })
	slices.SortFunc(third, func(a, b imp) int { return cmp.Compare(a.path, b.path) })
	return std, third
}

// ponytail: the dot heuristic misfiles a dotless consumer module path — `module myapp` importing
// "myapp/graph/model" groups with stdlib. Cosmetic only: the file still compiles, and gqlgen runs
// goimports on its next generate, which moves it back. Compare against the file's own module path
// the day that churn shows up in a real diff.
func isStdlib(path string) bool {
	first, _, _ := strings.Cut(path, "/")
	return first != "" && !strings.Contains(first, ".")
}

// renderImportBlock renders a canonical parenthesized import block: stdlib group, blank line,
// third-party group, each sorted by path. Comments inside the original block are dropped — the
// file is gqlgen-owned, and gqlgen rewrites the block through goimports on its own next run.
//
// ponytail: two groups, which is plain goimports. A consumer running goimports with a
// -local prefix (this repo's own .golangci.yml sets one) wants a third group and will see a lint
// finding on generated code until their next `gqlgen generate` rewrites it. Splitting a third
// group needs the consumer's prefix, which luimagen is not told — add an Options field the day
// somebody is actually blocked by the finding rather than guessing the prefix from the module path.
func renderImportBlock(std, third []imp) string {
	var b strings.Builder
	b.WriteString("import (")
	writeGroup := func(group []imp) {
		for _, im := range group {
			b.WriteString("\n\t")
			if im.name != "" {
				b.WriteString(im.name + " ")
			}
			b.WriteString(strconv.Quote(im.path))
		}
	}
	writeGroup(std)
	if len(std) > 0 && len(third) > 0 {
		b.WriteString("\n")
	}
	writeGroup(third)
	b.WriteString("\n)")
	return b.String()
}
