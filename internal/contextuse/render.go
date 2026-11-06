package contextuse

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
)

// RenderHygiene is the default view: who reads what, then what the
// steering offers that nobody opens and whether the twin fits through
// the result cap. Lore read counts come from sessions, so they are only
// bureaucracy-wide when sessions spans every project.
func RenderHygiene(w io.Writer, sessions []Session, inv Inventory, projects []string) {
	rateTable(w, sessions, false)

	loreReads := counts(sessions, func(s Session) []string { return s.Lore })
	fmt.Fprintf(w, "\nLORE  %d entries, %d read by these sessions\n", len(inv.Lore), len(loreReads))
	unreadAndTop(w, inv.Lore, loreReads)

	for _, p := range projects {
		var ps []Session
		for _, s := range sessions {
			if s.Project == p {
				ps = append(ps, s)
			}
		}
		if topics := inv.Knowledge[p]; len(topics) > 0 || inv.IndexLines[p] > 0 {
			reads := counts(ps, func(s Session) []string { return s.Topics })
			own := map[string]int{}
			for k, n := range reads {
				if name, ok := strings.CutPrefix(k, p+"/"); ok {
					own[name] = n
				}
			}
			index := 0
			for _, s := range ps {
				if s.Index {
					index++
				}
			}
			fmt.Fprintf(w, "\nKNOWLEDGE  %s: index %d lines, read in %d of %d sessions; %d topics\n",
				p, inv.IndexLines[p], index, len(ps), len(topics))
			unreadAndTop(w, topics, own)
		}
		if docs := inv.Twin[p]; len(docs) > 0 {
			total := 0
			for _, d := range docs {
				total += d.Tokens()
			}
			fmt.Fprintf(w, "\nTWIN  %s: ~%s tokens across %d docs; codex result cap ~%s\n",
				p, kTokens(total), len(docs), kTokens(CodexResultCapTokens))
			tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
			for _, d := range docs {
				fmt.Fprintf(tw, "  %s\t%d lines\t~%s tokens", d.Name, d.Lines, kTokens(d.Tokens()))
				if d.Tokens() > CodexResultCapTokens {
					fmt.Fprint(tw, "\tover the cap")
				}
				fmt.Fprintln(tw)
			}
			tw.Flush()
		}
	}
}

// RenderByBackend is the adoption table by backend and month, the shape
// the first audit's design canvas reports, with each period's misses.
func RenderByBackend(w io.Writer, sessions []Session) {
	rateTable(w, sessions, true)
	misses := map[string]int{}
	for _, s := range sessions {
		for _, m := range s.Misses {
			misses[s.Agent+"\x00"+m]++
		}
	}
	if len(misses) == 0 {
		return
	}
	type row struct {
		agent, path string
		n           int
	}
	var rows []row
	for k, n := range misses {
		agent, path, _ := strings.Cut(k, "\x00")
		rows = append(rows, row{agent, path, n})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].n != rows[j].n {
			return rows[i].n > rows[j].n
		}
		return rows[i].agent+rows[i].path < rows[j].agent+rows[j].path
	})
	fmt.Fprintf(w, "\nNAMED MISSES  (path as written; most frequent first)\n")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for i, r := range rows {
		if i == 20 {
			fmt.Fprintf(tw, "  …\t%d more\t\n", len(rows)-20)
			break
		}
		fmt.Fprintf(tw, "  %s\t%d\t%s\n", r.agent, r.n, r.path)
	}
	tw.Flush()
}

// RenderSessions writes one JSON object per session, for hand review.
func RenderSessions(w io.Writer, sessions []Session) error {
	enc := json.NewEncoder(w)
	for _, s := range sessions {
		if err := enc.Encode(s); err != nil {
			return err
		}
	}
	return nil
}

