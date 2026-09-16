package model_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// selfImport is the one caesium package these tests may import: the package
// under test.
const selfImport = "github.com/caesium-cloud/caesium/test/model"

// allowedImports is the complete set this package may depend on.
//
// Every entry is here because the model needs it to be a model, and nothing
// here can start a server, open a socket, or reach a product decision function.
var allowedImports = map[string]bool{
	// Property-based testing and linearizability checking: the model's whole
	// reason for existing. C1 owns these two dependencies.
	"pgregory.net/rapid":                true,
	"github.com/anishathalye/porcupine": true,
}

// allowedStdlib is the standard-library surface this package may use. It is an
// allowlist rather than a denylist on purpose: a denylist has to anticipate
// every way to reach the outside world, and an allowlist only has to be
// correct about what is already needed.
var allowedStdlib = map[string]bool{
	"fmt":           true,
	"go/parser":     true,
	"go/token":      true,
	"os":            true,
	"path/filepath": true,
	"reflect":       true,
	"sort":          true,
	"strconv":       true,
	"strings":       true,
	"testing":       true,
	"time":          true,
}

// TestModelPackageIsIndependent enforces the one structural claim this whole
// package rests on: it is an INDEPENDENT model.
//
// A reference model that imports the code it checks is not a second opinion,
// it is a mirror, and a differential test against a mirror passes no matter
// what either side does. Likewise a "pure" model that can open a socket is one
// careless import away from becoming an integration test that runs in the unit
// lane and fails in CI for reasons nobody can reproduce.
//
// Enforcing it as a test rather than a convention is the difference between a
// rule and a wish. The plan requires this package to be untagged, hermetic and
// product-independent; this is what makes that requirement hold six months from
// now.
func TestModelPackageIsIndependent(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}

	fset := token.NewFileSet()
	checked := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", entry.Name()), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", entry.Name(), err)
		}
		checked++
		for _, spec := range file.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("%s: unquote import %s: %v", entry.Name(), spec.Path.Value, err)
			}
			switch {
			case path == selfImport:
				continue
			case strings.HasPrefix(path, "github.com/caesium-cloud/caesium/"):
				t.Errorf("%s imports the product package %q: the model must not depend on the "+
					"decision logic it is meant to independently check", entry.Name(), path)
			case !strings.Contains(strings.SplitN(path, "/", 2)[0], "."):
				if !allowedStdlib[path] {
					t.Errorf("%s imports stdlib %q, which is not on the allowlist in "+
						"independence_test.go: add it there if the model genuinely needs it, and "+
						"never to reach the network, a socket, or a subprocess", entry.Name(), path)
				}
			default:
				if !allowedImports[path] {
					t.Errorf("%s imports third-party %q: C1 owns only the model dependencies "+
						"listed in independence_test.go; a proxy or container library needs its "+
						"own assigned owner", entry.Name(), path)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no Go files were inspected; the guard would pass vacuously")
	}
}

// TestModelPackageIsUntagged checks that no file here carries a build
// constraint. The package must compile and run under plain `go test ./...`, and
// a stray //go:build tag would silently remove it from the unit lane while
// leaving a green result behind.
func TestModelPackageIsUntagged(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		src, err := os.ReadFile(entry.Name())
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		for _, line := range strings.Split(string(src), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "//") {
				if strings.HasPrefix(line, "//go:build") || strings.HasPrefix(line, "// +build") {
					t.Errorf("%s carries a build constraint (%q); test/model must run in the unit lane",
						entry.Name(), line)
				}
				continue
			}
			if strings.HasPrefix(line, "package ") {
				break
			}
		}
	}
}
