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

	"github.com/modulecollective/moe/internal/agentdefault"
	"github.com/modulecollective/moe/internal/git/gittest"
)

// One resident server, real heartbeat children, real CLI stage orchestration,
// fake vendor binaries. The wrapper bypasses quota so this regression spends
// no subscription reads or turns; selection and vendor argv remain production.
func TestAgentAndStylesheetReachHeartbeatWithoutRestart(t *testing.T) {
	root := gittest.InitSeeded(t)
	gittest.WriteAndCommit(t, root, "bureaucracy.conf", "", "mark bureaucracy")
	binDir := t.TempDir()
	realBin := filepath.Join(binDir, "real-moe")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	build := exec.CommandContext(ctx, "go", "build", "-o", realBin, "../../cmd/moe")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	wrapper := filepath.Join(binDir, "moe")
	if err := os.WriteFile(wrapper, []byte(fmt.Sprintf("#!/bin/sh\nexec %q --no-cooldown \"$@\"\n", realBin)), 0o755); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(binDir, "dispatch.log")
	script := `#!/bin/sh
set -eu
prompt="$*"
model=
next=0
for arg in "$@"; do
 if [ "$next" = 1 ]; then model="$arg"; next=0; fi
 if [ "$arg" = --model ]; then next=1; fi
done
printf '%s %s\n' "${0##*/}" "$model" >> "$MOE_AGENT_TEST_LOG"
canvas=$(printf '%s' "$prompt" | awk '/Your canvas for this document is the single file:/ {getline; gsub(/^ +| +$/, ""); print; exit}')
test -n "$canvas"
printf '# Pulse\n\n## Gate\n\n\x60\x60\x60json\n{"status":"ok"}\n\x60\x60\x60\n' > "$canvas"
case "$0" in
 */codex) printf '{"type":"thread.started","thread_id":"12345678-1234-4234-8234-123456789abc"}\n' ;;
 *) printf '{"type":"result","subtype":"success","session_id":"12345678-1234-4234-8234-123456789abc"}\n' ;;
esac
`
	// POSIX printf does not require support for \x escapes.
	script = strings.ReplaceAll(script, `\x60`, "`")
	for _, name := range []string{"claude", "codex"} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("MOE_HOME", root)
	t.Setenv("MOE_FORCE_AGENT", "")
	t.Setenv("MOE_AGENT_TEST_LOG", log)
	gittest.WriteAndCommit(t, root, "model-stylesheet.css", "pulse.pulse {agent:claude;model-claude:c1;model-codex:x1;}", "initial policy")
	gate := &fakeHeartbeat{due: []string{"_cabinet"}}
	var output syncBuf
	s := newTestServer(t, Options{Root: root, MoeBin: wrapper, Heartbeat: gate, Dynamic: true, Logger: &output})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.children.shutdown(ctx, io.Discard)
	})
	var previous *child
	tick := func(want string) {
		t.Helper()
		s.heartbeatTick()
		c, ok := s.children.get("heartbeat:_cabinet")
		if !ok || c == previous {
			t.Fatalf("heartbeat did not spawn: %s", output.String())
		}
		previous = c
		select {
		case <-c.settled:
		case <-time.After(30 * time.Second):
			t.Fatalf("child did not settle: %s", output.String())
		}
		if c.exitErr != nil {
			t.Fatalf("child: %v\n%s", c.exitErr, output.String())
		}
		b, err := os.ReadFile(log)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSpace(string(b)), "\n")
		if lines[len(lines)-1] != want {
			t.Fatalf("vendor argv %q want %q", lines, want)
		}
		t.Logf("heartbeat vendor argv: %s", want)
	}
	tick("claude c1")
	gittest.WriteAndCommit(t, root, "model-stylesheet.css", "pulse.pulse {agent:claude;model-claude:c2;model-codex:x2;}", "change policy without restart")
	tick("claude c2")
	if _, err := agentdefault.Set(root, "codex", nil, time.Now); err != nil {
		t.Fatal(err)
	}
	tick("codex x2")
	if _, err := agentdefault.Set(root, "", nil, time.Now); err != nil {
		t.Fatal(err)
	}
	tick("claude c2")
}
