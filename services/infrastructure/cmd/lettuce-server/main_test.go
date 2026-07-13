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
