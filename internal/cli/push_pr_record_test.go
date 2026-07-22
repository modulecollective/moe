package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/push"
	"github.com/modulecollective/moe/internal/run"
)

// failPRRecordCommits refuses only the PR record subject. Push
// synthesis and fixture commits still land, leaving the test at the
// same boundary as a real StageAndCommit failure.
func failPRRecordCommits(t *testing.T, root, projectID, runID string) func() {
	t.Helper()
	hooks := t.TempDir()
	hook := filepath.Join(hooks, "commit-msg")
	subject := "push: " + projectID + "/" + runID
	script := fmt.Sprintf("#!/bin/sh\n[ \"$(head -1 \"$1\")\" = %q ] && exit 1\nexit 0\n", subject)
	if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "config", "core.hooksPath", hooks)
	return func() {
		t.Helper()
		gittest.Run(t, root, "config", "--unset", "core.hooksPath")
	}
}

type prRecordFixture struct {
	*pushFixture
	url        string
	ghLog      string
	synthCalls *int
}

// newPRRecordFixture adapts the local-origin push fixture to the PR
// path. Synthesis is represented by an already-committed push canvas;
// the stub only counts calls, so a failed record commit leaves exactly
// run.json staged plus the pending marker, matching production.
func newPRRecordFixture(t *testing.T) *prRecordFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell shim gh fake only works on unix-y OSes")
	}
	f := newPushFixture(t)
	const fakeRemote = "https://github.com/owner/repo.git"
	const url = "https://github.com/owner/repo/pull/77"
	addInsteadOfRewrite(t, fakeRemote, f.origin)
	writeFile(t, filepath.Join(f.root, "projects", f.projectID, "project.json"),
		`{"id":"`+f.projectID+`","submodule":"projects/`+f.projectID+`/src","remote":"`+fakeRemote+`","default_branch":"main"}`+"\n")
	writeContent(t, f.root, f.projectID, f.runID, "push",
		"# Push\n\n## PR body\n\nPrepared body.\n\n## Ship readiness\n\nGreen.\n\n## Conflicts surfaced\n\n")
	gittest.Run(t, f.root, "add",
		filepath.Join("projects", f.projectID, "project.json"),
		run.ContentPath(f.projectID, f.runID, "push"))
	gittest.Run(t, f.root, "commit", "-m", "prepare existing PR fixture")

	synthCalls := 0
	prev := runStageSession
	runStageSession = func(_, _, docID string, _ stageSessionOpts, _, _ io.Writer) int {
		if docID != "push" {
			t.Fatalf("unexpected synthesis document %q", docID)
		}
		synthCalls++
		return 0
	}
	t.Cleanup(func() { runStageSession = prev })

	dir := t.TempDir()
	ghLog := filepath.Join(dir, "gh.log")
	script := `#!/bin/sh
printf '%s\n' "$*" >> ` + ghLog + `
if [ "$1" = "pr" ] && [ "$2" = "list" ]; then
  echo '[{"url":"` + url + `"}]'
  exit 0
fi
echo "fake gh: unsupported invocation: $*" >&2
exit 2
`
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	return &prRecordFixture{pushFixture: f, url: url, ghLog: ghLog, synthCalls: &synthCalls}
}

func (f *prRecordFixture) ghCalls(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(f.ghLog)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func readPRPendingRecord(t *testing.T, f *pushFixture) pendingPRRecord {
	t.Helper()
	data, err := os.ReadFile(prRecordPendingPath(f.root, f.reloadRun()))
	if err != nil {
		t.Fatalf("read %s: %v", prRecordPendingName, err)
	}
	var p pendingPRRecord
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatalf("parse %s: %v\n%s", prRecordPendingName, err, data)
	}
	return p
}

func prPendingExists(f *pushFixture) bool {
	_, err := os.Stat(prRecordPendingPath(f.root, f.reloadRun()))
	return err == nil
}

func strandPRRecord(t *testing.T, f *prRecordFixture) (lift func(), stderr string) {
	t.Helper()
	lift = failPRRecordCommits(t, f.root, f.projectID, f.runID)
	_, stderr, code := f.runInRoot("sdlc", "push", "--pr", f.projectID+"/"+f.runID)
	if code == 0 {
		t.Fatalf("expected PR record commit failure; stderr=%s", stderr)
	}
	return lift, stderr
}

