package chore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/repolock"
)

// Spec is chore.json plus prompt.md as the operator wrote them. The
// scalars stay wire strings so a write lands what was typed: a
// Definition holds parsed durations and would write `90d` back as
// `2160h0m0s`.
type Spec struct {
	Trigger, Workflow, Cadence, Cooldown, When string
	Prompt                                     string
}

var (
	// ErrNotFound: no chore by that project/name.
	ErrNotFound = errors.New("no such chore")
	// ErrExists: Create on a name already registered.
	ErrExists = errors.New("chore already exists")
	// ErrChanged: UpdateChecked found a definition other than the one
	// the editor started from.
	ErrChanged = errors.New("the chore changed since it was opened")
)

// InvalidError is a definition refused before anything was written.
type InvalidError struct{ Err error }

func (e *InvalidError) Error() string { return e.Err.Error() }
func (e *InvalidError) Unwrap() error { return e.Err }

func invalid(format string, args ...any) error {
	return &InvalidError{Err: fmt.Errorf(format, args...)}
}

// namePattern is the run slug pattern: a chore's name becomes the
// IDBase of the runs it opens.
var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// Dir is the chore's directory relative to the bureaucracy root, in
// slash form (the shape git log and git add take).
func Dir(projectID, name string) string {
	return filepath.ToSlash(filepath.Join(project.Dir(projectID), "chores", name))
}

// choreJSON is the on-disk wire shape of chore.json. Field order is
// the file's key order.
type choreJSON struct {
	Trigger  string `json:"trigger,omitempty"`
	Workflow string `json:"workflow,omitempty"`
	Cadence  string `json:"cadence,omitempty"`
	Cooldown string `json:"cooldown,omitempty"`
	When     string `json:"when,omitempty"`
}

// LoadSpec reads a chore's definition as written. A missing chore.json
// is ErrNotFound.
func LoadSpec(root, projectID, name string) (Spec, error) {
	dir := filepath.Join(root, Dir(projectID, name))
	b, err := os.ReadFile(filepath.Join(dir, "chore.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Spec{}, fmt.Errorf("chore %s/%s: %w (read chore.json: %v)", projectID, name, ErrNotFound, err)
		}
		return Spec{}, fmt.Errorf("chore %s/%s: read chore.json: %w", projectID, name, err)
	}
	var cj choreJSON
	if err := json.Unmarshal(b, &cj); err != nil {
		return Spec{}, fmt.Errorf("chore %s/%s: parse chore.json: %w", projectID, name, err)
	}
	s := Spec{Trigger: cj.Trigger, Workflow: cj.Workflow, Cadence: cj.Cadence, Cooldown: cj.Cooldown, When: cj.When}
	if b, err := os.ReadFile(filepath.Join(dir, "prompt.md")); err == nil {
		s.Prompt = string(b)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Spec{}, fmt.Errorf("chore: read prompt.md: %w", err)
	}
	return s, nil
}

// parse turns the wire strings into a Definition, without EditedAt and
// before Validate. It is the one step loadOne and the writer share.
func (s Spec) parse(projectID, name string) (Definition, error) {
	d := Definition{
		Project: projectID, Name: name, Workflow: DefaultWorkflow,
		Trigger: s.Trigger, When: strings.TrimSpace(s.When), Prompt: s.Prompt,
	}
	if s.Workflow != "" {
		d.Workflow = s.Workflow
	}
	var err error
	if s.Cooldown != "" {
		if d.Cooldown, err = ParseDuration(s.Cooldown); err != nil {
			return d, fmt.Errorf("chore %s/%s cooldown: %w", projectID, name, err)
		}
	}
	if s.Cadence != "" {
		if d.Cadence, err = ParseDuration(s.Cadence); err != nil {
			return d, fmt.Errorf("chore %s/%s cadence: %w", projectID, name, err)
		}
	}
	return d, nil
}

// normalized is the spec as the writer stores it: scalars trimmed, the
// default workflow left implicit (as every hand-written chore.json has
// it), and a blank prompt dropped (open falls back to `# <name>`).
func (s Spec) normalized() Spec {
	for _, f := range []*string{&s.Trigger, &s.Workflow, &s.Cadence, &s.Cooldown, &s.When} {
		*f = strings.TrimSpace(*f)
	}
	if s.Workflow == DefaultWorkflow {
		s.Workflow = ""
	}
	if strings.TrimSpace(s.Prompt) == "" {
		s.Prompt = ""
	}
	return s
}

// Check parses and validates s as project/name's definition without
// writing anything. Every refusal is an *InvalidError.
func (s Spec) Check(projectID, name string) error {
	if !namePattern.MatchString(name) {
		return invalid("chore name %q must match %s", name, namePattern)
	}
	s = s.normalized()
	for k, v := range map[string]string{"trigger": s.Trigger, "workflow": s.Workflow, "cadence": s.Cadence, "cooldown": s.Cooldown, "when": s.When} {
		if strings.ContainsAny(v, "\r\n") {
			return invalid("chore %s/%s: %s must be one line", projectID, name, k)
		}
	}
	d, err := s.parse(projectID, name)
	if err != nil {
		return &InvalidError{Err: err}
	}
	if err := Validate(d); err != nil {
		return &InvalidError{Err: err}
	}
	return nil
}

