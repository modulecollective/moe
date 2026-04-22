package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/modulecollective/moe/internal/bureaucracy"
	"github.com/modulecollective/moe/internal/managed"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/sandbox"
)

func init() {
	Register(&Command{
		Name:    "status",
		Summary: "check a dispatched managed session and reconcile it when done",
		Run:     runStatus,
	})
}

// runStatus is the async-flow reconciler: one command, safe to run
// any time. If the session is still running, it just prints state.
// If the session is terminal (completed / failed / cancelled) and
// hasn't been reconciled yet, it:
//
//  1. fetches the full event stream and writes it to thread.jsonl,
//  2. fetches moe/<run-id> on the project's sandbox clone so the
//     operator can inspect the agent's pushed branch locally,
//  3. commits a `work: update <doc>` turn against the bureaucracy
//     with the standard trailer block,
//  4. clears Document.Managed so the next `moe dispatch` can run
//     without --force.
//
// The design goal: operators never have to remember "is it time to
// collect yet?" — they just run `moe status` and it does the right
// thing for the current state.
func runStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe status <project> <run> <document>")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 3 {
		fs.Usage()
		return 2
	}
	projectID, runID, docID := fs.Arg(0), fs.Arg(1), fs.Arg(2)

	cwd, err := os.Getwd()
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	root, err := bureaucracy.Find(cwd, os.Getenv)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	md, err := run.Load(root, projectID, runID)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	doc, ok := md.Documents[docID]
	if !ok || doc.Managed == "" {
		moePrintf(stderr, "document %q has no dispatched managed session; run `moe dispatch` first\n", docID)
		return 1
	}

	client, err := managed.NewClientFromEnv()
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	ctx := context.Background()
	sess, err := client.GetSession(ctx, doc.Managed)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	moePrintf(stdout, "%s/%s/%s — managed session %s: %s\n",
		md.Project, md.ID, docID, sess.ID, sess.Status)

	if !sess.Terminal() {
		moePrintln(stdout, "still running; rerun `moe status` to reconcile once it finishes.")
		moePrintf(stdout, "follow live: moe tail %s %s %s\n", md.Project, md.ID, docID)
		return 0
	}

	if err := reconcile(ctx, client, root, md, doc, docID, sess, stdout, stderr); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	return 0
}

// reconcile walks through the post-session cleanup in order and stops
// at the first step that fails. Each step is idempotent on its own
// (re-writing thread.jsonl is fine; `git fetch` is fine; the commit
// refuses ErrNothingToCommit if nothing changed) so a partial failure
// followed by a rerun makes progress instead of wedging.
func reconcile(
	ctx context.Context,
	client *managed.Client,
	root string,
	md *run.Metadata,
	doc *run.Document,
	docID string,
	sess *managed.SessionResponse,
	stdout, stderr io.Writer,
) error {
	threadPath := filepath.Join(root, run.ThreadPath(md.Project, md.ID, docID))
	if err := writeTranscript(ctx, client, doc.Managed, threadPath); err != nil {
		return fmt.Errorf("write transcript: %w", err)
	}
	moePrintf(stdout, "wrote transcript: %s\n", run.ThreadPath(md.Project, md.ID, docID))

	// The project sandbox clone is where the operator will git-diff
	// the agent's work. Fetching moe/<run-id> makes the pushed
	// branch available locally without forcing the operator to
	// remember the fetch incantation.
	if clonePath, err := sandbox.Ensure(root, md.Project, md.ID); err == nil {
		if err := fetchBranch(clonePath, md.ID); err != nil {
			moePrintf(stderr, "note: could not fetch moe/%s in sandbox clone: %v\n", md.ID, err)
		} else {
			moePrintf(stdout, "fetched branch moe/%s into %s\n", md.ID, clonePath)
		}
	}

	// Clear the pointer so the next dispatch doesn't see a stale
	// "already running" state. Save happens inside the commit step
	// so the JSON update and the trailer commit land atomically.
	doc.Managed = ""
	if err := run.Save(root, md); err != nil {
		return fmt.Errorf("save run.json: %w", err)
	}

	docDir := run.DocDir(md.Project, md.ID, docID)
	runJSON := filepath.Join(run.Dir(md.Project, md.ID), "run.json")
	msg := fmt.Sprintf(`work: update %s (managed %s)

MoE-Run: %s
MoE-Project: %s
MoE-Document: %s
MoE-Session: %s
MoE-Managed-Session: %s
MoE-Managed-Status: %s
`, docID, sess.ID, md.ID, md.Project, docID, doc.Session, sess.ID, sess.Status)
	if err := run.StageAndCommit(root, msg, docDir, runJSON); err != nil {
		return fmt.Errorf("commit turn: %w", err)
	}
	moePrintf(stdout, "committed reconciliation for %s/%s/%s\n", md.Project, md.ID, docID)
	return nil
}

// writeTranscript streams the session's events and writes them as JSONL
// to dest, creating parent dirs. Each event is one line; matches the
// shape stage sessions already write so downstream readers don't care
// which executor produced the transcript.
func writeTranscript(ctx context.Context, client *managed.Client, sessionID, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()

	events := make(chan managed.Event, 64)
	done := make(chan error, 1)
	go func() { done <- client.StreamEvents(ctx, sessionID, events) }()

	enc := json.NewEncoder(f)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return nil
			}
			if len(ev.Raw) > 0 {
				if _, err := f.Write(append([]byte(ev.Raw), '\n')); err != nil {
					return err
				}
			} else if err := enc.Encode(ev); err != nil {
				return err
			}
		case err := <-done:
			close(events)
			// Drain anything still buffered before returning.
			for ev := range events {
				if len(ev.Raw) > 0 {
					_, _ = f.Write(append([]byte(ev.Raw), '\n'))
				} else {
					_ = enc.Encode(ev)
				}
			}
			return err
		}
	}
}

// fetchBranch runs `git fetch origin moe/<id>:moe/<id>` inside the
// sandbox clone so the agent's pushed work is reachable as a local
// branch. Refusing with --force intentionally — if the operator has
// local work on that name we don't want to clobber it; they can merge
// or rename manually.
func fetchBranch(clonePath, runID string) error {
	branch := "moe/" + runID
	cmd := exec.Command("git", "-C", clonePath, "fetch", "origin", branch+":"+branch)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
