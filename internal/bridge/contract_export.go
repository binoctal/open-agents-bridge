package bridge

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ExtractDispatchTypes walks the non-test Go sources of sourceDir and returns
// the sorted, de-duplicated string literals of every `switch <ident>.Type`
// dispatch — the bridge's WS message vocabulary. AST-based (formatting
// immune, catches multi-label cases a grep misses) and the single extractor
// shared by `scripts/dump-message-types` (which writes the JSON artifact the
// main repo's contracts package pins) and contract_export_test.go (which
// fails when the artifact is stale). G23 / parity rule 3.
func ExtractDispatchTypes(sourceDir string) ([]string, error) {
	files, err := parseGoSources(sourceDir)
	if err != nil {
		return nil, err
	}
	types := map[string]bool{}
	for _, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			sw, ok := n.(*ast.SwitchStmt)
			if !ok {
				return true
			}
			// Only dispatch switches: tag reads <ident>.Type (msg.Type and
			// friends). Other switches (permission actions, stop reasons,
			// cli types) are not part of the wire vocabulary.
			sel, ok := sw.Tag.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Type" {
				return true
			}
			for _, stmt := range sw.Body.List {
				cc, ok := stmt.(*ast.CaseClause)
				if !ok {
					continue
				}
				for _, expr := range cc.List {
					if lit, ok := expr.(*ast.BasicLit); ok && lit.Kind == token.STRING {
						var s string
						if err := json.Unmarshal([]byte(lit.Value), &s); err == nil && s != "" {
							types[s] = true
						}
					}
				}
			}
			return true
		})
	}
	out := make([]string, 0, len(types))
	for t := range types {
		out = append(out, t)
	}
	sort.Strings(out)
	return out, nil
}

// ExtractAgentStatuses returns the sorted string values of every constant of
// type AgentStatus in sourceDir (protocol/types.go) — the six-state
// vocabulary pinned as AGENT_STATUSES in the contracts package and mirrored
// into bridge-message-types.json, so a status renamed or added on either end
// without regenerating fails both suites (G23).
func ExtractAgentStatuses(sourceDir string) ([]string, error) {
	files, err := parseGoSources(sourceDir)
	if err != nil {
		return nil, err
	}
	statuses := map[string]bool{}
	for _, file := range files {
		for _, decl := range file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			// Const blocks can share one type declaration: the type rides the
			// first ValueSpec and later specs omit it.
			blockType := ""
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				if vs.Type != nil {
					if id, ok := vs.Type.(*ast.Ident); ok {
						blockType = id.Name
					} else {
						blockType = ""
					}
				}
				if blockType != "AgentStatus" {
					continue
				}
				for _, val := range vs.Values {
					if lit, ok := val.(*ast.BasicLit); ok && lit.Kind == token.STRING {
						var s string
						if err := json.Unmarshal([]byte(lit.Value), &s); err == nil && s != "" {
							statuses[s] = true
						}
					}
				}
			}
		}
	}
	out := make([]string, 0, len(statuses))
	for s := range statuses {
		out = append(out, s)
	}
	sort.Strings(out)
	return out, nil
}

func parseGoSources(sourceDir string) ([]*ast.File, error) {
	entries, err := os.ReadDir(sourceDir)
	if err != nil {
		return nil, fmt.Errorf("read dir: %w", err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(sourceDir, name), nil, 0)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}
		files = append(files, file)
	}
	return files, nil
}