// textKeys is the text form's header, in the order Text writes it.
var textKeys = []string{"trigger", "cadence", "cooldown", "when", "workflow"}

func (s *Spec) field(key string) *string {
	switch key {
	case "trigger":
		return &s.Trigger
	case "cadence":
		return &s.Cadence
	case "cooldown":
		return &s.Cooldown
	case "when":
		return &s.When
	case "workflow":
		return &s.Workflow
	}
	return nil
}

// Text is the chore as one editable document — the header keys, a
// `---` line, then the prompt verbatim — for $EDITOR and the moe ui
// editor. Every key is written, blank when unset, so the form shows
// what can be set. It is an editor-buffer format only; on disk the
// chore stays chore.json + prompt.md.
func (s Spec) Text() string {
	var b strings.Builder
	for _, k := range textKeys {
		if v := *s.field(k); v != "" {
			b.WriteString(k + ": " + v + "\n")
		} else {
			b.WriteString(k + ":\n")
		}
	}
	b.WriteString("---\n")
	b.WriteString(s.Prompt)
	return b.String()
}

// ParseSpecText reads the text form back. The header ends at the first
// line that is exactly `---`; above it every non-blank line is
// `key: value` with a known key, each at most once. Everything below
// is the prompt, so a `---` inside the prompt is safe.
func ParseSpecText(text string) (Spec, error) {
	var s Spec
	seen := map[string]bool{}
	rest := text
	for n := 1; ; n++ {
		line, after, ok := strings.Cut(rest, "\n")
		if !ok && line == "" {
			return Spec{}, invalid("no `---` line separates the header from the prompt")
		}
		line = strings.TrimSuffix(line, "\r")
		rest = after
		if line == "---" {
			if ok {
				s.Prompt = rest
			}
			return s, nil
		}
		if !ok {
			return Spec{}, invalid("no `---` line separates the header from the prompt")
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		key, value, found := strings.Cut(line, ":")
		key = strings.TrimSpace(key)
		f := s.field(key)
		if !found || f == nil {
			return Spec{}, invalid("line %d: want `key: value` with a key of %s, got %q", n, strings.Join(textKeys, ", "), line)
		}
		if seen[key] {
			return Spec{}, invalid("line %d: %s is set twice", n, key)
		}
		seen[key] = true
		*f = strings.TrimSpace(value)
	}
}

// Create registers project/name. It refuses a name already registered.
func Create(root, projectID, name string, s Spec) error {
	return write(root, projectID, name, nil, s, true)
}

// Update replaces an existing chore's definition. It refuses a missing
// name, and returns nil without committing when nothing changed.
func Update(root, projectID, name string, s Spec) error {
	return write(root, projectID, name, nil, s, false)
}

// UpdateChecked is Update for an editor held open a while (moe ui): it
// writes only if the definition on disk still equals expected, the one
// the editor started from, and otherwise returns ErrChanged.
func UpdateChecked(root, projectID, name string, expected, s Spec) error {
	return write(root, projectID, name, &expected, s, false)
}

// Remove deletes the chore's directory in one commit. Its history —
// completions, skips — stays in the journal under its key.
func Remove(root, projectID, name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("chore %s/%s: %w", projectID, name, ErrNotFound)
	}
	rel := Dir(projectID, name)
	key := projectID + "/" + name
	return repolock.With(root, repolock.Options{Purpose: "chore-remove", Run: key}, func() error {
		if _, err := LoadSpec(root, projectID, name); err != nil {
			return err
		}
		snap, err := snapDir(root, rel)
		if err != nil {
			return err
		}
		if err := os.RemoveAll(filepath.Join(root, rel)); err != nil {
			return snap.restore(root, fmt.Errorf("remove %s: %w", rel, err))
		}
		return commitDir(root, rel, "chore: remove "+key, snap)
	})
}

