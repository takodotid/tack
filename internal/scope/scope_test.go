// Package scope tests the rules of [RFC#0001] and [RFC#0002] Requirement 1 that the code's structure decides: who
// starts a run, what Tack can change, what it imports, and how it is built. It reads the packages of the binary
// cmd/tack, test files left out, with golang.org/x/tools/go/packages.
package scope

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"go/types"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/types/typeutil"
)

const (
	module = "github.com/takodotid/tack"
	binary = module + "/cmd/tack"
	api    = module + "/api/v1"
	engine = module + "/internal/engine"
	driver = module + "/internal/driver"
)

var (
	loadOnce sync.Once
	graph    map[string]*packages.Package // every package the binary imports, by path
	typed    map[string]*packages.Package // the module's packages of the binary, with syntax and types
	loadErr  error
)

// load reads the binary's import graph, then the module's packages in it with their syntax and types. The packages
// of other modules come from export data.
func load(t *testing.T) {
	t.Helper()
	loadOnce.Do(func() {
		cfg := &packages.Config{Mode: packages.NeedName | packages.NeedFiles | packages.NeedImports | packages.NeedDeps, Dir: "../.."}
		roots, err := packages.Load(cfg, binary)
		if err != nil {
			loadErr = err
			return
		}
		graph = map[string]*packages.Package{}
		var own []string
		packages.Visit(roots, nil, func(p *packages.Package) {
			graph[p.PkgPath] = p
			if inModule(p.PkgPath) {
				own = append(own, p.PkgPath)
			}
		})
		cfg = &packages.Config{Mode: packages.NeedName | packages.NeedFiles | packages.NeedImports | packages.NeedTypes |
			packages.NeedSyntax | packages.NeedTypesInfo, Dir: "../.."}
		pkgs, err := packages.Load(cfg, own...)
		if err != nil {
			loadErr = err
			return
		}
		typed = map[string]*packages.Package{}
		for _, p := range pkgs {
			if len(p.Errors) > 0 {
				loadErr = fmt.Errorf("%s: %v", p.PkgPath, p.Errors)
				return
			}
			typed[p.PkgPath] = p
		}
	})
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if len(typed) < 10 {
		t.Fatalf("read %d packages of the module, want every package of the binary", len(typed))
	}
}

func inModule(path string) bool { return path == module || strings.HasPrefix(path, module+"/") }

func short(path string) string { return strings.TrimPrefix(path, module+"/") }

// sortedKeys returns the module's typed packages in a stable order.
func sortedKeys() []string {
	var keys []string
	for k := range typed {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// enclosing names the function declaration around pos in p: "pkg.Func" or "pkg.(*Type).Method".
func enclosing(p *packages.Package, pos token.Pos) string {
	for _, f := range p.Syntax {
		if pos < f.Pos() || pos >= f.End() {
			continue
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || pos < fd.Pos() || pos >= fd.End() {
				continue
			}
			if fn, ok := p.TypesInfo.Defs[fd.Name].(*types.Func); ok {
				name := fn.Name()
				if recv := fn.Type().(*types.Signature).Recv(); recv != nil {
					name = "(" + types.TypeString(recv.Type(), types.RelativeTo(p.Types)) + ")." + name
				}
				return short(p.PkgPath) + "." + name
			}
		}
		return short(p.PkgPath) + " (outside a function)"
	}
	return short(p.PkgPath) + " (no file)"
}

// method returns the method name of the named type typeName in package pkg.
func method(t *testing.T, pkg, typeName, name string) *types.Func {
	t.Helper()
	p := typed[pkg]
	if p == nil {
		t.Fatalf("package %s is not in the binary", pkg)
	}
	obj, ok := p.Types.Scope().Lookup(typeName).(*types.TypeName)
	if !ok {
		t.Fatalf("%s has no type %s", pkg, typeName)
	}
	named := obj.Type().(*types.Named)
	for i := range named.NumMethods() {
		if m := named.Method(i); m.Name() == name {
			return m
		}
	}
	t.Fatalf("%s.%s has no method %s", pkg, typeName, name)
	return nil
}

// references returns where the module's packages refer to obj, by enclosing function, sorted.
func references(obj types.Object) []string {
	var out []string
	for _, k := range sortedKeys() {
		p := typed[k]
		for id, o := range p.TypesInfo.Uses {
			if o == obj {
				out = append(out, enclosing(p, id.Pos()))
			}
		}
	}
	sort.Strings(out)
	return out
}

// calls visits each call in the module's packages with the function it calls, nil for a call of a function value.
func calls(visit func(p *packages.Package, call *ast.CallExpr, fn *types.Func)) {
	for _, k := range sortedKeys() {
		p := typed[k]
		for _, f := range p.Syntax {
			ast.Inspect(f, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					fn, _ := typeutil.Callee(p.TypesInfo, call).(*types.Func)
					visit(p, call, fn)
				}
				return true
			})
		}
	}
}

