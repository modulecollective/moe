package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	moe "github.com/modulecollective/moe"
	"github.com/modulecollective/moe/internal/executor"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
)

// shelveTimeout bounds the headless shelve invocation. The model's job
// is tiny — it reads the article + index from the prompt and returns
// a two-field JSON object — but Sonnet still needs room to breathe
// past cold-start and first-token latency. Too tight produces
// spurious retries for the operator; too loose masks hung calls.
const shelveTimeout = 5 * time.Minute

// shelveDecision is the JSON shape the model is told to return: a
// category to file the article under and a one-line hook for the
// index bullet. Every other piece of work (copying the file, editing
// the index, removing the old file on a category change) is
// deterministic Go code — see runShelve and applyIndexPatch.
type shelveDecision struct {
	Category string `json:"category"`
	Hook     string `json:"hook"`
}

// runShelve is the kb-shelve stage. It files the summarized article
// onto the project's knowledge shelf via a hybrid flow: a headless
// `claude -p` call that returns {category, hook} as JSON, followed by
// a deterministic Go copy + index patch + optional old-file rm. No
// review step — the commit (or absence of one) is the done state.
//
// Precondition: the run's summarize stage must have been written
// (i.e. a `MoE-Document: summarize` work-turn commit exists on HEAD).
// We key off the trailer rather than file contents because the trailer
// is what every other moe flow (dash, Next, chaining) already treats
// as the summarize-is-done signal.
func runShelve(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("kb shelve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe workflow kb shelve <project> <run>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Files the summarized article into projects/<project>/knowledge/<category>/<topic>.md")
		moePrintln(stderr, "and patches knowledge/index.md. The model picks category + hook; Go does the filing.")
		moePrintln(stderr, "No review step — the commit (or absence of one) is the done state.")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 2 {
		fs.Usage()
		return 2
	}
	projectID, runID := fs.Arg(0), fs.Arg(1)

	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}
	if err := requireProject(root, projectID); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if err := requireCleanTree(root); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	md, err := run.Load(root, projectID, runID)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if md.Workflow != "kb" {
		moePrintf(stderr, "run %s/%s is a %s run, not kb\n", projectID, runID, md.Workflow)
		return 1
	}

	// Precondition: summarize must have been written for this run.
	// LatestWorkTurnSHA returns ("", zero, nil) when no matching commit
	// exists — that's the "summary never happened" case we refuse on.
	sha, _, err := run.LatestWorkTurnSHA(root, md.Project, md.ID, "summarize")
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if sha == "" {
		moePrintf(stderr, "shelve: summarize has not been written for %s/%s yet; run `moe workflow kb summarize %s %s` first\n",
			projectID, runID, projectID, runID)
		return 1
	}

	summaryPath := filepath.Join(root, run.ContentPath(md.Project, md.ID, "summarize"))
	summary, err := os.ReadFile(summaryPath)
	if err != nil {
		moePrintf(stderr, "shelve: read summary: %v\n", err)
		return 1
	}
	if len(bytes.TrimSpace(summary)) == 0 {
		moePrintf(stderr, "shelve: summary is empty; run `moe workflow kb summarize %s %s` and write an article first\n", projectID, runID)
		return 1
	}

	knowledgeDir := filepath.Join(root, "projects", md.Project, "knowledge")
	indexPath := filepath.Join(knowledgeDir, "index.md")
	// A fresh project's shelf may not exist yet; treat ENOENT as an
	// empty index. Anything else propagates.
	indexBytes, err := os.ReadFile(indexPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		moePrintf(stderr, "shelve: read index: %v\n", err)
		return 1
	}
	indexBody := string(indexBytes)

	decision, err := askShelveDecision(md, summary, indexBody, stderr)
	if err != nil {
		moePrintf(stderr, "shelve: %v\n", err)
		return 1
	}
	if decision.Category = strings.TrimSpace(decision.Category); decision.Category == "" {
		moePrintf(stderr, "shelve: model returned empty category\n")
		return 1
	}
	if decision.Hook = strings.TrimSpace(decision.Hook); decision.Hook == "" {
		moePrintf(stderr, "shelve: model returned empty hook\n")
		return 1
	}
	categorySlug := slugifyCategory(decision.Category)
	if categorySlug == "" {
		moePrintf(stderr, "shelve: model returned unslug-able category %q\n", decision.Category)
		return 1
	}

	newRel := filepath.Join(categorySlug, md.ID+".md")
	newAbs := filepath.Join(knowledgeDir, newRel)

	oldRel := findExistingBulletPath(indexBody, md.ID)

	moePrintf(stdout, "category: %s\n", decision.Category)
	moePrintf(stdout, "hook:     %s\n", decision.Hook)

	if err := os.MkdirAll(filepath.Dir(newAbs), 0o755); err != nil {
		moePrintf(stderr, "shelve: mkdir: %v\n", err)
		return 1
	}
	if err := os.WriteFile(newAbs, summary, 0o644); err != nil {
		moePrintf(stderr, "shelve: write article: %v\n", err)
		return 1
	}

	// On a category change, remove the old file. Defensive: only
	// remove paths under the project's knowledge dir and only when the
	// old path isn't the same file we just wrote. Missing old files
	// are tolerated — someone may have manually cleaned up.
	if oldRel != "" && oldRel != newRel {
		oldAbs := filepath.Join(knowledgeDir, oldRel)
		if rel, err := filepath.Rel(knowledgeDir, oldAbs); err == nil && !strings.HasPrefix(rel, "..") && rel != "." {
			if err := os.Remove(oldAbs); err != nil && !errors.Is(err, os.ErrNotExist) {
				moePrintf(stderr, "shelve: remove old %s: %v\n", oldAbs, err)
				return 1
			}
		}
	}

	newBullet := fmt.Sprintf("- [%s](%s) — %s", md.Title, newRel, decision.Hook)
	patched := applyIndexPatch(indexBody, md.ID, decision.Category, newBullet)
	if err := os.WriteFile(indexPath, []byte(patched), 0o644); err != nil {
		moePrintf(stderr, "shelve: write index: %v\n", err)
		return 1
	}

	commitErr := withRepoLock(root, repolock.Options{
		Purpose: "kb-shelve",
		Run:     projectID + "/" + runID,
	}, func() error {
		return commitShelve(root, md)
	})
	switch {
	case errors.Is(commitErr, run.ErrNothingToCommit):
		moePrintln(stdout, "shelve: no changes; shelf is up to date")
	case commitErr != nil:
		moePrintf(stderr, "shelve: commit: %v\n", commitErr)
		return 1
	default:
		moePrintf(stdout, "shelved %s/%s at %s\n", projectID, runID, newRel)
	}
	return 0
}

