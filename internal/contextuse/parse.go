package contextuse

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Context categories a mention can fall in.
const (
	catSkill  = "skill"
	catIndex  = "index"
	catTopic  = "topic"
	catLore   = "lore"
	catTwin   = "twin"
	catIntent = "intent"
	catPrior  = "prior"
)

// mention is one context path a call read or searched.
type mention struct {
	cat  string
	key  string // "project/topic.md", lore slug, run slug, …
	path string // as written, for the basename match and the miss list
}

// session-level accumulator: mentions keyed by the call that made them,
// resolved when the call's result arrives.
type parser struct {
	s       *Session
	pending map[string][]mention
	seen    map[string]map[string]bool // cat → keys
}

// parseTranscript fills s from one thread JSONL. Lines that aren't JSON
// (a truncated tail) end the parse with an error after counting what
// came before.
func parseTranscript(s *Session, agent string, r io.Reader) error {
	p := &parser{s: s, pending: map[string][]mention{}, seen: map[string]map[string]bool{}}
	br := bufio.NewReaderSize(r, 1<<20)
	var perr error
	for {
		line, err := br.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var rec record
			if jerr := json.Unmarshal(line, &rec); jerr != nil {
				perr = jerr
				break
			}
			if agent == "codex" {
				p.codex(&rec)
			} else {
				p.claude(&rec)
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			perr = err
			break
		}
	}
	for _, ms := range p.pending {
		for _, m := range ms {
			if m.cat != catSkill {
				s.Unpaired++
			}
		}
	}
	p.finish()
	return perr
}

// record is the union of the two backends' line shapes, only the fields
// this package reads.
type record struct {
	Type    string `json:"type"`
	Message *struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	Payload *struct {
		Type      string          `json:"type"`
		Name      string          `json:"name"`
		CallID    string          `json:"call_id"`
		Input     string          `json:"input"`
		Arguments string          `json:"arguments"`
		Output    json.RawMessage `json:"output"`
	} `json:"payload"`
}

type claudeBlock struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

func (p *parser) claude(rec *record) {
	if rec.Message == nil || (rec.Type != "assistant" && rec.Type != "user") {
		return
	}
	var blocks []claudeBlock
	if json.Unmarshal(rec.Message.Content, &blocks) != nil {
		return // a plain-string prompt
	}
	for _, b := range blocks {
		switch b.Type {
		case "tool_use":
			var in struct {
				Command  string `json:"command"`
				FilePath string `json:"file_path"`
				Path     string `json:"path"`
				Skill    string `json:"skill"`
			}
			_ = json.Unmarshal(b.Input, &in)
			var ms []mention
			switch b.Name {
			case "Skill":
				if strings.HasSuffix(in.Skill, "moe-context") {
					p.s.Skill = true
				}
			case "Bash":
				ms = p.commandMentions(in.Command)
			case "Read":
				ms = p.pathMentions(in.FilePath)
			case "Grep":
				ms = p.pathMentions(in.Path)
			}
			if len(ms) > 0 {
				p.pending[b.ID] = ms
			}
		case "tool_result":
			ms, ok := p.pending[b.ToolUseID]
			if !ok {
				continue
			}
			delete(p.pending, b.ToolUseID)
			text := textOf(b.Content)
			// A Read error names no path; the call only had the one.
			single := b.IsError && len(ms) == 1 && strings.Contains(text, "does not exist")
			p.resolve(ms, text, strings.Contains(text, "<persisted-output>"), single)
		}
	}
}

func (p *parser) codex(rec *record) {
	if rec.Type != "response_item" || rec.Payload == nil {
		return
	}
	pl := rec.Payload
	switch pl.Type {
	case "custom_tool_call":
		var ms []mention
		for _, lit := range jsStringLiterals(pl.Input) {
			ms = append(ms, p.commandMentions(lit)...)
		}
		p.addPending(pl.CallID, ms)
	case "function_call":
		var args struct {
			Cmd     json.RawMessage `json:"cmd"`
			Command json.RawMessage `json:"command"`
		}
		_ = json.Unmarshal([]byte(pl.Arguments), &args)
		cmd := stringOrList(args.Cmd)
		if cmd == "" {
			cmd = stringOrList(args.Command)
		}
		p.addPending(pl.CallID, p.commandMentions(cmd))
	case "custom_tool_call_output", "function_call_output":
		ms, ok := p.pending[pl.CallID]
		if !ok {
			return
		}
		delete(p.pending, pl.CallID)
		text := textOf(pl.Output)
		p.resolve(ms, text, strings.Contains(text, "Warning: truncated output"), false)
	}
}

