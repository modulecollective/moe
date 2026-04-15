package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/modulecollective/moe/internal/bureaucracy"
	"github.com/modulecollective/moe/internal/request"
	"github.com/modulecollective/moe/internal/sandbox"
	"github.com/modulecollective/moe/internal/stage"
)

func init() {
	Register(&Command{
		Name:    "sign",
		Summary: "sign off on a request stage (design, code)",
		Run:     runSign,
	})
	Register(&Command{
		Name:    "unsign",
		Summary: "reverse a previous moe sign (cascades to dependent stages)",
		Run:     runUnsign,
	})
}

func runSign(args []string, stdout, stderr io.Writer) int {
	return runSignUnsign(args, stdout, stderr, true)
}

func runUnsign(args []string, stdout, stderr io.Writer) int {
	return runSignUnsign(args, stdout, stderr, false)
}

// runSignUnsign dispatches the top-level `moe sign` / `moe unsign`. Each
// stage flip lands as its own commit on main so the journal stays faithful
// to operator intent — on unsign that includes a cascade flip for every
// dependent stage that was still signed (reopening a prerequisite must
// invalidate stages that assumed it).
func runSignUnsign(args []string, stdout, stderr io.Writer, signing bool) int {
	verb := "sign"
	if !signing {
		verb = "unsign"
	}
	fs := flag.NewFlagSet(verb, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintf(stderr, "usage: moe %s <project> <request> <stage>\n", verb)
		moePrintln(stderr, "")
		moePrintln(stderr, "stages:")
		for _, name := range stage.Names() {
			s, _ := stage.Lookup(name)
			moePrintf(stderr, "  %-7s  %s\n", name, s.Help)
		}
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 3 {
		fs.Usage()
		return 2
	}
	projectID, reqID, stageName := fs.Arg(0), fs.Arg(1), fs.Arg(2)

	st, ok := stage.Lookup(stageName)
	if !ok {
		moePrintf(stderr, "unknown stage %q; known: %s\n", stageName, strings.Join(stage.Names(), ", "))
		return 1
	}

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

	currentlySigned, err := stage.IsSigned(root, reqID, stageName)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if signing && currentlySigned {
		moePrintf(stdout, "%s/%s stage %q already signed; no change\n", projectID, reqID, stageName)
		return 0
	}
	if !signing && !currentlySigned {
		moePrintf(stdout, "%s/%s stage %q not signed; no change\n", projectID, reqID, stageName)
		return 0
	}

	// Preconditions apply on sign only — unsigning is always allowed, since
	// the operator is walking things back.
	if signing {
		for _, dep := range st.Requires {
			ok, err := stage.IsSigned(root, reqID, dep)
			if err != nil {
				moePrintf(stderr, "%v\n", err)
				return 1
			}
			if !ok {
				moePrintf(stderr, "cannot sign %q — prerequisite stage %q is not signed\n", stageName, dep)
				moePrintf(stderr, "run `moe sign %s %s %s` first\n", projectID, reqID, dep)
				return 1
			}
		}
	}

	if err := flipOneStage(root, md, projectID, reqID, stageName, signing, stdout); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	// Cascade: reopening a stage invalidates anything that required it. We
	// only do this on unsign — signing is already gated by preconditions, so
	// there's no downstream to flip. Run breadth-first so each cascade
	// commit is recorded individually in the journal.
	if !signing {
		queue := stage.Dependents(stageName)
		visited := map[string]bool{stageName: true}
		for len(queue) > 0 {
			next := queue[0]
			queue = queue[1:]
			if visited[next] {
				continue
			}
			visited[next] = true
			dependentSigned, err := stage.IsSigned(root, reqID, next)
			if err != nil {
				moePrintf(stderr, "%v\n", err)
				return 1
			}
			if !dependentSigned {
				continue
			}
			moePrintf(stdout, "cascading unsign: %q depended on %q\n", next, stageName)
			if err := flipOneStage(root, md, projectID, reqID, next, false, stdout); err != nil {
				moePrintf(stderr, "%v\n", err)
				return 1
			}
			queue = append(queue, stage.Dependents(next)...)
		}
	}
	return 0
}

// flipOneStage records a single MoE-Stage-Signed or -Unsigned commit for
// stageName, applying the stage's side-effect (today: only `code` flips the
// request status) inside the same commit. Used both for the explicitly
// requested stage and, during unsign, for each cascaded dependent.
func flipOneStage(root string, md *request.Metadata, projectID, reqID, stageName string, signing bool, stdout io.Writer) error {
	verb := "sign"
	trailer := "MoE-Stage-Signed"
	if !signing {
		verb = "unsign"
		trailer = "MoE-Stage-Unsigned"
	}

	var pathspecs []string
	if stageName == "code" {
		if signing {
			md.Status = "approved"
		} else {
			md.Status = "in_progress"
		}
		if err := request.Save(root, md); err != nil {
			return err
		}
		pathspecs = []string{request.RunDir(projectID, reqID) + "/request.json"}
	}

	msg := fmt.Sprintf(`%s: %s

MoE-Request: %s
MoE-Project: %s
%s: %s
`, verb, stageName, reqID, projectID, trailer, stageName)

	if err := request.CommitAllowEmpty(root, msg, pathspecs...); err != nil {
		return err
	}
	moePrintf(stdout, "%s %s/%s stage %q\n", verb, projectID, reqID, stageName)
	if stageName == "code" && signing {
		moePrintln(stdout, "  (target-repo PR open is not yet implemented; status flipped only)")
		// A signed `code` stage terminates the request's active work against
		// the target code, so the sandbox clone is no longer needed. Fire
		// and forget: a leftover clone is a disk nuisance, not a correctness
		// bug, so we don't fail the sign over it.
		if sandbox.Exists(root, projectID, reqID) {
			if err := sandbox.Remove(root, projectID, reqID); err != nil {
				moePrintf(stdout, "  (warning: failed to remove sandbox clone: %v)\n", err)
			} else {
				moePrintln(stdout, "  removed sandbox clone")
			}
		}
	}
	return nil
}
