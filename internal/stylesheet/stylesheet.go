// Package stylesheet is the CSS-ish model stylesheet: a checked-in file
// at the bureaucracy root (`model-stylesheet.css`) that declaratively
// binds a `model` and/or `agent` to each (workflow, stage) turn.
//
// The grammar is a deliberate subset of CSS — `selector { prop: val; }`
// rules plus `/* ... */` comments — parsed by this hand-rolled stdlib
// parser rather than a dependency, same spirit as internal/md. The file
// is syntactically valid CSS (so the `.css` extension buys editor
// highlighting and unknown properties are ignored, as a browser would),
// but the semantics are moe's own and diverge in two knowing ways:
//
//   - Selectors are reinterpreted. Two axes only — workflow and stage.
//     A bare identifier (`sdlc`) is a workflow; a leading-dot identifier
//     (`.review`) is a stage in any workflow; `sdlc.review` is exactly
//     one workflow stage; `*` is every turn. (In real CSS `sdlc.review`
//     means element-with-class; here it means workflow-dot-stage.)
//
//   - Specificity is flatter. `*`=0, `sdlc`=1, `.review`=1,
//     `sdlc.review`=2. Highest specificity wins per property; equal
//     specificity is broken by last-rule-in-file (CSS's own tie-break).
//     Real CSS ranks a class above a type selector; here `sdlc` and
//     `.review` tie. Properties cascade independently — the winning
//     `model` rule and the winning `agent` rule need not be the same
//     rule.
//
// Values are bare tokens handed verbatim to the vendor CLI's `--model` /
// resolved through the agent registry; moe keeps no model catalog and
// does no validation. A bad id fails loudly at turn start as the vendor
// CLI's own error, which is the truthful failure mode.
package stylesheet

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// FileName is the stylesheet's fixed name at the bureaucracy root.
const FileName = "model-stylesheet.css"

// Sheet is a parsed stylesheet: an ordered list of rules. The zero value
// (and a nil *Sheet) is a valid empty sheet whose Resolve returns
// ("", "") for everything — the no-rules default.
type Sheet struct {
	rules []rule
}

// rule is one `selector { decls }` block. decls maps property name to
// value; a property repeated within a block keeps the last value (map
// assignment), matching CSS.
type rule struct {
	sel   selector
	decls map[string]string
}

// selector is a parsed selector reduced to its two axes plus a
// specificity rank. An empty workflow (or stage) means that axis is
// unconstrained: `*` leaves both empty, `.review` leaves workflow empty,
// `sdlc` leaves stage empty.
type selector struct {
	workflow string
	stage    string
	spec     int
}

// matches reports whether sel applies to a (workflow, stage) turn. An
// unconstrained axis (empty string) matches anything.
func (sel selector) matches(workflow, stage string) bool {
	if sel.workflow != "" && sel.workflow != workflow {
		return false
	}
	if sel.stage != "" && sel.stage != stage {
		return false
	}
	return true
}

// Load reads and parses <root>/model-stylesheet.css. A missing file is
// not an error — it returns an empty Sheet, which is today's no-rules
// behaviour. A malformed file returns a parse error the caller is
// expected to surface loudly and refuse the turn on, never silently
// ignore.
func Load(root string) (*Sheet, error) {
	path := filepath.Join(root, FileName)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &Sheet{}, nil
		}
		return nil, fmt.Errorf("stylesheet: read %s: %w", path, err)
	}
	s, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("stylesheet %s: %w", path, err)
	}
	return s, nil
}

// Resolve returns the agent and model that apply to a (workflow, stage)
// turn. An empty return for either means no rule set it — the caller
// keeps its own default (the agent ladder's next rung, or the vendor
// CLI's default model). The two properties are resolved independently.
func (s *Sheet) Resolve(workflow, stage string) (agent, model string) {
	return s.property(workflow, stage, "agent"), s.property(workflow, stage, "model")
}

// property returns the winning value for one property across all
// matching rules: highest specificity wins, and equal specificity is
// broken by last-rule-in-file. Rules are stored in file order, so
// iterating forward with `>=` makes a later equal-specificity rule
// override an earlier one while a later lower-specificity rule does not.
func (s *Sheet) property(workflow, stage, prop string) string {
	if s == nil {
		return ""
	}
	bestSpec := -1
	val := ""
	for _, r := range s.rules {
		if !r.sel.matches(workflow, stage) {
			continue
		}
		v, ok := r.decls[prop]
		if !ok {
			continue
		}
		if r.sel.spec >= bestSpec {
			bestSpec = r.sel.spec
			val = v
		}
	}
	return val
}