func TestPushPRRecordFailurePersistsExactPendingRecord(t *testing.T) {
	f := newPRRecordFixture(t)
	defer withRideMode(rideStatic)()
	fired := stubFirePulse(t)

	lift, stderr := strandPRRecord(t, f)
	defer lift()

	if md := f.reloadRun(); md.Status != run.StatusPushed {
		t.Fatalf("status on disk: want %s, got %s", run.StatusPushed, md.Status)
	}
	if got := push.TrailerValue(f.root, f.projectID, f.runID, "MoE-PR"); got != "" {
		t.Fatalf("record commit failed but MoE-PR resolves to %q", got)
	}
	p := readPRPendingRecord(t, f.pushFixture)
	if p.URL != f.url {
		t.Fatalf("pending URL: want %q, got %q", f.url, p.URL)
	}
	for _, want := range []string{"MoE-PR: " + f.url, "MoE-Consent: static"} {
		if !strings.Contains(p.Msg, want) {
			t.Fatalf("pending message missing %q:\n%s", want, p.Msg)
		}
	}
	staged := gittest.Output(t, f.root, "diff", "--cached", "--name-only")
	if strings.TrimSpace(staged) != filepath.Join(run.Dir(f.projectID, f.runID), "run.json") {
		t.Fatalf("staged paths: want only run.json, got:\n%s", staged)
	}
	if len(*fired) != 0 {
		t.Fatalf("stranded record fired pulse before recovery: %v", *fired)
	}
	if !strings.Contains(stderr, "re-run `moe sdlc push --pr "+f.projectID+"/"+f.runID+"`") {
		t.Fatalf("stderr missing exact retry:\n%s", stderr)
	}
}

