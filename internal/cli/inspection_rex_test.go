package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const (
	inspectionSession = "session:11111111-1111-1111-1111-111111111111"
	inspectionOrigin  = "block:22222222-2222-2222-2222-222222222222"
	inspectionBlock   = "block:33333333-3333-3333-3333-333333333333"
)

type inspectionRexRecord struct {
	Args              []string
	File, Body, Cwd   string
	DirMode, FileMode uint32
}

// Run the test binary as a fake Rex executable. The subprocess boundary
// exercises actual argv/JSON/exit handling without contacting any server.
func TestInspectionRexProcess(t *testing.T) {
	if os.Getenv("MOE_INSPECTION_FAKE") != "1" {
		return
	}
	args := os.Args[slices.Index(os.Args, "--")+1:]
	mode := os.Getenv("MOE_INSPECTION_MODE")
	fail := func(err error) {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(98)
		}
	}
	record := inspectionRexRecord{Args: args}
	command := ""
	for _, arg := range args {
		if slices.Contains([]string{"whoami", "run", "wait", "block"}, arg) {
			command = arg
			break
		}
	}
	if command == "run" {
		file := args[len(args)-1]
		body, err := os.ReadFile(file)
		fail(err)
		info, err := os.Stat(file)
		fail(err)
		dirInfo, err := os.Stat(filepath.Dir(file))
		fail(err)
		record.File, record.Body, record.FileMode, record.DirMode = file, string(body), uint32(info.Mode().Perm()), uint32(dirInfo.Mode().Perm())
		i := slices.Index(args, "--cwd")
		record.Cwd = args[i+1]
	}
	f, err := os.OpenFile(os.Getenv("MOE_INSPECTION_LOG"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	fail(err)
	fail(json.NewEncoder(f).Encode(record))
	fail(f.Close())
	switch command {
	case "whoami":
		endpoint, revision := "unix:///tmp/inspection.sock", 10
		if mode == "remote" {
			endpoint = "ssh://remote"
		}
		if mode == "old" {
			revision = 9
		}
		fmt.Printf(`{"endpoint":%q,"api_revision":%d,"principal":{"transport":"unix","uid":%d}}`, endpoint, revision, os.Getuid())
	case "run":
		if mode == "lost-run" {
			os.Exit(1)
		}
		if mode == "invalid-id" {
			fmt.Print(`{"block_ids":["block:bogus"]}`)
			break
		}
		if mode == "origin-id" {
			fmt.Printf(`{"block_ids":[%q]}`, inspectionOrigin)
			break
		}
		fmt.Printf(`{"block_ids":[%q]}`, inspectionBlock)
	case "wait":
		if mode == "wait-unknown" || mode == "fast-exit" {
			os.Exit(1)
		}
		code := 0
		if mode == "viewer-fail" {
			code = 7
		}
		fmt.Printf(`{"exit_code":%d}`, code)
		os.Exit(code)
	case "block":
		switch {
		case slices.Contains(args, "ls"):
			if mode == "listing-fail" {
				fmt.Print(`{}`)
				break
			}
			fmt.Printf(`{"blocks":[{"block_id":%q}]}`, inspectionOrigin)
		case slices.Contains(args, "process"):
			if mode == "fast-exit" {
				fmt.Print(`{"last_exit":{"exit_code":0}}`)
			} else {
				fmt.Print(`{"last_exit":null}`)
			}
		case slices.Contains(args, "close"):
			if mode == "close-fail" {
				os.Exit(1)
			}
		case slices.Contains(args, "focus"):
			if mode == "focus-fail" {
				os.Exit(1)
			}
		default:
			os.Exit(97)
		}
	default:
		os.Exit(96)
	}
	os.Exit(0)
}

func fakeInspectionRex(t *testing.T, mode string) (rexInspectionLocation, string) {
	t.Helper()
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "rex")
	// Test-binary paths are generated under TempDir and contain no apostrophes.
	inspectionFile(t, path, "#!/bin/sh\nexec '"+bin+"' -test.run=^TestInspectionRexProcess$ -- \"$@\"\n", 0700)
	log := filepath.Join(t.TempDir(), "calls.jsonl")
	t.Setenv("MOE_INSPECTION_FAKE", "1")
	t.Setenv("MOE_INSPECTION_MODE", mode)
	t.Setenv("MOE_INSPECTION_LOG", log)
	t.Setenv("REX_SESSION", inspectionSession)
	t.Setenv("REX_BLOCK", inspectionOrigin)
	return rexInspectionLocation{bin: path, session: inspectionSession, origin: inspectionOrigin}, log
}

