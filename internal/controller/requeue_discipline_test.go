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

package controller

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoReconcileReturnsErrorWithRequeueAfter scans this package for
// `return ctrl.Result{RequeueAfter: X}, err`. controller-runtime inspects the
// error first and requeues through the rate limiter, discarding the Result, so
// such a site retries after 5ms, then 10ms, then 20ms instead of after X. On
// the backup path that is a hot loop that re-forks Redis with BGSAVE. Every
// return has to pick one: a delay, with the failure recorded on the object, or
// the bare error.
func TestNoReconcileReturnsErrorWithRequeueAfter(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}

	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}

		ast.Inspect(file, func(n ast.Node) bool {
			ret, ok := n.(*ast.ReturnStmt)
			if !ok || len(ret.Results) != 2 {
				return true
			}
			field, ok := requeueField(ret.Results[0])
			if !ok || isNilLiteral(ret.Results[1]) {
				return true
			}
			t.Errorf("%s: returns %s together with a non-nil error; "+
				"controller-runtime drops the delay and falls back to rate-limited backoff",
				fset.Position(ret.Pos()), field)
			return true
		})
	}
}

// requeueField reports the Requeue/RequeueAfter key set in a Result literal.
func requeueField(expr ast.Expr) (string, bool) {
	lit, ok := expr.(*ast.CompositeLit)
	if !ok || !isResultType(lit.Type) {
		return "", false
	}
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok {
			continue
		}
		if key.Name == "RequeueAfter" || key.Name == "Requeue" {
			return key.Name, true
		}
	}
	return "", false
}

// isResultType matches ctrl.Result and reconcile.Result, the two spellings the
// reconcilers use.
func isResultType(expr ast.Expr) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	return sel.Sel.Name == "Result" && (pkg.Name == "ctrl" || pkg.Name == "reconcile")
}

func isNilLiteral(expr ast.Expr) bool {
	ident, ok := expr.(*ast.Ident)
	return ok && ident.Name == "nil"
}
