package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestBG19_MainLaunchesAllBackgroundJobsViaSafeGo locks the BG-19 wiring the
// way the authz meta test locks router wrappers: it parses main.go and fails
// if any background job is launched with a bare `go worker.Start(ctx)`-style
// statement instead of safego.Go. A bare launch has no panic recovery, so one
// panicking ticker tick would kill (and then crash-loop) the whole head.
//
// Allowed bare `go` statements are function literals only — the two fail-fast
// server goroutines (HTTP ListenAndServe / gRPC Serve), which intentionally
// exit the process when their listener dies at startup. Everything else must
// route through safego.Go, including jobs added after this test was written.
// TestBG21e_MainWiresFinalizationTxRunner locks the ★BG-21e wiring: main.go's validation
// engine chain must call WithTxRunner(validation.NewPgxFinalizationTxRunner(...)) on the
// engine built by validation.NewEngine. This wiring shipped absent once — every atomicity
// test hand-injected the runner, so the deployed head silently ran the non-transactional
// passthrough (marks, VALIDATED flip, and credit rows as separate autocommits). The runtime
// twin of this guard is the boot assertion in server.newTransitioner; this test catches the
// regression at CI time, before a binary exists to boot.
func TestBG21e_MainWiresFinalizationTxRunner(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("failed to parse main.go: %v", err)
	}

	// chainRoot unwraps a method-chain receiver (x.M1().M2()...) to its innermost call and
	// reports whether that call is validation.NewEngine.
	rootsAtNewEngine := func(expr ast.Expr) bool {
		for {
			call, ok := expr.(*ast.CallExpr)
			if !ok {
				return false
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return false
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "validation" && sel.Sel.Name == "NewEngine" {
				return true
			}
			expr = sel.X
		}
	}

	wired := false
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "WithTxRunner" {
			return true
		}
		if !rootsAtNewEngine(sel.X) {
			return true
		}
		// The argument must be the production runner, not a stub.
		if len(call.Args) != 1 {
			return true
		}
		argCall, ok := call.Args[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		argSel, ok := argCall.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if pkg, ok := argSel.X.(*ast.Ident); ok && pkg.Name == "validation" && argSel.Sel.Name == "NewPgxFinalizationTxRunner" {
			wired = true
		}
		return true
	})

	if !wired {
		t.Fatal("main.go builds the validation engine without .WithTxRunner(validation.NewPgxFinalizationTxRunner(pool)): " +
			"the deployed head would finalize NON-atomically (★BG-21e) — marks, the VALIDATED flip, and credit rows " +
			"as separate autocommits. Wire the production runner into the validation.NewEngine chain.")
	}
}

func TestBG19_MainLaunchesAllBackgroundJobsViaSafeGo(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("failed to parse main.go: %v", err)
	}

	var bareLaunches []string
	var funcLitLaunches int
	ast.Inspect(f, func(n ast.Node) bool {
		goStmt, ok := n.(*ast.GoStmt)
		if !ok {
			return true
		}
		if _, isLit := goStmt.Call.Fun.(*ast.FuncLit); isLit {
			funcLitLaunches++
			return true
		}
		pos := fset.Position(goStmt.Pos())
		bareLaunches = append(bareLaunches, pos.String())
		return true
	})

	if len(bareLaunches) > 0 {
		t.Errorf("main.go launches background work without panic recovery (use safego.Go): %v", bareLaunches)
	}
	if funcLitLaunches > 2 {
		t.Errorf("main.go has %d bare `go func(){...}` launches; only the two fail-fast server goroutines are allowed — route new background jobs through safego.Go", funcLitLaunches)
	}
}
