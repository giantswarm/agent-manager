package cmd

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// serve starts the service's background work (the sweep of pending approval
// grant syncs, M2-S3-R-07): runServe calls svc.Start.
func TestServeStartsTheService(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "serve.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Start" {
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == "svc" {
				found = true
			}
		}
		return true
	})
	if !found {
		t.Fatal("serve.go must call svc.Start: pending grant syncs are swept at start")
	}
}
