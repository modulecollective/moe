package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/modulecollective/moe/internal/agent"
	"github.com/modulecollective/moe/internal/run"
)

const (
	identityA = "aaaaaaaa-1111-4aaa-8aaa-aaaaaaaaaaaa"
	identityB = "bbbbbbbb-2222-4bbb-8bbb-bbbbbbbbbbbb"
	identityC = "cccccccc-3333-4ccc-8ccc-cccccccccccc"
)

// ownedAgent models independent native stores: a resume for another backend's
// ID is a hard failure. Each mirror accumulates only its owner's history.
type ownedAgent struct {
	fakeAgent
	name, id, history string
	owned             map[string]bool
	returned          string
	execErr           error
}

func (f *ownedAgent) TranscriptExists(id, cwd string) (bool, error) { return f.owned[id], nil }
func (f *ownedAgent) TranscriptSessionID(path string) (string, error) {
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer file.Close()
	var record struct{ SessionID string }
	err = json.NewDecoder(file).Decode(&record)
	return record.SessionID, err
}
func (f *ownedAgent) finish(root, mirror, id string) (string, error) {
	f.owned[id] = true
	f.history += f.name + " turn\n"
	f.writes++
	if err := os.WriteFile(filepath.Join(root, f.canvasRel), []byte(fmt.Sprintf("# %s turn %d\n", f.name, f.writes)), 0o644); err != nil {
		return "", err
	}
	body := fmt.Sprintf("{\"sessionId\":%q,\"history\":%q}\n", id, f.history)
	if err := os.WriteFile(mirror, []byte(body), 0o644); err != nil {
		return "", err
	}
	return id, f.execErr
}
func (f *ownedAgent) Execute(r agent.Request) (string, error) {
	f.execCalls = append(f.execCalls, fakeExecCall{sessionID: r.SessionID, newSession: r.NewSession, model: r.Model})
	if !r.NewSession && !f.owned[r.SessionID] {
		return "", fmt.Errorf("foreign resume: %s", r.SessionID)
	}
	id := r.SessionID
	if r.NewSession {
		id = f.id
	}
	if f.returned != "" {
		id = f.returned
	}
	mirror := filepath.Join(r.Root, run.ThreadPathFor(f.name, r.Metadata.Project, r.Metadata.ID, r.DocID))
	return f.finish(r.Root, mirror, id)
}
func (f *ownedAgent) ExecuteOneShot(r agent.OneShotRequest) (string, error) {
	return f.finish(r.Root, r.ThreadPath, f.returned)
}

func registerOwned(t *testing.T, name, id string) *ownedAgent {
	t.Helper()
	f := &ownedAgent{name: name, id: id, owned: map[string]bool{}, fakeAgent: fakeAgent{canvasRel: run.ContentPath("tele", "fix-it", "design")}}
	agent.Register(name, f)
	t.Cleanup(func() { agent.Unregister(name) })
	return f
}

// This regression also compiles against the scalar baseline, where the
// first return visit wrongly launches fresh after probing the other backend's ID.
func TestBackendSwitchResumeIDs(t *testing.T) {
	_, _ = setupResumeFixture(t, "unused-baseline")
	a := registerOwned(t, "baseline-a", identityA)
	b := registerOwned(t, "baseline-b", identityB)
	for i, f := range []*ownedAgent{a, b, a, b} {
		driveDesignTurn(t, f.name, false)
		if i >= 2 {
			call := f.execCalls[1]
			if call.newSession || call.sessionID != f.id {
				t.Fatalf("returning backend %s: NewSession=%v ID=%s; want resume ID=%s", f.name, call.newSession, call.sessionID, f.id)
			}
		}
	}
}
