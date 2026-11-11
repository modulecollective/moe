package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/modulecollective/moe/internal/cliout"
	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/sandbox"
	"github.com/modulecollective/moe/internal/workspace"
)

type inspectionKind int

const (
	inspectCanvas inspectionKind = iota
	inspectDiff
)

type inspectionArtifact struct {
	kind        inspectionKind
	title, body string
}

func canvasArtifact(md *run.Metadata, stage, body string) inspectionArtifact {
	return inspectionArtifact{inspectCanvas, md.Project + "/" + md.ID + " " + stage,
		fmt.Sprintf("# %s/%s — %s\n\n%s", md.Project, md.ID, stage, body)}
}

// Deliberately avoid resolveRunWorkspacePath: its sandbox.Ensure can create
// a clone and rewrite excludes. Inspection may only read an existing tree.
//
// A failure names the path tried and the real stat cause, so a missing
// clone and an unreadable one stop reading alike.
func inspectionWorkspace(root string, md *run.Metadata) (string, error) {
	path := sandbox.Path(root, md.Project, md.ID)
	if md.Workspace != "" {
		path = workspace.Path(root, md.Project, md.Workspace)
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("source workspace: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("source workspace %s: not a directory", path)
	}
	return path, nil
}

func sourceDiffArtifact(root string, md *run.Metadata, out io.Writer) (inspectionArtifact, error) {
	a := inspectionArtifact{kind: inspectDiff, title: md.Project + "/" + md.ID + " source diff"}
	repo, err := inspectionWorkspace(root, md)
	if err != nil {
		return a, err
	}
	branch, err := defaultBranchForProject(root, md.Project)
	if err != nil {
		return a, err
	}
	resolve := func(ref string) (string, error) {
		oid, err := git.Output(repo, "rev-parse", "--verify", ref+"^{commit}")
		return strings.TrimSpace(oid), err
	}
	baseTip, err := resolve("refs/heads/" + branch)
	if err != nil {
		return a, err
	}
	tip, err := resolve("refs/heads/moe/" + md.ID)
	if err != nil {
		return a, err
	}
	base, err := git.Output(repo, "merge-base", baseTip, tip)
	if err != nil {
		return a, err
	}
	base = strings.TrimSpace(base)
	diff := func(format string) (string, error) {
		return git.Output(repo, "diff", "--no-ext-diff", "--no-textconv", "--no-color", format, base, tip, "--")
	}
	stat, err := diff("--stat")
	if err != nil {
		return a, err
	}
	a.body, err = diff("--patch")
	if err != nil {
		return a, err
	}
	fmt.Fprintf(out, "committed source changes against local refs\nrepository: %s\ndefault branch: %s\nmerge-base: %s\nrun tip: %s\n%s", repo, branch, base, tip, stat)
	// --no-optional-locks prevents status from refreshing the index on disk.
	status, err := git.Output(repo, "--no-optional-locks", "status", "--porcelain=v1", "--untracked-files=all")
	switch {
	case err != nil:
		fmt.Fprintf(out, "working tree state unknown: %v\n", err)
	case status != "":
		fmt.Fprintln(out, "uncommitted files excluded from source diff")
	}
	if a.body == "" {
		fmt.Fprintln(out, "no committed source changes")
	}
	return a, nil
}

type inspectionViewer struct {
	variable, command, less string
}

func selectInspectionViewer(kind inspectionKind) (inspectionViewer, bool) {
	variable := "MOE_MARKDOWN_PAGER"
	if kind == inspectDiff {
		variable = "MOE_DIFF_VIEWER"
	}
	v := inspectionViewer{variable: variable, command: strings.TrimSpace(os.Getenv(variable))}
	if v.command != "" {
		info, err := os.Stat("/bin/sh")
		return v, err == nil && info.Mode()&0111 != 0
	}
	var err error
	v.less, err = exec.LookPath("less")
	if err == nil {
		v.less, err = filepath.Abs(v.less)
	}
	return v, err == nil
}

func (v inspectionViewer) argv(file string) []string {
	args := []string{"/usr/bin/env", "PATH=" + os.Getenv("PATH"), "LESSSECURE=1", "LESSOPEN=", "LESSCLOSE=", "LESS="}
	if v.command != "" {
		return append(args, "/bin/sh", "-c", "exec "+v.command+` "$1"`, "moe-inspect", file)
	}
	return append(args, v.less, file)
}

var inspectionTerminal = func(w io.Writer) bool {
	return stdinIsTerminal() && cliout.RexStatusEnabled(w)
}

var launchInspection = presentRexArtifact

type promptInspection struct {
	root     string
	md       *run.Metadata
	canvas   inspectionArtifact
	location rexInspectionLocation
	viewers  map[inspectionKind]inspectionViewer
}

func newPromptInspection(root string, md *run.Metadata, stage, body string, offerDiff bool, out io.Writer) *promptInspection {
	if md.Workflow != "sdlc" || !inspectionTerminal(out) {
		return nil
	}
	bin, err := exec.LookPath("rex")
	if err != nil {
		return nil
	}
	bin, err = filepath.Abs(bin)
	if err != nil {
		return nil
	}
	p := &promptInspection{root: root, md: md, canvas: canvasArtifact(md, stage, body),
		location: rexInspectionLocation{bin: bin, session: os.Getenv("REX_SESSION"), origin: os.Getenv("REX_BLOCK")},
		viewers:  make(map[inspectionKind]inspectionViewer)}
	if strings.TrimSpace(body) != "" {
		if v, ok := selectInspectionViewer(inspectCanvas); ok {
			p.viewers[inspectCanvas] = v
		}
	}
	if _, err := inspectionWorkspace(root, md); offerDiff && err == nil {
		if v, ok := selectInspectionViewer(inspectDiff); ok {
			p.viewers[inspectDiff] = v
		}
	}
	return p
}

func (p *promptInspection) options(opts []promptOption) []promptOption {
	if p != nil {
		if _, ok := p.viewers[inspectCanvas]; ok {
			opts = append(opts, promptOption{'v', "view canvas"})
		}
		if _, ok := p.viewers[inspectDiff]; ok {
			opts = append(opts, promptOption{'f', "source diff"})
		}
	}
	return opts
}

func (p *promptInspection) inspect(answer string, sig <-chan os.Signal, out, errOut io.Writer) (handled, interrupted bool) {
	if p == nil || (answer != "v" && answer != "f") {
		return false, false
	}
	kind := inspectCanvas
	if answer == "f" {
		kind = inspectDiff
	}
	v, ok := p.viewers[kind]
	if !ok {
		return false, false
	}
	a := p.canvas
	if kind == inspectDiff {
		var err error
		a, err = sourceDiffArtifact(p.root, p.md, out)
		if err != nil {
			fmt.Fprintf(errOut, "inspection: %v\n", err)
			return true, false
		}
		if a.body == "" {
			return true, false
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() {
		select {
		case <-sig:
			cancel()
			done <- true
		case <-ctx.Done():
			done <- false
		}
	}()
	err := launchInspection(ctx, p.location, a, v)
	cancel()
	interrupted = <-done
	if err != nil {
		fmt.Fprintf(errOut, "inspection: %v\n", err)
		if _, unknown := errors.AsType[*inspectionLifetimeUnknown](err); unknown {
			clear(p.viewers)
		}
	}
	return true, interrupted
}

// errStaleDecision is readInspectionDecision's answer when the run left
// in_progress while its prompt waited. The run guard keeps cooperating
// invocations out, but not an older binary or another clone; acting on
// the answer then would re-enter a finished run.
var errStaleDecision = errors.New("stale decision")

// staleDecision reloads the run and reports whether an answer to its
// prompt can no longer be acted on. A reload failure counts as stale:
// launching against a run we can no longer read is the unsafe direction.
// A run with no run.json at all is left to the chosen verb, whose own load
// refuses it — moe never deletes a run to finish it, so absence is not
// the terminal case this check exists for.
func staleDecision(root string, md *run.Metadata, out io.Writer) bool {
	fresh, err := run.Load(root, md.Project, md.ID)
	if errors.Is(err, run.ErrRunNotFound) {
		return false
	}
	if err != nil {
		moePrintf(out, "%s/%s: cannot reload run state (%v) — nothing launched\n", md.Project, md.ID, err)
		return true
	}
	if fresh.Status != run.StatusInProgress {
		moePrintf(out, "%s/%s is now %s — nothing launched\n", md.Project, md.ID, fresh.Status)
		return true
	}
	return false
}

// Only completed stdin reads can enter inspection. An interrupted read exits
// immediately, because its goroutine may still own the shared reader. Every
// completed answer — inspection included — first re-checks the run, and a
// run that has left in_progress ends the prompt with errStaleDecision.
func readInspectionDecision(p *promptInspection, root string, md *run.Metadata, opts []promptOption, question, extra string, sig <-chan os.Signal, out, errOut io.Writer) (string, bool, error) {
	for {
		select {
		case <-sig:
			return "", true, nil
		default:
		}
		available := p.options(opts)
		moePrintf(out, "%s %s\n", question, renderPromptLabel(available))
		moePrintln(out, renderPromptLegend(available))
		if extra != "" {
			moePrintln(out, extra)
		}
		line, interrupted, err := readLineWithSignal(stdinSharedReader(), sig)
		if interrupted || (err != nil && err != io.EOF) || (err == io.EOF && line == "") {
			return line, interrupted, err
		}
		if staleDecision(root, md, out) {
			return "", false, errStaleDecision
		}
		handled, interrupted := p.inspect(strings.ToLower(strings.TrimSpace(line)), sig, out, errOut)
		if interrupted {
			return "", true, nil
		}
		if !handled {
			return line, false, err
		}
	}
}
