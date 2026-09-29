package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The copy-close census.
//
// Every path that closes a copy (sets the outcome on a live work_unit_assignment_history row)
// changes what the dispatch cache must believe: the closer's in-memory hold is gone, its
// machine holds one copy fewer, and a RETURNED / ABANDONED / EXPIRED close starts a cooldown
// the SQL landing enforces, so the still-staged candidate must bench the closer too. A close
// path that does not tell the cache leaves it re-offering the unit to a volunteer the landing
// will refuse, or counting a hold that no longer exists. That has happened once per new close
// path (the give-back, the deadline reaper, the transitioner, the held-copy reconcile), each
// fixed by wiring one more caller. This test makes the wiring a checked property instead:
//
//  1. It finds every SQL statement in the head that closes a copy, and fails when one appears
//     that is not listed in copyCloseStatements.
//  2. It finds every function that calls one of those repository methods, and fails unless the
//     same function also tells the cache (one of cacheCloseHooks), or is listed below with the
//     reason it need not.
//
// A new close path therefore fails here until it is routed through the cache's close hooks,
// or its exemption is written down beside the others.

// copyCloseStatementRE matches SQL that closes (or re-classifies) a copy: an UPDATE of the
// copy table that sets its outcome.
var copyCloseStatementRE = regexp.MustCompile(`(?is)\bUPDATE\s+work_unit_assignment_history\b.*?\bSET\b.*?\boutcome\s*=`)

// copyCloseStatements maps each function whose SQL closes copies to the method name callers
// reach it by.
var copyCloseStatements = map[string]string{
	"assignment.(*PgxRepository).UpdateOutcome":                "UpdateOutcome",
	"workunit.(*PgxWorkUnitRepository).CloseCopy":              "CloseCopy",
	"workunit.(*PgxWorkUnitRepository).CloseCopyByVolunteer":   "CloseCopyByVolunteer",
	"workunit.(*PgxWorkUnitRepository).ExpireLiveCopies":       "ExpireLiveCopies",
	"workunit.(*PgxWorkUnitRepository).ReleaseStaleHeldCopies": "ReleaseStaleHeldCopies",
	"workunit.(*PgxWorkUnitRepository).ReviveDeadLettered":     "ReviveDeadLettered",
}

// cacheCloseHooks are the calls that tell a dispatch cache about closed copies: the per-copy
// close (closeCopyLocked, reached through onCopyClosed and the fault monitor's copyClosed) and
// the whole-unit evictions (InvalidateWorkUnit, reached through the handlers' and the
// transitioner's invalidateDispatch, and onUnitDone).
var cacheCloseHooks = map[string]bool{
	"closeCopyLocked":    true,
	"onCopyClosed":       true,
	"copyClosed":         true,
	"InvalidateWorkUnit": true,
	"invalidateDispatch": true,
	"onUnitDone":         true,
}

// copyCloseWrappers close copies and leave the cache to their callers, which the census then
// checks in their place.
var copyCloseWrappers = map[string]string{
	"transition.(*Transitioner).decideAndApply":         "Evaluate, its caller, evicts the unit from the cache once the decision commits",
	"workunit.(*WorkUnitHandler).closeActiveAssignment": "its callers are checked instead (it has none today)",
}

// copyCloseExempt close copies without telling any cache, for the reason given.
var copyCloseExempt = map[string]string{
	"server.handleBrowserSubmitResult":                "browser copies are made by the database dispatch path and never held by a cache; the copy it closes had run-started, so no hold exists, and the reconcile recounts the machine's in-flight copies",
	"workunit.(*EnforcementDemoter).DemoteAndRequeue": "the unit is VALIDATED when its straggler copies close, and no cache stages or holds a unit that is not QUEUED; it re-enters dispatch only through a fresh refill",
}

type censusFunc struct {
	name  string
	pos   string
	decl  *ast.FuncDecl
	fname string // the bare function or method name
}

// censusSourceFuncs parses every non-test Go file of the head (internal/ and cmd/) and returns
// its function declarations.
func censusSourceFuncs(t *testing.T) []censusFunc {
	t.Helper()
	fset := token.NewFileSet()
	var out []censusFunc
	for _, root := range []string{"..", filepath.Join("..", "..", "cmd")} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				return perr
			}
			for _, decl := range f.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok || fd.Body == nil {
					continue
				}
				name := f.Name.Name + "."
				if fd.Recv != nil && len(fd.Recv.List) == 1 {
					name += "(" + receiverName(fd.Recv.List[0].Type) + ")."
				}
				name += fd.Name.Name
				out = append(out, censusFunc{name: name, pos: fset.Position(fd.Pos()).String(), decl: fd, fname: fd.Name.Name})
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	if len(out) == 0 {
		t.Fatalf("census found no source functions; wrong working directory?")
	}
	return out
}

