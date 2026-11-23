package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// notInSpec are API port routes openapi.yaml leaves out: the open health
// check and the MCP endpoint, which is its own protocol.
var notInSpec = []string{"GET /healthz", "/mcp"}

// TestOpenAPIMatchesRoutes fails while openapi.yaml and the routes API
// registers differ.
func TestOpenAPIMatchesRoutes(t *testing.T) {
	routes := apiRoutes(t)
	spec := specRoutes(t)
	for _, r := range routes {
		if !slices.Contains(spec, r) {
			t.Errorf("%s is registered in apiport.go but missing from openapi.yaml; update openapi.yaml", r)
		}
	}
	for _, r := range spec {
		if !slices.Contains(routes, r) {
			t.Errorf("%s is in openapi.yaml but not registered in apiport.go; update openapi.yaml", r)
		}
	}
}

// apiRoutes reads the patterns API passes to mux.Handle and mux.HandleFunc.
func apiRoutes(t *testing.T) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "apiport.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var routes []string
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "API" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (sel.Sel.Name != "Handle" && sel.Sel.Name != "HandleFunc") {
				return true
			}
			if x, ok := sel.X.(*ast.Ident); !ok || x.Name != "mux" {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				t.Fatalf("a route in API is not a string literal; this test reads only literals")
			}
			pattern, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(notInSpec, pattern) {
				routes = append(routes, pattern)
			}
			return true
		})
	}
	if len(routes) == 0 {
		t.Fatal("found no routes in API")
	}
	return routes
}

// specRoutes reads openapi.yaml's paths as "METHOD /path" patterns.
func specRoutes(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile("../../openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]yaml.Node `yaml:"paths"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	methods := []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"}
	var routes []string
	for path, item := range doc.Paths {
		for key := range item {
			if slices.Contains(methods, key) {
				routes = append(routes, strings.ToUpper(key)+" "+path)
			}
		}
	}
	return routes
}
