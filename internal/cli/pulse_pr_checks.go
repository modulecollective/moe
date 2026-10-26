package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/modulecollective/moe/internal/run"
)

// ownedPRChecks is evidence from the recorded PR, never inferred from a
// branch name or a historical workflow run. Error means unavailable.
type ownedPRChecks struct {
	Run    string
	PR     string
	State  string
	Head   string
	Checks []prCheck
	Error  string
}

type prCheck struct {
	Name    string
	Verdict string
	URL     string
}

func (c prCheck) failed() bool {
	switch c.Verdict {
	case "FAILURE", "ERROR", "TIMED_OUT", "ACTION_REQUIRED":
		return true
	}
	return false
}

func (p ownedPRChecks) actionable() bool {
	if p.Error != "" || p.State != "OPEN" || p.Head == "" {
		return false
	}
	for _, c := range p.Checks {
		if c.failed() {
			return true
		}
	}
	return false
}

const prCheckFields = "state,url,headRefOid,statusCheckRollup"

func gatherPRChecks(key, url string) ownedPRChecks {
	p := ownedPRChecks{Run: key, PR: url}
	raw, err := runGH("pr", "view", url, "--json", prCheckFields)
	if err != nil {
		p.Error = err.Error()
		return p
	}
	var v struct {
		State             string
		URL               string
		HeadRefOid        string
		StatusCheckRollup []struct {
			Type         string `json:"__typename"`
			Name         string
			WorkflowName string
			Context      string
			Status       string
			Conclusion   string
			State        string
			DetailsURL   string
			TargetURL    string
		}
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		p.Error = "parse gh pr view: " + err.Error()
		return p
	}
	if v.URL != url || v.HeadRefOid == "" || (v.State != "OPEN" && v.State != "CLOSED" && v.State != "MERGED") {
		p.Error = "gh pr view returned incomplete or mismatched PR identity"
		return p
	}
	p.State, p.Head = v.State, v.HeadRefOid
	for _, row := range v.StatusCheckRollup {
		var c prCheck
		switch row.Type {
		case "CheckRun":
			if row.Name == "" {
				p.Error = "gh pr view returned an unnamed CheckRun"
				return p
			}
			c.Name, c.URL = row.Name, row.DetailsURL
			if row.WorkflowName != "" {
				c.Name = row.WorkflowName + " / " + c.Name
			}
			c.Verdict = row.Status
			if row.Status == "COMPLETED" {
				c.Verdict = row.Conclusion
			}
		case "StatusContext":
			if row.Context == "" {
				p.Error = "gh pr view returned an unnamed StatusContext"
				return p
			}
			c.Name, c.Verdict, c.URL = row.Context, row.State, row.TargetURL
		default:
			// New rollup variants are unknown evidence, never failures.
			c = prCheck{Name: row.Name, Verdict: "UNKNOWN"}
		}
		if c.Verdict == "" {
			c.Verdict = "UNKNOWN"
		}
		p.Checks = append(p.Checks, c)
	}
	return p
}

func gatherOwnedPRChecks(sc *pulseScan, projectID string, stderr io.Writer) []ownedPRChecks {
	var out []ownedPRChecks
	for _, md := range sc.mds {
		if md.Project != projectID || md.Workflow != sdlcWorkflow || md.Status != run.StatusPushed {
			continue
		}
		key := md.Project + "/" + md.ID
		url := sc.idx.PRURL[key]
		if url == "" {
			continue
		}
		p := gatherPRChecks(key, url)
		if p.Error != "" {
			moePrintf(stderr, "pulse: PR checks: %s: %s\n", key, p.Error)
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Run < out[j].Run })
	return out
}

// Only actionable verdicts enter the wake-up fingerprint. Sorting removes
// API ordering and timestamps; healthy and pending changes spend no turn.
func prFailureSnapshot(prs []ownedPRChecks) string {
	var rows []string
	for _, p := range prs {
		if !p.actionable() {
			continue
		}
		for _, c := range p.Checks {
			if !c.failed() {
				continue
			}
			row, _ := json.Marshal([]string{p.Run, p.PR, p.Head, c.Name, c.URL, c.Verdict})
			rows = append(rows, string(row))
		}
	}
	sort.Strings(rows)
	return strings.Join(rows, "\n")
}

func renderOwnedPRChecks(sc *pulseScan, prs []ownedPRChecks) string {
	if len(prs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Checks on PRs owned by pushed runs (current PR rollup):\n")
	for _, p := range prs {
		fmt.Fprintf(&b, "- %s — %s, %s, head %s\n", p.Run, p.PR, p.State, p.Head)
		if p.Error != "" {
			fmt.Fprintf(&b, "  unavailable: %s\n", p.Error)
			continue
		}
		if len(p.Checks) == 0 {
			b.WriteString("  no checks reported (not a green verdict)\n")
		}
		for _, c := range p.Checks {
			fmt.Fprintf(&b, "  %s: %s — %s\n", c.Name, c.Verdict, c.URL)
		}
		for _, a := range sc.idx.CIRepairAttempts[p.Run] {
			fmt.Fprintf(&b, "  prior CI repair claim: PR %s, head %s, input IDs %v\n", a.PR, a.Head, a.InputIDs)
		}
		if p.actionable() {
			if _, hold := ciRepairEligibility(sc, p); hold != "" {
				fmt.Fprintf(&b, "  repair hold: %s\n", hold)
			}
		}
	}
	return b.String()
}

// The heartbeat hands its exact observation to its child. The child uses
// it for the survey as well as nomination resolution, so exit acknowledges
// only evidence actually offered, even if GitHub changes during a ride.
var heartbeatPRChecks []ownedPRChecks