func TestPushResumesPRRecordBeforeSynthesisOrGitHub(t *testing.T) {
	f := newPRRecordFixture(t)
	// Ship under dynamic consent, then recover under a different ride.
	// The committed value must come from the persisted shipping record.
	var lift func()
	func() {
		defer withRideMode(rideDynamic)()
		lift, _ = strandPRRecord(t, f)
	}()
	lift()
	defer withRideMode(rideStatic)()
	fired := stubFirePulse(t)
	synthBefore := *f.synthCalls
	ghBefore := f.ghCalls(t)

	stdout, stderr, code := f.runInRoot("sdlc", "push", f.projectID+"/"+f.runID)
	if code != 0 {
		t.Fatalf("resume: exit=%d\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "recorded PR: "+f.url) {
		t.Fatalf("resume stdout missing URL:\n%s", stdout)
	}
	if *f.synthCalls != synthBefore {
		t.Fatalf("resume reran synthesis: before=%d after=%d", synthBefore, *f.synthCalls)
	}
	if got := f.ghCalls(t); got != ghBefore {
		t.Fatalf("resume called gh again:\nbefore=%s\nafter=%s", ghBefore, got)
	}
	if prPendingExists(f.pushFixture) {
		t.Fatalf("%s should be removed after recovery", prRecordPendingName)
	}
	if got := push.TrailerValue(f.root, f.projectID, f.runID, "MoE-PR"); got != f.url {
		t.Fatalf("MoE-PR after recovery: want %q, got %q", f.url, got)
	}
	body := lastCommitMessage(t, f.root)
	if !strings.Contains(body, "MoE-Consent: dynamic") || strings.Contains(body, "MoE-Consent: static") {
		t.Fatalf("recovery did not preserve the shipping consent:\n%s", body)
	}
	if entries, err := git.Status(f.root); err != nil {
		t.Fatal(err)
	} else if len(entries) != 0 {
		t.Fatalf("bureaucracy tree dirty after recovery: %#v", entries)
	}
	wantFire := f.projectID + " " + f.runID
	if len(*fired) != 1 || (*fired)[0] != wantFire {
		t.Fatalf("firePulse fired %v, want exactly one %q", *fired, wantFire)
	}
}

func TestPushPRRecordRepeatedFailureKeepsMarker(t *testing.T) {
	f := newPRRecordFixture(t)
	defer withRideMode(rideStatic)()
	fired := stubFirePulse(t)
	lift, _ := strandPRRecord(t, f)
	defer lift()
	path := prRecordPendingPath(f.root, f.reloadRun())
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	synthBefore := *f.synthCalls
	ghBefore := f.ghCalls(t)

	_, stderr, code := f.runInRoot("sdlc", "push", f.projectID+"/"+f.runID)
	if code == 0 {
		t.Fatalf("repeated failure unexpectedly succeeded; stderr=%s", stderr)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("repeated failure rewrote pending record:\nbefore=%s\nafter=%s", before, after)
	}
	if *f.synthCalls != synthBefore || f.ghCalls(t) != ghBefore {
		t.Fatalf("direct retry repeated upstream work: synth %d→%d, gh %q→%q",
			synthBefore, *f.synthCalls, ghBefore, f.ghCalls(t))
	}
	if len(*fired) != 0 {
		t.Fatalf("failed retries fired pulse: %v", *fired)
	}
}

func TestPushPRRecordResumeAcceptsHandCommit(t *testing.T) {
	f := newPRRecordFixture(t)
	lift, _ := strandPRRecord(t, f)
	lift()
	p := readPRPendingRecord(t, f.pushFixture)
	gittest.Run(t, f.root, "commit", "-m", p.Msg)
	before := strings.TrimSpace(gittest.Output(t, f.root, "rev-list", "--count", "HEAD"))

	stdout, stderr, code := f.runInRoot("sdlc", "push", f.projectID+"/"+f.runID)
	if code != 0 {
		t.Fatalf("resume after hand commit: exit=%d\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	after := strings.TrimSpace(gittest.Output(t, f.root, "rev-list", "--count", "HEAD"))
	if after != before {
		t.Fatalf("resume created a duplicate record commit: before=%s after=%s", before, after)
	}
	if prPendingExists(f.pushFixture) {
		t.Fatalf("%s should be removed after hand-committed recovery", prRecordPendingName)
	}
}

func TestPushPRRecordRemovalFailureDefersPulseUntilCleanup(t *testing.T) {
	f := newPRRecordFixture(t)
	lift, _ := strandPRRecord(t, f)
	lift()
	defer withRideMode(rideStatic)()
	fired := stubFirePulse(t)

	origRemove := removePRRecordPending
	removeCalls := 0
	removePRRecordPending = func(root string, md *run.Metadata) error {
		removeCalls++
		if removeCalls == 1 {
			return fmt.Errorf("injected removal failure")
		}
		return origRemove(root, md)
	}
	t.Cleanup(func() { removePRRecordPending = origRemove })

	stdout, stderr, code := f.runInRoot("sdlc", "push", f.projectID+"/"+f.runID)
	if code == 0 {
		t.Fatalf("resume with removal failure unexpectedly succeeded\nstdout=%s\nstderr=%s", stdout, stderr)
	}
	if strings.Contains(stdout, "recorded PR:") {
		t.Fatalf("resume announced success before marker cleanup:\n%s", stdout)
	}
	if !strings.Contains(stderr, "remove "+prRecordPendingName+": injected removal failure") ||
		!strings.Contains(stderr, "cleanup is still pending") {
		t.Fatalf("resume stderr missing cleanup failure and retry:\n%s", stderr)
	}
	if !prPendingExists(f.pushFixture) {
		t.Fatalf("failed cleanup removed %s", prRecordPendingName)
	}
	if len(*fired) != 0 {
		t.Fatalf("failed cleanup fired pulse: %v", *fired)
	}

	stdout, stderr, code = f.runInRoot("sdlc", "push", f.projectID+"/"+f.runID)
	if code != 0 {
		t.Fatalf("cleanup retry: exit=%d\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "recorded PR: "+f.url) {
		t.Fatalf("cleanup retry stdout missing URL:\n%s", stdout)
	}
	if prPendingExists(f.pushFixture) {
		t.Fatalf("cleanup retry left %s", prRecordPendingName)
	}
	wantFire := f.projectID + " " + f.runID
	if len(*fired) != 1 || (*fired)[0] != wantFire {
		t.Fatalf("cleanup retries fired %v, want exactly one %q", *fired, wantFire)
	}
}

func TestPushPRRecordResumeThreadsPulseInterrupt(t *testing.T) {
	f := newPRRecordFixture(t)
	lift, _ := strandPRRecord(t, f)
	lift()
	defer withRideMode(rideStatic)()
	orig := firePulse
	firePulse = func(root, projectID, spawner string, stdout, stderr io.Writer) bool { return true }
	t.Cleanup(func() { firePulse = orig })
	t.Setenv("MOE_HOME", f.root)
	t.Setenv("NO_COLOR", "1")

	var stdout, stderr strings.Builder
	code, interrupted, err := runPushTyped("sdlc", []string{f.projectID + "/" + f.runID}, &stdout, &stderr)
	if err != nil || code != 0 {
		t.Fatalf("resume: exit=%d err=%v\nstdout=%s\nstderr=%s", code, err, stdout.String(), stderr.String())
	}
	if !interrupted {
		t.Fatal("PR-record recovery dropped the tail pulse interrupt")
	}
}

func TestPushPRRepairsMarkerlessPushedRun(t *testing.T) {
	f := newPRRecordFixture(t)
	md := f.reloadRun()
	md.Status = run.StatusPushed
	if err := run.Save(f.root, md); err != nil {
		t.Fatal(err)
	}
	runJSON := filepath.Join(run.Dir(f.projectID, f.runID), "run.json")
	gittest.Run(t, f.root, "add", runJSON)
	if err := os.WriteFile(prRecordPendingPath(f.root, md), []byte("not json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer withRideMode(rideStatic)()

	stdout, stderr, code := f.runInRoot("sdlc", "push", "--pr", f.projectID+"/"+f.runID)
	if code != 0 {
		t.Fatalf("legacy repair: exit=%d\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	if got := push.TrailerValue(f.root, f.projectID, f.runID, "MoE-PR"); got != f.url {
		t.Fatalf("repaired MoE-PR: want %q, got %q", f.url, got)
	}
	body := lastCommitMessage(t, f.root)
	if strings.Contains(body, "MoE-Consent") {
		t.Fatalf("markerless repair attributed the later retry as the ship:\n%s", body)
	}
	if !strings.Contains(stderr, prRecordPendingName+" is unreadable") {
		t.Fatalf("legacy repair did not warn about malformed marker:\n%s", stderr)
	}
	if prPendingExists(f.pushFixture) {
		t.Fatalf("successful reconstruction left stale %s", prRecordPendingName)
	}
}
