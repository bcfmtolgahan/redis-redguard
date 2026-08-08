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
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	redisv1alpha1 "github.com/bcfmtolgahan/redis-redguard/api/v1alpha1"
)

const (
	mainPath = "../../cmd/main.go"
	crdBases = "../../config/crd/bases"
	crdKust  = "../../config/crd/kustomization.yaml"
)

// packageSources lists the non-test Go files of this package.
func packageSources(t *testing.T) []string {
	t.Helper()
	all, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob package sources: %v", err)
	}
	var out []string
	for _, f := range all {
		if !strings.HasSuffix(f, "_test.go") {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		t.Fatal("no controller sources found; the test is looking in the wrong directory")
	}
	return out
}

// declaredReconcilers maps every "<Kind>Reconciler" struct in this package to
// the set of its field names.
func declaredReconcilers(t *testing.T) map[string]map[string]bool {
	t.Helper()
	out := map[string]map[string]bool{}
	fset := token.NewFileSet()
	for _, path := range packageSources(t) {
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok || !strings.HasSuffix(ts.Name.Name, "Reconciler") {
				return true
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				return true
			}
			fields := map[string]bool{}
			for _, fld := range st.Fields.List {
				for _, name := range fld.Names {
					fields[name.Name] = true
				}
			}
			out[ts.Name.Name] = fields
			return true
		})
	}
	return out
}

// registeredReconcilers maps every reconciler that cmd/main.go hands to the
// manager to the set of fields its composite literal assigns.
func registeredReconcilers(t *testing.T) map[string]map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, mainPath, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", mainPath, err)
	}

	out := map[string]map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "SetupWithManager" {
			return true
		}
		lit, ok := unwrapCompositeLit(sel.X)
		if !ok {
			return true
		}
		name, ok := compositeLitTypeName(lit)
		if !ok {
			return true
		}
		fields := map[string]bool{}
		for _, elt := range lit.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if key, ok := kv.Key.(*ast.Ident); ok {
				fields[key.Name] = true
			}
		}
		out[name] = fields
		return true
	})
	return out
}

// unwrapCompositeLit strips the parentheses and address-of around the
// (&controller.XReconciler{...}).SetupWithManager(mgr) idiom.
func unwrapCompositeLit(e ast.Expr) (*ast.CompositeLit, bool) {
	for {
		switch v := e.(type) {
		case *ast.ParenExpr:
			e = v.X
		case *ast.UnaryExpr:
			e = v.X
		case *ast.CompositeLit:
			return v, true
		default:
			return nil, false
		}
	}
}

func compositeLitTypeName(lit *ast.CompositeLit) (string, bool) {
	switch t := lit.Type.(type) {
	case *ast.SelectorExpr:
		return t.Sel.Name, true
	case *ast.Ident:
		return t.Name, true
	default:
		return "", false
	}
}

// TestMainRegistersEveryReconciler guards the class of bug where a controller
// is compiled into the binary but never handed to the manager, so the CRs it
// owns sit unreconciled forever.
func TestMainRegistersEveryReconciler(t *testing.T) {
	declared := declaredReconcilers(t)
	if len(declared) == 0 {
		t.Fatal("no reconcilers found in the controller package")
	}
	registered := registeredReconcilers(t)
	for name := range declared {
		if _, ok := registered[name]; !ok {
			t.Errorf("%s is never registered with the manager in cmd/main.go; the controller ships dead", name)
		}
	}
}

// TestMainSuppliesEveryDependency pins the RESTConfig gap: the pod-exec paths
// in the backup and restore controllers need a *rest.Config, and a reconciler
// whose Recorder is left nil records no events. ExternalEvents left nil ships
// the switch-master subscription dead: SetupWithManager skips the channel
// source and failover detection silently degrades to polling only.
func TestMainSuppliesEveryDependency(t *testing.T) {
	registered := registeredReconcilers(t)
	for name, fields := range declaredReconcilers(t) {
		assigned, ok := registered[name]
		if !ok {
			continue
		}
		for _, dep := range []string{"RESTConfig", "Recorder", "ExternalEvents"} {
			if fields[dep] && !assigned[dep] {
				t.Errorf("%s declares %s but cmd/main.go never assigns it", name, dep)
			}
		}
	}
}

// TestSetupWiresExternalEvents guards the channel-source registration: with
// the ExternalEvents field declared but never handed to WatchesRawSource, the
// watcher publishes into a channel nothing drains and a promotion waits for
// the periodic pass exactly as before.
func TestSetupWiresExternalEvents(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "redissentinel_controller.go", nil, 0)
	if err != nil {
		t.Fatalf("parse redissentinel_controller.go: %v", err)
	}

	var wired bool
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "SetupWithManager" || fn.Recv == nil {
			return true
		}
		var usesRawSource, usesEvents bool
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.SelectorExpr:
				if v.Sel.Name == "WatchesRawSource" {
					usesRawSource = true
				}
				if v.Sel.Name == "ExternalEvents" {
					usesEvents = true
				}
			case *ast.Ident:
				if v.Name == "ExternalEvents" {
					usesEvents = true
				}
			}
			return true
		})
		wired = wired || (usesRawSource && usesEvents)
		return true
	})
	if !wired {
		t.Error("SetupWithManager never passes ExternalEvents to WatchesRawSource; switch-master events would be produced but never enqueue a reconcile")
	}
}