func receiverName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.StarExpr:
		return "*" + receiverName(x.X)
	case *ast.Ident:
		return x.Name
	case *ast.IndexExpr:
		return receiverName(x.X)
	case *ast.IndexListExpr:
		return receiverName(x.X)
	}
	return "?"
}

// closesCopies reports whether a function body carries a copy-closing SQL statement. A string
// built from several literals joined with + is judged whole, with its non-literal parts
// replaced by a placeholder.
func closesCopies(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if found {
			return false
		}
		switch x := n.(type) {
		case *ast.BinaryExpr:
			if x.Op != token.ADD {
				return true
			}
			var sb strings.Builder
			if !flattenStringConcat(x, &sb) {
				return true
			}
			if copyCloseStatementRE.MatchString(sb.String()) {
				found = true
			}
			return false
		case *ast.BasicLit:
			if x.Kind == token.STRING {
				if s, err := strconv.Unquote(x.Value); err == nil && copyCloseStatementRE.MatchString(s) {
					found = true
				}
			}
		}
		return true
	})
	return found
}

// flattenStringConcat writes a + chain's string literals in order, with " ? " for any other
// operand. It reports false when the chain holds no string literal at all.
func flattenStringConcat(e ast.Expr, sb *strings.Builder) bool {
	switch x := e.(type) {
	case *ast.BinaryExpr:
		if x.Op == token.ADD {
			l := flattenStringConcat(x.X, sb)
			r := flattenStringConcat(x.Y, sb)
			return l || r
		}
	case *ast.ParenExpr:
		return flattenStringConcat(x.X, sb)
	case *ast.BasicLit:
		if x.Kind == token.STRING {
			if s, err := strconv.Unquote(x.Value); err == nil {
				sb.WriteString(s)
				return true
			}
		}
	}
	sb.WriteString(" ? ")
	return false
}

// calledNames returns the names of every function or method a body calls.
func calledNames(body *ast.BlockStmt) map[string]bool {
	out := make(map[string]bool)
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.SelectorExpr:
			out[fn.Sel.Name] = true
		case *ast.Ident:
			out[fn.Name] = true
		}
		return true
	})
	return out
}

func TestCopyCloseCensus(t *testing.T) {
	funcs := censusSourceFuncs(t)

	// 1. Every copy-closing statement is known.
	found := make(map[string]string)
	for _, f := range funcs {
		if closesCopies(f.decl.Body) {
			found[f.name] = f.pos
		}
	}
	for name, pos := range found {
		if _, ok := copyCloseStatements[name]; !ok {
			t.Errorf("%s (%s) closes copies in SQL but is not in the copy-close census: add it to "+
				"copyCloseStatements, and make every caller tell the dispatch cache", name, pos)
		}
	}
	for name := range copyCloseStatements {
		if _, ok := found[name]; !ok {
			t.Errorf("copyCloseStatements lists %s, which no longer closes copies in SQL: remove it", name)
		}
	}

	// 2. Every caller tells the cache, or is written down.
	closeMethods := make(map[string]bool)
	for _, m := range copyCloseStatements {
		closeMethods[m] = true
	}
	for name := range copyCloseWrappers {
		closeMethods[name[strings.LastIndex(name, ".")+1:]] = true
	}
	listed := make(map[string]bool)
	var bypass []string
	for _, f := range funcs {
		if _, impl := copyCloseStatements[f.name]; impl {
			continue
		}
		calls := calledNames(f.decl.Body)
		var closes []string
		for m := range closeMethods {
			if calls[m] && m != f.fname {
				closes = append(closes, m)
			}
		}
		if len(closes) == 0 {
			continue
		}
		if _, ok := copyCloseWrappers[f.name]; ok {
			listed[f.name] = true
			continue
		}
		if _, ok := copyCloseExempt[f.name]; ok {
			listed[f.name] = true
			continue
		}
		hooked := false
		for h := range cacheCloseHooks {
			if calls[h] {
				hooked = true
				break
			}
		}
		if !hooked {
			sort.Strings(closes)
			bypass = append(bypass, f.name+" ("+f.pos+") calls "+strings.Join(closes, ", "))
		}
	}
	sort.Strings(bypass)
	for _, b := range bypass {
		t.Errorf("copy close path bypasses the dispatch cache: %s without calling any of its close hooks "+
			"(closeCopyLocked / onCopyClosed / copyClosed / InvalidateWorkUnit / invalidateDispatch / onUnitDone). "+
			"Route it through the cache, or add it to the census with the reason it need not.", b)
	}
	for name := range copyCloseWrappers {
		if !listed[name] {
			t.Errorf("copyCloseWrappers lists %s, which no longer closes copies: remove it", name)
		}
	}
	for name := range copyCloseExempt {
		if !listed[name] {
			t.Errorf("copyCloseExempt lists %s, which no longer closes copies: remove it", name)
		}
	}
}
