package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/agent"
	"github.com/modulecollective/moe/internal/agent/claude"
	"github.com/modulecollective/moe/internal/agent/codex"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
)

func TestBackendSwitchRoundTrip(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprint(reverse), func(t *testing.T) {
			root, _ := setupResumeFixture(t, "unused-switch")
			a := registerOwned(t, "owned-a", identityA)
			b := registerOwned(t, "owned-b", identityB)
			binding := loadRun(t, root, "tele", "fix-it")
			binding.Agent = b.name
			if err := run.Save(root, binding); err != nil {
				t.Fatal(err)
			}
			gittest.Commit(t, root, "opposite run binding")
			first, second := a, b
			if reverse {
				first, second = b, a
			}
			for _, f := range []*ownedAgent{first, second, first, second} {
				driveDesignTurn(t, f.name, false)
				md := loadRun(t, root, "tele", "fix-it")
				if got := md.Documents["design"].Sessions[f.name]; got != f.id {
					t.Fatalf("saved slot %s = %q, want %s", f.name, got, f.id)
				}
				if md.Documents["design"].Session != "" {
					t.Fatal("new metadata wrote legacy scalar")
				}
				msg := gittest.Output(t, root, "log", "-1", "--format=%B", "--grep=^work: update design")
				if !strings.Contains(msg, "MoE-Session: "+f.id) {
					t.Fatalf("wrong turn trailer: %s", msg)
				}
			}
			for _, f := range []*ownedAgent{a, b} {
				if len(f.execCalls) != 2 || !f.execCalls[0].newSession || f.execCalls[1].newSession || f.execCalls[1].sessionID != f.id {
					t.Fatalf("%s dispatches: %+v", f.name, f.execCalls)
				}
				body, err := os.ReadFile(filepath.Join(root, run.ThreadPathFor(f.name, "tele", "fix-it", "design")))
				if err != nil {
					t.Fatal(err)
				}
				if strings.Count(string(body), f.name+" turn") != 2 {
					t.Fatalf("lost mirror history: %s", body)
				}
			}
			// A model-only change and clearing the reap note must both resume.
			md := loadRun(t, root, "tele", "fix-it")
			md.Reaped = &run.ReapNote{Doc: "design"}
			if err := run.Save(root, md); err != nil {
				t.Fatal(err)
			}
			gittest.Commit(t, root, "reaped marker")
			var out bytes.Buffer
			if code := runStageSession("tele", "fix-it", "design", stageSessionOpts{Agent: a.name, Model: "another-model", SkipNextStage: true}, &out, &out); code != 0 {
				t.Fatalf("model turn: %s", &out)
			}
			call := a.execCalls[2]
			if call.newSession || call.sessionID != a.id || call.model != "another-model" {
				t.Fatalf("model/tombstone reset: %+v", call)
			}
			if loadRun(t, root, "tele", "fix-it").Reaped != nil {
				t.Fatal("reap note survived")
			}
		})
	}
}

func TestBackendHeadlessIdentityWriteback(t *testing.T) {
	for _, withError := range []bool{false, true} {
		t.Run(fmt.Sprint(withError), func(t *testing.T) {
			root, _ := setupResumeFixture(t, "unused-headless")
			a := registerOwned(t, "headless-a", identityA)
			b := registerOwned(t, "headless-b", identityB)
			a.returned = identityA
			if withError {
				a.execErr = errors.New("executor failed after returning ID")
			}
			code := driveDesignTurnCode(t, a.name, true)
			if (code != 0) != withError {
				t.Fatalf("exit=%d error=%v", code, withError)
			}
			a.execErr = nil
			a.returned = ""
			driveDesignTurn(t, b.name, false)
			driveDesignTurn(t, a.name, false)
			if call := a.execCalls[0]; call.newSession || call.sessionID != identityA {
				t.Fatalf("headless return lost: %+v", call)
			}
			a.returned = identityC
			driveDesignTurn(t, a.name, true)
			md := loadRun(t, root, "tele", "fix-it")
			if md.Documents["design"].Sessions[a.name] != identityC || md.Documents["design"].Sessions[b.name] != identityB {
				t.Fatalf("headless overwrote wrong slot: %+v", md.Documents["design"])
			}
		})
	}
}

