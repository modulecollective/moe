package run

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/modulecollective/moe/internal/git"
)

// CloneRetention is elapsed time since the latest proven terminal transition.
// It never applies to resumable runs, regardless of inactivity.
const CloneRetention = 168 * time.Hour

// TerminalEvidence identifies the committed transition, rather than an activity
// timestamp or a local observation of the run. It authorizes no mutation alone.
type TerminalEvidence struct {
	Commit   string    `json:"commit"`
	Status   string    `json:"status"`
	At       time.Time `json:"at"`
	Deadline time.Time `json:"retention_deadline"`
}

func IsTerminal(status string) bool {
	return status == StatusClosed || status == StatusMerged || status == StatusPromoted
}

func (e TerminalEvidence) RetentionExpired(now time.Time) bool {
	return IsTerminal(e.Status) && !e.At.IsZero() && !now.Before(e.Deadline) && e.Deadline.Equal(e.At.Add(CloneRetention))
}

// LatestTerminalTransition reads only canonical main history for the exact
// run.json path. The caller freshly loads md under ownership. Any mismatch,
// uncommitted status, absent boundary or ambiguous history refuses evidence.
// Later commits which merely edit an already terminal run do not reset age.
func LatestTerminalTransition(ctx context.Context, root string, md *Metadata, now time.Time) (TerminalEvidence, error) {
	var none TerminalEvidence
	if !IsTerminal(md.Status) {
		return none, errors.New("run is not terminal")
	}
	if !idPattern.MatchString(md.ID) || !idPattern.MatchString(md.Project) {
		return none, errors.New("invalid terminal run identity")
	}
	read := func(args ...string) (string, error) { return git.Inspect(ctx, root, 4<<20, args...) }
	// The path walk must not treat a shallow boundary as the beginning of a
	// run. Replacements/grafts can also manufacture a transition.
	shallow, err := read("rev-parse", "--is-shallow-repository")
	if err != nil {
		return none, err
	}
	if strings.TrimSpace(shallow) != "false" {
		return none, errors.New("shallow journal history")
	}
	replacements, err := read("for-each-ref", "--format=%(refname)", "refs/replace/")
	if err != nil {
		return none, err
	}
	if replacements != "" {
		return none, errors.New("replacement journal history")
	}
	graftPath, err := read("rev-parse", "--path-format=absolute", "--git-path", "info/grafts")
	if err != nil {
		return none, err
	}
	if _, err := os.Lstat(strings.TrimSpace(graftPath)); err == nil {
		return none, errors.New("grafted journal history")
	} else if !errors.Is(err, os.ErrNotExist) {
		return none, err
	}
	config, err := read("config", "--list")
	if err != nil {
		return none, err
	}
	for line := range strings.SplitSeq(strings.ToLower(config), "\n") {
		key, _, _ := strings.Cut(line, "=")
		if key == "extensions.partialclone" || strings.HasSuffix(key, ".promisor") {
			return none, errors.New("partial journal history")
		}
	}
	path := filepath.ToSlash(filepath.Join(Dir(md.Project, md.ID), "run.json"))
	tip, err := read("rev-parse", "--verify", "refs/heads/main^{commit}")
	if err != nil {
		return none, err
	}
	tip = strings.TrimSpace(tip)
	body, err := read("show", tip+":"+path)
	if err != nil {
		return none, err
	}
	var committed Metadata
	if err := json.Unmarshal([]byte(body), &committed); err != nil {
		return none, fmt.Errorf("committed run metadata: %w", err)
	}
	if !reflect.DeepEqual(md, &committed) {
		return none, errors.New("fresh run metadata differs from canonical journal")
	}
	history, err := read("log", "--first-parent", "--full-history", "--format=%H", tip, "--", path)
	if err != nil {
		return none, err
	}
	commits := strings.Fields(history)
	if len(commits) == 0 || len(commits) > 4096 {
		return none, errors.New("missing or excessive terminal history")
	}
	var candidate TerminalEvidence
	for _, commit := range commits {
		body, err := read("show", commit+":"+path)
		if err != nil {
			return none, err
		}
		var historical Metadata
		if err := json.Unmarshal([]byte(body), &historical); err != nil {
			return none, fmt.Errorf("historical run metadata: %w", err)
		}
		if historical.Project != md.Project || historical.ID != md.ID || historical.Workflow != md.Workflow || historical.Created != md.Created {
			return none, errors.New("historical run identity mismatch")
		}
		switch historical.Status {
		case md.Status:
			meta, err := read("show", "-s", "--format=%ct%x00%(trailers:key=MoE-Project,valueonly)%x00%(trailers:key=MoE-Run,valueonly)%x00%(trailers:key=MoE-Workflow,valueonly)", commit)
			if err != nil {
				return none, err
			}
			parts := strings.Split(meta, "\x00")
			if len(parts) != 4 || strings.TrimSpace(parts[1]) != md.Project || strings.TrimSpace(parts[2]) != md.ID || strings.TrimSpace(parts[3]) != md.Workflow {
				return none, errors.New("terminal history lacks matching journal attribution")
			}
			seconds, err := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
			if err != nil || seconds <= 0 {
				return none, errors.New("invalid terminal commit time")
			}
			at := time.Unix(seconds, 0).UTC()
			if at.After(now) {
				return none, errors.New("future terminal commit time")
			}
			candidate = TerminalEvidence{Commit: commit, Status: md.Status, At: at, Deadline: at.Add(CloneRetention)}
		case StatusInProgress, StatusPushed:
			if candidate.Commit == "" {
				return none, errors.New("terminal transition missing")
			}
			return candidate, nil
		default:
			return none, errors.New("contradictory terminal history")
		}
	}
	return none, errors.New("terminal history has no preceding nonterminal state")
}
