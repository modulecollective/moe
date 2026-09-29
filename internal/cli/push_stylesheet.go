package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/stylesheet"
)

// A run's stages propose a model stylesheet (proposedStylesheetPath);
// push applies it in the commit where the run ships. A model switch is
// config that pairs with code — a pricing row, a version bump — so it
// goes live with that code, not when the stage that wrote it closes.

// checkProposedStylesheet refuses a push whose proposed stylesheet
// wouldn't load. It runs before any origin work, so a typo stops the
// ship instead of landing in the root file after the merge, where every
// later turn on the box would refuse on it. Model names go unchecked —
// there is no registry; a bad one surfaces as the backend's own error in
// the first turn's transcript.
func checkProposedStylesheet(root string, md *run.Metadata) error {
	rel := proposedStylesheetPath(md)
	data, err := os.ReadFile(filepath.Join(root, rel))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("push: read proposed stylesheet: %w", err)
	}
	s, err := stylesheet.Parse(data)
	if err == nil {
		err = s.Validate(stylesheetVocab())
	}
	if err != nil {
		return fmt.Errorf("push: proposed stylesheet %s: %w\n"+
			"       fix it in a stage turn (`moe %s code %s/%s`) and re-run push",
			rel, err, md.Workflow, md.Project, md.ID)
	}
	return nil
}

// applyProposedStylesheet copies the run's proposed stylesheet over the
// root one and returns the root path for the ship record to stage, or
// "" when the run proposed nothing. The run copy stays put as the record
// of what was proposed.
func applyProposedStylesheet(root string, md *run.Metadata) (string, error) {
	data, err := os.ReadFile(filepath.Join(root, proposedStylesheetPath(md)))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read proposed stylesheet: %w", err)
	}
	if err := os.WriteFile(filepath.Join(root, stylesheet.FileName), data, 0o644); err != nil {
		return "", fmt.Errorf("apply proposed stylesheet: %w", err)
	}
	return stylesheet.FileName, nil
}

// stylesheetCloseCleanup wraps a close cleanup so a `ship: none` close
// applies and stages the proposed stylesheet inside the close's lock
// window — after the dirty-tree gate, which would otherwise refuse the
// applied file. The returned restore puts the root file back when the
// close fails: left applied, it would both wedge every later close on
// the repo-wide dirty gate and steer every turn on the box.
func stylesheetCloseCleanup(inner closeCleanup) (closeCleanup, func(root string)) {
	var prior []byte
	var hadPrior, applied bool
	cleanup := func(root string, md *run.Metadata) error {
		if inner != nil {
			if err := inner(root, md); err != nil {
				return err
			}
		}
		data, err := os.ReadFile(filepath.Join(root, stylesheet.FileName))
		switch {
		case err == nil:
			prior, hadPrior = data, true
		case !errors.Is(err, os.ErrNotExist):
			return fmt.Errorf("read root stylesheet: %w", err)
		}
		rel, err := applyProposedStylesheet(root, md)
		if err != nil || rel == "" {
			return err
		}
		applied = true
		return run.Stage(root, rel)
	}
	restore := func(root string) {
		if !applied {
			return
		}
		_ = git.Run(root, "reset", "-q", "--", stylesheet.FileName)
		path := filepath.Join(root, stylesheet.FileName)
		if hadPrior {
			_ = os.WriteFile(path, prior, 0o644)
		} else {
			_ = os.Remove(path)
		}
	}
	return cleanup, restore
}