func writeIdentityMirror(t *testing.T, path, id string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(fmt.Sprintf("{\"type\":\"user\",\"sessionId\":%q}\n", id)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyBackendAdoption(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		native, mirror, legacy string
		wantFresh              bool
		want                   string
	}{
		{name: "native without mirror", native: identityA, legacy: identityA, want: identityA},
		{name: "native outranks lagging mirror", native: identityA, mirror: identityB, legacy: identityA, want: identityA},
		{name: "other backend owns scalar", mirror: identityB, legacy: identityA, want: identityB},
		{name: "mirror only", mirror: identityB, want: identityB},
		{name: "unrecoverable", legacy: identityA, wantFresh: true},
		{name: "first use", wantFresh: true},
		{name: "invalid legacy", legacy: "old-id", wantFresh: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, "config"))
			mirror := filepath.Join(root, "thread-claude.jsonl")
			if tc.native != "" {
				writeIdentityMirror(t, filepath.Join(root, "config", "projects", "old-bucket", tc.native+".jsonl"), tc.native)
			}
			if tc.mirror != "" {
				writeIdentityMirror(t, mirror, tc.mirror)
			}
			doc := &run.Document{Session: tc.legacy, Sessions: map[string]string{"codex": identityC}}
			var diagnostics bytes.Buffer
			id, fresh, changed, err := selectBackendSession(claude.Agent{}, doc, "claude", filepath.Join(root, "cwd"), mirror, root, &diagnostics)
			if err != nil {
				t.Fatal(err)
			}
			if fresh != tc.wantFresh || !changed || !run.ValidSessionID(id) || (tc.want != "" && id != tc.want) {
				t.Fatalf("selection %q fresh=%v changed=%v", id, fresh, changed)
			}
			if tc.wantFresh {
				if tc.legacy == "" && diagnostics.Len() != 0 {
					t.Fatalf("first use warned of history loss: %s", &diagnostics)
				}
				if tc.legacy != "" && !strings.Contains(diagnostics.String(), "has no recoverable claude identity") {
					t.Fatalf("missing identity-evidence diagnostic: %s", &diagnostics)
				}
				if strings.Contains(diagnostics.String(), "prior chat history not recoverable") {
					t.Fatalf("unowned legacy identity claimed history loss: %s", &diagnostics)
				}
			}
			if doc.Session != tc.legacy || doc.Sessions["codex"] != identityC {
				t.Fatalf("other identity changed: %+v", doc)
			}
			md := &run.Metadata{ID: "r", Project: "p", Workflow: "sdlc", Documents: map[string]*run.Document{"design": doc}}
			if err := run.Save(root, md); err != nil {
				t.Fatal(err)
			}
			reloaded := loadRun(t, root, "p", "r")
			if reloaded.Documents["design"].Sessions["claude"] != id || reloaded.Documents["design"].Session != tc.legacy {
				t.Fatal("adoption did not survive reload")
			}
		})
	}
}

func TestBackendRecoveryIdentityControls(t *testing.T) {
	for _, tc := range []struct {
		name, mirror string
		backend      string
		wantErr      bool
	}{
		{"matching Claude", identityA, "claude", false},
		{"stale Claude", identityB, "claude", false},
		{"missing Codex rollout", identityA, "codex", false},
		{"malformed Claude", "broken", "claude", true},
		{"conflicting Claude", "conflict", "claude", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, "config"))
			t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
			mirror := filepath.Join(root, "thread.jsonl")
			var body string
			switch tc.mirror {
			case "broken":
				body = "{broken\n"
			case "conflict":
				body = fmt.Sprintf("{\"sessionId\":%q}\n{\"sessionId\":%q}\n", identityA, identityB)
			default:
				if tc.backend == "claude" {
					body = fmt.Sprintf("{\"sessionId\":%q}\n", tc.mirror)
				} else {
					body = fmt.Sprintf("{\"type\":\"session_meta\",\"payload\":{\"id\":%q}}\n", tc.mirror)
				}
			}
			if err := os.WriteFile(mirror, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			doc := &run.Document{Sessions: map[string]string{tc.backend: identityA, "other": identityC}}
			var a agent.Agent = claude.Agent{}
			if tc.backend == "codex" {
				a = codex.Agent{}
			}
			var diagnostics bytes.Buffer
			id, fresh, changed, err := selectBackendSession(a, doc, tc.backend, root, mirror, root, &diagnostics)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error=%v", err)
			}
			if tc.wantErr {
				got, _ := os.ReadFile(mirror)
				if string(got) != body {
					t.Fatal("corrupt evidence overwritten")
				}
				return
			}
			if tc.name == "matching Claude" {
				if id != identityA || fresh || changed {
					t.Fatalf("matching restore: %s %v %v", id, fresh, changed)
				}
			} else {
				if id == identityA || !fresh || !changed || !strings.Contains(diagnostics.String(), "prior chat history not recoverable") {
					t.Fatalf("missing history: %s %v %v %s", id, fresh, changed, &diagnostics)
				}
			}
			if doc.Sessions["other"] != identityC {
				t.Fatal("reset another backend")
			}
		})
	}
}

