package metrics

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// metricsPkgPath is the import path controllers use to reach this package.
const metricsPkgPath = "github.com/redguard/redguard/pkg/metrics"

// writeMethods are the Prometheus calls that put a value into a series.
// DeletePartialMatch, Reset and WithLabelValues alone do not.
var writeMethods = map[string]bool{
	"Set":              true,
	"Inc":              true,
	"Dec":              true,
	"Add":              true,
	"Sub":              true,
	"Observe":          true,
	"SetToCurrentTime": true,
}

// TestEveryRegisteredMetricIsWritten fails when a collector is declared and
// registered here but no operator code ever puts a value into it. Such a
// collector exports a permanently zero series, and any alert written against
// it can never fire. A placeholder ".Add(0)" does not count as a write: it is
// how the four Redis INFO counters used to discard the value they had just
// parsed.
func TestEveryRegisteredMetricIsWritten(t *testing.T) {
	declared, registered := declaredVecs(t)
	if len(declared) == 0 {
		t.Fatal("no Prometheus vectors found in metrics.go; the parser is broken, not the code")
	}

	written := metricWriteSites(t, repoRoot(t))

	for _, name := range declared {
		if !registered[name] {
			t.Errorf("%s is declared but never passed to MustRegister, so it is never exported", name)
		}
		if !written[name] {
			t.Errorf("%s is registered but never written outside pkg/metrics; it would export a permanently zero series", name)
		}
	}

	for name := range registered {
		if !slices.Contains(declared, name) {
			t.Errorf("%s is registered but is not a package-level Prometheus vector", name)
		}
	}
}

// runtimeMetrics are the metrics the alert rules may use that this package
// does not own: controller-runtime registers them for every controller the
// Manager runs.
var runtimeMetrics = map[string]bool{
	"controller_runtime_reconcile_total":        true,
	"controller_runtime_reconcile_errors_total": true,
	"controller_runtime_reconcile_time_seconds": true,
	"controller_runtime_active_workers":         true,
}

// alertMetricPattern matches the metric names the shipped alert rules may
// reference. Everything this operator exports is prefixed redis_.
var alertMetricPattern = regexp.MustCompile(`\b(redis_[a-z0-9_]+|controller_runtime_[a-z0-9_]+)\b`)

// TestEveryAlertReferencesAWrittenMetric fails when a shipped alert evaluates
// a metric nothing exports. Such an alert is silently dead: its expression
// matches no series, so it never fires and never reports that it cannot.
func TestEveryAlertReferencesAWrittenMetric(t *testing.T) {
	exported := exportedMetricNames(t)

	path := filepath.Join(repoRoot(t), "config", "prometheus", "prometheusrule.yaml")
	rules, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	seen := map[string]bool{}
	for _, match := range alertMetricPattern.FindAllString(string(rules), -1) {
		if seen[match] {
			continue
		}
		seen[match] = true
		if !exported[match] && !runtimeMetrics[match] {
			t.Errorf("alert rules reference %s, which no collector exports", match)
		}
	}
	if len(seen) == 0 {
		t.Fatal("no metric names found in prometheusrule.yaml; the scan is broken, not the rules")
	}
}

// exportedMetricNames maps the Prometheus name of every vector declared here.
func exportedMetricNames(t *testing.T) map[string]bool {
	t.Helper()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "metrics.go", nil, 0)
	if err != nil {
		t.Fatalf("parse metrics.go: %v", err)
	}

	names := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !isPrometheusVecConstructor(call) || len(call.Args) == 0 {
			return true
		}
		opts, ok := call.Args[0].(*ast.CompositeLit)
		if !ok {
			return true
		}
		for _, elt := range opts.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok || key.Name != "Name" {
				continue
			}
			if lit, ok := kv.Value.(*ast.BasicLit); ok {
				if name, err := strconv.Unquote(lit.Value); err == nil {
					names[name] = true
				}
			}
		}
		return true
	})

	return names
}

