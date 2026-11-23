package run

import (
	"fmt"
	"strings"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/trailers"
)

// FollowupDestination resolves an extraction receipt from reachable history.
// The original idea remains the receipt's destination after promotion or close.
// Caller holds the repository lock while resolving and recording extraction.
func FollowupDestination(root, key string) (project, id string, err error) {
	parts := strings.Split(key, "/")
	if len(parts) != 4 {
		return "", "", fmt.Errorf("run: invalid followup key %q: expected four slug components", key)
	}
	for _, part := range parts {
		if !idPattern.MatchString(part) {
			return "", "", fmt.Errorf("run: invalid followup key %q", key)
		}
	}
	out, err := git.Output(root, "log", "--format=%B%x00", "--grep="+trailers.GrepPattern("MoE-Followup-Key", key))
	if err != nil {
		return "", "", fmt.Errorf("run: read followup receipts: %w", err)
	}
	for _, record := range strings.Split(out, "\x00") {
		if strings.TrimSpace(record) == "" {
			continue
		}
		var p, r string
		for _, line := range strings.Split(record, "\n") {
			if v, ok := strings.CutPrefix(line, "MoE-Project: "); ok {
				p = v
			}
			if v, ok := strings.CutPrefix(line, "MoE-Run: "); ok {
				r = v
			}
		}
		if !idPattern.MatchString(p) || !idPattern.MatchString(r) || p != parts[2] {
			return "", "", fmt.Errorf("run: invalid destination %q for followup receipt %q", p+"/"+r, key)
		}
		if id != "" && (project != p || id != r) {
			return "", "", fmt.Errorf("run: inconsistent destinations %s/%s and %s/%s for followup receipt %q", project, id, p, r, key)
		}
		project, id = p, r
	}
	return project, id, nil
}
