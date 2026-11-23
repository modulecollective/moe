package cli

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"regexp"
	"time"

	"github.com/modulecollective/moe/internal/run"
)

type cloneResult struct {
	Project  string                `json:"project"`
	Run      string                `json:"run"`
	Path     string                `json:"path"`
	Identity any                   `json:"identity"`
	Terminal *run.TerminalEvidence `json:"terminal"`
	Outcome  string                `json:"outcome"`
	Reason   string                `json:"reason"`
	Detail   string                `json:"detail,omitempty"`
	Receipt  string                `json:"receipt"`
}

var cloneRefPart = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

func splitCloneRef(value string) (string, string, error) {
	p, r, err := splitProjectRun(value)
	if err != nil {
		return "", "", err
	}
	if !cloneRefPart.MatchString(p) || !cloneRefPart.MatchString(r) {
		return "", "", fmt.Errorf("expected exact project/run names, got %q", value)
	}
	return p, r, nil
}

type cloneReport struct {
	Version    int           `json:"version"`
	Root       string        `json:"root"`
	Invocation string        `json:"invocation_id"`
	Apply      bool          `json:"apply"`
	Candidates []cloneResult `json:"candidates"`
	Error      string        `json:"error,omitempty"`
}

func runCloneGC(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("clone gc", flag.ContinueOnError)
	fs.SetOutput(stderr)
	apply := fs.Bool("apply", false, "apply eligible terminal cleanup")
	asJSON := fs.Bool("json", false, "emit versioned JSON; hook output goes to stderr")
	fs.Usage = func() { moePrintln(stderr, "usage: moe clone gc [--apply] [--json] [<project>/<run>]") }
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	filter := ""
	if fs.NArg() == 1 {
		p, r, err := splitCloneRef(fs.Arg(0))
		if err != nil {
			moePrintf(stderr, "%v\n", err)
			return 2
		}
		filter = p + "/" + r
	}
	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	report := cloneReport{Version: 1, Root: root, Invocation: hex.EncodeToString(nonce[:]), Apply: *apply, Candidates: []cloneResult{}}
	keys, err := cloneInventory(root)
	if err != nil {
		moePrintf(stderr, "clone gc inventory: %v\n", err)
		if *asJSON {
			report.Error = "inventory_failed"
			if err := json.NewEncoder(stdout).Encode(report); err != nil {
				moePrintf(stderr, "clone gc output: %v\n", err)
			}
		}
		return 1
	}
	code := 0
	for _, key := range keys {
		if filter != "" && filter != key {
			continue
		}
		p, r, _ := splitProjectRun(key)
		hookOut := stdout
		if *asJSON {
			hookOut = stderr
		}
		result := collectClone(root, p, r, *apply, filter != "", time.Now().UTC(), hookOut, stderr)
		report.Candidates = append(report.Candidates, result)
		if result.Outcome == "failed" {
			code = 1
		}
	}
	if *asJSON {
		if err := json.NewEncoder(stdout).Encode(report); err != nil {
			moePrintf(stderr, "clone gc output: %v\n", err)
			return 1
		}
	} else {
		for _, r := range report.Candidates {
			moePrintf(stdout, "%s/%s: %s (%s)", r.Project, r.Run, r.Outcome, r.Reason)
			if r.Detail != "" {
				moePrintf(stdout, " — %s", r.Detail)
			}
			if r.Receipt != "" {
				moePrintf(stdout, "; evidence: %s", r.Receipt)
			}
			moePrintln(stdout, "")
		}
		if len(report.Candidates) == 0 {
			moePrintln(stdout, "clone gc: no candidates")
		}
	}
	return code
}
