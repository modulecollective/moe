package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/modulecollective/moe/internal/atomicfile"
	"github.com/modulecollective/moe/internal/sandbox"
)

const hookFireCompletion = "completed.json"

type hookFireRecord struct {
	Version int    `json:"version"`
	Project string `json:"project"`
	Wrapper string `json:"wrapper"`
}

// Open each namespace component separately: OpenRoot alone would follow a
// symlink at its initial path. Keep the final handle through cleanup.
func openHookFireParent(root, projectID string, create bool) (*os.Root, error) {
	dir, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	for _, name := range []string{".moe", "hook-fire", projectID} {
		info, err := dir.Lstat(name)
		if errors.Is(err, os.ErrNotExist) && create {
			err = dir.Mkdir(name, 0o755)
			if err == nil || errors.Is(err, os.ErrExist) {
				info, err = dir.Lstat(name)
			}
		}
		if err != nil {
			dir.Close()
			if errors.Is(err, os.ErrNotExist) && !create {
				return nil, nil
			}
			return nil, fmt.Errorf("hook fire: scratch parent %s: %w", name, err)
		}
		if !info.IsDir() {
			dir.Close()
			return nil, fmt.Errorf("hook fire: scratch parent %s is not an ordinary directory", name)
		}
		next, err := dir.OpenRoot(name)
		dir.Close()
		if err != nil {
			return nil, fmt.Errorf("hook fire: open scratch parent %s: %w", name, err)
		}
		dir = next
	}
	return dir, nil
}

func mintHookFireSandbox(root, projectID string) (sandboxPath, runID string, err error) {
	parent, err := openHookFireParent(root, projectID, true)
	if err != nil {
		return "", "", err
	}
	defer parent.Close()
	wrapper, err := os.MkdirTemp(parent.Name(), hookFirePrefix)
	if err != nil {
		return "", "", fmt.Errorf("hook fire: allocate scratch: %w", err)
	}
	runID = filepath.Base(wrapper)
	sandboxPath = filepath.Join(wrapper, "sandbox")
	_, err = sandbox.EnsureAt(root, projectID, sandboxPath)
	return sandboxPath, runID, err
}

func completeHookFireSandbox(sandboxPath, projectID, runID string) error {
	body, _ := json.Marshal(hookFireRecord{Version: 1, Project: projectID, Wrapper: runID})
	return atomicfile.Write(filepath.Join(filepath.Dir(sandboxPath), hookFireCompletion), body, 0o600)
}

func cleanPriorHookFireSandboxes(root, projectID string, stderr io.Writer) error {
	parent, err := openHookFireParent(root, projectID, false)
	if err != nil || parent == nil {
		return err
	}
	defer parent.Close()
	entries, err := fs.ReadDir(parent.FS(), ".")
	if err != nil {
		return fmt.Errorf("hook fire: read scratch parent %s: %w", parent.Name(), err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, hookFirePrefix) {
			continue
		}
		err := validateHookFireScratch(parent, projectID, name)
		if err == nil {
			err = parent.RemoveAll(name)
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			moePrintf(stderr, "hook fire: retaining %s: %v\n", filepath.Join(parent.Name(), name), err)
		}
	}
	return nil
}

func validateHookFireScratch(parent *os.Root, projectID, name string) error {
	for _, part := range []string{name, filepath.Join(name, "sandbox"), filepath.Join(name, hookFireCompletion)} {
		info, err := parent.Lstat(part)
		if err != nil {
			// A missing entry is a concurrent cleaner; a missing child is retained
			// scratch that never finished, and deserves a diagnostic.
			if part != name && errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("missing %s", filepath.Base(part))
			}
			return err
		}
		if part == filepath.Join(name, hookFireCompletion) {
			if !info.Mode().IsRegular() {
				return fmt.Errorf("%s is not a regular file", hookFireCompletion)
			}
		} else if !info.IsDir() {
			return fmt.Errorf("%s is not an ordinary directory", part)
		}
	}
	body, err := parent.ReadFile(filepath.Join(name, hookFireCompletion))
	if err != nil {
		return err
	}
	record, err := decodeHookFireRecord(body)
	if err != nil {
		return fmt.Errorf("invalid %s: %w", hookFireCompletion, err)
	}
	if record.Version != 1 || record.Project != projectID || record.Wrapper != name {
		return fmt.Errorf("completion version or identity mismatch")
	}
	entries, err := fs.ReadDir(parent.FS(), name)
	if err != nil {
		return err
	}
	if len(entries) != 2 {
		return fmt.Errorf("unexpected wrapper contents")
	}
	return nil
}

// The record is a closed schema, including duplicate keys and trailing JSON.
func decodeHookFireRecord(body []byte) (hookFireRecord, error) {
	var record hookFireRecord
	dec := json.NewDecoder(bytes.NewReader(body))
	token, err := dec.Token()
	if err != nil {
		return record, err
	}
	if token != json.Delim('{') {
		return record, fmt.Errorf("expected object")
	}
	seen := make(map[string]bool)
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return record, err
		}
		field := key.(string)
		if seen[field] {
			return record, fmt.Errorf("duplicate field %q", field)
		}
		seen[field] = true
		switch field {
		case "version":
			err = dec.Decode(&record.Version)
		case "project":
			err = dec.Decode(&record.Project)
		case "wrapper":
			err = dec.Decode(&record.Wrapper)
		default:
			return record, fmt.Errorf("unknown field %q", field)
		}
		if err != nil {
			return record, err
		}
	}
	if _, err := dec.Token(); err != nil {
		return record, err
	}
	if len(seen) != 3 {
		return record, fmt.Errorf("missing fields")
	}
	if _, err := dec.Token(); err != io.EOF {
		return record, fmt.Errorf("trailing data")
	}
	return record, nil
}
