package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type rexInspectionLocation struct {
	bin, endpoint, session, origin string
}

// Full IDs avoid Rex's label/suffix/focus resolution, including on cleanup.
func inspectionRexID(id, kind string) bool {
	value, ok := strings.CutPrefix(id, kind+":")
	if !ok || len(value) != 36 {
		return false
	}
	for i, c := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
		} else if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return false
		}
	}
	return true
}

func (r rexInspectionLocation) call(ctx context.Context, stream bool, args ...string) ([]byte, error) {
	if !stream {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
	}
	argv := []string{"--autostart=false", "--timeout", "5s"}
	if r.endpoint != "" {
		argv = append(argv, "-S", r.endpoint)
	}
	cmd := exec.CommandContext(ctx, r.bin, append(argv, args...)...)
	cmd.WaitDelay = time.Second
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	if err != nil {
		err = fmt.Errorf("rex %s: %w: %s", args[0], err, strings.TrimSpace(errOut.String()))
	}
	return out.Bytes(), err
}

func (r *rexInspectionLocation) discover(ctx context.Context) error {
	if !inspectionRexID(r.session, "session") || !inspectionRexID(r.origin, "block") {
		return fmt.Errorf("Rex inspection requires full REX_SESSION and REX_BLOCK IDs")
	}
	out, err := r.call(ctx, false, "whoami", "--json")
	if err != nil {
		return err
	}
	var who struct {
		Endpoint    string `json:"endpoint"`
		APIRevision int    `json:"api_revision"`
		Principal   struct {
			Transport string `json:"transport"`
			UID       *int   `json:"uid"`
		} `json:"principal"`
	}
	if err := json.Unmarshal(out, &who); err != nil {
		return fmt.Errorf("unrecognized Rex whoami reply: %w", err)
	}
	endpoint, err := url.Parse(who.Endpoint)
	if err != nil || endpoint.Scheme != "unix" || endpoint.Host != "" || !filepath.IsAbs(endpoint.Path) ||
		endpoint.RawQuery != "" || endpoint.Fragment != "" || who.Principal.Transport != "unix" ||
		who.Principal.UID == nil || *who.Principal.UID != os.Getuid() {
		return fmt.Errorf("Rex inspection requires a same-account local Unix server")
	}
	if who.APIRevision < 10 {
		return fmt.Errorf("Rex inspection requires preview.13/API revision 10 or newer (got %d)", who.APIRevision)
	}
	r.endpoint = who.Endpoint
	present, err := r.originPresent(ctx)
	if err != nil {
		return err
	}
	if !present {
		return fmt.Errorf("originating Rex block %s no longer exists in %s", r.origin, r.session)
	}
	return nil
}

func (r rexInspectionLocation) originPresent(ctx context.Context) (bool, error) {
	out, err := r.call(ctx, false, "block", "ls", "--json", "--short=false", "-s", r.session)
	if err != nil {
		return false, err
	}
	var ls struct {
		Blocks []struct {
			ID string `json:"block_id"`
		} `json:"blocks"`
	}
	if err := json.Unmarshal(out, &ls); err != nil || ls.Blocks == nil {
		return false, fmt.Errorf("unrecognized Rex block listing")
	}
	present := false
	for _, b := range ls.Blocks {
		if !inspectionRexID(b.ID, "block") {
			return false, fmt.Errorf("invalid Rex block ID in listing")
		}
		present = present || b.ID == r.origin
	}
	return present, nil
}

func inspectionExit(out []byte) (int, bool) {
	var payload struct {
		Code *int `json:"exit_code"`
	}
	if json.Unmarshal(bytes.TrimSpace(out), &payload) == nil && payload.Code != nil && *payload.Code >= -1 {
		return *payload.Code, true
	}
	return 0, false
}

