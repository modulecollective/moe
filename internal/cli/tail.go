package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/modulecollective/moe/internal/bureaucracy"
	"github.com/modulecollective/moe/internal/managed"
	"github.com/modulecollective/moe/internal/request"
)

func init() {
	Register(&Command{
		Name:    "tail",
		Summary: "follow an in-flight Managed Agents session's event stream",
		Run:     runTail,
	})
}

// runTail opens the managed session's event stream and prints one
// line per event. Detaching (Ctrl-C) does not affect the session — it
// keeps running server-side.
//
// Event rendering is intentionally terse. A structured `moe tail
// --json` is a natural follow-up if operators want to pipe into jq;
// today the goal is just "is it alive, and what's it doing?"
func runTail(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("tail", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe tail <project> <request> <document>")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 3 {
		fs.Usage()
		return 2
	}
	projectID, reqID, docID := fs.Arg(0), fs.Arg(1), fs.Arg(2)

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
	md, err := request.Load(root, projectID, reqID)
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

	// SIGINT detaches cleanly; the server-side session keeps running.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigs
		moePrintln(stderr, "")
		moePrintln(stderr, "(detaching; session keeps running)")
		cancel()
	}()

	events := make(chan managed.Event, 32)
	done := make(chan error, 1)
	go func() { done <- client.StreamEvents(ctx, doc.Managed, events) }()

	for {
		select {
		case ev, ok := <-events:
			if !ok {
				continue
			}
			renderEvent(stdout, ev)
		case err := <-done:
			close(events)
			if err != nil && ctx.Err() == nil {
				moePrintf(stderr, "stream: %v\n", err)
				return 1
			}
			return 0
		}
	}
}

func renderEvent(w io.Writer, ev managed.Event) {
	ts := ""
	if !ev.Time.IsZero() {
		ts = ev.Time.Format("15:04:05") + " "
	}
	fmt.Fprintf(w, "%s%s\n", ts, ev.Type)
}
