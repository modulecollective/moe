package agent

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

// sidChan returns a closed, buffered channel preloaded with sid (or
// empty-and-closed when sid is ""), mirroring how the backends hand a
// drained-and-closed channel to FinishOneShot.
func sidChan(sid string) <-chan string {
	ch := make(chan string, 1)
	if sid != "" {
		ch <- sid
	}
	close(ch)
	return ch
}

// TestFinishOneShotTimeoutMirrorsAndReturnsSid is the regression: on a
// deadline kill the helper must still drain the sid, mirror the
// transcript, and return the sid (codex used to return "" with no
// mirror here).
func TestFinishOneShotTimeoutMirrorsAndReturnsSid(t *testing.T) {
	var gotSid, gotDest string
	copyTranscript := func(sid, dest string) (bool, error) {
		gotSid, gotDest = sid, dest
		return true, nil
	}
	r := OneShotRequest{ThreadPath: "/dest/thread.jsonl", Timeout: time.Minute}

	sid, err := FinishOneShot(sidChan("sess-1"), r, true, errors.New("signal: killed"), "codex: exec", copyTranscript)

	if sid != "sess-1" {
		t.Errorf("sid = %q, want sess-1", sid)
	}
	if err == nil || !strings.Contains(err.Error(), "codex: exec timed out after") {
		t.Errorf("err = %v, want a codex timeout error", err)
	}
	if gotSid != "sess-1" || gotDest != "/dest/thread.jsonl" {
		t.Errorf("mirror called with (%q, %q), want (sess-1, /dest/thread.jsonl)", gotSid, gotDest)
	}
}

// TestFinishOneShotNoTimeoutReturnsWaitErr: a non-deadline failure
// returns the raw wait error verbatim, not a timeout error.
func TestFinishOneShotNoTimeoutReturnsWaitErr(t *testing.T) {
	waitErr := errors.New("exit status 1")
	called := false
	copyTranscript := func(sid, dest string) (bool, error) { called = true; return true, nil }
	r := OneShotRequest{ThreadPath: "/dest/thread.jsonl"}

	sid, err := FinishOneShot(sidChan("sess-2"), r, false, waitErr, "claude: -p", copyTranscript)

	if sid != "sess-2" {
		t.Errorf("sid = %q, want sess-2", sid)
	}
	if !errors.Is(err, waitErr) {
		t.Errorf("err = %v, want the raw waitErr", err)
	}
	if !called {
		t.Error("mirror should run even on a non-timeout exit")
	}
}

// TestFinishOneShotSkipsMirror: the mirror is skipped when there's no
// destination or no captured sid (nothing to copy / nowhere to look).
func TestFinishOneShotSkipsMirror(t *testing.T) {
	cases := []struct {
		name       string
		sid        string
		threadPath string
	}{
		{"no thread path", "sess-3", ""},
		{"no sid", "", "/dest/thread.jsonl"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			copyTranscript := func(sid, dest string) (bool, error) { called = true; return true, nil }
			r := OneShotRequest{ThreadPath: tc.threadPath}

			gotSid, err := FinishOneShot(sidChan(tc.sid), r, false, nil, "claude: -p", copyTranscript)

			if called {
				t.Error("mirror should be skipped")
			}
			if gotSid != tc.sid {
				t.Errorf("sid = %q, want %q", gotSid, tc.sid)
			}
			if err != nil {
				t.Errorf("err = %v, want nil", err)
			}
		})
	}
}

// TestFinishOneShotCopyErrorDoesNotOverrideExit: a mirror failure is
// reported on Stderr but the returned error is still the turn's exit
// (here, the timeout), never the copy error.
func TestFinishOneShotCopyErrorDoesNotOverrideExit(t *testing.T) {
	var stderr bytes.Buffer
	copyTranscript := func(sid, dest string) (bool, error) {
		return false, errors.New("disk full")
	}
	r := OneShotRequest{ThreadPath: "/dest/thread.jsonl", Timeout: time.Minute, Stderr: &stderr}

	sid, err := FinishOneShot(sidChan("sess-4"), r, true, errors.New("signal: killed"), "codex: exec", copyTranscript)

	if sid != "sess-4" {
		t.Errorf("sid = %q, want sess-4", sid)
	}
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("err = %v, want the timeout error, not the copy error", err)
	}
	if !strings.Contains(stderr.String(), "save transcript: disk full") {
		t.Errorf("stderr = %q, want the copy error reported", stderr.String())
	}
}