func inspectionRexRecords(t *testing.T, log string) []inspectionRexRecord {
	t.Helper()
	f, err := os.Open(log)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var records []inspectionRexRecord
	s := bufio.NewScanner(f)
	for s.Scan() {
		var r inspectionRexRecord
		if err := json.Unmarshal(s.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		records = append(records, r)
	}
	if err := s.Err(); err != nil {
		t.Fatal(err)
	}
	return records
}

func TestInspectionRexLifecycle(t *testing.T) {
	for _, mode := range []string{"ok", "viewer-fail", "fast-exit", "wait-unknown", "lost-run", "invalid-id", "origin-id", "close-fail", "focus-fail", "remote", "old", "listing-fail"} {
		t.Run(mode, func(t *testing.T) {
			r, log := fakeInspectionRex(t, mode)
			a := inspectionArtifact{kind: inspectDiff, title: `demo/run source diff '$;`, body: "diff --git a/file b/file\n"}
			v := inspectionViewer{variable: "MOE_DIFF_VIEWER", command: "hunk patch"}
			err := presentRexArtifact(context.Background(), r, a, v)
			wantErr := mode != "ok" && mode != "fast-exit"
			if (err != nil) != wantErr {
				t.Fatalf("err=%v", err)
			}
			records := inspectionRexRecords(t, log)
			var runRecord *inspectionRexRecord
			var closeCount, focusCount, runCount int
			for i, c := range records {
				if !slices.Contains(c.Args, "--autostart=false") {
					t.Fatalf("autostart: %v", c.Args)
				}
				if i > 0 {
					at := slices.Index(c.Args, "-S")
					if at < 0 || c.Args[at+1] != "unix:///tmp/inspection.sock" || !slices.Contains(c.Args, inspectionSession) {
						t.Fatalf("target: %v", c.Args)
					}
				}
				if slices.Contains(c.Args, "run") {
					runCount++
					runRecord = &records[i]
				}
				if slices.Contains(c.Args, "close") {
					closeCount++
					if c.Args[len(c.Args)-1] != inspectionBlock {
						t.Fatalf("closed unrelated block: %v", c.Args)
					}
				}
				if slices.Contains(c.Args, "focus") {
					focusCount++
					if c.Args[len(c.Args)-1] != inspectionOrigin {
						t.Fatalf("focus: %v", c.Args)
					}
				}
			}
			if slices.Contains([]string{"remote", "old", "listing-fail"}, mode) {
				if runCount != 0 {
					t.Fatal("launched with unsupported server")
				}
				return
			}
			if runCount != 1 {
				t.Fatalf("run count=%d", runCount)
			}
			if runRecord.Body != a.body || runRecord.FileMode != 0600 || runRecord.DirMode != 0700 || runRecord.Cwd != filepath.Dir(runRecord.File) {
				t.Fatalf("snapshot: %+v", runRecord)
			}
			for _, arg := range []string{"--shell=none", "--focus=true", "--keep-open", "/usr/bin/env", "PATH=" + os.Getenv("PATH"), "LESSSECURE=1", "LESSOPEN=", "LESSCLOSE=", "LESS=", "/bin/sh", "exec hunk patch \"$1\"", runRecord.File} {
				if !slices.Contains(runRecord.Args, arg) {
					t.Fatalf("missing %q: %v", arg, runRecord.Args)
				}
			}
			for _, arg := range runRecord.Args {
				if strings.HasPrefix(arg, "REX_BLOCK=") {
					t.Fatal("parent Rex environment copied to child")
				}
			}
			retained := slices.Contains([]string{"wait-unknown", "lost-run", "invalid-id", "origin-id"}, mode)
			_, statErr := os.Stat(runRecord.File)
			if retained {
				if statErr != nil || !strings.Contains(err.Error(), filepath.Dir(runRecord.File)) || closeCount != 0 || focusCount != 0 {
					t.Fatalf("unknown lifetime: err=%v stat=%v close=%d focus=%d", err, statErr, closeCount, focusCount)
				}
				t.Cleanup(func() { os.RemoveAll(filepath.Dir(runRecord.File)) })
			} else if !os.IsNotExist(statErr) || closeCount != 1 || focusCount != 1 {
				t.Fatalf("cleanup: stat=%v close=%d focus=%d", statErr, closeCount, focusCount)
			}
			if mode == "viewer-fail" && !strings.Contains(err.Error(), "MOE_DIFF_VIEWER viewer exited 7") {
				t.Fatalf("viewer failure: %v", err)
			}
		})
	}
}