func (p *parser) addPending(id string, ms []mention) {
	// A skill load counts on the call, result or not: the body arriving
	// is checked separately (the topic's mid-body heading test).
	ms = slices.DeleteFunc(ms, func(m mention) bool {
		if m.cat == catSkill {
			p.s.Skill = true
			return true
		}
		return false
	})
	if len(ms) > 0 {
		p.pending[id] = ms
	}
}

// resolve settles one call's mentions against its result text.
func (p *parser) resolve(ms []mention, text string, truncated, singleMiss bool) {
	missLines := notFoundLines(text)
	for _, m := range ms {
		if m.cat == catSkill {
			p.s.Skill = true
			continue
		}
		if singleMiss || namesPath(missLines, m.path) {
			p.s.Misses = append(p.s.Misses, m.path)
			continue
		}
		p.s.Reads++
		if truncated {
			p.s.Truncated++
		}
		if p.seen[m.cat] == nil {
			p.seen[m.cat] = map[string]bool{}
		}
		p.seen[m.cat][m.key] = true
	}
}

func (p *parser) finish() {
	keys := func(cat string) []string {
		var out []string
		for k := range p.seen[cat] {
			out = append(out, k)
		}
		slices.Sort(out)
		return out
	}
	p.s.Index = len(p.seen[catIndex]) > 0
	p.s.Topics = keys(catTopic)
	p.s.Lore = keys(catLore)
	p.s.Twin = keys(catTwin)
	p.s.Intents = keys(catIntent)
	p.s.PriorRuns = keys(catPrior)
	slices.Sort(p.s.Misses)
	p.s.Misses = slices.Compact(p.s.Misses)
}

var notFoundPhrases = []string{"No such file or directory", "does not exist", "cannot access"}

func notFoundLines(text string) []string {
	var out []string
	for line := range strings.SplitSeq(text, "\n") {
		for _, ph := range notFoundPhrases {
			if strings.Contains(line, ph) {
				out = append(out, line)
				break
			}
		}
	}
	return out
}

// namesPath reports whether an error line names path as written. The
// knowledge topic's rule is the basename, because a bare non-zero exit
// in a multi-command script is usually rg finding nothing; but a Codex
// call often reads one doc by two paths, and a basename match would pin
// the relative path's miss on the absolute read beside it. So the whole
// path must appear, less any leading `${var}` (the error line shows it
// expanded). A path still carrying a glob or variable can't be matched
// and is never counted as a miss.
func namesPath(lines []string, path string) bool {
	if i := strings.LastIndex(path, "}"); i >= 0 {
		path = path[i+1:]
	}
	if strings.ContainsAny(path, "$*?{") || strings.Trim(path, "/.") == "" {
		return false
	}
	for _, l := range lines {
		if strings.Contains(l, path) {
			return true
		}
	}
	return false
}

// Verb classes. A segment counts when its first word reads or searches.
var readVerbs = map[string]bool{
	"cat": true, "sed": true, "head": true, "tail": true, "awk": true, "cut": true,
	"jq": true, "less": true, "more": true, "nl": true, "bat": true, "tac": true,
	"moe": true, "grep": true, "rg": true, "egrep": true, "fgrep": true,
}

var gitReads = map[string]bool{"show": true, "log": true, "grep": true, "cat-file": true, "blame": true}

// skipWords lead a segment without being its verb.
var skipWords = map[string]bool{
	"do": true, "then": true, "else": true, "time": true, "sudo": true, "command": true,
	"!": true, "(": true, "{": true, "exec": true, "nice": true,
}

var segmentSplit = regexp.MustCompile(`&&|\|\||[;|\n]`)

// commandMentions finds the context paths a shell command reads.
func (p *parser) commandMentions(cmd string) []mention {
	if cmd == "" {
		return nil
	}
	segs := segmentSplit.Split(cmd, -1)
	anyRead := false
	for _, seg := range segs {
		if v := segmentVerb(seg); readVerbs[v] || v == "git" && gitReadSegment(seg) {
			anyRead = true
			break
		}
	}
	var out []mention
	for _, seg := range segs {
		v := segmentVerb(seg)
		switch {
		case readVerbs[v], v == "git" && gitReadSegment(seg):
		case (v == "for" || v == "while") && anyRead:
		default:
			continue
		}
		if v == "moe" {
			out = append(out, p.moeMentions(seg)...)
		}
		for _, tok := range pathTokens.FindAllString(seg, -1) {
			out = append(out, p.pathMentions(tok)...)
		}
	}
	return out
}

func segmentVerb(seg string) string {
	for f := range strings.FieldsSeq(seg) {
		f = strings.TrimLeft(f, "({")
		if f == "" || skipWords[f] || strings.Contains(f, "=") && !strings.Contains(f, "/") {
			continue
		}
		return filepath.Base(f)
	}
	return ""
}

