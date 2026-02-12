package source

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/profmate/profmate/internal/model"
)

func TestExtractOnlyMatchingCode(t *testing.T) {
	root := t.TempDir()
	code := "package shop\n\nfunc Search() {\n println(42)\n}\n\nfunc Unrelated() { panic(\"do not send\") }\n"
	if err := os.WriteFile(filepath.Join(root, "search.go"), []byte(code), 0600); err != nil {
		t.Fatal(err)
	}
	a := model.Analysis{Metrics: []model.Metric{{Top: []model.Function{{Frame: model.Frame{Name: "shop.Search", File: "/build/shop/search.go", Line: 4}}}}}}
	got, err := Extract(root, a)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "func Search") || strings.Contains(got, "do not send") {
		t.Fatal(got)
	}
	if !strings.Contains(got, "автоматически не проверено") {
		t.Fatal("missing build caveat")
	}
}

func TestRejectAmbiguousAndExternalSymlink(t *testing.T) {
	root := t.TempDir()
	external := t.TempDir()
	for _, dir := range []string{"a", "b"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, dir, "main.go"), []byte("package main\nfunc main() {}\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(external, "secret.go"), []byte("package main\nfunc Secret() {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(external, "secret.go"), filepath.Join(root, "secret.go")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/build/main.go", filepath.Join(external, "secret.go")} {
		a := model.Analysis{Metrics: []model.Metric{{Top: []model.Function{{Frame: model.Frame{Name: "main.main", File: path, Line: 2}}}}}}
		if got, err := Extract(root, a); err == nil {
			t.Fatalf("unexpected source: %s", got)
		}
	}
}

func TestExtractValidatesFunctionAndPackage(t *testing.T) {
	cases := []struct {
		name, code, symbol string
		want               bool
	}{
		{"wrong-function", "package shop\nfunc Unrelated() { println(1) }\n", "shop.Search", false},
		{"wrong-package", "package main\ntype conn struct{}\nfunc (*conn) serve() { println(1) }\n", "net/http.(*conn).serve", false},
		{"wrong-receiver", "package shop\ntype Handler struct{}\nfunc (*Handler) Search() { println(1) }\n", "shop.(*Other).Search", false},
		{"main", "package main\nfunc Search() { println(1) }\n", "main.Search", true},
		{"pointer-method", "package shop\ntype Handler struct{}\nfunc (*Handler) Search() { println(1) }\n", "example.com/shop.(*Handler).Search", true},
		{"value-method", "package shop\ntype Handler struct{}\nfunc (Handler) Search() { println(1) }\n", "example.com/shop.Handler.Search", true},
		{"closure", "package shop\nfunc Search() { println(1) }\n", "example.com/shop.Search.func1.1", true},
		{"goroutine-wrapper", "package shop\nfunc Search() { println(1) }\n", "example.com/shop.Search.gowrap1", true},
		{"method-wrapper", "package shop\ntype Handler struct{}\nfunc (*Handler) Search() { println(1) }\n", "example.com/shop.(*Handler).Search-fm", true},
		{"generic-method", "package shop\ntype Handler[T any] struct{}\nfunc (*Handler[T]) Search() { println(1) }\n", "example.com/shop.(*Handler[go.shape.int]).Search", true},
		{"module-v2", "package shop\nfunc Search() { println(1) }\n", "example.com/shop/v2.Search", true},
		{"module-dot-v3", "package shop\nfunc Search() { println(1) }\n", "gopkg.in/shop.v3.Search", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "server.go"), []byte(tc.code), 0600); err != nil {
				t.Fatal(err)
			}
			line := int64(strings.Count(tc.code, "\n"))
			a := model.Analysis{Metrics: []model.Metric{{Top: []model.Function{{Frame: model.Frame{Name: tc.symbol, File: "/unrelated/build/server.go", Line: line}}}}}}
			got, err := Extract(root, a)
			if (err == nil) != tc.want {
				t.Fatalf("Extract() = %q, %v; want success=%v", got, err, tc.want)
			}
		})
	}
}

func TestExtractLongFunctionIncludesSampledLine(t *testing.T) {
	root := t.TempDir()
	code := "package shop\nfunc Search() {\n" + strings.Repeat(" // padding\n", 110) + " println(\"sampled operation\")\n" + strings.Repeat(" // after\n", 40) + "}\n"
	if err := os.WriteFile(filepath.Join(root, "search.go"), []byte(code), 0600); err != nil {
		t.Fatal(err)
	}
	line := int64(strings.Count(strings.Split(code, " println(")[0], "\n") + 1)
	a := model.Analysis{Metrics: []model.Metric{{Top: []model.Function{{Frame: model.Frame{Name: "shop.Search", File: "/build/shop/search.go", Line: line}}}}}}
	got, err := Extract(root, a)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "sampled operation") {
		t.Fatalf("sampled line was omitted: %s", got)
	}
	if strings.Contains(got, "func Search()") {
		t.Fatal("expected a bounded window around the late sampled line, not the beginning of the function")
	}
	if count := strings.Count(got, " | "); count > 80 {
		t.Fatalf("snippet has %d lines; limit is 80", count)
	}
}