// askShelveDecision runs the headless claude -p call and parses the
// model's JSON response. The article and index are inlined into the
// prompt so the model doesn't need Read access — we still pass
// --allowed-tools Read as a defence-in-depth belt (a sharp-edged
// denial would also be fine, but Read is harmless since there's
// nothing interesting in this directory).
func askShelveDecision(md *run.Metadata, summary []byte, indexBody string, stderr io.Writer) (shelveDecision, error) {
	var sys bytes.Buffer
	if soul := moe.Soul(); soul != "" {
		sys.WriteString(soul)
		sys.WriteString("\n---\n\n")
	}
	if frag := moe.Stage("kb", "shelve"); frag != "" {
		sys.WriteString(frag)
	}

	var user bytes.Buffer
	fmt.Fprintf(&user, "Run slug (topic): %s\nRun title: %s\nProject: %s\n\n", md.ID, md.Title, md.Project)
	user.WriteString("Current index.md (may be empty):\n\n")
	user.WriteString("----- BEGIN index.md -----\n")
	user.WriteString(indexBody)
	if !strings.HasSuffix(indexBody, "\n") {
		user.WriteByte('\n')
	}
	user.WriteString("----- END index.md -----\n\n")
	user.WriteString("Article to shelve:\n\n")
	user.WriteString("----- BEGIN article -----\n")
	user.Write(summary)
	if !bytes.HasSuffix(summary, []byte("\n")) {
		user.WriteByte('\n')
	}
	user.WriteString("----- END article -----\n\n")
	user.WriteString(`Respond with exactly one JSON object and nothing else:
{"category": "<category>", "hook": "<hook>"}
`)

	out, err := executor.ExecuteHeadless(executor.HeadlessRequest{
		Model:        "sonnet",
		AllowedTools: "Read",
		SystemPrompt: sys.String(),
		UserPrompt:   user.String(),
		Timeout:      shelveTimeout,
		Stderr:       stderr,
	})
	if err != nil {
		return shelveDecision{}, err
	}
	return parseShelveDecision(out)
}

// parseShelveDecision pulls the first complete JSON object out of the
// model's stdout. The stage fragment tells the model to emit nothing
// but the JSON, but a lenient parse (trim whitespace, use Decoder's
// token-at-a-time read) lets an incidental trailing newline or stray
// whitespace slide without a retry loop.
func parseShelveDecision(stdout []byte) (shelveDecision, error) {
	stdout = bytes.TrimSpace(stdout)
	if len(stdout) == 0 {
		return shelveDecision{}, fmt.Errorf("model produced no output")
	}
	// Tolerate the model wrapping the object in a ```json``` fence.
	if idx := bytes.Index(stdout, []byte("{")); idx > 0 {
		stdout = stdout[idx:]
	}
	var d shelveDecision
	dec := json.NewDecoder(bytes.NewReader(stdout))
	if err := dec.Decode(&d); err != nil {
		return shelveDecision{}, fmt.Errorf("parse model JSON: %w (output was %q)", err, stdout)
	}
	return d, nil
}

// slugifyCategory lowercases the model's category name and collapses
// runs of non-alphanumeric characters to single dashes, so the on-
// disk directory stays filesystem-friendly regardless of what the
// model picked. "CI/CD" → "ci-cd"; "Operating Systems" → "operating-
// systems". The display name in the index keeps the original
// capitalisation — only the path is slugified.
func slugifyCategory(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	prevDash := true
	for _, r := range strings.ToLower(name) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.TrimRight(b.String(), "-")
}

// commitShelve stages the project's knowledge/ tree and commits with
// the shelve-trailer the ladder expects. An unchanged shelf (summary
// byte-identical to what's already filed, same category, same hook)
// produces no diff and returns ErrNothingToCommit — that's the
// "shelf is up to date" path.
func commitShelve(root string, md *run.Metadata) error {
	knowledgeRel := filepath.Join("projects", md.Project, "knowledge")
	if err := run.Stage(root, knowledgeRel); err != nil {
		return err
	}
	if !run.HasStagedChanges(root) {
		return run.ErrNothingToCommit
	}
	msg := fmt.Sprintf(`work: update shelve

MoE-Run: %s
MoE-Project: %s
MoE-Workflow: %s
MoE-Document: shelve
`, md.ID, md.Project, md.Workflow)
	return run.StageAndCommit(root, msg, knowledgeRel)
}
