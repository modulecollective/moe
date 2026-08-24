package serve

import (
	"net/http"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/project"
)

// TestProjectShipRouteSetsAndRendersTheSwitch: the hub's second switch
// writes the same field every unflagged ship reads.
func TestProjectShipRouteSetsAndRendersTheSwitch(t *testing.T) {
	s := modeServer(t)
	if rec := postForm(t, s, "/projects/alpha/ship", "ship=merge"); rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /projects/alpha/ship = %d, want 303: %s", rec.Code, rec.Body.String())
	}
	if got := project.ReadShip(s.opts.Root, "alpha"); got != project.ShipMerge {
		t.Fatalf("ReadShip after the click = %q, want merge", got)
	}
	body := getBody(t, s, "/projects/alpha")
	if !strings.Contains(body, `aria-disabled="true">merge<`) {
		t.Errorf("hub should render merge as the current, inert choice:\n%s", body)
	}
	if !strings.Contains(body, `value="pr"`) {
		t.Errorf("hub missing the pr submit:\n%s", body)
	}
	// And back to pr, which is stored as absent rather than as a word.
	if rec := postForm(t, s, "/projects/alpha/ship", "ship=pr"); rec.Code != http.StatusSeeOther {
		t.Fatalf("POST ship=pr = %d, want 303: %s", rec.Code, rec.Body.String())
	}
	if got := project.ReadShip(s.opts.Root, "alpha"); got != project.ShipPR {
		t.Errorf("ReadShip after pr = %q, want pr", got)
	}
}

// TestProjectShipRouteRejectsAnUnknownRoute: an unrecognised route 400s
// rather than normalizing — the write path is the only thing keeping
// project.json's ship field to two spellings.
func TestProjectShipRouteRejectsAnUnknownRoute(t *testing.T) {
	s := modeServer(t)
	for _, form := range []string{"ship=", "ship=ff", "ship=PR", "ship=rebase"} {
		if rec := postForm(t, s, "/projects/alpha/ship", form); rec.Code != http.StatusBadRequest {
			t.Errorf("POST /projects/alpha/ship %q = %d, want 400", form, rec.Code)
		}
	}
	if got := project.ReadShip(s.opts.Root, "alpha"); got != project.ShipPR {
		t.Errorf("a rejected route must not have been written: %q", got)
	}
}

// TestProjectShipRouteWritesConfig: setting a route writes config and
// spawns nothing — it is a preference the pulse reads later, so it
// answers like the mode switch beside it.
func TestProjectShipRouteWritesConfig(t *testing.T) {
	gittest.SetupEnv(t)
	root := t.TempDir()
	seedRun(t, root, "alpha", "fix-it", "sdlc")
	gittest.InitAt(t, root)
	gittest.Commit(t, root, "seed")
	gittest.Run(t, root, "add", "-A")
	gittest.Commit(t, root, "seed projects")
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})
	if rec := postForm(t, s, "/projects/alpha/ship", "ship=merge"); rec.Code != http.StatusSeeOther {
		t.Fatalf("POST ship = %d, want 303: %s", rec.Code, rec.Body.String())
	}
	if got := project.ReadShip(root, "alpha"); got != project.ShipMerge {
		t.Errorf("ReadShip = %q, want merge", got)
	}
}

// TestProjectsIndexBadgesTheShipRoute: the index is where projects get
// compared, so the route rides every row — pr included.
func TestProjectsIndexBadgesTheShipRoute(t *testing.T) {
	s := modeServer(t)
	if err := project.SetShip(s.opts.Root, "alpha", project.ShipMerge); err != nil {
		t.Fatal(err)
	}
	body := getBody(t, s, "/projects")
	if !strings.Contains(body, `<span class="badge ship">merge</span>`) {
		t.Errorf("projects index missing the ship badge:\n%s", body)
	}
}