func gitReadSegment(seg string) bool {
	for f := range strings.FieldsSeq(seg) {
		if gitReads[f] {
			return true
		}
	}
	return false
}

// moeMentions catches the CLI read routes that carry no path: `moe
// intent cat <p>/<slug>` and `moe <workflow> log <p>/<slug>`.
func (p *parser) moeMentions(seg string) []mention {
	f := strings.Fields(seg)
	for i := 0; i+3 < len(f); i++ {
		if filepath.Base(f[i]) != "moe" {
			continue
		}
		_, slug, ok := strings.Cut(f[i+3], "/")
		switch {
		case !ok:
		case f[i+1] == "intent" && f[i+2] == "cat":
			return []mention{{cat: catIntent, key: slug, path: f[i+3]}}
		case f[i+2] == "log" && slug != p.s.Run:
			return []mention{{cat: catPrior, key: slug, path: f[i+3]}}
		}
	}
	return nil
}

var (
	pathTokens = regexp.MustCompile("[^\\s'\"`;|&()<>,]+")
	reProject  = regexp.MustCompile(`projects/([^/]+)/(?:digital-twin|knowledge)`)
	reTwin     = regexp.MustCompile(`digital-twin(?:/([^/]*))?$`)
	reIndex    = regexp.MustCompile(`knowledge/index\.md$`)
	reTopic    = regexp.MustCompile(`knowledge/topics/([^/]*)$`)
	reLore     = regexp.MustCompile(`(?:^|/)lore/([A-Za-z0-9._-]+)\.md$`)
	reRun      = regexp.MustCompile(`runs/([A-Za-z0-9][A-Za-z0-9._-]*)/(documents/intent/)?`)
)

// pathMentions classifies one path-ish token. A twin doc or topic that
// came through a glob, a brace or a shell variable keys as "*".
func (p *parser) pathMentions(tok string) []mention {
	if tok == "" {
		return nil
	}
	if strings.HasSuffix(tok, "skills/moe-context/SKILL.md") {
		return []mention{{cat: catSkill, path: tok}}
	}
	project := p.s.Project
	if m := reProject.FindStringSubmatch(tok); m != nil {
		project = m[1]
	}
	clean := strings.TrimSuffix(tok, "/")
	if m := reTwin.FindStringSubmatch(clean); m != nil {
		return []mention{{cat: catTwin, key: project + "/" + concrete(m[1]), path: tok}}
	}
	if reIndex.MatchString(clean) {
		return []mention{{cat: catIndex, key: project, path: tok}}
	}
	if m := reTopic.FindStringSubmatch(clean); m != nil {
		return []mention{{cat: catTopic, key: project + "/" + concrete(m[1]), path: tok}}
	}
	if m := reLore.FindStringSubmatch(clean); m != nil {
		return []mention{{cat: catLore, key: m[1], path: tok}}
	}
	if m := reRun.FindStringSubmatch(clean); m != nil {
		switch {
		case m[2] != "":
			return []mention{{cat: catIntent, key: m[1], path: tok}}
		case m[1] != p.s.Run:
			return []mention{{cat: catPrior, key: m[1], path: tok}}
		}
	}
	return nil
}

// concrete keeps a real doc name and folds anything a shell would still
// expand (or nothing at all) to "*".
func concrete(name string) string {
	if name == "" || strings.ContainsAny(name, "*?{$[") || !strings.HasSuffix(name, ".md") {
		return "*"
	}
	return name
}

// jsStringLiterals returns every string literal in a Codex exec script
// — double, single or backtick quoted — with JSON-style escapes undone.
// Commands live in literals whether they are a `cmd:` value or an array
// element mapped over later, so every literal is a candidate command.
func jsStringLiterals(src string) []string {
	var out []string
	for i := 0; i < len(src); i++ {
		q := src[i]
		if q != '"' && q != '\'' && q != '`' {
			continue
		}
		var b strings.Builder
		j := i + 1
		for ; j < len(src) && src[j] != q; j++ {
			if src[j] == '\\' && j+1 < len(src) {
				j++
				switch src[j] {
				case 'n':
					b.WriteByte('\n')
				case 't':
					b.WriteByte('\t')
				default:
					b.WriteByte(src[j])
				}
				continue
			}
			b.WriteByte(src[j])
		}
		out = append(out, b.String())
		i = j
	}
	return out
}

// textOf flattens a tool result: a string, or an array of text blocks.
func textOf(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var b strings.Builder
	for _, bl := range blocks {
		b.WriteString(bl.Text)
		b.WriteByte('\n')
	}
	return b.String()
}

func stringOrList(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var l []string
	if json.Unmarshal(raw, &l) == nil {
		return strings.Join(l, " ")
	}
	return ""
}