func write(root, projectID, name string, expected *Spec, s Spec, create bool) error {
	if err := s.Check(projectID, name); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(root, project.Dir(projectID), "project.json")); err != nil {
		return invalid("project %s not registered: %v", projectID, err)
	}
	s = s.normalized()
	rel := Dir(projectID, name)
	key := projectID + "/" + name
	purpose, verb := "chore-edit", "edit"
	if create {
		purpose, verb = "chore-add", "add"
	}
	return repolock.With(root, repolock.Options{Purpose: purpose, Run: key}, func() error {
		cur, err := LoadSpec(root, projectID, name)
		switch {
		case create && err == nil:
			return fmt.Errorf("chore %s: %w", key, ErrExists)
		case create && errors.Is(err, ErrNotFound):
			// Not there yet: the case Create wants. A directory left
			// without a chore.json is overwritten below.
		case err != nil:
			return err
		}
		if !create {
			if expected != nil && cur.normalized() != expected.normalized() {
				return fmt.Errorf("chore %s: %w", key, ErrChanged)
			}
			// The copy that keeps an unchanged save from committing. An
			// empty-diff commit touching the directory would still read
			// as "definition changed" and make a mechanical chore due.
			if cur.normalized() == s {
				return nil
			}
		}
		snap, err := snapDir(root, rel)
		if err != nil {
			return err
		}
		if err := writeFiles(root, rel, s); err != nil {
			return snap.restore(root, err)
		}
		return commitDir(root, rel, "chore: "+verb+" "+key, snap)
	})
}

func writeFiles(root, rel string, s Spec) error {
	dir := filepath.Join(root, rel)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("chore: mkdir %s: %w", rel, err)
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	// A `when` is prose: keep its & and < literal, as a hand-written
	// file would.
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(choreJSON{Trigger: s.Trigger, Workflow: s.Workflow, Cadence: s.Cadence, Cooldown: s.Cooldown, When: s.When}); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "chore.json"), b.Bytes(), 0o644); err != nil {
		return fmt.Errorf("chore: write chore.json: %w", err)
	}
	prompt := filepath.Join(dir, "prompt.md")
	if s.Prompt == "" {
		if err := os.Remove(prompt); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("chore: remove prompt.md: %w", err)
		}
		return nil
	}
	if err := os.WriteFile(prompt, []byte(s.Prompt), 0o644); err != nil {
		return fmt.Errorf("chore: write prompt.md: %w", err)
	}
	return nil
}

// commitDir stages rel and commits only rel, so anything else the
// operator has staged stays out of the chore's commit. A failed commit
// puts the directory back as snapped.
func commitDir(root, rel, msg string, snap dirSnap) error {
	if err := git.Run(root, "add", "-A", "--", rel); err != nil {
		return snap.restore(root, err)
	}
	if git.Probe(root, "diff", "--cached", "--quiet", "--", rel) {
		// The tree already matches HEAD — an uncommitted hand edit put
		// back. Nothing to record.
		return nil
	}
	if err := git.Run(root, "commit", "-m", msg, "--", rel); err != nil {
		return snap.restore(root, err)
	}
	return nil
}

// dirSnap is a chore directory as a writer found it: each file's bytes
// and index entry, plus the two files the writer may create.
type dirSnap struct {
	rel   string
	files []fileSnap
}

type fileSnap struct {
	rel     string
	bytes   []byte
	existed bool
	index   string // `git ls-files -s` line; "" when not in the index
}

func snapDir(root, rel string) (dirSnap, error) {
	snap := dirSnap{rel: rel}
	paths := map[string]bool{rel + "/chore.json": true, rel + "/prompt.md": true}
	err := filepath.WalkDir(filepath.Join(root, rel), func(p string, d os.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if !d.IsDir() {
			r, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			paths[filepath.ToSlash(r)] = true
		}
		return nil
	})
	if err != nil {
		return snap, fmt.Errorf("chore: read %s: %w", rel, err)
	}
	for p := range paths {
		f := fileSnap{rel: p}
		b, err := os.ReadFile(filepath.Join(root, p))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return snap, fmt.Errorf("chore: read %s: %w", p, err)
		}
		f.bytes, f.existed = b, err == nil
		if f.index, err = git.Output(root, "ls-files", "-s", "--", p); err != nil {
			return snap, fmt.Errorf("chore: read index entry for %s: %w", p, err)
		}
		snap.files = append(snap.files, f)
	}
	return snap, nil
}

// restore undoes a writer's change after a failed write or commit —
// bytes and index entries, as snapped — and says nothing was recorded.
func (s dirSnap) restore(root string, cause error) error {
	var errs []error
	for _, f := range s.files {
		if fl := strings.Fields(f.index); len(fl) >= 2 {
			errs = append(errs, git.Run(root, "update-index", "--add", "--cacheinfo", fl[0]+","+fl[1]+","+f.rel))
		} else {
			errs = append(errs, git.Run(root, "update-index", "--force-remove", "--", f.rel))
		}
		abs := filepath.Join(root, f.rel)
		if f.existed {
			errs = append(errs, os.MkdirAll(filepath.Dir(abs), 0o755), os.WriteFile(abs, f.bytes, 0o644))
		} else if err := os.Remove(abs); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	// A create that failed leaves no empty directory behind for LoadAll
	// to trip on. Remove refuses a non-empty one, which is the point.
	_ = os.Remove(filepath.Join(root, s.rel))
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("chore: commit failed (%w); restoring %s also failed: %v", cause, s.rel, err)
	}
	return fmt.Errorf("chore: commit failed, nothing recorded: %w", cause)
}
