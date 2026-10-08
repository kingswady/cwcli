package main

import (
	"go/build"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// layers is what each internal package may import from this module: code
// only calls downward, and nothing but cmd/cw uses commands.
var layers = map[string][]string{
	"output":     {},
	"platform":   {},
	"config":     {"platform"},
	"mcpbridge":  {},
	"selfupdate": {"platform"},
	"commands":   {"config", "mcpbridge", "output", "platform", "selfupdate"},
}

const module = "github.com/kingswady/cwcli/internal/"

func TestEachLayerOnlyUsesTheLayersBelowIt(t *testing.T) {
	dirs, err := os.ReadDir(filepath.Join("..", "..", "internal"))
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range dirs {
		allowed, known := layers[dir.Name()]
		if !known {
			t.Errorf("internal/%s is not in the layer map: decide what it may use", dir.Name())
			continue
		}
		pkg, err := build.ImportDir(filepath.Join("..", "..", "internal", dir.Name()), 0)
		if err != nil {
			t.Fatal(err)
		}
		var used []string
		for _, path := range append(pkg.Imports, pkg.TestImports...) {
			if name, ok := strings.CutPrefix(path, module); ok && name != dir.Name() {
				used = append(used, name)
			}
		}
		sort.Strings(used)
		for _, name := range used {
			if !contains(allowed, name) {
				t.Errorf("internal/%s imports internal/%s; it may use only %v", dir.Name(), name, allowed)
			}
		}
	}
}

func contains(list []string, item string) bool {
	for _, x := range list {
		if x == item {
			return true
		}
	}
	return false
}
