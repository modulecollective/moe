// Package servepause owns the bureaucracy-wide pause on new automatic sweeps.
package servepause

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

	"github.com/modulecollective/moe/internal/atomicfile"
	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/repolock"
)

const FileName = "serve-pause.json"
const Lifetime = 24 * time.Hour

var ErrStale = errors.New("serve pause changed; refresh and try again")
var ErrRollback = errors.New("serve pause rollback failed")

type State struct {
	Revision  int       `json:"revision"`
	ExpiresAt time.Time `json:"expires_at,omitzero"`
}

func (s State) Active(now time.Time) bool {
	return now.Before(s.ExpiresAt)
}

// Read treats only a missing file as an unconfigured pause.
func Read(root string) (State, error) {
	b, err := os.ReadFile(filepath.Join(root, FileName))
	if errors.Is(err, os.ErrNotExist) {
		return State{}, nil
	}
	if err != nil {
		return State{}, fmt.Errorf("serve pause: %w", err)
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
	if err == nil && s.Revision < 1 {
		err = errors.New("invalid revision")
	}
	if err == nil {
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(b, &fields)
		for name, value := range fields {
			if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				err = fmt.Errorf("%s must not be null", name)
			}
		}
		if !s.ExpiresAt.IsZero() {
			_, offset := s.ExpiresAt.Zone()
			if offset != 0 {
				err = errors.New("expiry must be UTC")
			}
		} else if _, present := fields["expires_at"]; present {
			err = errors.New("expiry must be a nonzero timestamp")
		}
	}
	if err != nil {
		return State{}, fmt.Errorf("serve pause %s: %w", FileName, err)
	}
	return s, nil
}

// Summary names the admission brake without implying current work stopped.
func (s State) Summary(now time.Time) string {
	if s.Active(now) {
		return "new automatic sweeps paused until " + s.ExpiresAt.Local().Format("2006-01-02 15:04 MST")
	}
	return "new automatic sweeps unpaused"
}

// Check serializes admission with the writer, including a rollback. The lock
// ends before any host or process work; success admits this sweep and its ride.
func Check(root string, now func() time.Time) error {
	return repolock.With(root, repolock.Options{Purpose: "automatic sweep admission"}, func() error {
		s, err := Read(root)
		if err != nil {
			return err
		}
		at := now()
		if s.Active(at) {
			return errors.New(s.Summary(at))
		}
		return nil
	})
}

// Set locks and commits exactly this setting. expected is nil for a CLI
// invocation, or the revision displayed by a web/UI control. now is evaluated
// under the lock so lock contention cannot consume the new window.
func Set(root string, desired bool, expected *int, now func() time.Time) (State, error) {
	var result State
	err := repolock.With(root, repolock.Options{Purpose: "serve-pause"}, func() error {
		entries, err := git.Status(root)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if !e.Untracked() {
				return fmt.Errorf("serve pause: tracked tree is dirty: %s", e.Path)
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
		if (!desired && s.ExpiresAt.IsZero()) || (desired && s.Active(at)) {
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
		next := State{Revision: s.Revision + 1}
		if desired {
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
			label := "unpause"
			if desired {
				label = "pause for 24 hours"
			}
			err = git.Run(root, "commit", "-m", "Set automatic sweep pause: "+label, "--", FileName)
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
			return fmt.Errorf("serve pause: nothing recorded: %w", err)
		}
		result = next
		return nil
	})
	return result, err
}
