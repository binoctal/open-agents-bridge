package bridge

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// Guard: every branch of handlePermissionRequest that resolves a permission
// request locally must also report the decision to the server.
//
// The gap this closes was never a coding mistake — it was a branch nobody
// thought to record. Requests the bridge's rules engine answers never reach the
// server at all, so the WebSocket path that records every other approval cannot
// see them: a device with permissive rules could auto-approve dangerous tool
// calls all day and `permission_requests` stayed empty. On a report that reads
// as "nothing happened", not "nothing was recorded".
//
// Branches here multiply over time (the user's own rules, blocking command
// alerts, org policy next), so the invariant cannot rest on whoever adds the
// next one remembering. Asserting merely that the file mentions
// reportPermissionDecision would not help: a new unreported branch would keep
// the test green. So this walks the actual `switch outcome` and checks each
// case body.
//
// When it fails, add the report to the branch it names. There is nothing to
// widen.
func TestEveryLocalPermissionOutcomeIsReported(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "command_alerts.go", nil, 0)
	if err != nil {
		t.Fatalf("parse command_alerts.go: %v", err)
	}

	sw := findOutcomeSwitch(t, file)

	checked := 0
	for _, stmt := range sw.Body.List {
		clause, ok := stmt.(*ast.CaseClause)
		if !ok {
			continue
		}
		// The default clause forwards the request to the web client, which puts
		// it through UserRoom and onto the normal recording path.
		if clause.List == nil {
			continue
		}
		checked++

		name := caseName(clause)
		if !bodyCalls(clause.Body, "reportPermissionDecision") {
			t.Errorf(
				"case %s resolves the request locally but never reports it: "+
					"the server never sees this request, so the decision leaves no trace anywhere",
				name,
			)
		}
		if !bodyCalls(clause.Body, "Resolve") {
			t.Errorf("case %s no longer resolves the request — is this guard still looking at the right switch?", name)
		}
	}

	// Non-vacuity: if the switch stops having decided branches, the loop above
	// asserts nothing and passes silently.
	if checked < 2 {
		t.Fatalf("expected at least 2 locally-decided branches, found %d", checked)
	}
}

// findOutcomeSwitch returns the `switch outcome` inside handlePermissionRequest.
func findOutcomeSwitch(t *testing.T, file *ast.File) *ast.SwitchStmt {
	t.Helper()

	var fn *ast.FuncDecl
	for _, decl := range file.Decls {
		d, ok := decl.(*ast.FuncDecl)
		if ok && d.Name.Name == "handlePermissionRequest" {
			fn = d
			break
		}
	}
	if fn == nil {
		t.Fatal("handlePermissionRequest not found in command_alerts.go")
	}

	var found *ast.SwitchStmt
	ast.Inspect(fn, func(n ast.Node) bool {
		sw, ok := n.(*ast.SwitchStmt)
		if !ok {
			return true
		}
		if ident, ok := sw.Tag.(*ast.Ident); ok && ident.Name == "outcome" {
			found = sw
			return false
		}
		return true
	})
	if found == nil {
		t.Fatal("`switch outcome` not found in handlePermissionRequest")
	}
	return found
}

func caseName(clause *ast.CaseClause) string {
	names := make([]string, 0, len(clause.List))
	for _, expr := range clause.List {
		if ident, ok := expr.(*ast.Ident); ok {
			names = append(names, ident.Name)
			continue
		}
		names = append(names, "<expr>")
	}
	return strings.Join(names, ", ")
}

// bodyCalls reports whether any statement in the body calls the named function
// or method, including inside a `go` statement.
func bodyCalls(body []ast.Stmt, name string) bool {
	found := false
	for _, stmt := range body {
		ast.Inspect(stmt, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fn := call.Fun.(type) {
			case *ast.SelectorExpr:
				if fn.Sel.Name == name {
					found = true
				}
			case *ast.Ident:
				if fn.Name == name {
					found = true
				}
			}
			return !found
		})
		if found {
			return true
		}
	}
	return false
}
