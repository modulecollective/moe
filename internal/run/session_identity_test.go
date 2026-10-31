package run

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDocumentBackendSessions(t *testing.T) {
	const legacy = "aaaaaaaa-1111-4aaa-8aaa-aaaaaaaaaaaa"
	var doc Document
	if err := json.Unmarshal([]byte(`{"session":"`+legacy+`"}`), &doc); err != nil {
		t.Fatal(err)
	}
	doc.SetSession("codex", "bbbbbbbb-2222-4bbb-8bbb-bbbbbbbbbbbb")
	doc.SetSession("claude", legacy)
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var reloaded Document
	if err := json.Unmarshal(body, &reloaded); err != nil {
		t.Fatal(err)
	}
	if reloaded.Session != legacy || reloaded.Sessions["claude"] != legacy || reloaded.Sessions["codex"] != doc.Sessions["codex"] {
		t.Fatalf("lost identity: %s", body)
	}
	var fresh Document
	fresh.SetSession("codex", legacy)
	body, err = json.Marshal(fresh)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), `"session":`) || !strings.Contains(string(body), `"sessions":`) {
		t.Fatalf("new metadata must write only keyed IDs: %s", body)
	}
	for _, tc := range []struct {
		doc  Document
		want bool
	}{
		{Document{}, false}, {Document{Session: legacy}, true},
		{Document{Sessions: map[string]string{"claude": ""}}, false},
		{Document{Sessions: map[string]string{"codex": legacy}}, true},
	} {
		if got := tc.doc.HasSession(); got != tc.want {
			t.Errorf("HasSession(%+v)=%v, want %v", tc.doc, got, tc.want)
		}
	}
}

func TestEnsureDocumentDoesNotSelectBackend(t *testing.T) {
	root := t.TempDir()
	md := &Metadata{ID: "r", Project: "p"}
	first, changed, err := EnsureDocument(root, md, "code")
	if err != nil || !changed {
		t.Fatalf("create: %v %v", changed, err)
	}
	if first.HasSession() {
		t.Fatal("directory creation invented conversation identity")
	}
	first.Session = "legacy"
	first.SetSession("claude", "invalid-but-unselected")
	other, _, err := EnsureDocument(root, md, "design")
	if err != nil {
		t.Fatal(err)
	}
	other.SetSession("codex", "another-session")
	same, changed, err := EnsureDocument(root, md, "code")
	if err != nil || changed || same != first || first.Session != "legacy" || first.Sessions["claude"] != "invalid-but-unselected" || other.Sessions["codex"] != "another-session" {
		t.Fatalf("creation changed session metadata: %+v %v %v", same, changed, err)
	}
}
