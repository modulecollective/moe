package chore

import (
	"strings"
	"testing"
	"time"
)

func TestStateStatusPrecedenceAndLocalTime(t *testing.T) {
	saved := time.Local
	time.Local = time.FixedZone("CHORETEST", 7*3600+1800)
	t.Cleanup(func() { time.Local = saved })
	next := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	st := State{Definition: Definition{When: "landed changes need judgment"}, Due: true, Reasons: []string{"cadence"}, OpenRun: "existing", CooldownBlocking: true, NextEligible: next}
	if got := st.Status(); got != "open run existing" {
		t.Fatal(got)
	}
	st.OpenRun = ""
	if got, want := st.Status(), "cooling down until "+next.Local().Format("2006-01-02 15:04 MST"); got != want {
		t.Fatalf("%s != %s", got, want)
	}
	st.CooldownBlocking = false
	if got := st.Status(); got != "due — cadence" {
		t.Fatal(got)
	}
	st.Due = false
	if got := st.Status(); got != "judged — pulse decides" || strings.Contains(got, "2026") {
		t.Fatal(got)
	}
	st.Definition.When = ""
	if got := st.Status(); got != "not due" {
		t.Fatal(got)
	}
}

func TestDefinitionSummary(t *testing.T) {
	for _, tc := range []struct {
		d    Definition
		want string
	}{
		{Definition{When: "full authored condition"}, "when full authored condition"},
		{Definition{Trigger: "*.go", Cadence: 30 * 24 * time.Hour}, "on *.go · every 30d"},
		{Definition{Cadence: 90 * time.Minute}, "every 1h30m0s"},
	} {
		if got := tc.d.Summary(); got != tc.want {
			t.Fatalf("%q != %q", got, tc.want)
		}
	}
}