// TestOnlyTackRunStartsARun: [RFC#0001] Requirement 1. Only the command tack run refers to the engine's Start, no
// interface hides a call of it, and only Start creates a SiteFailover.
func TestOnlyTackRunStartsARun(t *testing.T) {
	load(t)
	start := method(t, engine, "Env", "Start")
	if got, want := references(start), []string{"internal/cli.newRun"}; !slices.Equal(got, want) {
		t.Errorf("engine.(*Env).Start is referred to in %v, want only %v", got, want)
	}
	// A call through an interface that Env implements refers to the interface's method, not to Start.
	for _, k := range sortedKeys() {
		p := typed[k]
		for expr, tv := range p.TypesInfo.Types {
			it, ok := tv.Type.Underlying().(*types.Interface)
			if !ok {
				continue
			}
			for i := range it.NumMethods() {
				if m := it.Method(i); m.Name() == "Start" && types.Identical(m.Type(), start.Type()) {
					t.Errorf("%s: an interface declares Start with the signature of engine.(*Env).Start", p.Fset.Position(expr.Pos()))
				}
			}
		}
	}

	// Each call that can create an object, with a SiteFailover as an argument.
	var creates []string
	calls(func(p *packages.Package, call *ast.CallExpr, fn *types.Func) {
		if fn == nil || !slices.Contains([]string{"Create", "Patch", "Apply"}, fn.Name()) {
			return
		}
		for _, a := range call.Args {
			if isSiteFailover(p.TypesInfo.TypeOf(a)) {
				creates = append(creates, enclosing(p, call.Pos())+" calls "+fn.Name())
			}
		}
	})
	sort.Strings(creates)
	want := []string{
		"internal/engine.(*Env).create calls Create",   // the ledger, through engine.Ledger
		"internal/ledger.(*Store).Create calls Create", // the dry run
		"internal/ledger.(*Store).Create calls Create", // the create
	}
	if !slices.Equal(creates, want) {
		t.Errorf("calls that can create a SiteFailover:\n  %s\nwant:\n  %s", strings.Join(creates, "\n  "), strings.Join(want, "\n  "))
	}
	if got, want := references(method(t, engine, "Env", "create")), []string{"internal/engine.(*Env).Start"}; !slices.Equal(got, want) {
		t.Errorf("engine.(*Env).create is referred to in %v, want only %v", got, want)
	}
	// ledger.Store is the engine's Ledger; its Create is called through the interface, in (*Env).create above.
	if got := references(method(t, module+"/internal/ledger", "Store", "Create")); len(got) > 0 {
		t.Errorf("ledger.(*Store).Create is called directly in %v", got)
	}
}

// isSiteFailover reports whether t is a SiteFailover of the API, or a pointer to one.
func isSiteFailover(t types.Type) bool {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	n, ok := t.(*types.Named)
	return ok && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == api && n.Obj().Name() == "SiteFailover"
}

