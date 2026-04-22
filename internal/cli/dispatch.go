package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/modulecollective/moe/internal/bureaucracy"
	"github.com/modulecollective/moe/internal/managed"
	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/sandbox"
)

func init() {
	Register(&Command{
		Name:    "dispatch",
		Summary: "fire off an async Managed Agents session for a document",
		Run:     runDispatch,
	})
}

// runDispatch kicks off an async Managed Agents session for a
// document. In the normal path it POSTs /v1/sessions and stores the
// returned session id on the document so `moe tail` and `moe status`
// can find it later. With --dry-run it prints the payload instead of
// calling the API — useful for validating submodule expansion and
// resource layout without real credentials wired up.
//
// Refuses to re-fire if a managed session is already recorded on the
// document; pass --force to overwrite. This protects against double
// spend and accidentally abandoning an in-flight run.
func runDispatch(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("dispatch", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dryRun := fs.Bool("dry-run", false, "print the /v1/sessions body instead of POSTing it")
	force := fs.Bool("force", false, "replace an existing managed session id on the document")
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe dispatch [--dry-run] [--force] <project> <run> <document>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Creates a session on Anthropic's Managed Agents API and records its")
		moePrintln(stderr, "id on the document. Use `moe tail` to watch and `moe status` to")
		moePrintln(stderr, "reconcile results when the session terminates.")
		moePrintln(stderr, "")
		moePrintln(stderr, "Required env for a real call: ANTHROPIC_API_KEY,")
		moePrintln(stderr, "MOE_MANAGED_AGENT_ID, MOE_MANAGED_ENVIRONMENT_ID,")
		moePrintln(stderr, "MOE_GITHUB_TOKEN_WRITE, MOE_GITHUB_TOKEN_READ.")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 3 {
		fs.Usage()
		return 2
	}
	projectID, runID, docID := fs.Arg(0), fs.Arg(1), fs.Arg(2)

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

	md, err := run.Load(root, projectID, runID)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	doc, ok := md.Documents[docID]
	if !ok || doc.Session == "" {
		moePrintf(stderr, "document %q not opened yet; run `moe %s %s %s %s` once first\n",
			docID, md.Workflow, docID, projectID, runID)
		return 1
	}
	if doc.Managed != "" && !*force && !*dryRun {
		moePrintf(stderr, "document %q already has managed session %s; run `moe status` to reconcile it, or --force to replace it\n",
			docID, doc.Managed)
		return 1
	}

	body, err := buildDispatchSession(root, md, doc, docID, *dryRun)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	if *dryRun {
		b, err := body.MarshalIndent()
		if err != nil {
			moePrintf(stderr, "%v\n", err)
			return 1
		}
		if _, err := stdout.Write(append(b, '\n')); err != nil {
			return 1
		}
		moePrintln(stderr, "")
		moePrintln(stderr, "(dry-run) JSON above is the body for POST /v1/sessions.")
		return 0
	}

	client, err := managed.NewClientFromEnv()
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	resp, err := client.CreateSession(context.Background(), body)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	doc.Managed = resp.ID
	if err := run.Save(root, md); err != nil {
		moePrintf(stderr, "save run.json: %v\n", err)
		return 1
	}
	// Commit so the session-id lands in the bureaucracy's history.
	// Using a distinct subject ("dispatch") keeps stage-turn grepping
	// (subject "work: update") clean.
	runJSON := filepath.Join(run.Dir(md.Project, md.ID), "run.json")
	msg := fmt.Sprintf(`dispatch: %s (managed %s)

MoE-Run: %s
MoE-Project: %s
MoE-Document: %s
MoE-Session: %s
MoE-Managed-Session: %s
`, docID, resp.ID, md.ID, md.Project, docID, doc.Session, resp.ID)
	if err := run.StageAndCommit(root, msg, runJSON); err != nil {
		moePrintf(stderr, "commit dispatch record: %v\n", err)
		return 1
	}

	moePrintf(stdout, "dispatched %s/%s/%s as managed session %s (state: %s)\n",
		md.Project, md.ID, docID, resp.ID, resp.Status)
	moePrintf(stdout, "follow: moe tail %s %s %s\n", md.Project, md.ID, docID)
	moePrintf(stdout, "collect: moe status %s %s %s\n", md.Project, md.ID, docID)
	return 0
}

// buildDispatchSession assembles the POST /v1/sessions body for this
// document. Extracted so both real and dry-run modes run through
// identical construction — the only difference is whether we POST.
//
// When dryRun is true we tolerate missing env vars by substituting
// visible placeholders ("$FOO_BAR") into token fields; that keeps the
// dry-run output legible for operators iterating on the shape without
// creds. Real mode keeps the env-var string literally, which will
// cause the server to reject the request — a loud failure is the
// right signal.
func buildDispatchSession(root string, md *run.Metadata, doc *run.Document, docID string, dryRun bool) (*managed.Session, error) {
	pj, err := project.Load(root, md.Project)
	if err != nil {
		return nil, err
	}

	// Pull the per-run sandbox clone into existence if it's not
	// already there. We need it on disk so we can parse .gitmodules
	// and read pinned SHAs for each submodule. The clone is cheap
	// (APFS clonefile on macOS, plain copy elsewhere) and identical
	// to what `moe sdlc code` would produce.
	clonePath, err := sandbox.Ensure(root, md.Project, md.ID)
	if err != nil {
		return nil, err
	}

	// Tokens are read from env, never persisted. Dry-run substitutes
	// visible placeholders so the emitted JSON is self-documenting.
	projectToken := tokenOr("MOE_GITHUB_TOKEN_WRITE", dryRun)
	bureauToken := tokenOr("MOE_GITHUB_TOKEN_READ", dryRun)

	submodules, err := managed.ExpandSubmodules(clonePath, projectToken)
	if err != nil {
		return nil, err
	}

	bureauRemote, bureauSHA, err := bureaucracyPinning(root)
	if err != nil {
		return nil, err
	}

	agentID := tokenOr("MOE_MANAGED_AGENT_ID", dryRun)
	environmentID := tokenOr("MOE_MANAGED_ENVIRONMENT_ID", dryRun)

	prompt, err := buildSystemPrompt(root, md, docID, clonePath)
	if err != nil {
		return nil, err
	}

	return managed.BuildSession(managed.Params{
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
			"moe_run":          md.ID,
			"moe_document":     docID,
			"moe_session_uuid": doc.Session,
		},
	})
}

// tokenOr returns the env var's value, or a "$NAME" placeholder in
// dry-run mode. Real mode returns "" when unset so the downstream
// validator (or the API server) can surface the specific missing var.
func tokenOr(key string, dryRun bool) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	if dryRun {
		return "$" + key
	}
	return ""
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
