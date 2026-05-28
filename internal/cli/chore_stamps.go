package cli

import (
	"path/filepath"
	"strings"

	"github.com/modulecollective/moe/internal/chore"
	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/project"
)

func touchedChoresForBranch(root, projectID, clonePath, baseBranch, branch string) []string {
	paths := changedPathsBetween(clonePath, "origin/"+baseBranch+"..."+branch)
	return touchedChores(root, projectID, paths)
}

func touchedChoresForCommit(root, projectID, sha string) []string {
	if sha == "" {
		return nil
	}
	dir := filepath.Join(root, project.SubmoduleDir(projectID))
	_ = git.Run(dir, "fetch", "--quiet", "origin", sha)
	paths := changedPathsBetween(dir, sha+"^1", sha)
	return touchedChores(root, projectID, paths)
}

func changedPathsBetween(dir string, revs ...string) []string {
	args := append([]string{"diff", "--name-only"}, revs...)
	out, err := git.Output(dir, args...)
	if err != nil {
		return nil
	}
	var paths []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			paths = append(paths, line)
		}
	}
	return paths
}

func touchedChores(root, projectID string, paths []string) []string {
	defs, err := chore.LoadAll(root)
	if err != nil {
		return nil
	}
	return chore.MatchChangedPaths(defs, projectID, paths)
}