// rateTable prints the share of sessions that reached each kind of
// context, per backend with pulse apart, and per month when byPeriod.
// TRUNC is truncated reads over paired reads; MISS is sessions with at
// least one named miss.
func rateTable(w io.Writer, sessions []Session, byPeriod bool) {
	type key struct{ agent, period, kind string }
	groups := map[key][]Session{}
	for _, s := range sessions {
		k := key{agent: s.Agent, kind: "stage"}
		if s.Pulse() {
			k.kind = "pulse"
		}
		if byPeriod {
			k.period = "untimed"
			if !s.When.IsZero() {
				k.period = s.When.Local().Format("2006-01")
			}
		}
		groups[k] = append(groups[k], s)
	}
	keys := make([]key, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.agent != b.agent {
			return a.agent < b.agent
		}
		if a.kind != b.kind {
			return a.kind > b.kind // stage before pulse
		}
		return a.period < b.period
	})

	fmt.Fprintf(w, "SESSIONS  %d transcripts; share reading each kind of context at least once\n", len(sessions))
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	head := "AGENT\tKIND\t"
	if byPeriod {
		head += "PERIOD\t"
	}
	fmt.Fprintln(tw, head+"N\tSKILL\tANY\tINDEX\tTOPIC\tLORE\tTWIN\tINTENT\tPRIOR\tTRUNC\tMISS\t")
	for _, k := range keys {
		ss := groups[k]
		share := func(pred func(Session) bool) string {
			n := 0
			for _, s := range ss {
				if pred(s) {
					n++
				}
			}
			return pct(n, len(ss))
		}
		reads, trunc := 0, 0
		for _, s := range ss {
			reads += s.Reads
			trunc += s.Truncated
		}
		row := k.agent + "\t" + k.kind + "\t"
		if byPeriod {
			row += k.period + "\t"
		}
		fmt.Fprintf(tw, "%s%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t\n", row, len(ss),
			share(func(s Session) bool { return s.Skill }),
			share(Session.DeepRead),
			share(func(s Session) bool { return s.Index }),
			share(func(s Session) bool { return len(s.Topics) > 0 }),
			share(func(s Session) bool { return len(s.Lore) > 0 }),
			share(func(s Session) bool { return len(s.Twin) > 0 }),
			share(func(s Session) bool { return len(s.Intents) > 0 }),
			share(func(s Session) bool { return len(s.PriorRuns) > 0 }),
			pct(trunc, reads),
			share(func(s Session) bool { return len(s.Misses) > 0 }))
	}
	tw.Flush()
}

// counts is how many sessions touched each key.
func counts(sessions []Session, keys func(Session) []string) map[string]int {
	out := map[string]int{}
	for _, s := range sessions {
		for _, k := range keys(s) {
			out[k]++
		}
	}
	return out
}

// unreadAndTop lists what nobody read (the prune candidates) and the
// ten most-read entries. A "*" key — a glob read — is not an entry.
func unreadAndTop(w io.Writer, inventory []string, reads map[string]int) {
	var unread []string
	for _, name := range inventory {
		if reads[name] == 0 && reads[strings.TrimSuffix(name, ".md")] == 0 {
			unread = append(unread, name)
		}
	}
	fmt.Fprintf(w, "  unread (%d):\n", len(unread))
	for _, name := range unread {
		fmt.Fprintf(w, "    %s\n", name)
	}
	type row struct {
		name string
		n    int
	}
	var top []row
	for name, n := range reads {
		if name != "*" {
			top = append(top, row{name, n})
		}
	}
	sort.Slice(top, func(i, j int) bool {
		if top[i].n != top[j].n {
			return top[i].n > top[j].n
		}
		return top[i].name < top[j].name
	})
	if len(top) > 10 {
		top = top[:10]
	}
	if len(top) > 0 {
		fmt.Fprintf(w, "  most read (sessions):\n")
	}
	for _, r := range top {
		fmt.Fprintf(w, "    %5d  %s\n", r.n, r.name)
	}
}

func pct(n, of int) string {
	if of == 0 {
		return "—"
	}
	return fmt.Sprintf("%.1f%%", 100*float64(n)/float64(of))
}

func kTokens(n int) string {
	if n >= 1000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprintf("%d", n)
}