func TestLegacySwitchAdoptsBothBackends(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	claudeMirror := filepath.Join(root, "thread-claude.jsonl")
	codexMirror := filepath.Join(root, "thread-codex.jsonl")
	writeIdentityMirror(t, claudeMirror, identityB)
	codexBody := fmt.Sprintf("{\"type\":\"session_meta\",\"payload\":{\"id\":%q}}\n", identityA)
	if err := os.WriteFile(codexMirror, []byte(codexBody), 0o644); err != nil {
		t.Fatal(err)
	}
	native := filepath.Join(root, "codex", "sessions", "2026", "10", "30", "rollout-date-"+identityA+".jsonl")
	if err := os.MkdirAll(filepath.Dir(native), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(native, []byte(codexBody), 0o644); err != nil {
		t.Fatal(err)
	}
	md := &run.Metadata{ID: "r", Project: "p", Workflow: "sdlc", Agent: "claude", Documents: map[string]*run.Document{"design": {Session: identityA}}}
	for _, backend := range []string{"claude", "codex", "claude", "codex"} {
		a, err := agent.Get(backend)
		if err != nil {
			t.Fatal(err)
		}
		mirror := claudeMirror
		want := identityB
		if backend == "codex" {
			mirror = codexMirror
			want = identityA
		}
		id, fresh, _, err := selectBackendSession(a, md.Documents["design"], backend, root, mirror, root, io.Discard)
		if err != nil || fresh || id != want {
			t.Fatalf("%s adopted %q fresh=%v: %v", backend, id, fresh, err)
		}
		if err := run.Save(root, md); err != nil {
			t.Fatal(err)
		}
		md = loadRun(t, root, "p", "r")
	}
	if md.Documents["design"].Session != identityA || len(md.Documents["design"].Sessions) != 2 {
		t.Fatalf("lost legacy identities: %+v", md.Documents["design"])
	}
}

func TestBackendSelectionHealsOnlySelectedSlot(t *testing.T) {
	root := t.TempDir()
	doc := &run.Document{Session: identityA, Sessions: map[string]string{"selected": "invalid", "other": "also-invalid"}}
	id, fresh, changed, err := selectBackendSession(&fakeAgent{}, doc, "selected", root, filepath.Join(root, "missing"), root, io.Discard)
	if err != nil || !fresh || !changed || !run.ValidSessionID(id) {
		t.Fatalf("healing %s %v %v: %v", id, fresh, changed, err)
	}
	if doc.Session != identityA || doc.Sessions["other"] != "also-invalid" {
		t.Fatalf("healed unselected metadata: %+v", doc)
	}
}

func TestBackendEmptyReturnPreservesSelectedSlot(t *testing.T) {
	for _, withError := range []bool{false, true} {
		t.Run(fmt.Sprint(withError), func(t *testing.T) {
			const backend = "empty-return"
			root, f := setupResumeFixture(t, backend)
			md := loadRun(t, root, "tele", "fix-it")
			md.Documents["design"] = &run.Document{Session: identityC, Sessions: map[string]string{backend: identityA, "other": identityB}}
			if err := run.Save(root, md); err != nil {
				t.Fatal(err)
			}
			gittest.Commit(t, root, "existing backend slots")
			if withError {
				f.oneShotErr = errors.New("no ID returned on failure")
			}
			code := driveDesignTurnCode(t, backend, true)
			if (code != 0) != withError {
				t.Fatalf("exit=%d error=%v", code, withError)
			}
			doc := loadRun(t, root, "tele", "fix-it").Documents["design"]
			if doc.Sessions[backend] != identityA || doc.Sessions["other"] != identityB || doc.Session != identityC {
				t.Fatalf("empty return changed identities: %+v", doc)
			}
		})
	}
}

func TestBackendNativeCacheRecoveryIgnoresCorruptMirror(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, "config"))
	native := filepath.Join(root, "config", "projects", "old-cwd", identityA+".jsonl")
	writeIdentityMirror(t, native, identityA)
	mirror := filepath.Join(root, "thread-claude.jsonl")
	if err := os.WriteFile(mirror, []byte("{broken\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	doc := &run.Document{Sessions: map[string]string{"claude": identityA, "codex": identityB}}
	var diagnostics bytes.Buffer
	id, fresh, changed, err := selectBackendSession(claude.Agent{}, doc, "claude", root, mirror, root, &diagnostics)
	if err != nil || id != identityA || fresh || changed {
		t.Fatalf("native recovery %q %v %v: %v", id, fresh, changed, err)
	}
	if !strings.Contains(diagnostics.String(), "recovered from cache") {
		t.Fatalf("missing native recovery notice: %s", &diagnostics)
	}
	if doc.Sessions["codex"] != identityB {
		t.Fatal("native recovery changed another backend")
	}
}