// TestNoExec: [RFC#0001] Requirement 2. The step types are those of [RFC#0002] Requirement 3, none runs a command;
// the binary links none of client-go's transports that exec into a pod, attach to one or forward its port; the only
// subresource the module names is status; and only the Git package runs a program, git.
func TestNoExec(t *testing.T) {
	load(t)
	var steps []string
	scope := typed[api].Types.Scope()
	for _, name := range scope.Names() {
		c, ok := scope.Lookup(name).(*types.Const)
		if ok && c.Type().String() == api+".StepType" {
			steps = append(steps, constant.StringVal(c.Val()))
		}
	}
	sort.Strings(steps)
	want := []string{"Capture", "Create", "Git", "Hold", "Patch", "Reconcile", "Record", "Release", "Routing", "VerifyGit", "VerifyNoop", "Wait"}
	if !slices.Equal(steps, want) {
		t.Errorf("step types %v, want those of RFC0002 Requirement 3: %v", steps, want)
	}
	for _, bad := range []string{"k8s.io/client-go/tools/remotecommand", "k8s.io/client-go/tools/portforward", "k8s.io/client-go/transport/spdy", "k8s.io/client-go/transport/websocket"} {
		if _, ok := graph[bad]; ok {
			t.Errorf("the binary links %s, which can exec into a pod, attach to one or forward its port", bad)
		}
	}
	for _, path := range sortedKeys() {
		for imp := range graph[path].Imports {
			if imp == "os/exec" && path != module+"/internal/gitops" {
				t.Errorf("%s imports os/exec; only internal/gitops runs a program, git", short(path))
			}
		}
	}
	// The only request through a pod's proxy: a GET of the instance manager's status, from the cnpg driver.
	proxied := 0
	calls(func(p *packages.Package, call *ast.CallExpr, fn *types.Func) {
		if fn == nil || fn.Pkg() == nil {
			return
		}
		switch {
		case fn.Name() == "AbsPath" && fn.Pkg().Path() == "k8s.io/client-go/rest":
			if p.PkgPath != module+"/internal/kube" {
				t.Errorf("%s: a request by path; only internal/kube makes one", p.Fset.Position(call.Pos()))
			}
		case fn.Name() == "ProxyGet" && fn.Pkg().Path() == "k8s.io/client-go/kubernetes/typed/core/v1":
			t.Errorf("%s: client-go's ProxyGet; Tack proxies only through kube.ProxyGet", p.Fset.Position(call.Pos()))
		case fn.Name() == "ProxyGet" && strings.HasPrefix(fn.Pkg().Path(), module+"/internal/kube"):
			if p.PkgPath == module+"/internal/kube" {
				return
			}
			// engine.SiteChecks passes a driver's read on to the site's clients, as it does each request.
			if p.PkgPath == module+"/internal/engine" && types.ExprString(call.Args[5]) == "port" && types.ExprString(call.Args[6]) == "path" {
				return
			}
			port, path := p.TypesInfo.Types[call.Args[5]].Value, p.TypesInfo.Types[call.Args[6]].Value
			if p.PkgPath != module+"/internal/driver/cnpg" || port == nil || path == nil ||
				port.String() != "8000" || constant.StringVal(path) != "/pg/status" {
				t.Errorf("%s: a GET through a pod's proxy that is not the cnpg driver's read of /pg/status on port 8000", p.Fset.Position(call.Pos()))
			}
			proxied++
		}
	})
	if proxied == 0 {
		t.Error("no call of kube.ProxyGet found: the check of the proxy reads nothing")
	}
	calls(func(p *packages.Package, call *ast.CallExpr, fn *types.Func) {
		if fn == nil || fn.Name() != "SubResource" || len(call.Args) != 1 {
			return
		}
		tv := p.TypesInfo.Types[call.Args[0]]
		if tv.Value == nil || constant.StringVal(tv.Value) != "status" {
			t.Errorf("%s: subresource %s; Tack writes only status", p.Fset.Position(call.Pos()), types.ExprString(call.Args[0]))
		}
	})
}

// clientCalls returns the methods of controller-runtime's client that the package calls, sorted and unique.
func clientCalls(path string) []string {
	var out []string
	calls(func(p *packages.Package, call *ast.CallExpr, fn *types.Func) {
		if p.PkgPath != path || fn == nil || fn.Pkg() == nil || fn.Pkg().Path() != "sigs.k8s.io/controller-runtime/pkg/client" {
			return
		}
		if sig, ok := fn.Type().(*types.Signature); ok && sig.Recv() != nil && !slices.Contains(out, fn.Name()) {
			out = append(out, fn.Name())
		}
	})
	sort.Strings(out)
	return out
}

// TestStraitIsOnlyRead: [RFC#0001] Requirement 3 and [RFC#0008] Requirement 1. The Strait reader calls no method of
// a Kubernetes client but Get, and no other package of the binary names Strait's API group. strait_test.go runs the
// reader with a client that fails on any other call.
func TestStraitIsOnlyRead(t *testing.T) {
	load(t)
	strait := module + "/internal/strait"
	if got := clientCalls(strait); !slices.Equal(got, []string{"Get"}) {
		t.Errorf("internal/strait calls the client's %v, want Get only", got)
	}
	for _, path := range sortedKeys() {
		p := typed[path]
		if path == strait {
			continue
		}
		for _, f := range p.Syntax {
			ast.Inspect(f, func(n ast.Node) bool {
				if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING && strings.Contains(lit.Value, "strait.tako.id") {
					t.Errorf("%s names Strait's API group: %s", p.Fset.Position(lit.Pos()), lit.Value)
				}
				return true
			})
		}
	}
}

