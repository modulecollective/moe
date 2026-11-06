package contextuse

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
)

// seedRun writes run.json straight to disk (run.Scan reads nothing
// else) and commits a thread for one stage with the work-turn trailer
// the journal index dates it by.
func seedRun(t *testing.T, root, project, id, workflow, stage, agent, thread string) {
	t.Helper()
	write(t, filepath.Join(root, run.Dir(project, id), "run.json"), mustJSON(t, run.Metadata{
		ID: id, Project: project, Status: run.StatusInProgress, Workflow: workflow,
		Documents: map[string]*run.Document{stage: {}},
	}))
	write(t, filepath.Join(root, run.ThreadPathFor(agent, project, id, stage)), thread)
	gittest.Run(t, root, "add", "-A")
	gittest.Run(t, root, "commit", "-q", "-m",
		"work: update "+stage+"\n\nMoE-Run: "+id+"\nMoE-Project: "+project+"\nMoE-Document: "+stage+"\n")
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// lines joins records into a JSONL body.
func lines(t *testing.T, recs ...any) string {
	var b strings.Builder
	for _, r := range recs {
		b.WriteString(mustJSON(t, r) + "\n")
	}
	return b.String()
}

type m = map[string]any

func claudeCall(id, name string, input m) m {
	return m{"type": "assistant", "message": m{"content": []m{{"type": "tool_use", "id": id, "name": name, "input": input}}}}
}

func claudeResult(id, text string, isError bool) m {
	return m{"type": "user", "message": m{"content": []m{{"type": "tool_result", "tool_use_id": id, "content": text, "is_error": isError}}}}
}

func codexExec(id, script string) m {
	return m{"type": "response_item", "payload": m{"type": "custom_tool_call", "call_id": id, "name": "exec", "input": script}}
}

func codexExecCommand(id, cmd string) m {
	return m{"type": "response_item", "payload": m{"type": "function_call", "call_id": id, "name": "exec_command",
		"arguments": `{"cmd":` + strings.TrimSpace(mustJSONString(cmd)) + `}`}}
}

func mustJSONString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func codexOutput(id, text string) m {
	return m{"type": "response_item", "payload": m{"type": "custom_tool_call_output", "call_id": id,
		"output": []m{{"type": "input_text", "text": "Script completed\nOutput:\n"}, {"type": "input_text", "text": text}}}}
}

func fixture(t *testing.T) (root string, sessions []Session) {
	t.Helper()
	root = gittest.InitSeeded(t)
	b := root + "/projects/p"
	write(t, filepath.Join(root, "lore", "read-me.md"), "x\n")
	write(t, filepath.Join(root, "lore", "never-read.md"), "x\n")
	write(t, b+"/knowledge/index.md", "# k\n\n- [t](topics/opened.md)\n")
	write(t, b+"/knowledge/topics/opened.md", "x\n")
	write(t, b+"/knowledge/topics/ignored.md", "x\n")
	write(t, b+"/digital-twin/vision.md", "# Vision\n")
	write(t, b+"/digital-twin/architecture.md", "# Architecture\n"+strings.Repeat("x", 4*CodexResultCapTokens+40)+"\n")

	// Claude: a skill load, a clean twin read, a lore read that came back
	// persisted (truncated), a knowledge-index Read that wasn't there, a
	// listing (not a read), its own canvas (not context) and another
	// run's canvas (a prior-run read). One call never got a result.
	claude := lines(t,
		claudeCall("s", "Skill", m{"skill": "moe-context"}),
		claudeResult("s", "Launching skill: moe-context", false),
		claudeCall("r1", "Read", m{"file_path": b + "/digital-twin/vision.md"}),
		claudeResult("r1", "# Vision", false),
		claudeCall("b1", "Bash", m{"command": "cat " + root + "/lore/read-me.md && ls " + b + "/digital-twin"}),
		claudeResult("b1", "<persisted-output>\nOutput too large", false),
		claudeCall("r2", "Read", m{"file_path": "/elsewhere/projects/p/knowledge/index.md"}),
		claudeResult("r2", "File does not exist.", true),
		claudeCall("b2", "Bash", m{"command": "sed -n 1,40p " + b + "/runs/stage-run/documents/design/content.md; cat " + b + "/runs/old-run/documents/design/content.md"}),
		claudeResult("b2", "...", false),
		claudeCall("b3", "Bash", m{"command": "grep -n Rex " + b + "/knowledge/topics/opened.md"}),
	)
	seedRun(t, root, "p", "stage-run", "sdlc", "design", "claude", claude)

	// Codex: commands in a backtick array mapped over later — the shape
	// that defeats `cmd:` extraction — whose script output was cut; then
	// a relative twin path that misses.
	script := "const p=\"" + root + "\"; const cmds=[[\"skill\",`cat '" + root + "/skills/moe-context/SKILL.md'`],[\"arch\",`cat '${p}/projects/p/digital-twin/architecture.md'`],[\"intent\",`cat '${p}/projects/p/runs/north-star/documents/intent/content.md'`]];"
	codex := lines(t,
		codexExec("c1", script),
		codexOutput("c1", "Warning: truncated output (original token count: 12000)\nTotal output lines: 3\n{\"chunk_id\":\"a\"}"),
		codexExecCommand("c2", "cat digital-twin/vision.md"),
		m{"type": "response_item", "payload": m{"type": "function_call_output", "call_id": "c2",
			"output": "cat: digital-twin/vision.md: No such file or directory"}},
	)
	seedRun(t, root, "p", "pulse-run", "pulse", "pulse", "codex", codex)

	sessions, err := Gather(root, Filter{}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 {
		t.Fatalf("want 2 sessions, got %d: %+v", len(sessions), sessions)
	}
	return root, sessions
}

func byAgent(sessions []Session, agent string) Session {
	i := slices.IndexFunc(sessions, func(s Session) bool { return s.Agent == agent })
	return sessions[i]
}

func TestGatherClaude(t *testing.T) {
	_, sessions := fixture(t)
	s := byAgent(sessions, "claude")
	if !s.Skill {
		t.Error("Skill call not counted as a skill load")
	}
	if want := []string{"p/vision.md"}; !slices.Equal(s.Twin, want) {
		t.Errorf("Twin = %v, want %v (an ls is not a read)", s.Twin, want)
	}
	if want := []string{"read-me"}; !slices.Equal(s.Lore, want) {
		t.Errorf("Lore = %v, want %v", s.Lore, want)
	}
	if s.Index {
		t.Error("a Read that failed must not count as an index read")
	}
	if want := []string{"/elsewhere/projects/p/knowledge/index.md"}; !slices.Equal(s.Misses, want) {
		t.Errorf("Misses = %v, want %v", s.Misses, want)
	}
	if want := []string{"old-run"}; !slices.Equal(s.PriorRuns, want) {
		t.Errorf("PriorRuns = %v, want %v (own canvas is not a prior run)", s.PriorRuns, want)
	}
	// vision, lore, old-run read; the lore read arrived persisted; the
	// grep of a topic never got a result.
	if s.Reads != 3 || s.Truncated != 1 || s.Unpaired != 1 {
		t.Errorf("Reads/Truncated/Unpaired = %d/%d/%d, want 3/1/1", s.Reads, s.Truncated, s.Unpaired)
	}
	if len(s.Topics) != 0 {
		t.Errorf("an unpaired read must not count as a topic read: %v", s.Topics)
	}
}

func TestGatherCodex(t *testing.T) {
	_, sessions := fixture(t)
	s := byAgent(sessions, "codex")
	if !s.Skill || !s.Pulse() {
		t.Errorf("Skill=%v Pulse=%v, want both", s.Skill, s.Pulse())
	}
	if want := []string{"p/architecture.md"}; !slices.Equal(s.Twin, want) {
		t.Errorf("Twin = %v, want %v", s.Twin, want)
	}
	if want := []string{"north-star"}; !slices.Equal(s.Intents, want) {
		t.Errorf("Intents = %v, want %v", s.Intents, want)
	}
	if s.Reads != 2 || s.Truncated != 2 {
		t.Errorf("Reads/Truncated = %d/%d, want 2/2 (both reads sat in the cut call)", s.Reads, s.Truncated)
	}
	if want := []string{"digital-twin/vision.md"}; !slices.Equal(s.Misses, want) {
		t.Errorf("Misses = %v, want %v", s.Misses, want)
	}
}

func TestRenderHygiene(t *testing.T) {
	root, sessions := fixture(t)
	// A glob read and a lore file outside the inventory (a scratch
	// board's) are read keys with no entry; the header must not count them.
	sessions[0].Lore = append(sessions[0].Lore, "*", "scratch-only")
	var out bytes.Buffer
	RenderHygiene(&out, sessions, ReadInventory(root, []string{"p"}), []string{"p"})
	got := out.String()
	for _, want := range []string{
		"LORE  2 entries, 1 read by these sessions",
		"  unread (1):\n    never-read\n",
		"KNOWLEDGE  p: index 3 lines, read in 0 of 2 sessions; 2 topics",
		"  unread (2):\n    ignored.md\n    opened.md\n",
		"TWIN  p:",
		"over the cap",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("hygiene view missing %q in:\n%s", want, got)
		}
	}
	if strings.Count(got, "over the cap") != 1 {
		t.Errorf("only architecture.md is over the cap:\n%s", got)
	}
}

func TestRenderByBackend(t *testing.T) {
	_, sessions := fixture(t)
	var out bytes.Buffer
	RenderByBackend(&out, sessions)
	got := out.String()
	for _, want := range []string{"PERIOD", "TRUNC", "NAMED MISSES", "digital-twin/vision.md"} {
		if !strings.Contains(got, want) {
			t.Errorf("by-backend view missing %q in:\n%s", want, got)
		}
	}
}

func TestRenderSessionsJSON(t *testing.T) {
	_, sessions := fixture(t)
	var out bytes.Buffer
	if err := RenderSessions(&out, sessions); err != nil {
		t.Fatal(err)
	}
	ls := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(ls) != 2 {
		t.Fatalf("want one line per session, got %d", len(ls))
	}
	var s Session
	if err := json.Unmarshal([]byte(ls[0]), &s); err != nil {
		t.Fatal(err)
	}
	if s.Project != "p" || s.Run == "" {
		t.Errorf("round-tripped session lost its identity: %+v", s)
	}
}

func TestJSStringLiterals(t *testing.T) {
	got := jsStringLiterals(`const a="x\"y"; f('it''s', ` + "`cat '${p}/a'`" + `)`)
	want := []string{`x"y`, "it", "s", "cat '${p}/a'"}
	if !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

// The CLI read routes carry no path: `moe intent cat <p>/<slug>` is an
// intent read, `moe <wf> log <p>/<slug>` a prior-run read unless it is
// the session's own run, and a short `moe sdlc log` reads nothing.
func TestCommandMentionsMoeRoutes(t *testing.T) {
	p := &parser{s: &Session{Project: "p", Run: "mine"}}
	for _, tc := range []struct {
		cmd       string
		cat, key  string
		wantNoHit bool
	}{
		{cmd: "moe intent cat p/north-star", cat: catIntent, key: "north-star"},
		{cmd: "cd x && moe sdlc log p/older | head", cat: catPrior, key: "older"},
		{cmd: "moe sdlc log p/mine", wantNoHit: true},
		{cmd: "moe sdlc log", wantNoHit: true},
		{cmd: "ls /b/projects/p/digital-twin", wantNoHit: true},
		{cmd: "for f in /b/projects/p/digital-twin/*.md; do cat \"$f\"; done", cat: catTwin, key: "p/*"},
	} {
		got := p.commandMentions(tc.cmd)
		if tc.wantNoHit {
			if len(got) != 0 {
				t.Errorf("%q: want no mention, got %+v", tc.cmd, got)
			}
			continue
		}
		if len(got) != 1 || got[0].cat != tc.cat || got[0].key != tc.key {
			t.Errorf("%q: got %+v, want one %s/%s", tc.cmd, got, tc.cat, tc.key)
		}
	}
}

// A relative path's not-found line must not be pinned on the absolute
// read of the same doc in the same call; a `${p}`-prefixed path matches
// its expansion.
func TestNamesPath(t *testing.T) {
	lines := []string{"cat: digital-twin/architecture.md: No such file or directory"}
	if namesPath(lines, "/b/projects/moe/digital-twin/architecture.md") {
		t.Error("absolute read blamed for the relative path's miss")
	}
	if !namesPath(lines, "digital-twin/architecture.md") {
		t.Error("relative path's own miss not matched")
	}
	abs := []string{"cat: /b/projects/moe/runs/x/documents/intent/content.md: No such file or directory"}
	if !namesPath(abs, "${p}/projects/moe/runs/x/documents/intent/content.md") {
		t.Error("${p}-prefixed path not matched against its expansion")
	}
	if namesPath(lines, "/b/digital-twin/*.md") {
		t.Error("a glob can't be matched and must not count as a miss")
	}
}
