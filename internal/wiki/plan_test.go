package wiki

import (
	"strings"
	"testing"
)

func TestPlanPromptSectionRefusesOpen(t *testing.T) {
	if _, err := PlanPromptSection(Config{Mode: Open}); err == nil {
		t.Fatal("PlanPromptSection should refuse open-schema")
	}
}

func TestPlanPromptSectionRendersClosed(t *testing.T) {
	got, err := PlanPromptSection(Config{
		Mode:       Closed,
		Name:       "twin",
		ContentDir: "/x/projects/p/digital-twin",
		ManagedDocs: []ManagedDoc{
			{Filename: "vision.md", Title: "Vision", Purpose: "north star"},
			{Filename: "roadmap.md", Title: "Roadmap", Purpose: "what's next"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"## Wiki: twin (closed-schema)",
		"vision.md — Vision",
		"roadmap.md — Roadmap",
		"Plan pass (closed-schema)",
		"Edit roadmap.md only",
		"`moe twin reflect`",
		"`moe twin claim`",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("plan prompt missing %q in:\n%s", want, got)
		}
	}
}
