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
			name:    "qualified chained target links as-is",
			project: "alpha",
			note:    "sdlc:code · chained → beta/child-run",
			want:    `sdlc:code · chained → <a href="/run/beta/child-run">beta/child-run</a>`,
		},
		{
			name:    "chained and spawned co-occur on one note",
			project: "moe",
			note:    "sdlc:code · chained → moe/live-child · spawned → pulse-1",
			want:    `sdlc:code · chained → <a href="/run/moe/live-child">moe/live-child</a> · spawned → <a href="/run/moe/pulse-1">pulse-1</a>`,
		},
		{
			name:    "settled-chain target links and the status suffix stays plain",
			project: "moe",
			note:    "sdlc:code · chained after moe/head-run (merged)",
			want:    `sdlc:code · chained after <a href="/run/moe/head-run">moe/head-run</a> (merged)`,
		},
		{
			name:    "settled-chain hint co-occurs with an outgoing chain hint",
			project: "moe",
			note:    "sdlc:code · chained after moe/head-run (merged) · chained → moe/live-child",
			want:    `sdlc:code · chained after <a href="/run/moe/head-run">moe/head-run</a> (merged) · chained → <a href="/run/moe/live-child">moe/live-child</a>`,
		},
		{
			name:    "chained after with no valid target is left alone",
			project: "moe",
			note:    "sdlc:code · chained after (merged)",
			want:    "sdlc:code · chained after (merged)",
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