// TestNoTrigger: [RFC#0001] Requirement 4. No package of the binary imports what watches the API or runs a
// controller, and none calls a client's Watch. TestOnlyTackRunStartsARun shows that only tack run starts a run.
func TestNoTrigger(t *testing.T) {
	load(t)
	banned := []string{
		"k8s.io/client-go/informers", "k8s.io/client-go/tools/cache", "k8s.io/client-go/tools/watch",
		"k8s.io/apimachinery/pkg/watch", "k8s.io/client-go/tools/leaderelection",
		"sigs.k8s.io/controller-runtime", "sigs.k8s.io/controller-runtime/pkg/manager", "sigs.k8s.io/controller-runtime/pkg/cache",
		"sigs.k8s.io/controller-runtime/pkg/controller", "sigs.k8s.io/controller-runtime/pkg/source",
		"sigs.k8s.io/controller-runtime/pkg/builder", "sigs.k8s.io/controller-runtime/pkg/handler",
		"sigs.k8s.io/controller-runtime/pkg/reconcile", "sigs.k8s.io/controller-runtime/pkg/webhook",
	}
	for _, path := range sortedKeys() {
		for imp := range graph[path].Imports {
			for _, b := range banned {
				if imp == b || strings.HasPrefix(imp, b+"/") && !strings.HasPrefix(b, "sigs.k8s.io/controller-runtime") {
					t.Errorf("%s imports %s", short(path), imp)
				}
			}
		}
	}
	calls(func(p *packages.Package, call *ast.CallExpr, fn *types.Func) {
		if fn != nil && fn.Name() == "Watch" {
			t.Errorf("%s: %s calls Watch", p.Fset.Position(call.Pos()), enclosing(p, call.Pos()))
		}
	})
}

// TestDriversAreBuiltIn: [RFC#0001] Requirement 5. Only a package under internal/driver registers a driver, in its
// init; the binary imports each such package; and no package of the module imports Go's plugin package. Of the
// binary's dependencies, only kustomize's plugin loader imports it, which Flux's build turns off; the release builds
// without cgo, where plugin.Open is not implemented.
func TestDriversAreBuiltIn(t *testing.T) {
	load(t)
	register := typed[driver].Types.Scope().Lookup("Register")
	if register == nil {
		t.Fatal("internal/driver has no Register")
	}
	var drivers []string
	for _, k := range sortedKeys() {
		p := typed[k]
		for id, o := range p.TypesInfo.Uses {
			if o != register {
				continue
			}
			where := enclosing(p, id.Pos())
			if !strings.HasPrefix(p.PkgPath, driver+"/") || where != short(p.PkgPath)+".init" {
				t.Errorf("%s registers a driver; only the init of a package under internal/driver may", where)
			}
			drivers = append(drivers, p.PkgPath)
		}
	}
	if len(drivers) == 0 {
		t.Error("no driver registers")
	}
	for _, d := range drivers {
		if _, ok := graph[binary].Imports[d]; !ok {
			t.Errorf("cmd/tack does not import the driver %s", short(d))
		}
	}
	var importers []string
	for path, p := range graph {
		if _, ok := p.Imports["plugin"]; ok {
			importers = append(importers, path)
		}
	}
	sort.Strings(importers)
	if want := []string{"sigs.k8s.io/kustomize/api/internal/plugins/loader"}; !slices.Equal(importers, want) {
		t.Errorf("the binary's packages that import Go's plugin package: %v, want %v; no package may load a driver", importers, want)
	}
}

// TestWrittenInGo: [RFC#0001] Requirement 6. Each package of the module in the binary is Go only: no cgo, and no C,
// C++ or assembly file, also behind a build constraint.
func TestWrittenInGo(t *testing.T) {
	load(t)
	for _, path := range sortedKeys() {
		p := graph[path]
		if len(p.OtherFiles) > 0 {
			t.Errorf("%s has files that are not Go: %v", short(path), p.OtherFiles)
		}
		for _, f := range p.IgnoredFiles {
			if !strings.HasSuffix(f, ".go") {
				t.Errorf("%s has a file that is not Go: %s", short(path), f)
			}
		}
		for _, f := range append(slices.Clone(p.GoFiles), p.IgnoredFiles...) {
			if !strings.HasSuffix(f, ".go") {
				continue
			}
			file, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, imp := range file.Imports {
				if imp.Path.Value == `"C"` {
					t.Errorf("%s uses cgo", f)
				}
			}
		}
	}
}

// TestEngineImportsNoDriver: [RFC#0002] Requirement 1. The engine packages import no driver package, directly or
// through another package; only the binary's wiring, cmd/tack, imports one.
func TestEngineImportsNoDriver(t *testing.T) {
	load(t)
	isDriver := func(path string) bool { return strings.HasPrefix(path, driver+"/") }
	for _, name := range []string{"engine", "flux", "gitops", "strait", "cli"} {
		path := module + "/internal/" + name
		p := graph[path]
		if p == nil {
			t.Errorf("internal/%s is not in the binary", name)
			continue
		}
		packages.Visit([]*packages.Package{p}, func(q *packages.Package) bool {
			if isDriver(q.PkgPath) {
				t.Errorf("internal/%s imports the driver %s", name, short(q.PkgPath))
			}
			return inModule(q.PkgPath)
		}, nil)
	}
	for _, path := range sortedKeys() {
		for imp := range graph[path].Imports {
			if isDriver(imp) && path != binary {
				t.Errorf("%s imports the driver %s; only cmd/tack may", short(path), short(imp))
			}
		}
	}
}
