// Package agentdefault owns the bureaucracy-wide, expiring backend default.
// Permanent model policy remains in the model stylesheet.
package agentdefault

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/modulecollective/moe/internal/agent"
	"github.com/modulecollective/moe/internal/atomicfile"
	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/repolock"
)

const FileName = "agent-default.json"
const Lifetime = 24 * time.Hour

var ErrStale = errors.New("agent default changed; refresh and try again")
var ErrRollback = errors.New("agent default rollback failed")

type State struct {
	Revision  int       `json:"revision"`
	Agent     string    `json:"agent,omitempty"`
	ExpiresAt time.Time `json:"expires_at,omitzero"`
}

func (s State) Active(now time.Time) string {
	if now.Before(s.ExpiresAt) {
		return s.Agent
	}
	return ""
}

// Read treats only a missing file as an unconfigured default.
func Read(root string) (State, error) {
	b, err := os.ReadFile(filepath.Join(root, FileName))
	if errors.Is(err, os.ErrNotExist) {
		return State{}, nil
	}
	if err != nil {
		return State{}, fmt.Errorf("agent default: %w", err)
	}
	var s State
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	err = d.Decode(&s)
	if err == nil {
		var rest any
		if e := d.Decode(&rest); e != io.EOF {
			err = fmt.Errorf("expected one JSON record")
		}
	}
	// A JSON null must not silently masquerade as a reset record.
	if err == nil && (len(bytes.TrimSpace(b)) == 0 || bytes.TrimSpace(b)[0] != '{') {
		err = errors.New("expected an object")
	}
	if err == nil && (s.Revision < 1 || (s.Agent == "") != s.ExpiresAt.IsZero()) {
		err = errors.New("invalid revision or incomplete agent/expiry")
	}
	if err == nil && s.Agent != "" {
		_, err = agent.Get(s.Agent)
	}
	if err != nil {
		return State{}, fmt.Errorf("agent default %s: %w", FileName, err)
	}
	return s, nil
}

// Summary describes the default, not the backend of any executing run.
// A force override is known only for the process rendering this text.
func (s State) Summary(now time.Time) string {
	label := "Default: stylesheet"
	if name := s.Active(now); name != "" {
		label = "Default: " + strings.ToUpper(name[:1]) + name[1:] + " until " + s.ExpiresAt.Local().Format("2006-01-02 15:04 MST")
	}
	if force := os.Getenv("MOE_FORCE_AGENT"); force != "" {
		label += " (this process forces " + force + ")"
	}
	return label
}

// Set locks and commits exactly this setting. expected is nil for a CLI
// invocation, or the revision displayed by a web/UI control. now is evaluated
// under the lock so lock contention cannot consume the new window.
func Set(root, desired string, expected *int, now func() time.Time) (State, error) {
	var result State
	err := repolock.With(root, repolock.Options{Purpose: "agent-default"}, func() error {
		if desired != "" {
			if _, err := agent.Get(desired); err != nil {
				return err
			}
		}
		entries, err := git.Status(root)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if !e.Untracked() {
				return fmt.Errorf("agent default: tracked tree is dirty: %s", e.Path)
			}
		}
		s, err := Read(root)
		if err != nil {
			return err
		}
		result = s
		at := now().UTC()
		// Matching requests are idempotent even if they carry an older
		// revision. Reset clears expired records as well as active ones.
		if (desired == "" && s.Agent == "") || (desired != "" && s.Active(at) == desired) {
			return nil
		}
		if expected != nil && *expected != s.Revision {
			return ErrStale
		}
		path := filepath.Join(root, FileName)
		old, err := os.ReadFile(path)
		existed := err == nil
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		index, err := git.Output(root, "ls-files", "-s", "--", FileName)
		if err != nil {
			return err
		}
		next := State{Revision: s.Revision + 1, Agent: desired}
		if desired != "" {
			next.ExpiresAt = at.Add(Lifetime)
		}
		body, err := json.MarshalIndent(next, "", "  ")
		if err != nil {
			return err
		}
		err = atomicfile.Write(path, append(body, '\n'), 0o644)
		if err == nil {
			err = git.Run(root, "add", "--", FileName)
		}
		if err == nil {
			label := desired
			if label == "" {
				label = "stylesheet"
			}
			err = git.Run(root, "commit", "-m", "Set temporary default agent: "+label, "--", FileName)
		}
		if err != nil {
			var restore error
			if existed {
				restore = atomicfile.Write(path, old, 0o644)
			} else if e := os.Remove(path); !errors.Is(e, os.ErrNotExist) {
				restore = e
			}
			var stage error
			if f := strings.Fields(index); len(f) >= 2 {
				stage = git.Run(root, "update-index", "--add", "--cacheinfo", f[0]+","+f[1]+","+FileName)
			} else {
				stage = git.Run(root, "update-index", "--force-remove", "--", FileName)
			}
			if undo := errors.Join(restore, stage); undo != nil {
				return errors.Join(err, fmt.Errorf("%w: %w", ErrRollback, undo))
			}
			return fmt.Errorf("agent default: nothing recorded: %w", err)
		}
		result = next
		return nil
	})
	return result, err
}
