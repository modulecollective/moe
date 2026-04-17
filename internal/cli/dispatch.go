package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/modulecollective/moe/internal/bureaucracy"
	"github.com/modulecollective/moe/internal/managed"
	"github.com/modulecollective/moe/internal/request"
	"github.com/modulecollective/moe/internal/sandbox"
)

func init() {
	Register(&Command{
		Name:    "dispatch",
		Summary: "fire off an async Managed Agents session for a document (dry-run today)",
		Run:     runDispatch,
	})
}

// runDispatch prepares the payload that would be POSTed to Anthropic's
// /v1/sessions for a `moe work`-equivalent turn and prints it. Today it
// stops there — the HTTP client and the symmetric `moe status` / `moe
// tail` commands that collect and follow a running session are the
// next iterations. Keeping dispatch dry-run-only means the payload
// shape (submodule expansion, read-only bureaucracy mount, branch
// naming) can be validated against a real bureaucracy without needing
// real API credentials wired up.
func runDispatch(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("dispatch", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe dispatch <project> <request> <document>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Emits the JSON payload for POST /v1/sessions on Anthropic's")
		moePrintln(stderr, "Managed Agents API. Does not call the API yet.")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 3 {
		fs.Usage()
		return 2
	}
	projectID, reqID, docID := fs.Arg(0), fs.Arg(1), fs.Arg(2)

	cwd, err := os.Getwd()
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	root, err := bureaucracy.Find(cwd, os.Getenv)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	md, err := request.Load(root, projectID, reqID)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	doc, ok := md.Documents[docID]
	if !ok || doc.Session == "" {
		moePrintf(stderr, "document %q not opened yet; run `moe work %s %s %s` once first\n",
			docID, projectID, reqID, docID)
		return 1
	}

	pj, err := loadProjectJSON(root, md.Project)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	// Pull the per-request sandbox clone into existence if it's not
	// already there. We need it on disk so we can parse .gitmodules and
	// read pinned SHAs for each submodule. The clone is cheap (APFS
	// clonefile on macOS, plain copy elsewhere) and identical to what
	// `moe work` would produce.
	clonePath, err := sandbox.Ensure(root, md.Project, md.ID)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	// Read the PATs from env. We deliberately do not store tokens in
	// the bureaucracy — that's a commit away from leaking on `git push`.
	// Callers plumb them in via env per invocation.
	projectToken := os.Getenv("MOE_GITHUB_TOKEN_WRITE")
	if projectToken == "" {
		projectToken = "$MOE_GITHUB_TOKEN_WRITE"
	}
	bureauToken := os.Getenv("MOE_GITHUB_TOKEN_READ")
	if bureauToken == "" {
		bureauToken = "$MOE_GITHUB_TOKEN_READ"
	}

	submodules, err := managed.ExpandSubmodules(clonePath, projectToken)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	bureauRemote, bureauSHA, err := bureaucracyPinning(root)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	agentID := envOr("MOE_MANAGED_AGENT_ID", "$MOE_MANAGED_AGENT_ID")
	environmentID := envOr("MOE_MANAGED_ENVIRONMENT_ID", "$MOE_MANAGED_ENVIRONMENT_ID")

	prompt, err := buildSystemPrompt(root, md, docID, clonePath)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	sess, err := managed.BuildSession(managed.Params{
		AgentID:          agentID,
		EnvironmentID:    environmentID,
		ProjectRepo:      pj.Remote,
		ProjectBranch:    "moe/" + md.ID,
		ProjectToken:     projectToken,
		BureaucracyRepo:  bureauRemote,
		BureaucracySHA:   bureauSHA,
		BureaucracyToken: bureauToken,
		Submodules:       submodules,
		Prompt:           prompt,
		Metadata: map[string]string{
			"moe_project":      md.Project,
			"moe_request":      md.ID,
			"moe_document":     docID,
			"moe_session_uuid": doc.Session,
		},
	})
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	body, err := sess.MarshalIndent()
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if _, err := stdout.Write(append(body, '\n')); err != nil {
		return 1
	}
	moePrintln(stderr, "")
	moePrintln(stderr, "(dry-run) The JSON above is the body for POST /v1/sessions.")
	moePrintln(stderr, "Set MOE_GITHUB_TOKEN_WRITE / _READ and MOE_MANAGED_AGENT_ID /")
	moePrintln(stderr, "MOE_MANAGED_ENVIRONMENT_ID before the real POST ships.")
	return 0
}

// projectJSON is the subset of project.Metadata we need here. Declared
// locally to avoid importing the whole project package just for its
// Remote / DefaultBranch fields.
type projectJSON struct {
	Remote        string `json:"remote"`
	DefaultBranch string `json:"default_branch"`
}

func loadProjectJSON(root, projectID string) (*projectJSON, error) {
	path := filepath.Join(root, "requests", projectID, "project.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("dispatch: read %s: %w", path, err)
	}
	p := &projectJSON{}
	if err := json.Unmarshal(b, p); err != nil {
		return nil, fmt.Errorf("dispatch: parse %s: %w", path, err)
	}
	if p.Remote == "" {
		return nil, fmt.Errorf("dispatch: %s has no remote", path)
	}
	return p, nil
}

// bureaucracyPinning returns the bureaucracy's origin URL and current
// HEAD SHA. Both are needed so the managed agent mounts the bureaucracy
// read-only and pinned to the exact state the operator fired off from.
func bureaucracyPinning(root string) (remote, sha string, err error) {
	remote, err = gitOutput(root, "remote", "get-url", "origin")
	if err != nil {
		return "", "", fmt.Errorf("dispatch: bureaucracy remote: %w", err)
	}
	sha, err = gitOutput(root, "rev-parse", "HEAD")
	if err != nil {
		return "", "", fmt.Errorf("dispatch: bureaucracy HEAD: %w", err)
	}
	return remote, sha, nil
}

func gitOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