// Parse parses stylesheet source into a Sheet. Comments are stripped
// first, then the body is read as a sequence of `selector { decls }`
// blocks. Structural errors (unterminated comment or block, malformed
// selector, a declaration missing its colon or value) return an error
// with a 1-based line number. Unknown property names are not errors —
// they are legal CSS a browser would ignore, and Resolve simply never
// reads them.
func Parse(src []byte) (*Sheet, error) {
	s, err := stripComments(string(src))
	if err != nil {
		return nil, err
	}
	var rules []rule
	i := 0
	for {
		for i < len(s) && isSpace(s[i]) {
			i++
		}
		if i >= len(s) {
			break
		}
		open := strings.IndexByte(s[i:], '{')
		if open < 0 {
			return nil, fmt.Errorf("line %d: selector %q has no '{'", lineAt(s, i), strings.TrimSpace(s[i:]))
		}
		selText := strings.TrimSpace(s[i : i+open])
		sel, err := parseSelector(selText, lineAt(s, i))
		if err != nil {
			return nil, err
		}
		rest := s[i+open+1:]
		closeRel := strings.IndexByte(rest, '}')
		if closeRel < 0 {
			return nil, fmt.Errorf("line %d: block for %q has no '}'", lineAt(s, i), selText)
		}
		body := rest[:closeRel]
		if strings.IndexByte(body, '{') >= 0 {
			return nil, fmt.Errorf("line %d: block for %q has a stray '{' (missing '}'?)", lineAt(s, i), selText)
		}
		decls, err := parseDecls(body, lineAt(s, i+open+1))
		if err != nil {
			return nil, err
		}
		rules = append(rules, rule{sel: sel, decls: decls})
		i = i + open + 1 + closeRel + 1
	}
	return &Sheet{rules: rules}, nil
}

// parseSelector reduces one selector token to its two axes and
// specificity. Recognised shapes: `*`, `<workflow>`, `.<stage>`,
// `<workflow>.<stage>`.
func parseSelector(text string, line int) (selector, error) {
	if text == "" {
		return selector{}, fmt.Errorf("line %d: empty selector", line)
	}
	if text == "*" {
		return selector{spec: 0}, nil
	}
	if strings.HasPrefix(text, ".") {
		stage := text[1:]
		if !isIdent(stage) {
			return selector{}, fmt.Errorf("line %d: invalid stage selector %q", line, text)
		}
		return selector{stage: stage, spec: 1}, nil
	}
	if wf, stage, ok := strings.Cut(text, "."); ok {
		if !isIdent(wf) || !isIdent(stage) {
			return selector{}, fmt.Errorf("line %d: invalid selector %q", line, text)
		}
		return selector{workflow: wf, stage: stage, spec: 2}, nil
	}
	if !isIdent(text) {
		return selector{}, fmt.Errorf("line %d: invalid workflow selector %q", line, text)
	}
	return selector{workflow: text, spec: 1}, nil
}

// parseDecls parses the `prop: val; prop: val` body of a block. The
// trailing semicolon is optional (CSS allows it either way). An empty
// property or value, or a declaration with no colon, is a parse error.
func parseDecls(body string, line int) (map[string]string, error) {
	decls := map[string]string{}
	for part := range strings.SplitSeq(body, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		rawProp, rawVal, ok := strings.Cut(part, ":")
		if !ok {
			return nil, fmt.Errorf("line %d: declaration %q missing ':'", line, part)
		}
		prop := strings.TrimSpace(rawProp)
		val := strings.TrimSpace(rawVal)
		if prop == "" {
			return nil, fmt.Errorf("line %d: declaration %q has empty property", line, part)
		}
		if val == "" {
			return nil, fmt.Errorf("line %d: property %q has empty value", line, prop)
		}
		decls[prop] = val
	}
	return decls, nil
}

// stripComments removes `/* ... */` spans, replacing each with a single
// space (so tokens on either side don't fuse) plus one newline per
// newline the comment spanned (so line numbers in later parse errors
// stay accurate). An unterminated comment is a parse error.
func stripComments(s string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); {
		if i+1 < len(s) && s[i] == '/' && s[i+1] == '*' {
			end := strings.Index(s[i+2:], "*/")
			if end < 0 {
				return "", fmt.Errorf("line %d: unterminated comment", lineAt(s, i))
			}
			comment := s[i+2 : i+2+end]
			b.WriteByte(' ')
			b.WriteString(strings.Repeat("\n", strings.Count(comment, "\n")))
			i = i + 2 + end + 2
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String(), nil
}

// lineAt returns the 1-based line number of byte offset i in s.
func lineAt(s string, i int) int {
	if i > len(s) {
		i = len(s)
	}
	return 1 + strings.Count(s[:i], "\n")
}

// isIdent reports whether s is a non-empty run of workflow/stage name
// characters: ASCII letters, digits, underscore, and hyphen.
func isIdent(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// isSpace reports whether c is CSS whitespace.
func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f'
}
