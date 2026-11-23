package session

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/repolock"
)

func TestGCClaimHeld(t *testing.T) {
	now := time.Now().UTC()
	pid := exitedPid(t)
	if repolock.ProcessAlive(pid) {
		t.Fatal("exited fixture PID must be dead in the reader's namespace")
	}
	dead := Claim{
		Branch: BranchName("moe", "r1", "design"),
		Owner:  fmt.Sprintf("%s/%d", hostnameOrSkip(t), pid), PidNS: repolock.PidNamespace(),
		StartedAt: now.Add(-time.Hour), HeartbeatAt: now.Add(-2 * StaleAfter),
	}
	for _, tc := range []struct {
		name string
		body string
		edit func(*Claim)
		held bool
	}{
		{name: "dead operator"},
		{name: "dead machine", edit: func(c *Claim) { c.Machine = true }},
		{name: "legacy namespace", edit: func(c *Claim) { c.PidNS = "" }},
		{name: "released dead", edit: func(c *Claim) { c.ReleasedAt = now }},
		{name: "live stale", held: true, edit: func(c *Claim) { c.Owner = repolock.OwnerString(hostnameOrSkip(t)) }},
		{name: "released live", held: true, edit: func(c *Claim) { c.Owner = repolock.OwnerString(hostnameOrSkip(t)); c.ReleasedAt = now }},
		{name: "fresh dead", held: true, edit: func(c *Claim) { c.HeartbeatAt = now }},
		{name: "released fresh dead", held: true, edit: func(c *Claim) { c.HeartbeatAt = now; c.ReleasedAt = now }},
		{name: "zero beat", held: true, edit: func(c *Claim) { c.HeartbeatAt = time.Time{} }},
		{name: "foreign host", held: true, edit: func(c *Claim) { c.Owner = "other-host/99999999" }},
		{name: "foreign namespace", held: true, edit: func(c *Claim) { c.PidNS = "foreign-namespace" }},
		{name: "invalid owner", held: true, edit: func(c *Claim) { c.Owner = "invalid" }},
		{name: "missing branch", held: true, edit: func(c *Claim) { c.Branch = "" }},
		{name: "wrong branch", held: true, edit: func(c *Claim) { c.Branch += "-other" }},
		{name: "truncated", held: true, body: `{"branch":`},
		{name: "timestamp", held: true, body: `{"heartbeat_at":"invalid"}`},
		{name: "wrong type", held: true, body: `{"branch":17}`},
		{name: "empty object", held: true, body: `{}`},
		{name: "null", held: true, body: `null`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			path := ClaimPath(root, "moe", "r1", "design")
			c := dead
			if tc.edit != nil {
				tc.edit(&c)
			}
			if err := writeClaim(path, c); err != nil {
				t.Fatal(err)
			}
			if tc.body != "" {
				if err := os.WriteFile(path, []byte(tc.body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if got := GCClaimHeld(root, "moe", "r1", "design", now); got != tc.held {
				t.Errorf("GCClaimHeld = %v, want %v", got, tc.held)
			}
		})
	}
}

func TestGCClaimHeldFilesystem(t *testing.T) {
	for _, kind := range []string{"absent directories", "absent file", "directory", "dangling symlink", "regular symlink", "unreadable", "stat permission", "stat not directory"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			path := ClaimPath(root, "moe", "r1", "design")
			parent := filepath.Dir(path)
			if kind == "absent directories" {
				if GCClaimHeld(root, "moe", "r1", "design", time.Now()) {
					t.Fatal("absent directories held")
				}
				return
			}
			if err := os.MkdirAll(parent, 0o755); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "absent file":
			case "directory":
				if err := os.Mkdir(path, 0o755); err != nil {
					t.Fatal(err)
				}
			case "dangling symlink", "regular symlink":
				target := filepath.Join(root, "target")
				if kind == "regular symlink" {
					c := Claim{
						Branch: BranchName("moe", "r1", "design"),
						Owner:  fmt.Sprintf("%s/%d", hostnameOrSkip(t), exitedPid(t)),
						PidNS:  repolock.PidNamespace(), HeartbeatAt: time.Now().Add(-2 * StaleAfter),
					}
					if err := writeClaim(target, c); err != nil {
						t.Fatal(err)
					}
					if !claimDead(c, time.Now()) {
						t.Fatal("symlink target must otherwise prove death")
					}
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			case "unreadable", "stat permission":
				if os.Geteuid() == 0 {
					t.Skip("requires non-root permission checks")
				}
				if err := os.WriteFile(path, []byte(`{}`), 0o644); err != nil {
					t.Fatal(err)
				}
				if kind == "unreadable" {
					if err := os.Chmod(path, 0); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = os.Chmod(path, 0o644) })
					if _, err := os.ReadFile(path); !os.IsPermission(err) {
						t.Fatalf("fixture read must fail: %v", err)
					}
				} else {
					if err := os.Chmod(parent, 0); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })
					if _, err := os.Lstat(path); !os.IsPermission(err) {
						t.Fatalf("fixture stat must fail: %v", err)
					}
				}
			case "stat not directory":
				if err := os.Remove(parent); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(parent, nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if got, want := GCClaimHeld(root, "moe", "r1", "design", time.Now()), kind != "absent file"; got != want {
				t.Errorf("GCClaimHeld = %v, want %v", got, want)
			}
		})
	}
}