// declaredVecs returns the package-level Prometheus vector names in metrics.go
// and the set of names handed to MustRegister.
func declaredVecs(t *testing.T) ([]string, map[string]bool) {
	t.Helper()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "metrics.go", nil, 0)
	if err != nil {
		t.Fatalf("parse metrics.go: %v", err)
	}

	var declared []string
	registered := map[string]bool{}

	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.GenDecl:
			if d.Tok != token.VAR {
				continue
			}
			for _, spec := range d.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok || len(value.Names) != 1 || len(value.Values) != 1 {
					continue
				}
				if isPrometheusVecConstructor(value.Values[0]) {
					declared = append(declared, value.Names[0].Name)
				}
			}
		case *ast.FuncDecl:
			if d.Name.Name != "init" {
				continue
			}
			ast.Inspect(d, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "MustRegister" {
					return true
				}
				for _, arg := range call.Args {
					if ident, ok := arg.(*ast.Ident); ok {
						registered[ident.Name] = true
					}
				}
				return true
			})
		}
	}

	return declared, registered
}

func isPrometheusVecConstructor(expr ast.Expr) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "prometheus" {
		return false
	}
	return strings.HasPrefix(sel.Sel.Name, "New") && strings.HasSuffix(sel.Sel.Name, "Vec")
}

// metricWriteSites scans every non-test Go file outside this package and
// returns the vector names that are written at least once. A vector handed to
// a helper as an argument counts too: that is how the INFO counters reach
// AddCounterDelta.
func metricWriteSites(t *testing.T, root string) map[string]bool {
	t.Helper()

	written := map[string]bool{}
	selfDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	skipDirs := map[string]bool{".git": true, "bin": true, "dist": true, "testdata": true, "planning": true}

	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if skipDirs[entry.Name()] || path == selfDir {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		fset := token.NewFileSet()
		file, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return parseErr
		}
		alias := metricsImportAlias(file)
		if alias == "" {
			return nil
		}
		collectWrites(file, alias, written)
		return nil
	})
	if err != nil {
		t.Fatalf("scan %s: %v", root, err)
	}

	return written
}

// metricsImportAlias returns the local name this file uses for pkg/metrics, or
// "" when it does not import it.
func metricsImportAlias(file *ast.File) string {
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil || path != metricsPkgPath {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		return "metrics"
	}
	return ""
}

func collectWrites(file *ast.File, alias string, written map[string]bool) {
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}

		for _, arg := range call.Args {
			if name := selectedMetric(arg, alias); name != "" {
				written[name] = true
			}
		}

		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !writeMethods[sel.Sel.Name] {
			return true
		}
		name := receiverMetric(sel.X, alias)
		if name == "" {
			return true
		}
		if sel.Sel.Name == "Add" && isZeroLiteral(call.Args) {
			return true
		}
		written[name] = true
		return true
	})
}

// selectedMetric matches a bare "alias.Name" reference.
func selectedMetric(expr ast.Expr, alias string) string {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	ident, ok := sel.X.(*ast.Ident)
	if !ok || ident.Name != alias {
		return ""
	}
	return sel.Sel.Name
}

// receiverMetric unwraps a "alias.Name.WithLabelValues(...)" receiver chain
// down to the vector it started from.
func receiverMetric(expr ast.Expr, alias string) string {
	for {
		switch e := expr.(type) {
		case *ast.CallExpr:
			expr = e.Fun
		case *ast.SelectorExpr:
			if name := selectedMetric(e, alias); name != "" {
				return name
			}
			expr = e.X
		default:
			return ""
		}
	}
}

func isZeroLiteral(args []ast.Expr) bool {
	if len(args) != 1 {
		return false
	}
	lit, ok := args[0].(*ast.BasicLit)
	if !ok {
		return false
	}
	switch lit.Value {
	case "0", "0.0", "0.":
		return true
	}
	return false
}

func repoRoot(t *testing.T) string {
	t.Helper()

	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root %s has no go.mod: %v", root, err)
	}
	return root
}
