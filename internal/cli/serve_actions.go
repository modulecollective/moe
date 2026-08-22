package cli

import (
	"slices"
	"sort"

	"github.com/modulecollective/moe/internal/serve"
)

// serveWorkflowDecl is the declaration a workflow makes at init() time
// — next to its RegisterGroup call, mirroring registerCascadeDispatcher
// — about how `moe serve` should front its runs. Declaring nothing
// keeps today's read-only per-run page; the decl is the opt-in.
//
// The web-visible stage set is not declared directly: it defaults to
// every registered stage verb (group.Lookup(stage) != nil), with
// excludeStages carving out the exceptions. sdlc excludes push —
// push stays terminal/CLI-only, a recorded decision.
type serveWorkflowDecl struct {
	// excludeStages are registered stage verbs the web must not act on.
	excludeStages []string
	// tagTarget offers the workflow as an idea tag destination — the
	// licence a sweep spends when it promotes.
	tagTarget bool
}

var serveWorkflowDecls = map[string]serveWorkflowDecl{}

// registerServeWorkflow records a workflow's serve declaration. Called
// from the workflow's init(); panics on duplicates — same fail-loud
// contract as RegisterWorkflow.
func registerServeWorkflow(workflow string, decl serveWorkflowDecl) {
	if _, dup := serveWorkflowDecls[workflow]; dup {
		panic("cli: duplicate serve declaration for workflow " + workflow)
	}
	serveWorkflowDecls[workflow] = decl
}

// lookupServeWorkflowUI composes the serve-facing view of one
// workflow's declaration: the web-visible stage verbs (ladder order,
// minus exclusions and stages with no registered verb) and whether a
// close pipeline is registered. ok=false — no
// declaration, or the paired group/workflow isn't registered — keeps
// the run read-only in serve.
func lookupServeWorkflowUI(workflow string) (serve.WorkflowUI, bool) {
	decl, ok := serveWorkflowDecls[workflow]
	if !ok {
		return serve.WorkflowUI{}, false
	}
	wf, err := LookupWorkflow(workflow)
	if err != nil {
		return serve.WorkflowUI{}, false
	}
	g, err := LookupGroup(workflow)
	if err != nil {
		return serve.WorkflowUI{}, false
	}
	var stages []string
	for _, stage := range wf.Stages() {
		if slices.Contains(decl.excludeStages, stage) {
			continue
		}
		if g.Lookup(stage) == nil {
			continue
		}
		stages = append(stages, stage)
	}
	_, hasClose := lookupCloseRegistration(workflow)
	return serve.WorkflowUI{
		Stages:    stages,
		Perpetual: wf.Perpetual(),
		Close:     hasClose,
	}, true
}

// serveTagWorkflows lists the workflows serve offers as idea tag
// destinations. sdlc is pinned first — it is the list's default, the
// workflow an untagged POST falls back to — and the rest follow in name
// order so the list is deterministic regardless of init() registration
// order. A workflow with no stages is dropped: a tag is a licence to run
// a ladder, and there would be none to run.
func serveTagWorkflows() []string {
	names := make([]string, 0, len(serveWorkflowDecls))
	for name, decl := range serveWorkflowDecls {
		if decl.tagTarget {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	if i := slices.Index(names, "sdlc"); i > 0 {
		names = append([]string{"sdlc"}, append(names[:i], names[i+1:]...)...)
	}
	out := make([]string, 0, len(names))
	for _, name := range names {
		wf, err := LookupWorkflow(name)
		if err != nil || len(wf.Stages()) == 0 {
			continue
		}
		out = append(out, name)
	}
	return out
}
