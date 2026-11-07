package run

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/trailers"
)

func TestQuotaRetryResolutionPreservesConcurrentUpdate(t *testing.T) {
	root := newTestRoot(t)
	commit := func(r QuotaRetry) {
		t.Helper()
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		// No run trailer: retry-only journal events still enter the index.
		gittest.Run(t, root, "commit", "--allow-empty", "-m", "quota retry\n\n"+trailers.Block{QuotaRetry: string(b), Consent: "dynamic"}.String())
	}
	read := func() map[string]QuotaRetry {
		t.Helper()
		idx, err := BuildJournalIndex(root)
		if err != nil {
			t.Fatal(err)
		}
		return idx.QuotaRetries
	}
	a := QuotaRetry{Project: "a", Token: "a/pulse", NotBefore: time.Now().UTC(), Targets: []QuotaTarget{{Project: "a", Run: "work", Stage: "code", Backend: "claude", Reason: "spent"}}}
	commit(a)
	observed := a.Fingerprint()
	b := a
	b.Project, b.Token = "b", "b/pulse"
	commit(b)
	a.Targets = append(a.Targets, QuotaTarget{Project: "a", Run: "other", Stage: "test", Backend: "codex", Reason: "spent"})
	commit(a)
	commit(QuotaRetry{Project: "a", Resolves: map[string]string{a.Token: observed}})
	if got := read(); len(got) != 2 || len(got[a.Token].Targets) != 2 {
		t.Fatalf("stale resolution lost new deferral: %+v", got)
	}
	commit(QuotaRetry{Project: "a", Resolves: map[string]string{a.Token: a.Fingerprint()}})
	if got := read(); len(got) != 1 || got[b.Token].Project != "b" {
		t.Fatalf("resolution failed or crossed projects: %+v", got)
	}
	commit(a)
	if got := read(); len(got) != 2 {
		t.Fatalf("newer event hidden by old resolution: %+v", got)
	}
}

func TestQuotaRetryStaleResolutionCannotResurrectResolvedUpdate(t *testing.T) {
	root := newTestRoot(t)
	commit := func(r QuotaRetry) {
		t.Helper()
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		gittest.Run(t, root, "commit", "--allow-empty", "-m", "quota retry\n\n"+trailers.Block{QuotaRetry: string(b), Consent: "dynamic"}.String())
	}
	read := func() map[string]QuotaRetry {
		t.Helper()
		// Rebuild from the journal, as after restart, without the memoized index.
		idx, err := buildJournalIndex(root)
		if err != nil {
			t.Fatal(err)
		}
		return idx.QuotaRetries
	}
	a := QuotaRetry{Project: "a", Token: "a/pulse", NotBefore: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC), Targets: []QuotaTarget{{Project: "a", Run: "work", Stage: "code", Backend: "claude", Reason: "spent"}}}
	commit(a)
	observed := read()[a.Token].Fingerprint()
	a.Targets = append(a.Targets, QuotaTarget{Project: "a", Run: "other", Stage: "test", Backend: "codex", Reason: "spent"})
	commit(a)
	updated := read()[a.Token].Fingerprint()
	if updated != a.Fingerprint() || updated == observed {
		t.Fatal("updated aggregate did not produce a new observed version")
	}
	commit(QuotaRetry{Project: "a", Resolves: map[string]string{a.Token: updated}})
	if got := read(); len(got) != 0 {
		t.Fatalf("updated aggregate was not resolved: %+v", got)
	}
	commit(QuotaRetry{Project: "a", Resolves: map[string]string{a.Token: observed}})
	if got := read(); len(got) != 0 {
		t.Fatalf("stale resolution resurrected discharged retry: %+v", got)
	}
	// A later update is a new obligation even with identical scheduling evidence.
	commit(a)
	if got := read(); len(got) != 1 || got[a.Token].Fingerprint() != updated {
		t.Fatalf("newer event hidden by old resolution: %+v", got)
	}
}

func TestQuotaRetryRejectsMalformedJournalEvidence(t *testing.T) {
	for _, payload := range []string{`{`, `{}`, `{"project":"a","token":"a/pulse"}`, `{"project":"a","resolves":{"b/pulse":"hash"}}`} {
		t.Run(payload, func(t *testing.T) {
			root := newTestRoot(t)
			gittest.Run(t, root, "commit", "--allow-empty", "-m", "bad retry\n\n"+trailers.Block{QuotaRetry: payload}.String())
			if _, err := BuildJournalIndex(root); err == nil {
				t.Fatal("malformed retry silently dropped")
			}
		})
	}
}
