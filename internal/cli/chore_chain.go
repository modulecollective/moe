package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/modulecollective/moe/internal/chore"
	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/trailers"
)

type choreChainMode int

const (
	choreChainOffer choreChainMode = iota
	choreChainNote
)

func maybeOfferChoreChain(root string, parentMD *run.Metadata, mode choreChainMode, stdout, stderr io.Writer) int {
	touched, err := choreTouchedByPush(root, parentMD)
	if err != nil {
		moePrintf(stderr, "chore chain: %v\n", err)
		return 0
	}
	if len(touched) == 0 {
		return 0
	}
	states, err := gatherChoreStates(root, parentMD.Project)
	if err != nil {
		moePrintf(stderr, "chore chain: %v\n", err)
		return 0
	}
	due := triggeredDueChores(states, touched)
	if len(due) == 0 {
		return 0
	}
	if mode == choreChainNote || !stdinIsTerminal() {
		for _, s := range due {
			moePrintf(stdout, "chore %s is now due -- run `moe chore open %s`\n", s.Definition.Key(), s.Definition.Key())
		}
		return 0
	}
	for _, s := range due {
		code := offerChoreChain(root, parentMD, s, stdout, stderr)
		if code != 0 {
			return code
		}
	}
	return 0
}

func triggeredDueChores(states []chore.State, touched []string) []chore.State {
	touchedSet := make(map[string]bool, len(touched))
	for _, key := range touched {
		touchedSet[key] = true
	}
	var due []chore.State
	for _, s := range states {
		if !s.Due || !touchedSet[s.Definition.Key()] {
			continue
		}
		if !choreStateHasReason(s, "changed paths") {
			continue
		}
		due = append(due, s)
	}
	sort.Slice(due, func(i, j int) bool {
		return due[i].Definition.Key() < due[j].Definition.Key()
	})
	return due
}

func choreStateHasReason(s chore.State, reason string) bool {
	for _, r := range s.Reasons {
		if r == reason {
			return true
		}
	}
	return false
}

func offerChoreChain(root string, parentMD *run.Metadata, s chore.State, stdout, stderr io.Writer) int {
	key := s.Definition.Key()
	moePrintf(stdout, "chore %s is now due from this merge -- open and chain it now? [Y/n]\n", key)
	sig, stopSig := installSigint()
	defer stopSig()
	line, interrupted, err := readLineWithSignal(stdinSharedReader(), sig)
	if interrupted {
		moePrintln(stdout, "^C")
		return 0
	}
	if err != nil && err != io.EOF {
		moePrintf(stderr, "chore chain: read stdin: %v\n", err)
		return 1
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	if answer != "" && !strings.HasPrefix(answer, "y") {
		return 0
	}
	choreMD, code := openDueChore(root, s.Definition.Project, s.Definition.Name, stdout, stderr)
	if code != 0 {
		return code
	}
	if err := spliceChoreChain(root, parentMD.Project+"/"+parentMD.ID, choreMD.Project+"/"+choreMD.ID); err != nil {
		moePrintf(stderr, "chore chain: %v\n", err)
		return 1
	}
	return promptNextStage(root, choreMD, "", stdout, stderr)
}

func spliceChoreChain(root, parentKey, choreKey string) error {
	idx, err := run.BuildJournalIndex(root)
	if err != nil {
		return fmt.Errorf("build index: %w", err)
	}
	desired := []string{parentKey, choreKey}
	if child := idx.ChainedChild[parentKey]; child != "" {
		desired = append(desired, child)
	}
	adds, removes := diffChainEdit([][]string{desired}, idx.ChainedChild)
	if len(adds) == 0 && len(removes) == 0 {
		return nil
	}
	block := trailers.Block{
		ChainedTo:        adds,
		ChainedToRemoved: removes,
	}
	msg := fmt.Sprintf("chain: insert chore %s after %s\n\n", choreKey, parentKey) + block.String()
	return repolock.With(root, repolock.Options{Purpose: "chore-chain", Run: parentKey}, func() error {
		return git.Run(root, "commit", "--allow-empty", "-m", msg)
	})
}

func choreTouchedByPush(root string, md *run.Metadata) ([]string, error) {
	out, err := git.Output(root, "log", "--fixed-strings", "--grep", "MoE-Run: "+md.ID, "--format=%B%x1e")
	if err != nil {
		return nil, err
	}
	for _, record := range strings.Split(out, "\x1e") {
		runID, projectID, docID := "", "", ""
		var touched []string
		for _, line := range strings.Split(record, "\n") {
			line = strings.TrimSpace(line)
			if v, ok := strings.CutPrefix(line, "MoE-Run:"); ok && runID == "" {
				runID = strings.TrimSpace(v)
				continue
			}
			if v, ok := strings.CutPrefix(line, "MoE-Project:"); ok && projectID == "" {
				projectID = strings.TrimSpace(v)
				continue
			}
			if v, ok := strings.CutPrefix(line, "MoE-Document:"); ok && docID == "" {
				docID = strings.TrimSpace(v)
				continue
			}
			if v, ok := strings.CutPrefix(line, "MoE-Chore-Touched:"); ok {
				if key := strings.TrimSpace(v); key != "" {
					touched = append(touched, key)
				}
			}
		}
		if runID == md.ID && projectID == md.Project && docID == "push" {
			sort.Strings(touched)
			return touched, nil
		}
	}
	return nil, nil
}
