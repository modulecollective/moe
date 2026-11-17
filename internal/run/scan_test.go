package run

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
)

// globScan is the implementation Scan replaced, kept as the reference
// TestScanMatchesGlob compares against.
func globScan(root string) ([]*Metadata, error) {
	matches, err := filepath.Glob(filepath.Join(root, "projects", "*", "runs", "*", "run.json"))
	if err != nil {
		return nil, err
	}
	var out []*Metadata
	for _, path := range matches {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		md := &Metadata{}
		if err := json.Unmarshal(b, md); err != nil {
			return nil, err
		}
		out = append(out, md)
	}
	return out, nil
}

// writeRun writes a minimal run.json for project/id under root.
func writeRun(t *testing.T, root, project, id, status string) {
	t.Helper()
	md := &Metadata{
		ID: id, Project: project, Status: status, Workflow: "sdlc",
		Documents: map[string]*Document{"design": {Sessions: map[string]string{"claude": "s1"}}},
	}
	if err := os.MkdirAll(filepath.Join(root, Dir(project, id)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Save(root, md); err != nil {
		t.Fatal(err)
	}
}

// countReads swaps readRunFile for a counting wrapper for the test.
func countReads(t *testing.T) *atomic.Int64 {
	t.Helper()
	var n atomic.Int64
	orig := readRunFile
	readRunFile = func(p string) ([]byte, error) {
		n.Add(1)
		return orig(p)
	}
	t.Cleanup(func() { readRunFile = orig })
	return &n
}

func scanKeys(t *testing.T, mds []*Metadata) []string {
	t.Helper()
	var keys []string
	for _, md := range mds {
		keys = append(keys, md.Project+"/"+md.ID+":"+md.Status)
	}
	return keys
}

func mustScan(t *testing.T, root string) []*Metadata {
	t.Helper()
	mds, err := Scan(root)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	return mds
}

func TestScanMatchesGlob(t *testing.T) {
	root := t.TempDir()
	if mds := mustScan(t, root); len(mds) != 0 {
		t.Fatalf("no projects/: got %d runs", len(mds))
	}
	writeRun(t, root, "beta", "b-1", StatusMerged)
	writeRun(t, root, "alpha", "a-2", StatusInProgress)
	writeRun(t, root, "alpha", "a-1", StatusClosed)
	// An empty runs/, a project with no runs/, a run dir with no
	// run.json, a stray file where a project or run dir would be.
	for _, d := range []string{"projects/empty/runs", "projects/noruns/docs", "projects/alpha/runs/no-json/documents"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"projects/README.md", "projects/alpha/runs/notes.txt"} {
		if err := os.WriteFile(filepath.Join(root, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := globScan(root)
	if err != nil {
		t.Fatal(err)
	}
	for pass := 1; pass <= 2; pass++ { // cold, then memo hit
		got := mustScan(t, root)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("pass %d: Scan = %v, glob = %v", pass, scanKeys(t, got), scanKeys(t, want))
		}
	}
	if k := strings.Join(scanKeys(t, want), " "); k != "alpha/a-1:closed alpha/a-2:in_progress beta/b-1:merged" {
		t.Fatalf("fixture order = %q", k)
	}
}

func TestScanMemoHitSkipsReads(t *testing.T) {
	root := t.TempDir()
	writeRun(t, root, "p", "r1", StatusMerged)
	writeRun(t, root, "p", "r2", StatusClosed)
	reads := countReads(t)
	first := mustScan(t, root)
	if n := reads.Load(); n != 2 {
		t.Fatalf("cold scan read %d files, want 2", n)
	}
	second := mustScan(t, root)
	if n := reads.Load(); n != 2 {
		t.Fatalf("warm scan re-read: %d reads total, want 2", n)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("warm scan differs: %v vs %v", scanKeys(t, first), scanKeys(t, second))
	}
}

// The negative control for the hit test: a cache that never expires
// passes that one, and fails this.
func TestScanSeesSavedChange(t *testing.T) {
	root := t.TempDir()
	writeRun(t, root, "p", "r1", StatusMerged)
	writeRun(t, root, "p", "r2", StatusClosed)
	reads := countReads(t)
	mustScan(t, root)
	md, err := Load(root, "p", "r2")
	if err != nil {
		t.Fatal(err)
	}
	md.Status = StatusInProgress
	if err := Save(root, md); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(scanKeys(t, mustScan(t, root)), " ")
	if got != "p/r1:merged p/r2:in_progress" {
		t.Fatalf("after Save: %s", got)
	}
	if n := reads.Load(); n != 3 {
		t.Fatalf("reads = %d, want 3 (two cold, one changed)", n)
	}
}

func TestScanNewAndDeletedRuns(t *testing.T) {
	root := t.TempDir()
	writeRun(t, root, "p", "old", StatusMerged)
	writeRun(t, root, "p", "keep", StatusMerged)
	mustScan(t, root)
	if err := os.RemoveAll(filepath.Join(root, Dir("p", "old"))); err != nil {
		t.Fatal(err)
	}
	writeRun(t, root, "q", "new", StatusInProgress)
	got := strings.Join(scanKeys(t, mustScan(t, root)), " ")
	if got != "p/keep:merged q/new:in_progress" {
		t.Fatalf("after add/remove: %s", got)
	}
	scanMemo.Lock()
	n := len(scanMemo.entries[root])
	scanMemo.Unlock()
	if n != 2 {
		t.Fatalf("memo holds %d entries, want 2 (deleted run lingers)", n)
	}
}

func TestScanReturnsCopies(t *testing.T) {
	root := t.TempDir()
	writeRun(t, root, "p", "r", StatusMerged)
	mds := mustScan(t, root)
	mds[0].Status = StatusInProgress
	mds[0].Documents["design"].Sessions["claude"] = "mutated"
	mds[0].Documents["code"] = &Document{}
	again := mustScan(t, root)[0]
	if again.Status != StatusMerged {
		t.Fatalf("scalar mutation leaked: status %q", again.Status)
	}
	if len(again.Documents) != 1 || again.Documents["design"].Sessions["claude"] != "s1" {
		t.Fatalf("Documents mutation leaked: %+v", again.Documents)
	}
}

func TestScanDoesNotCacheErrors(t *testing.T) {
	root := t.TempDir()
	writeRun(t, root, "p", "ok", StatusMerged)
	bad := filepath.Join(root, Dir("p", "bad"), "run.json")
	if err := os.MkdirAll(filepath.Dir(bad), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Scan(root); err == nil || !strings.Contains(err.Error(), "parse") {
		t.Fatalf("malformed run.json: err = %v", err)
	}
	if _, err := Scan(root); err == nil {
		t.Fatal("second scan of malformed run.json succeeded: error was cached away")
	}
	writeRun(t, root, "p", "bad", StatusClosed)
	got := strings.Join(scanKeys(t, mustScan(t, root)), " ")
	if got != "p/bad:closed p/ok:merged" {
		t.Fatalf("after fix: %s", got)
	}
	// A workflow-less run.json is refused the same way, warm or cold.
	if err := os.WriteFile(bad, []byte(`{"id":"bad","project":"p"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Scan(root); err == nil || !strings.Contains(err.Error(), "workflow is required") {
		t.Fatalf("workflow-less run.json: err = %v", err)
	}
}

// TestMetadataCloneSharesNothing fills every field of Metadata (and the
// structs it points at) and checks clone leaves no pointer or map
// shared. A new reference-typed field fails the fill check below until
// fillAll knows how to set it, then fails the sharing check until
// clone copies it.
func TestMetadataCloneSharesNothing(t *testing.T) {
	m := &Metadata{}
	fillAll(t, reflect.ValueOf(m).Elem(), "Metadata")
	c := m.clone()
	if !reflect.DeepEqual(m, c) {
		t.Fatalf("clone differs:\n%+v\n%+v", m, c)
	}
	assertDisjoint(t, reflect.ValueOf(m).Elem(), reflect.ValueOf(c).Elem(), "Metadata")
}

func fillAll(t *testing.T, v reflect.Value, path string) {
	t.Helper()
	switch v.Kind() {
	case reflect.String:
		v.SetString(path)
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int64:
		v.SetInt(1)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			fillAll(t, v.Field(i), path+"."+v.Type().Field(i).Name)
		}
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		fillAll(t, v.Elem(), path)
	case reflect.Map:
		v.Set(reflect.MakeMap(v.Type()))
		k := reflect.New(v.Type().Key()).Elem()
		fillAll(t, k, path+"[k]")
		e := reflect.New(v.Type().Elem()).Elem()
		fillAll(t, e, path+"[v]")
		v.SetMapIndex(k, e)
	default:
		t.Fatalf("%s: fillAll does not handle %s; teach it, and Metadata.clone", path, v.Kind())
	}
}

func assertDisjoint(t *testing.T, a, b reflect.Value, path string) {
	t.Helper()
	switch a.Kind() {
	case reflect.Struct:
		for i := 0; i < a.NumField(); i++ {
			assertDisjoint(t, a.Field(i), b.Field(i), path+"."+a.Type().Field(i).Name)
		}
	case reflect.Pointer:
		if a.IsNil() {
			return
		}
		if a.Pointer() == b.Pointer() {
			t.Errorf("%s: clone shares the pointer", path)
			return
		}
		assertDisjoint(t, a.Elem(), b.Elem(), path)
	case reflect.Map:
		if a.IsNil() {
			return
		}
		if a.Pointer() == b.Pointer() {
			t.Errorf("%s: clone shares the map", path)
			return
		}
		keys := a.MapKeys()
		sort.Slice(keys, func(i, j int) bool { return fmt.Sprint(keys[i]) < fmt.Sprint(keys[j]) })
		for _, k := range keys {
			assertDisjoint(t, a.MapIndex(k), b.MapIndex(k), fmt.Sprintf("%s[%v]", path, k))
		}
	}
}