// TestNoReconcilerCallsRecorderUnguarded pins the nil-Recorder panic: a
// reconciler built without SetupWithManager has no Recorder, and the network
// policy path records an event on the success branch of every reconcile.
func TestNoReconcilerCallsRecorderUnguarded(t *testing.T) {
	for _, path := range packageSources(t) {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if bytes.Contains(src, []byte("r.Recorder.Event")) {
			t.Errorf("%s calls r.Recorder.Event directly; use recordEvent so a nil Recorder cannot panic", path)
		}
	}
}

// TestRecordEventToleratesNilRecorder covers the reconciler built by hand, for
// example in a unit test, where SetupWithManager never ran.
func TestRecordEventToleratesNilRecorder(t *testing.T) {
	r := &RedisSentinelReconciler{}
	recordEvent(r.Recorder, &redisv1alpha1.RedisSentinel{}, corev1.EventTypeWarning, "Test", "message")
}

// protocolClientPackages are the packages whose constructors dial Redis
// directly. Controllers must go through redisclient.Factory instead, so the
// TLS settings resolved per reconcile reach every connection.
var protocolClientPackages = map[string]bool{
	"github.com/bcfmtolgahan/redis-redguard/pkg/redisutils":    true,
	"github.com/bcfmtolgahan/redis-redguard/internal/sentinel": true,
}

// TestNoControllerConstructsProtocolClientsDirectly guards the factory seam:
// a direct redisutils/sentinel constructor call bypasses the TLS config and
// silently dials plaintext against a TLS-only pod.
func TestNoControllerConstructsProtocolClientsDirectly(t *testing.T) {
	fset := token.NewFileSet()
	for _, path := range packageSources(t) {
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		local := map[string]bool{}
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			if !protocolClientPackages[p] {
				continue
			}
			name := p[strings.LastIndex(p, "/")+1:]
			if imp.Name != nil {
				name = imp.Name.Name
			}
			local[name] = true
		}
		if len(local) == 0 {
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok || !local[id.Name] || !strings.HasPrefix(sel.Sel.Name, "New") {
				return true
			}
			t.Errorf("%s:%d calls %s.%s directly; construct clients through r.RedisFactory so spec.tls is honoured",
				path, fset.Position(call.Pos()).Line, id.Name, sel.Sel.Name)
			return true
		})
	}
}

// TestEveryFactoryCallThreadsTLSConfig pins the blind-operator bug: with
// spec.tls enabled the pods listen on TLS only, so a factory call whose
// tlsConfig argument is the literal nil hardcodes plaintext no matter what
// the spec says. Every call site must pass a config resolved from the spec.
func TestEveryFactoryCallThreadsTLSConfig(t *testing.T) {
	fset := token.NewFileSet()
	for _, path := range packageSources(t) {
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if sel.Sel.Name != "NewClient" && sel.Sel.Name != "NewSentinelPool" {
				return true
			}
			if len(call.Args) == 0 {
				return true
			}
			last, ok := call.Args[len(call.Args)-1].(*ast.Ident)
			if ok && last.Name == "nil" {
				t.Errorf("%s:%d passes a literal nil tlsConfig to %s; thread the config built by tlsutil.BuildClientTLSConfig",
					path, fset.Position(call.Pos()).Line, sel.Sel.Name)
			}
			return true
		})
	}
}

// TestFailoverHandlingRunsInReconcileNotBareGoroutine forbids fire-and-forget
// goroutines in the controllers: work spawned outside the reconcile acts on a
// topology snapshot that may be stale by the time it runs, a panic inside it
// kills the operator process, and a shutdown truncates it midway.
func TestFailoverHandlingRunsInReconcileNotBareGoroutine(t *testing.T) {
	fset := token.NewFileSet()
	for _, path := range packageSources(t) {
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			g, ok := n.(*ast.GoStmt)
			if !ok {
				return true
			}
			t.Errorf("%s:%d starts a goroutine; do the work inline in the reconcile so it is bounded by the request context, survives no panic alone, and acts on freshly verified state",
				path, fset.Position(g.Pos()).Line)
			return true
		})
	}
}

// TestEveryCRDIsShipped guards against a generated CRD that no install path
// applies, which leaves the matching kind unknown to the API server.
func TestEveryCRDIsShipped(t *testing.T) {
	kust, err := os.ReadFile(crdKust)
	if err != nil {
		t.Fatalf("read %s: %v", crdKust, err)
	}
	entries, err := os.ReadDir(crdBases)
	if err != nil {
		t.Fatalf("read %s: %v", crdBases, err)
	}
	found := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		found++
		if !bytes.Contains(kust, []byte(e.Name())) {
			t.Errorf("CRD %s is missing from %s; users can never apply that kind", e.Name(), crdKust)
		}
	}
	if found == 0 {
		t.Fatalf("no CRD bases found under %s", crdBases)
	}
}
