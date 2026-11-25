//go:build linux

package clonereclaim

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestInventorySkipsInvalidNames(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	sentinel := filepath.Join(outside, "sentinel")
	writeFixture(t, sentinel, "outside survives")
	files := map[string]string{sentinel: "outside survives"}
	links := []string{}
	var warnings []string
	for _, name := range []string{"clones", "clone-reclamation"} {
		base := filepath.Join(root, ".moe", name)
		for _, key := range []string{"www/z-last", "www/a-first", "aaa/unregistered"} {
			writeFixture(t, filepath.Join(base, key, "contents"), "valid")
		}
		for _, parent := range []string{base, filepath.Join(base, "www")} {
			kind := "project"
			if parent != base {
				kind = "run"
			}
			for _, invalid := range []string{".claude", "INVALID", "bad\nname", ".link", ".file"} {
				path := filepath.Join(parent, invalid)
				warnings = append(warnings, fmt.Sprintf("clone gc inventory: skipping %q: invalid %s ID\n", path, kind))
				switch invalid {
				case ".link":
					if err := os.Symlink(outside, path); err != nil {
						t.Fatal(err)
					}
					links = append(links, path)
				case ".file":
					writeFixture(t, path, "file survives")
					files[path] = "file survives"
				default:
					if err := os.MkdirAll(filepath.Join(path, ".cc-writes"), 0o700); err != nil {
						t.Fatal(err)
					}
					child := filepath.Join(path, "valid-looking", "sentinel")
					writeFixture(t, child, "subtree survives")
					files[child] = "subtree survives"
				}
			}
		}
	}
	writeFixture(t, filepath.Join(root, ".moe", "clone-reclamation", "www", "retained-only", "receipt"), "retained")
	var diagnostics bytes.Buffer
	keys, err := Inventory(root, &diagnostics)
	want := []string{"aaa/unregistered", "www/a-first", "www/retained-only", "www/z-last"}
	if err != nil || !reflect.DeepEqual(keys, want) {
		t.Fatalf("inventory = %v, %v; want %v", keys, err, want)
	}
	remaining := diagnostics.String()
	for _, warning := range warnings {
		if strings.Count(remaining, warning) != 1 {
			t.Fatalf("expected one %q in %q", warning, remaining)
		}
		remaining = strings.ReplaceAll(remaining, warning, "")
	}
	if remaining != "" {
		t.Fatalf("unexpected diagnostics: %q", remaining)
	}
	for path, want := range files {
		if got, err := os.ReadFile(path); err != nil || string(got) != want {
			t.Errorf("skipped content %q = %q, %v", path, got, err)
		}
	}
	for _, path := range links {
		if got, err := os.Readlink(path); err != nil || got != outside {
			t.Errorf("skipped link %q = %q, %v", path, got, err)
		}
	}
}

func TestInventoryLimits(t *testing.T) {
	for _, base := range []string{"clones", "clone-reclamation"} {
		for _, kind := range []string{"project", "run", "candidate"} {
			t.Run(base+"/"+kind, func(t *testing.T) {
				root := t.TempDir()
				parent := filepath.Join(root, ".moe", base)
				limit := 8192
				if kind == "project" {
					limit = 4096
				}
				for i := 0; i <= limit; i++ {
					var path string
					switch kind {
					case "project":
						path = filepath.Join(parent, fmt.Sprintf(".invalid-%04d", i))
					case "run":
						path = filepath.Join(parent, "www", fmt.Sprintf(".invalid-%04d", i))
					case "candidate":
						path = filepath.Join(parent, fmt.Sprintf("p-%d", i/4096), fmt.Sprintf("r-%04d", i))
					}
					writeFixture(t, path, "retained")
				}
				keys, err := Inventory(root, io.Discard)
				if err == nil || err.Error() != kind+" inventory limit exceeded" || keys != nil {
					t.Fatalf("overflow = %v, %v", keys, err)
				}
			})
		}
	}
}
