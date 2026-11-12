package cli

// Revealing a run's existing Rex terminal from moe ui. Optional: with no
// rex binary, no server, no app client or no capability, the reveal says
// so and everything else in the console works. It never creates a
// terminal, moves a block or sends keystrokes — focus the exact block,
// then ask one app client to show that session's window.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/modulecollective/moe/internal/runsock"
	"github.com/modulecollective/moe/internal/ui"
)

// rexTimeout bounds each rex call. A timed-out client.perform may still
// be carried out by the app later, which the result says.
var rexTimeout = 5 * time.Second

// rexCall runs `rex api call METHOD ARGS` and returns its JSON result.
// The seam tests replace; production execs the installed binary with
// argv only, never a shell, and never autostarts a server.
var rexCall = func(ctx context.Context, method string, args any) ([]byte, error) {
	b, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, rexTimeout+time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "rex", "--autostart=false", "--timeout", rexTimeout.String(), "--color", "never",
		"api", "call", method, string(b))
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("rex %s: %s", method, msg)
		}
		return nil, fmt.Errorf("rex %s: %w", method, err)
	}
	return out, nil
}

type rexClient struct {
	ID    string
	Label string
}

// revealTerminal reveals the Rex block the run's live owner reports. It
// re-reads the owner first: the terminal must belong to the same owner
// generation the console showed, and only the owner's full IDs are used.
func revealTerminal(ctx context.Context, root string, a ui.Action) ui.Result {
	id := runsock.Identity{Root: root, Project: a.Project, Run: a.Run}
	pctx, cancel := context.WithTimeout(ctx, controlDialTimeout)
	st, err := runsock.ReadStatus(pctx, runsock.RuntimeDir(), id)
	cancel()
	switch {
	case err != nil:
		return ui.Result{Message: "terminal: no live owner answered (" + err.Error() + ")"}
	case a.Generation == "" || st.Generation != a.Generation:
		return ui.Result{Message: "terminal: the run's owner changed since it was shown; look again"}
	case st.Terminal == nil:
		return ui.Result{Message: "terminal: the owner reports no Rex terminal"}
	}
	if _, err := exec.LookPath("rex"); err != nil {
		return ui.Result{Message: "terminal: rex is not installed here"}
	}
	session, block := st.Terminal.Session, st.Terminal.Block

	window, err := rexBlockWindow(ctx, session, block)
	if err != nil {
		return ui.Result{Message: "terminal: " + err.Error()}
	}
	client, choices, err := rexPickClient(ctx, a.Client)
	if err != nil {
		return ui.Result{Message: "terminal: " + err.Error()}
	}
	if client == "" {
		var cs []ui.Choice
		for _, c := range choices {
			cs = append(cs, ui.Choice{ID: c.ID, Label: c.Label})
		}
		return ui.Result{Message: "terminal: several Rex apps are connected; pick one", Choices: cs}
	}
	if _, err := rexCall(ctx, "session.focus_block", map[string]string{"session_id": session, "block_id": block}); err != nil {
		return ui.Result{Message: "terminal: focus failed: " + err.Error()}
	}
	_, err = rexCall(ctx, "client.perform", map[string]any{
		"client_id":  client,
		"action":     "session.select",
		"args":       map[string]string{"session_id": session, "window_id": window},
		"origin":     "api",
		"timeout_ms": rexTimeout.Milliseconds(),
	})
	label := fmt.Sprintf("%s/%s's terminal (block %s)", a.Project, a.Run, block)
	if err != nil {
		if strings.Contains(err.Error(), "timeout") {
			return ui.Result{Uncertain: true, Message: "terminal: the app did not answer in time; it may still switch to " + label}
		}
		return ui.Result{Message: "terminal: the block is focused, but the app did not switch: " + err.Error()}
	}
	lines := []string{"focused " + label + " and asked the app to show it"}
	if st.Origin.Sweep != "" {
		lines = append(lines, "this terminal hosts sweep "+st.Origin.Sweep+"; it may show other runs it walked")
	}
	return ui.Result{OK: true, Message: "revealed " + label, Lines: lines}
}

// rexBlockWindow finds the window holding block. A detached block has
// none: v1 reports it rather than placing it anywhere.
func rexBlockWindow(ctx context.Context, session, block string) (string, error) {
	out, err := rexCall(ctx, "session.view", map[string]string{"session_id": session})
	if err != nil {
		return "", err
	}
	var view struct {
		Windows []struct {
			WindowID string `json:"window_id"`
			Layers   []struct {
				Blocks []struct {
					BlockID string `json:"block_id"`
				} `json:"blocks"`
			} `json:"layers"`
		} `json:"windows"`
	}
	if err := json.Unmarshal(out, &view); err != nil {
		return "", fmt.Errorf("unreadable session view: %w", err)
	}
	for _, w := range view.Windows {
		for _, l := range w.Layers {
			for _, b := range l.Blocks {
				if b.BlockID == block {
					return w.WindowID, nil
				}
			}
		}
	}
	return "", errors.New("the block is detached or gone; there is no window to show (moe ui does not place blocks)")
}

// rexPickClient chooses the app client to ask: the one named, else the
// invoking REX_CLIENT when it is a connected app, else the only
// connected app. Several eligible apps return choices instead of a
// guess. Only clients that offer session.select are eligible.
func rexPickClient(ctx context.Context, chosen string) (string, []rexClient, error) {
	out, err := rexCall(ctx, "client.list", map[string]string{})
	if err != nil {
		return "", nil, err
	}
	var list struct {
		Clients []struct {
			ClientID string `json:"client_id"`
			Info     struct {
				Kind     string `json:"kind"`
				Name     string `json:"name"`
				Label    string `json:"label"`
				Platform string `json:"platform"`
			} `json:"info"`
			State string `json:"connection_state"`
		} `json:"clients"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return "", nil, fmt.Errorf("unreadable client list: %w", err)
	}
	var apps []rexClient
	for _, c := range list.Clients {
		if c.Info.Kind != "app" || c.State != "connected" {
			continue
		}
		if !rexClientCanSelect(ctx, c.ClientID) {
			continue
		}
		label := strings.TrimSpace(c.Info.Name + " " + c.Info.Label)
		if c.Info.Platform != "" {
			label += " (" + c.Info.Platform + ")"
		}
		apps = append(apps, rexClient{ID: c.ClientID, Label: label})
	}
	has := func(id string) bool {
		for _, c := range apps {
			if c.ID == id {
				return true
			}
		}
		return false
	}
	switch {
	case chosen != "":
		if has(chosen) {
			return chosen, nil, nil
		}
		return "", nil, errors.New("that app is no longer connected")
	case has(os.Getenv("REX_CLIENT")):
		return os.Getenv("REX_CLIENT"), nil, nil
	case len(apps) == 1:
		return apps[0].ID, nil, nil
	case len(apps) == 0:
		return "", nil, errors.New("no connected Rex app can show a session")
	}
	return "", apps, nil
}

func rexClientCanSelect(ctx context.Context, id string) bool {
	out, err := rexCall(ctx, "client.inspect", map[string]string{"client_id": id})
	if err != nil {
		return false
	}
	var c struct {
		Actions []struct {
			Name string `json:"name"`
		} `json:"actions"`
	}
	if json.Unmarshal(out, &c) != nil {
		return false
	}
	for _, a := range c.Actions {
		if a.Name == "session.select" {
			return true
		}
	}
	return false
}
