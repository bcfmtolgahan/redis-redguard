/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package utils

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// directClusterCommands lists the places in dir where an exec.Command call
// names kubectl or helm outright. Such a call takes its target from the ambient
// environment; the pinned equivalent goes through Cluster.Command, which passes
// the binary in a variable and is therefore never reported here.
func directClusterCommands(dir string) ([]string, error) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, nil, 0)
	if err != nil {
		return nil, err
	}

	var found []string
	for _, pkg := range pkgs {
		for path, file := range pkg.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				bin, ok := commandBinary(call)
				if !ok {
					return true
				}
				switch binaryName(bin) {
				case "kubectl", "helm":
					found = append(found, fmt.Sprintf("%s:%d: exec.Command(%q, ...)",
						filepath.Base(path), fset.Position(call.Pos()).Line, bin))
				}
				return true
			})
		}
	}
	return found, nil
}

// commandBinary returns the literal binary an exec.Command or
// exec.CommandContext call names, if it names one literally at all.
func commandBinary(call *ast.CallExpr) (string, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "exec" {
		return "", false
	}

	arg := 0
	switch sel.Sel.Name {
	case "Command":
	case "CommandContext":
		arg = 1
	default:
		return "", false
	}
	if len(call.Args) <= arg {
		return "", false
	}

	lit, ok := call.Args[arg].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	value, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return value, true
}

func TestDetectorFindsADirectClusterCommand(t *testing.T) {
	dir := t.TempDir()
	body := `package sample

import "os/exec"

func run() {
	_ = exec.Command("kubectl", "delete", "namespace", "redguard-system")
	_ = exec.Command("/opt/homebrew/bin/helm", "uninstall", "redguard")
	_ = exec.Command("kind", "delete", "cluster", "--name", "x")
}
`
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte(body), 0o600); err != nil {
		t.Fatalf("write sample: %v", err)
	}

	found, err := directClusterCommands(dir)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(found) != 2 {
		t.Fatalf("detector reported %d direct commands, want 2: %v", len(found), found)
	}
}

// TestNoDirectClusterCommands is the audit itself: every kubectl and helm
// invocation in the harness has to be built by Cluster.Command, which names
// this run's kubeconfig and context on the command line. One call site that
// spells the binary out reopens the hole this guard exists to close.
func TestNoDirectClusterCommands(t *testing.T) {
	for _, dir := range []string{".", "../e2e"} {
		found, err := directClusterCommands(dir)
		if err != nil {
			t.Fatalf("scan %s: %v", dir, err)
		}
		for _, hit := range found {
			t.Errorf("%s/%s builds a cluster command outside Cluster.Command", dir, hit)
		}
	}
}
