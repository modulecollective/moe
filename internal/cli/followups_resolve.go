package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/trailers"
)

// resolveFollowup is shared by turn-end, manual and terminal extraction.
// Every entry, tagged or not, has stable retry identity: a receipt from an
// earlier attempt wins, then a live owner under the same slug, and only
// then a new idea carrying the receipt. Harvest runs after every
// successful turn, so without the receipt a retry after a partial failure
// would mint a `-2` duplicate.
func resolveFollowup(root, sourceProject, sourceRun string, fu parsedFollowup, body string, tr trailers.Block) (md *run.Metadata, liveLink bool, err error) {
	key := sourceProject + "/" + sourceRun + "/" + fu.project + "/" + fu.slug
	p, id, err := run.FollowupDestination(root, key)
	if err != nil {
		return nil, false, err
	}
	if id != "" {
		md, err := run.Load(root, p, id)
		if err != nil {
			return nil, false, fmt.Errorf("followup receipt %q target %s/%s is missing or unreadable: %w", key, p, id, err)
		}
		return md, false, nil
	}
	matches, err := matchingLiveRuns(root, fu.project, fu.slug)
	if err != nil {
		return nil, false, err
	}
	for _, md := range matches {
		if md.Project == sourceProject && md.ID == sourceRun {
			return nil, false, fmt.Errorf("followup %q matches its own source %s/%s", key, sourceProject, sourceRun)
		}
	}
	if len(matches) > 1 {
		names := make([]string, len(matches))
		for i, md := range matches {
			names[i] = md.Project + "/" + md.ID
		}
		return nil, false, fmt.Errorf("followup %q has ambiguous live owners: %s", key, strings.Join(names, ", "))
	}
	if len(matches) == 1 {
		return matches[0], true, nil
	}
	tr.FollowupKey = key
	md, err = createIdea(root, fu.project, fu.slug, body, fu.promoteTo, tr)
	return md, false, err
}

// A live-owner link has no idea-open receipt. Commit it before returning;
// on failure restore the unchecked source and its original index entry so
// retry re-judges the owner. Created ideas retain their own durable receipts.
func commitFollowupAudit(root, projectID, runID, workflow string, lines []string) error {
	rel := run.FollowupsPath(projectID, runID)
	abs := filepath.Join(root, rel)
	before, err := os.ReadFile(abs)
	if err != nil {
		return err
	}
	index, err := git.Output(root, "ls-files", "-s", "--", rel)
	if err != nil {
		return err
	}
	fields := strings.Fields(index)
	if len(fields) != 0 && (len(fields) != 4 || fields[2] != "0") {
		return fmt.Errorf("unmerged index entry for %s", rel)
	}
	if err := os.WriteFile(abs, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		return err
	}
	msg := fmt.Sprintf("Link follow-up to its live owner for %s/%s\n\n", projectID, runID) +
		trailers.Block{Project: projectID, Run: runID, Workflow: workflow, Consent: walkConsent()}.String()
	err = run.Stage(root, rel)
	if err == nil {
		err = git.Run(root, "commit", "--only", "-m", msg, "--", rel)
	}
	if err != nil {
		var restoreIndex error
		if len(fields) != 0 {
			restoreIndex = git.Run(root, "update-index", "--add", "--cacheinfo", fields[0]+","+fields[1]+","+rel)
		} else {
			restoreIndex = git.Run(root, "update-index", "--force-remove", "--", rel)
		}
		return errors.Join(err, os.WriteFile(abs, before, 0o644), restoreIndex)
	}
	return nil
}
