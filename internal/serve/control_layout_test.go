package serve

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"os/exec"
	"strings"
	"testing"
)

func TestRunControlLayout(t *testing.T) {
	s, _, _ := controlServer(t, RunControlView{}, nil)
	for _, provenance := range []bool{false, true} {
		for _, ordinary := range []bool{false, true} {
			vm := runVM{
				RowNote: "concise note", CanvasLinks: []canvasLink{{Stage: "code", Body: template.HTML("<p>the work</p>")}},
				RunStatus: runStatusVM{StatusURL: "/status", ControlURL: "/control", ControlID: "request-id", Control: &RunControlView{
					Summary: "detailed summary", Lines: []string{"owner: generation"}, Revision: 7,
					Actions: []string{"pause", "handoff", "park", "finish", "resume"}, Requests: []string{"receipt"},
				}},
				Traces: runTracesVM{Followups: []runTraceVM{{Title: "trace"}}},
			}
			if provenance {
				vm.Provenance = []ProvHop{{Verb: "opened by operator"}}
			}
			if ordinary {
				vm.Actions = []runAction{{Label: "Close Run", Href: "/close", Method: "POST"}}
			}
			var out bytes.Buffer
			if err := s.tmpl.ExecuteTemplate(&out, "run_fragment.html", vm); err != nil {
				t.Fatal(err)
			}
			body := out.String()
			order := []string{"concise note", `<section class="actions">`, `id="run-controls"`, `<section class="canvases">`}
			if provenance {
				order = append(order, `<section class="provenance">`)
			}
			order = append(order, `<section class="traces">`, `id="run-status"`, "detailed summary", "owner: generation", "receipt")
			last := -1
			for _, marker := range order {
				pos := strings.Index(body, marker)
				if pos <= last {
					t.Fatalf("provenance=%v ordinary=%v: %q out of order", provenance, ordinary, marker)
				}
				last = pos
			}
			actions := body[strings.Index(body, `<section class="actions">`):strings.Index(body, `<section class="canvases">`)]
			if strings.Count(actions, `class="action-form"`) != 5+len(vm.Actions) || strings.Count(actions, `class="action"`) != 5+len(vm.Actions) || strings.Count(actions, `name="revision" value="7"`) != 5 || strings.Count(actions, `name="id" value="request-id"`) != 5 {
				t.Fatal("controls lost shared action styling or hidden fields")
			}
			if strings.Contains(body[strings.Index(body, `id="run-status"`):], "<form") || strings.Contains(actions, "owner:") {
				t.Fatal("diagnostics and controls are mixed")
			}
		}
	}
}

// Execute the served script against real page/fragment bytes, with a small
// DOM substitute for the two live slots. This covers refresh races and form
// data; browser layout and native focus/scroll behavior need separate probes.
func TestRunControlRefreshScript(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is needed to exercise the served JavaScript")
	}
	view := RunControlView{Actions: []string{"pause", "handoff", "park", "finish"}, Requests: []string{"receipt"}}
	s, _, _ := controlServer(t, view, nil)
	reads := 0
	s.opts.RunControl = func(string, string) (RunControlView, bool, error) {
		reads++
		return view, true, nil
	}
	oldID := newControlID
	t.Cleanup(func() { newControlID = oldID })
	newControlID = func() string { return fmt.Sprintf("web-%d", reads) }
	fixtures := map[string]string{"page": get(t, s, "/run/alpha/fix-it").Body.String()}
	view.Actions, view.Revision = []string{"resume"}, 7
	fixtures["resume"] = get(t, s, "/run/alpha/fix-it/status").Body.String()
	view.Actions = nil
	fixtures["empty"] = get(t, s, "/run/alpha/fix-it/status").Body.String()
	view.Actions, view.Revision = []string{"pause", "finish"}, 8
	fixtures["active"] = get(t, s, "/run/alpha/fix-it/status").Body.String()
	if reads != 4 {
		t.Fatalf("each page/fragment must use one observation, got %d", reads)
	}
	input, err := json.Marshal(fixtures)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, "testdata/run_control_refresh.js")
	cmd.Stdin = bytes.NewReader(input)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("refresh script: %v\n%s", err, output)
	}
}
