package claude

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTranscriptSessionID(t *testing.T) {
	const first = "aaaaaaaa-1111-4aaa-8aaa-aaaaaaaaaaaa"
	const second = "bbbbbbbb-2222-4bbb-8bbb-bbbbbbbbbbbb"
	record := func(id string) string { return fmt.Sprintf(`{"sessionId":%q}`, id) + "\n" }
	for _, tc := range []struct {
		name, body, want string
		bad              bool
	}{
		{"missing", "", "", false},
		{"bookkeeping", "{\"type\":\"bookkeeping\"}\n", "", false},
		{"large record before identity", "{\"text\":\"" + strings.Repeat("x", 1024*1024) + "\"}\n" + record(first), first, false},
		{"consistent", record(first) + record(first), first, false},
		{"conflict", record(first) + record(second), "", true},
		{"invalid ID", record("not-an-id"), "", true},
		{"malformed", "{broken\n", "", true},
		{"truncated after identity", record(first) + "{", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "thread.jsonl")
			if tc.name != "missing" {
				if err := os.WriteFile(path, []byte(tc.body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got, err := (Agent{}).TranscriptSessionID(path)
			if (err != nil) != tc.bad || got != tc.want {
				t.Fatalf("got %q, %v; want %q, error=%v", got, err, tc.want, tc.bad)
			}
		})
	}
	t.Run("unreadable", func(t *testing.T) {
		if _, err := (Agent{}).TranscriptSessionID(t.TempDir()); err == nil {
			t.Fatal("directory should fail")
		}
	})
}
