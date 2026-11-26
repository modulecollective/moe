package serve

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runowner"
	"github.com/modulecollective/moe/internal/servepause"
	"github.com/modulecollective/moe/internal/session"
	"github.com/modulecollective/moe/internal/trailers"
)

// The ticker's due verdict is supplied; every path after it is production:
// real child CLI, pulse session, fake survey, actual floor and sibling stage.
func TestPauseAfterAdmissionAllowsRealSurveyRideBesideHuman(t *testing.T) {
	root := gittest.InitSeeded(t)
	gittest.WriteAndCommit(t, root, "bureaucracy.conf", "", "mark bureaucracy")
	source := gittest.InitSeeded(t)
	gittest.Run(t, root, "-c", "protocol.file.allow=always", "submodule", "add", "-b", "main", source, "projects/widget/src")
	gittest.WriteAndCommit(t, root, "projects/widget/project.json", fmt.Sprintf(`{"id":"widget","submodule":"projects/widget/src","remote":%q,"default_branch":"main"}`, source), "register widget")
	if _, err := run.New(root, "widget", run.Options{ID: "human", Workflow: "sdlc", SeedDocs: map[string]string{"design": "# Human draft\n"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := run.New(root, "widget", run.Options{ID: "sibling", Workflow: "sdlc", DesignOnly: true, SpawnedBy: "_cabinet/seed", Trailers: trailers.Block{Workflow: "sdlc", Consent: "dynamic", SpawnedBy: "_cabinet/seed"}, SeedDocs: map[string]string{"design": "# Sibling brief\n"}}); err != nil {
		t.Fatal(err)
	}
	gittest.WriteAndCommit(t, root, "model-stylesheet.css", "* {agent:claude;}\n", "policy")
	human, err := session.Open(root, "widget", "human", "design")
	if err != nil {
		t.Fatal(err)
	}
	stop, err := session.Hold(human, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	owner, err := runowner.Acquire(root, "widget", "human")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Release() })
	draft := filepath.Join(human.WorktreePath, "draft")
	if err := os.WriteFile(draft, []byte("human bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	humanTip := gittest.Output(t, root, "rev-parse", human.Branch)
	claim, ok := session.ReadClaim(root, "widget", "human", "design")
	if !ok {
		t.Fatal("no human claim")
	}
	dir := t.TempDir()
	realBin, wrapper := filepath.Join(dir, "real-moe"), filepath.Join(dir, "moe")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "go", "build", "-o", realBin, "../../cmd/moe").CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	if err := os.WriteFile(wrapper, []byte(fmt.Sprintf("#!/bin/sh\nexec %q --no-cooldown \"$@\"\n", realBin)), 0o755); err != nil {
		t.Fatal(err)
	}
	entered, release, log := filepath.Join(dir, "entered"), filepath.Join(dir, "release"), filepath.Join(dir, "turns")
	script := `#!/bin/sh
set -eu
prompt="$*"
canvas=$(printf '%s' "$prompt" | awk '/Your canvas for this document is the single file:/ {getline; gsub(/^ +| +$/, ""); print; exit}')
test -n "$canvas"
printf '%s\n' "$canvas" >> "$MOE_SCALING_TURNS"
case "$canvas" in
 */documents/pulse/content.md)
  touch "$MOE_SCALING_ENTERED"
  i=0
  while [ ! -e "$MOE_SCALING_RELEASE" ] && [ "$i" -lt 1000 ]; do sleep 0.01; i=$((i+1)); done
  printf '# Pulse\n\n## Gate\n\nFENCEjson\n{"status":"ok","next":{"run":"widget/sibling","why":"ready sibling"}}\nFENCE\n' > "$canvas"
  ;;
 */runs/sibling/documents/design/content.md)
  printf '# Design\n\n## Gate\n\nFENCEjson\n{"status":"ready"}\nFENCE\n\n## Decision\n\nVerified sibling turn.\n' > "$canvas"
  ;;
 *) exit 9 ;;
esac
printf '{"type":"result","subtype":"success","session_id":"12345678-1234-4234-8234-123456789abc"}\n'
`
	script = strings.ReplaceAll(script, "FENCE", "```")
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("MOE_HOME", root)
	t.Setenv("MOE_FORCE_AGENT", "")
	t.Setenv("MOE_SCALING_TURNS", log)
	t.Setenv("MOE_SCALING_ENTERED", entered)
	t.Setenv("MOE_SCALING_RELEASE", release)
	gate := &fakeHeartbeat{due: []string{"widget"}}
	var output syncBuf
	s := newTestServer(t, Options{Root: root, MoeBin: wrapper, Heartbeat: gate, Dynamic: true, Logger: &output})
	t.Cleanup(func() {
		_ = os.WriteFile(release, nil, 0o644)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.children.shutdown(ctx, io.Discard)
	})
	s.heartbeatTick()
	waitFor(t, "real survey", func() bool { _, err := os.Stat(entered); return err == nil })
	if _, err := servepause.Set(root, true, nil, time.Now); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		s.heartbeatTick()
	}
	if gate.dispatchingCount() != 1 {
		t.Fatal("running survey replaced")
	}
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	c, ok := s.children.get("heartbeat:_cabinet")
	if !ok {
		t.Fatal("missing child")
	}
	select {
	case <-c.settled:
	case <-time.After(30 * time.Second):
		t.Fatalf("child timeout: %s", output.String())
	}
	if c.exitErr != nil {
		t.Fatalf("child failed: %v %s", c.exitErr, output.String())
	}
	turns, err := os.ReadFile(log)
	if err != nil || !strings.Contains(string(turns), "/runs/sibling/documents/design/content.md") || strings.Contains(string(turns), "/runs/human/") {
		t.Fatalf("turns: %s %v\n%s", turns, err, output.String())
	}
	if body, err := os.ReadFile(draft); err != nil || string(body) != "human bytes" {
		t.Fatalf("human draft changed: %s %v", body, err)
	}
	after, ok := session.ReadClaim(root, "widget", "human", "design")
	if !ok || after.Owner != claim.Owner || !after.StartedAt.Equal(claim.StartedAt) || after.Branch != claim.Branch || gittest.Output(t, root, "rev-parse", human.Branch) != humanTip {
		t.Fatal("human session changed")
	}
	if busy, err := runowner.Busy(root, "widget", "human"); err != nil || !busy {
		t.Fatalf("human owner lost: %v %v", busy, err)
	}
	s.heartbeatTick()
	if gate.dispatchingCount() != 1 || len(gate.cleanList()) != 1 || !gate.cleanList()[0] {
		t.Fatalf("pause/completion: %s", output.String())
	}
	t.Log("heartbeat → real pulse CLI → fake survey → sibling design; human branch, claim, owner and draft preserved; next sweep paused")
}
