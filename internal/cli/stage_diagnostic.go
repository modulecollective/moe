package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/modulecollective/moe/internal/agent"
	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/trailers"
)

// A diagnostic is evidence of a refusal, never a work turn. In particular,
// its commit must not carry MoE-Document or a work: update subject.
type stageFailure struct {
	Stage               string        `json:"stage"`
	Agent               string        `json:"agent"`
	BackendSessionID    string        `json:"backend_session_id"`
	ObservedAt          time.Time     `json:"observed_at"`
	Execution           turnExecution `json:"execution"`
	CanvasRefusal       string        `json:"canvas_refusal"`
	TranscriptAvailable bool          `json:"transcript_available"`
	TranscriptError     string        `json:"transcript_error,omitempty"`
	PromptAvailable     bool          `json:"prompt_available"`
}

type turnExecution struct {
	Outcome  string `json:"outcome"`
	ExitCode *int   `json:"exit_code"` // null is unknown, including timeout/signal.
	Error    string `json:"error,omitempty"`
	Timeout  string `json:"timeout,omitempty"`
}

func executionResult(err error) turnExecution {
	if err == nil {
		zero := 0
		return turnExecution{Outcome: "returned", ExitCode: &zero}
	}
	r := turnExecution{Outcome: "error", Error: err.Error()}
	var timeout *agent.TimeoutError
	var exit *exec.ExitError
	switch {
	case errors.As(err, &timeout):
		r.Outcome, r.Timeout = "timeout", timeout.Timeout.String()
	case errors.Is(err, agent.ErrInterrupted):
		r.Outcome = "interrupted"
	case errors.As(err, &exit) && exit.ExitCode() >= 0:
		code := exit.ExitCode()
		r.ExitCode = &code
	}
	return r
}

func diagnosticStageDir(projectID, runID, stage string) string {
	return filepath.Join(run.Dir(projectID, runID), "diagnostics", stage)
}

// saveStageFailure snapshots only the two mirrors and failure metadata onto
// canonical main. --only keeps an unrelated staged change out of this commit;
// cleanup touches only the newly allocated directory, even on hook failure.
func saveStageFailure(root string, in stageTurnInputs, spec stageTurnSpec, a agent.Agent, agentName, sid, prompt string, runErr error, refusal *canvasRefusalError) (string, error) {
	md := spec.Metadata
	id, err := run.NewSessionID()
	if err != nil {
		return "", err
	}
	rel := filepath.Join(diagnosticStageDir(md.Project, md.ID, spec.DocID), id)
	failure := stageFailure{
		Stage: spec.DocID, Agent: agentName, BackendSessionID: sid,
		ObservedAt: time.Now().UTC(), Execution: executionResult(runErr), CanvasRefusal: refusal.Reason,
	}
	// Read the returned session afresh: the ordinary document mirror can
	// still belong to a previous attempt when this executor never mirrored.
	// The prompt comes directly from the payload handed to the executor.
	files := map[string][]byte{"prompt-" + agentName + ".md": []byte(prompt)}
	failure.PromptAvailable = true
	if sid != "" {
		tmp, err := os.MkdirTemp("", "moe-diagnostic-*")
		if err != nil {
			return "", err
		}
		defer os.RemoveAll(tmp)
		thread := filepath.Join(tmp, "thread-"+agentName+".jsonl")
		found, err := a.CopyTranscript(sid, thread)
		if err != nil {
			failure.TranscriptError = err.Error()
		} else if found {
			b, err := os.ReadFile(thread)
			if err != nil {
				return "", err
			}
			files[filepath.Base(thread)] = b
			failure.TranscriptAvailable = true
		}
	}
	b, err := json.MarshalIndent(failure, "", "  ")
	if err != nil {
		return "", err
	}
	files["failure.json"] = append(b, '\n')
	err = repolock.With(root, stageLockOptions(in, "diagnostic"), func() (retErr error) {
		dir := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return err
		}
		// Exclusive creation, even in the astronomically unlikely UUID collision.
		if err := os.Mkdir(dir, 0o755); err != nil {
			return err
		}
		defer func() {
			if retErr != nil {
				retErr = errors.Join(retErr, git.Run(root, "reset", "-q", "HEAD", "--", rel), os.RemoveAll(dir))
			}
		}()
		paths := make([]string, 0, len(files))
		for name, data := range files {
			p := filepath.Join(rel, name)
			if err := os.WriteFile(filepath.Join(root, p), data, 0o644); err != nil {
				return err
			}
			paths = append(paths, p)
		}
		if err := run.Stage(root, paths...); err != nil {
			return err
		}
		block := trailers.Block{Run: md.ID, Project: md.Project, Workflow: md.Workflow, Consent: walkConsent()}
		if failure.Execution.Timeout != "" {
			block.TimedOut = failure.Execution.Timeout
		}
		msg := fmt.Sprintf("diagnostic: %s/%s %s canvas %s\n\n", md.Project, md.ID, spec.DocID, refusal.Reason) + block.String()
		args := append([]string{"commit", "--only", "-m", msg, "--"}, paths...)
		return git.Run(root, args...)
	})
	if err != nil {
		return "", err
	}
	return rel, nil
}

// latestStageFailure uses the journal's topological order, never mtimes or
// timestamps. A later normal turn supersedes failures without deleting them.
func latestStageFailure(root, projectID, runID, stage, agentArg string) (string, *stageFailure, error) {
	dir := diagnosticStageDir(projectID, runID, stage)
	log, err := git.Output(root, "log", "--topo-order", "--all-match",
		"--grep", trailers.GrepPattern("MoE-Project", projectID),
		"--grep", trailers.GrepPattern("MoE-Run", runID), "--format=%H%x00%B%x00")
	if err != nil {
		return "", nil, err
	}
	entries := strings.Split(log, "\x00")
	for i := 0; i+1 < len(entries); i += 2 {
		sha, body := strings.TrimSpace(entries[i]), entries[i+1]
		if strings.HasPrefix(body, "work: update "+stage+"\n") && strings.Contains(body, "\nMoE-Document: "+stage+"\n") {
			return "", nil, nil
		}
		if !strings.HasPrefix(body, fmt.Sprintf("diagnostic: %s/%s %s canvas ", projectID, runID, stage)) {
			continue
		}
		names, err := git.Output(root, "diff-tree", "--no-commit-id", "--name-only", "-r", "--diff-filter=A", sha, "--", dir)
		if err != nil {
			return "", nil, err
		}
		for _, name := range strings.Split(strings.TrimSpace(names), "\n") {
			if filepath.Base(name) != "failure.json" {
				continue
			}
			b, err := git.Output(root, "show", sha+":"+name)
			if err != nil {
				return "", nil, err
			}
			var failure stageFailure
			if err := json.Unmarshal([]byte(b), &failure); err != nil {
				return "", nil, fmt.Errorf("read diagnostic %s: %w", name, err)
			}
			if agentArg == "" || failure.Agent == agentArg {
				return filepath.Dir(name), &failure, nil
			}
		}
	}
	return "", nil, nil
}
