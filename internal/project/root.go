package project

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/repolock"
)

// RootID is the project that stands for the bureaucracy itself. It holds
// top-level intents and ideas and the one pulse, and has no source of its
// own. Registered ids derive from repository URLs under idPattern, which
// cannot start with an underscore, so none can collide with it.
const RootID = "_cabinet"

// ValidID reports whether id can name a project: a registered id, or the
// root project.
func ValidID(id string) bool {
	return id == RootID || idPattern.MatchString(id)
}

// EnsureRoot creates the root project if it does not exist yet, in a
// journal commit of its own, so an existing bureaucracy needs no operator
// step. trailerBlock is appended to the commit message and may be empty.
// The caller must not hold the repository lock.
func EnsureRoot(root, trailerBlock string) error {
	if rootExists(root) {
		return nil
	}
	return repolock.With(root, repolock.Options{Purpose: "root-project", Run: RootID}, func() error {
		return EnsureRootLocked(root, trailerBlock)
	})
}

// EnsureRootLocked is EnsureRoot for a caller already holding the lock.
// The commit is scoped to project.json, so it neither requires a clean
// tree nor sweeps up anything the caller has staged.
func EnsureRootLocked(root, trailerBlock string) error {
	if rootExists(root) {
		return nil
	}
	rel := filepath.Join(Dir(RootID), "project.json")
	abs := filepath.Join(root, rel)
	md := &Metadata{ID: RootID, Created: time.Now().Local().Format("2006-01-02")}
	if err := writeJSON(abs, md); err != nil {
		return fmt.Errorf("project: write %s: %w", rel, err)
	}
	msg := "Create root project " + RootID
	if block := strings.TrimSpace(trailerBlock); block != "" {
		msg += "\n\n" + block
	}
	err := git.Run(root, "add", "--", rel)
	if err == nil {
		err = git.Run(root, "commit", "-m", msg, "--", rel)
	}
	if err != nil {
		resetErr := git.Run(root, "reset", "-q", "--", rel)
		removeErr := os.RemoveAll(filepath.Join(root, Dir(RootID)))
		return errors.Join(fmt.Errorf("project: commit %s: %w", rel, err), resetErr, removeErr)
	}
	return nil
}

func rootExists(root string) bool {
	_, err := os.Stat(filepath.Join(root, Dir(RootID), "project.json"))
	return err == nil
}