func (r rexInspectionLocation) lastExit(block string) (int, bool, error) {
	out, err := r.call(context.Background(), false, "block", "call", "-s", r.session, "-b", block, "process")
	if err != nil {
		return 0, false, err
	}
	var proc struct {
		LastExit json.RawMessage `json:"last_exit"`
	}
	if json.Unmarshal(out, &proc) != nil || len(proc.LastExit) == 0 {
		return 0, false, fmt.Errorf("unrecognized Rex process reply")
	}
	if bytes.Equal(bytes.TrimSpace(proc.LastExit), []byte("null")) {
		return 0, false, nil
	}
	code, ok := inspectionExit(proc.LastExit)
	if !ok {
		return 0, false, fmt.Errorf("unrecognized Rex exit reply")
	}
	return code, true, nil
}

func (r rexInspectionLocation) closeAndRestore(block string) error {
	_, closeErr := r.call(context.Background(), false, "block", "close", "-s", r.session, block)
	present, focusErr := r.originPresent(context.Background())
	if focusErr == nil && present {
		_, focusErr = r.call(context.Background(), false, "block", "focus", "-s", r.session, r.origin)
	}
	return errors.Join(closeErr, focusErr)
}

func presentRexArtifact(ctx context.Context, r rexInspectionLocation, a inspectionArtifact, v inspectionViewer) (result error) {
	if err := r.discover(ctx); err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "moe-inspect-*")
	if err != nil {
		return err
	}
	retain := false
	block := ""
	defer func() {
		if retain {
			result = errors.Join(result, fmt.Errorf("viewer lifetime unknown (block %q); snapshot retained at %s", block, dir))
		} else {
			result = errors.Join(result, os.RemoveAll(dir))
		}
	}()
	name := "canvas.md"
	if a.kind == inspectDiff {
		name = "source.diff"
	}
	file, err := filepath.Abs(filepath.Join(dir, name))
	if err != nil {
		return err
	}
	if err := os.WriteFile(file, []byte(a.body), 0600); err != nil {
		return err
	}
	// Once creation is attempted, a missing reply is not evidence that no
	// process exists. Never retry creation or remove a potentially live input.
	retain = true
	args := []string{"run", "--json", "--keep-open", "--shell=none", "--focus=true", "-s", r.session, "--label", a.title, "--cwd", dir, "--"}
	out, runErr := r.call(ctx, false, append(args, v.argv(file)...)...)
	var created struct {
		IDs []string `json:"block_ids"`
	}
	if json.Unmarshal(out, &created) == nil && len(created.IDs) == 1 &&
		inspectionRexID(created.IDs[0], "block") && created.IDs[0] != r.origin {
		block = created.IDs[0]
	}
	if block == "" {
		return errors.Join(runErr, fmt.Errorf("unrecognized Rex creation reply; no viewer ID to clean up"))
	}
	if runErr != nil {
		code, exited, readErr := r.lastExit(block)
		if exited {
			retain = false
			return errors.Join(runErr, viewerExitError(v, code), r.closeAndRestore(block))
		}
		if ctx.Err() != nil {
			return errors.Join(runErr, readErr, r.closeAndRestore(block))
		}
		return errors.Join(runErr, readErr)
	}
	out, waitErr := r.call(ctx, true, "wait", "--json", "-s", r.session, block)
	code, exited := inspectionExit(out)
	if !exited {
		var readErr error
		code, exited, readErr = r.lastExit(block)
		if !exited {
			if ctx.Err() != nil {
				return errors.Join(ctx.Err(), waitErr, readErr, r.closeAndRestore(block))
			}
			return errors.Join(waitErr, readErr, fmt.Errorf("Rex did not report viewer exit"))
		}
	}
	retain = false
	// Rex wait mirrors the child's nonzero exit. A valid payload, rather
	// than the CLI exit status, establishes completion.
	return errors.Join(viewerExitError(v, code), r.closeAndRestore(block))
}

func viewerExitError(v inspectionViewer, code int) error {
	if code != 0 {
		return fmt.Errorf("%s viewer exited %d", v.variable, code)
	}
	return nil
}
