package serve

import "testing"

func TestNoteHTML(t *testing.T) {
	tests := []struct {
		name    string
		project string
		note    string
		want    string
	}{
		{
			name:    "bare spawned target qualifies with row project",
			project: "moe",
			note:    "sdlc:merged · spawned → pulse-2026-07-17-5",
			want:    `sdlc:merged · spawned → <a href="/run/moe/pulse-2026-07-17-5">pulse-2026-07-17-5</a>`,
		},
		{
			name:    "qualified promoted target links as-is",
			project: "moe",
			note:    "idea:promoted → moe/pulse-stage-2026-07-17",
			want:    `idea:promoted → <a href="/run/moe/pulse-stage-2026-07-17">moe/pulse-stage-2026-07-17</a>`,
		},
		{
			name:    "free-text note without a hint passes through unlinked",
			project: "moe",
			note:    "pull: rebalance the active set",
			want:    "pull: rebalance the active set",
		},
		{
			name:    "surrounding free text is HTML-escaped",
			project: "moe",
			note:    "pick <b>me</b> & spawned → child-run",
			want:    `pick &lt;b&gt;me&lt;/b&gt; &amp; spawned → <a href="/run/moe/child-run">child-run</a>`,
		},
		{
			name:    "verb without an arrow target is left alone",
			project: "moe",
			note:    "idea:promoted",
			want:    "idea:promoted",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := string(noteHTML(tt.project, tt.note)); got != tt.want {
				t.Errorf("noteHTML(%q, %q)\n  got:  %s\n  want: %s", tt.project, tt.note, got, tt.want)
			}
		})
	}
}
