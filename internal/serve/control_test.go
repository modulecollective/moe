package serve

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

type controlCall struct {
	project, run, action, id string
	revision                 int
}

// controlServer wires the three control callbacks to stubs that record
// what serve relays; serve's own job is rendering and relaying.
func controlServer(t *testing.T, view RunControlView, submitErr error) (*Server, *[]controlCall, *int) {
	t.Helper()
	root := seedInputRoot(t)
	var calls []controlCall
	reads := 0
	newControlID = func() string { return "web-fixed" }
	t.Cleanup(func() { newControlID = func() string { return "web-test" } })
	s := newTestServer(t, Options{
		Addr: "127.0.0.1:0", Root: root,
		RunControl: func(project, run string) (RunControlView, bool, error) {
			reads++
			return view, true, nil
		},
		RunStatuses: func(project string) (map[string]string, error) {
			return map[string]string{"alpha/fix-it": "held · waiting for operator"}, nil
		},
		SubmitControl: func(project, run, action, id string, revision int) (string, error) {
			calls = append(calls, controlCall{project, run, action, id, revision})
			return "ok", submitErr
		},
	})
	return s, &calls, &reads
}

// The run page carries the status block: the observed lines, the exact
// conversation command, and one no-JS form per valid action carrying
// the render's request id and hold revision.
func TestRunPageRendersStatusAndControls(t *testing.T) {
	view := RunControlView{
		Summary: "handed off", Lines: []string{"owner: none", "hold: handoff c-1 — held, ready to resume"},
		Revision: 3, Actions: []string{"resume"}, Conversation: "moe sdlc code alpha/fix-it --agent codex",
		Requests: []string{"Nov 12 08:00 handoff c-1 — completed"},
	}
	s, _, _ := controlServer(t, view, nil)
	body := get(t, s, "/run/alpha/fix-it")
	mustContain(t, body,
		`id="run-status"`, `data-refresh="/run/alpha/fix-it/status"`,
		"hold: handoff c-1", "moe sdlc code alpha/fix-it --agent codex",
		`action="/run/alpha/fix-it/control"`, `name="action" value="resume"`,
		`name="id" value="web-fixed"`, `name="revision" value="3"`,
		"control requests",
	)
	if strings.Contains(body.Body.String(), `value="pause"`) {
		t.Fatal("an action the view did not offer was rendered")
	}
	// The fragment route serves the block alone, for the refresher.
	frag := get(t, s, "/run/alpha/fix-it/status")
	mustContain(t, frag, `id="run-status"`, "hold: handoff c-1")
	if strings.Contains(frag.Body.String(), "<html") {
		t.Fatal("status fragment rendered a whole page")
	}
}

// A control POST relays exactly what the form carried and redirects to
// a fresh readback; a refusal is a 409 and nothing else happens.
func TestRunControlPostRelaysAndRedirects(t *testing.T) {
	s, calls, _ := controlServer(t, RunControlView{Actions: []string{"pause"}}, nil)
	rr := postInput(t, s, "/run/alpha/fix-it/control", "", url.Values{"action": {"pause"}, "id": {"web-1"}, "revision": {"2"}})
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/run/alpha/fix-it#run-status" {
		t.Fatalf("POST = %d %q", rr.Code, rr.Header().Get("Location"))
	}
	if want := (controlCall{"alpha", "fix-it", "pause", "web-1", 2}); len(*calls) != 1 || (*calls)[0] != want {
		t.Fatalf("calls = %+v, want %+v", *calls, want)
	}
	if rr := postInput(t, s, "/run/alpha/fix-it/control", "", url.Values{"action": {"pause"}}); rr.Code != http.StatusBadRequest {
		t.Fatalf("missing id = %d", rr.Code)
	}

	s, _, _ = controlServer(t, RunControlView{}, fmt.Errorf("%w: hold has changed since it was shown", ErrControlRefused))
	rr = postInput(t, s, "/run/alpha/fix-it/control", "", url.Values{"action": {"resume"}, "id": {"web-2"}, "revision": {"1"}})
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "hold has changed") {
		t.Fatalf("stale POST = %d %q", rr.Code, rr.Body.String())
	}
}

// Read routes write nothing: the status fragment and the dash's compact
// statuses only read, and the dash renders the overlay slot per row.
func TestStatusReadsAreReadOnly(t *testing.T) {
	s, calls, reads := controlServer(t, RunControlView{Summary: "agent running code 3m"}, nil)
	get(t, s, "/run/alpha/fix-it/status")
	body := get(t, s, "/run-status").Body.String()
	var m map[string]string
	if err := json.Unmarshal([]byte(body), &m); err != nil || m["alpha/fix-it"] != "held · waiting for operator" {
		t.Fatalf("run-status = %q, %v", body, err)
	}
	if len(*calls) != 0 || *reads != 1 {
		t.Fatalf("reads=%d submits=%d; reads must not write", *reads, len(*calls))
	}
}
