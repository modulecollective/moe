package ui

import (
	"fmt"
	"slices"
	"strings"
)

type pane int

const (
	paneBoard pane = iota
	paneDetail
	paneConsole
)

type mode int

const (
	modeNormal  mode = iota
	modeEdit         // a draft is open in the editor
	modeCommand      // the : line
	modeConfirm      // y/N on a pending action
	modeSave         // save / save+tag choice for an idea draft
	modeChoice       // pick a Rex client
	modeHelp
	modeSearch
)

// itemKey identifies one board occurrence.
type itemKey struct {
	project, run string
	bucket       int
}

func keyOf(it Item) itemKey { return itemKey{it.Project, it.Run, it.Bucket} }

// draftKey is what a draft is for: one target and one purpose. An answer
// is also bound to the question it answers.
type draftKey struct {
	project, run string
	purpose      string // note, answer, body, new-idea, new-intent
	question     int
}

type draft struct {
	key  draftKey
	ed   *editor
	base string // body: what the editor opened; new-*: the seed
	// uncertain holds the evidence of a save whose outcome is unknown;
	// the next submit must be confirmed.
	uncertain []string
	// refused is why the last save of this draft was turned down, kept
	// with the draft until it is resubmitted.
	refused   []string
	confirmed bool
	// seen is its target as the board last showed it, so the draft stays
	// reachable (see withKept) after the target leaves the board.
	seen *Item
}

func (d *draft) dirty() bool { return d.ed.String() != d.base }

// job is one mutation sequence on the worker: a save, then optionally a
// tag of the same idea, which runs only if the save succeeded.
type job struct {
	label  string
	steps  []Action
	draft  *draftKey // cleared only when the save succeeds
	target itemKey
}

type jobResult struct {
	job      job
	results  []Result // one per step run
	progress bool     // a successful save, before its tag completes
}

// effect is what a key asks of the loop beyond redrawing.
type effect int

const (
	effNone effect = iota
	effQuit
	effSuspend
)

// model is the console's whole state. Only the loop goroutine touches it.
type model struct {
	raw   Board // as the caller sent it
	board Board // raw plus kept-draft targets that left it
	sel   itemKey
	has   bool // sel names an item
	focus pane
	mode  mode

	animation int // index into the board's cached factory frames
	boardTop  int // first board line in view
	detailTop int

	drafts map[draftKey]*draft
	active *draft

	cmd         *editor
	search      *boardSearch
	cmdTarget   *Item // captured when : opened
	completion  *commandCompletion
	confirm     *pendingConfirm
	choices     []Choice
	choiceFor   Action
	saveDraft   *draft
	progress    int // already reported steps of the in-flight job
	busy        *job
	queue       chan<- job
	console     console
	observed    map[string]bool
	lastResult  map[string][]string // by project/run
	retry       map[string]Action   // uncertain control requests, by target+verb
	pendingSel  *itemKey            // select this item once it appears
	width, high int
	color       bool // cliout.Enabled on the actual output writer
}

type pendingConfirm struct {
	prompt string
	yes    func(m *model)
	top    int
}

func newModel(queue chan<- job) *model {
	return &model{
		drafts:     map[draftKey]*draft{},
		lastResult: map[string][]string{},
		retry:      map[string]Action{},
		queue:      queue,
		console:    console{follow: true},
		observed:   map[string]bool{},
	}
}

func (m *model) selected() (Item, bool) {
	if !m.has {
		return Item{}, false
	}
	for _, it := range m.board.Items {
		if keyOf(it) == m.sel {
			return it, true
		}
	}
	return Item{}, false
}

func (m *model) selectedIndex() int {
	for i, it := range m.board.Items {
		if keyOf(it) == m.sel {
			return i
		}
	}
	return -1
}

// setBoard replaces the board, keeping the selection on the same
// occurrence, else the same run in its new bucket, else the nearest
// surviving item — and saying so. Open editors, commands and prompts
// keep their captured targets.
func (m *model) setBoard(b Board) {
	if b.Seq != 0 && b.Seq < m.raw.Seq {
		return
	}
	issues := map[string]bool{}
	for _, issue := range b.Issues {
		issues[issue] = true
		if !m.observed[issue] {
			m.say(issue)
		}
	}
	m.observed = issues
	m.raw = b
	m.applyBoard()
}

// keptBucket marks an item withKept added: no dash bucket is negative.
const keptBucket = -1

const keptHeading = "KEPT DRAFTS"

// withKept is b plus one read-only item for each target that left the
// board while a draft for it is kept, for example when a run falls out
// of the filter. The item keeps the target's identity and what the board
// last showed of it, so the operator can still read the text, reopen it
// with the key that opened it, and discard it. It offers no action:
// a save goes to the writer, which refuses it, and the text is never
// retargeted. Drafts whose
// target is on b record it as last seen.
func (m *model) withKept(b Board) Board {
	present := map[string]Item{}
	for _, it := range b.Items {
		if _, ok := present[it.Label()]; !ok {
			present[it.Label()] = it
		}
	}
	var gone []Item
	listed := map[string]bool{}
	for _, d := range m.drafts {
		l := d.key.project + "/" + d.key.run
		if it, ok := present[l]; ok {
			d.seen = &it
			continue
		}
		if d.seen == nil || listed[l] {
			continue
		}
		listed[l] = true
		last := *d.seen
		gone = append(gone, Item{
			Project: last.Project, Run: last.Run, Bucket: keptBucket, Kind: last.Kind, Body: last.Body,
			Detail: append([]string{
				l + " is no longer on the board. Its kept draft stays listed here until it is saved or discarded.",
				"", "as the board last showed it:",
			}, last.Detail...),
		})
	}
	if len(gone) == 0 {
		return b
	}
	slices.SortFunc(gone, func(x, y Item) int { return strings.Compare(x.Label(), y.Label()) })
	b.Lines = append(slices.Clone(b.Lines), "", keptHeading)
	b.Items = slices.Clone(b.Items)
	for _, it := range gone {
		it.Line = len(b.Lines)
		b.Lines = append(b.Lines, "  "+SafeText(it.Label()))
		b.Items = append(b.Items, it)
	}
	return b
}

// dropDraft forgets a draft, and its kept-target row if that was the
// last draft holding it.
func (m *model) dropDraft(k draftKey) {
	delete(m.drafts, k)
	m.applyBoard()
}

// applyBoard rebuilds the shown board from the raw one and the drafts,
// then settles the selection on it.
func (m *model) applyBoard() {
	// Reconcile search last: a pending save may select a nonmatching item.
	if m.mode == modeSearch {
		previous, had := m.sel, m.has
		top := m.detailTop
		defer func() {
			m.detailTop = top
			m.reconcileSearch(previous, had)
		}()
	}
	b := m.withKept(m.raw)
	prevIdx := m.selectedIndex()
	prevSel, _ := m.selected()
	prevHeight := m.boardHeight()
	old := m.board
	m.board = b
	m.clampBoard()
	if m.pendingSel != nil {
		for _, it := range b.Items {
			if it.Project == m.pendingSel.project && it.Run == m.pendingSel.run {
				m.sel, m.has, m.pendingSel = keyOf(it), true, nil
				m.scrollToSelection()
				return
			}
		}
	}
	if len(b.Items) == 0 {
		m.has = false
		return
	}
	if !m.has {
		m.sel, m.has = keyOf(b.Items[0]), true
		m.scrollToSelection()
		return
	}
	if m.selectedIndex() >= 0 {
		it, _ := m.selected()
		if it.Line != prevSel.Line || m.boardHeight() != prevHeight {
			m.scrollToSelection()
		}
		return
	}
	for _, it := range b.Items {
		if it.Project == m.sel.project && it.Run == m.sel.run {
			m.sel = keyOf(it)
			if it.Bucket == keptBucket {
				m.say(it.Label() + " left the board; its kept draft is listed under " + keptHeading)
			}
			m.scrollToSelection()
			return
		}
	}
	gone := m.sel.project + "/" + m.sel.run
	i := min(max(prevIdx, 0), len(b.Items)-1)
	if prevIdx < 0 && len(old.Items) > 0 {
		i = 0
	}
	m.sel = keyOf(b.Items[i])
	m.detailTop = 0
	if m.mode != modeSearch {
		m.say(gone + " left the board; selected " + b.Items[i].Label())
	}
	m.scrollToSelection()
}

func (m *model) move(delta int) {
	if len(m.board.Items) == 0 {
		return
	}
	i := m.selectedIndex()
	if i < 0 {
		i = 0
	} else {
		i = min(max(i+delta, 0), len(m.board.Items)-1)
	}
	if k := keyOf(m.board.Items[i]); k != m.sel {
		m.sel, m.has, m.detailTop = k, true, 0
	}
	if i == 0 {
		m.boardTop = 0
	} else {
		m.scrollToSelection()
	}
}

func (m *model) scrollToSelection() {
	it, ok := m.selected()
	if !ok {
		return
	}
	h := m.boardHeight()
	if it.Line < m.boardTop {
		m.boardTop = it.Line
	}
	if it.Line >= m.boardTop+h {
		m.boardTop = it.Line - h + 1
	}
	m.clampBoard()
}

func (m *model) clampBoard() {
	m.boardTop = min(m.boardTop, max(len(m.board.Lines)-m.boardHeight(), 0))
	m.boardTop = max(m.boardTop, 0)
}

// layout is shared by full paints, navigation, cursors and animation.
// Geometry depends only on size and the complete overview, never input mode.
type layout struct {
	overview, board, detail, console   int // body heights
	overviewRow                        int
	boardRule, detailRule, consoleRule int
	boardRow, detailRow, consoleRow    int // body starts, after heading padding
}

func (m *model) layout() layout {
	l := layout{console: m.consoleHeight(), overviewRow: 2}
	// Banner, three rules, footer; a blank on each side of every rule.
	body := m.high - 5 - 6 - l.console
	if len(m.board.Overview) > 0 && body-len(m.board.Overview)-1 >= 6 {
		fits := true
		for _, line := range m.board.Overview {
			if textWidth(line) > m.width-1 {
				fits = false
				break
			}
		}
		if fits {
			l.overview = len(m.board.Overview)
		}
	}
	l.boardRule = 2
	if l.overview > 0 {
		body -= l.overview + 1 // the overview and its leading blank
		l.boardRule += l.overview + 1
	}
	l.detail = max(body-body*2/3, 2)
	l.board = max(body-l.detail, 1)
	l.boardRow = l.boardRule + 2
	l.detailRule = l.boardRow + l.board + 1
	l.detailRow = l.detailRule + 2
	l.consoleRule = l.detailRow + l.detail + 1
	l.consoleRow = l.consoleRule + 2
	return l
}

func (m *model) consoleHeight() int { return 6 }

func (m *model) boardHeight() int {
	return m.layout().board
}

func (m *model) detailHeight() int {
	return m.layout().detail
}

// tooSmall is the size below which neither pane is useful.
func (m *model) tooSmall() bool { return m.width < 40 || m.high < 20 }

func (m *model) resize(rows, cols int) {
	m.width, m.high = cols, rows
	m.clampBoard()
	m.scrollToSelection()
}

func (m *model) dirtyDrafts() int {
	n := 0
	for _, d := range m.drafts {
		if d.dirty() {
			n++
		}
	}
	return n
}

// key handles one key in the current mode.
func (m *model) key(k Key) effect {
	if k.Code == KeyCtrlZ {
		return effSuspend
	}
	if k.Code == KeyCtrlL {
		return effNone // the loop repaints after every key
	}
	switch m.mode {
	case modeEdit:
		return m.editKey(k)
	case modeCommand:
		return m.commandKey(k)
	case modeSearch:
		return m.searchKey(k)
	case modeConfirm:
		return m.confirmKey(k)
	case modeSave:
		return m.saveKey(k)
	case modeChoice:
		return m.choiceKey(k)
	case modeHelp:
		if k.Code == KeyCtrlC {
			return m.quit()
		}
		if k.Code == KeyEsc || k.Code == KeyRune && (k.Rune == '?' || k.Rune == 'q') || k.Code == KeyEnter {
			m.mode = modeNormal
			return effNone
		}
		return m.scrollKey(k, paneDetail)
	}
	return m.normalKey(k)
}

func (m *model) normalKey(k Key) effect {
	if k.Code == KeyCtrlC || k.Code == KeyRune && k.Rune == 'q' {
		return m.quit()
	}
	if k.Code == KeyTab || k.Code == KeyBackTab {
		if k.Code == KeyBackTab {
			m.focus = (m.focus + 2) % 3
		} else {
			m.focus = (m.focus + 1) % 3
		}
		return effNone
	}
	if k.Code == KeyRune {
		switch k.Rune {
		case '/':
			m.openSearch()
			return effNone
		case 'j':
			k = Key{Code: KeyDown}
		case 'k':
			k = Key{Code: KeyUp}
		case '?':
			m.mode, m.detailTop = modeHelp, 0
			return effNone
		case ':':
			m.openCommand()
			return effNone
		case 'n':
			m.startNote()
			return effNone
		case 'a':
			m.startAnswer()
			return effNone
		case 'e':
			m.startEditBody()
			return effNone
		case 't':
			m.reveal("")
			return effNone
		case 'g', 'd', 'u':
			var target *Item
			if it, ok := m.selected(); ok {
				target = &it
			}
			switch k.Rune {
			case 'g':
				return m.runCommand("tag sdlc", target)
			case 'd':
				return m.runCommand("tag design", target)
			default:
				return m.runCommand("untag", target)
			}
		}
	}
	return m.scrollKey(k, m.focus)
}

// scrollKey moves through a pane: selection on the board, lines in the
// details.
func (m *model) scrollKey(k Key, p pane) effect {
	if p == paneConsole {
		m.scrollConsole(k)
		return effNone
	}
	if p == paneBoard {
		switch k.Code {
		case KeyUp:
			m.move(-1)
		case KeyDown:
			m.move(1)
		case KeyPgUp:
			m.boardTop -= m.boardHeight()
			m.clampBoard()
		case KeyPgDn:
			m.boardTop += m.boardHeight()
			m.clampBoard()
		case KeyHome:
			m.boardTop = 0
		case KeyEnd:
			m.boardTop = len(m.board.Lines)
			m.clampBoard()
		}
		return effNone
	}
	switch k.Code {
	case KeyUp:
		m.detailTop--
	case KeyDown:
		m.detailTop++
	case KeyPgUp:
		m.detailTop -= m.detailHeight() - 1
	case KeyPgDn:
		m.detailTop += m.detailHeight() - 1
	case KeyHome:
		m.detailTop = 0
	case KeyEnd:
		m.detailTop = 1 << 30
	}
	m.detailTop = max(m.detailTop, 0) // the view clamps the bottom
	return effNone
}

func (m *model) quit() effect {
	if n := m.dirtyDrafts(); n > 0 {
		m.ask(fmt.Sprintf("Discard %s and quit?", plural(n, "unsaved draft")), nil) // nil yes: quit
		return effNone
	}
	return effQuit
}

func (m *model) ask(prompt string, yes func(*model)) {
	m.say(prompt + " y/N")
	m.confirm = &pendingConfirm{prompt: prompt, yes: yes}
	m.mode = modeConfirm
}

func (m *model) confirmationRows() []string {
	return wrapRows(m.confirm.prompt, max(m.width-1, 1))
}

func (m *model) confirmKey(k Key) effect {
	c := m.confirm
	h := max(m.high-3, 1)
	switch k.Code {
	case KeyUp:
		c.top--
	case KeyDown:
		c.top++
	case KeyPgUp:
		c.top -= h
	case KeyPgDn:
		c.top += h
	case KeyHome:
		c.top = 0
	case KeyEnd:
		c.top = len(m.confirmationRows())
	default:
		return m.answerConfirm(k)
	}
	c.top = min(max(c.top, 0), max(len(m.confirmationRows())-h, 0))
	return effNone
}

func (m *model) answerConfirm(k Key) effect {
	c := m.confirm
	if k.Code == KeyRune && (k.Rune == 'y' || k.Rune == 'Y') {
		m.confirm, m.mode = nil, modeNormal
		if c.yes == nil {
			return effQuit
		}
		c.yes(m)
		return effNone
	}
	if k.Code == KeyCtrlC && c.yes == nil {
		return effQuit // a second Ctrl-C at the quit prompt means it
	}
	m.confirm, m.mode = nil, modeNormal
	m.say("cancelled")
	return effNone
}

// --- drafts ---

func (m *model) openDraft(key draftKey, seed string) {
	// The save's outcome settles the draft it submitted; text added
	// while it is pending would be cleared by its acknowledgement.
	if m.busy != nil && m.busy.draft != nil && *m.busy.draft == key {
		m.say("saving this " + draftWhat(key) + " — wait for its outcome")
		return
	}
	d, ok := m.drafts[key]
	if !ok {
		d = &draft{key: key, ed: newEditor(seed, false), base: seed}
		for _, it := range m.raw.Items {
			if it.Project == key.project && it.Run == key.run {
				d.seen = &it
				break
			}
		}
		m.drafts[key] = d
	}
	if strings.HasPrefix(key.purpose, "new-") && d.seen == nil {
		kind := ItemIdea
		if key.purpose == "new-intent" {
			kind = ItemIntent
		}
		d.seen = &Item{Project: key.project, Run: key.run, Kind: kind, Detail: []string{draftWhat(key) + ": not created until save"}}
		m.applyBoard()
	}
	m.active, m.mode, m.detailTop = d, modeEdit, 0
	m.say("opened " + draftWhat(key))
}

func (m *model) startNote() {
	it, ok := m.selected()
	if ok && m.reopenKept(draftKey{project: it.Project, run: it.Run, purpose: "note"}) {
		return
	}
	if !ok || !it.Notes {
		m.say("notes: select a live run or an open idea")
		return
	}
	m.openDraft(draftKey{project: it.Project, run: it.Run, purpose: "note"}, "")
}

// reopenKept opens a draft already kept under key. Text outlives its
// target's eligibility — a capture closed or a run finished mid-edit —
// so reading and discarding it must not hinge on the flags that gate a
// new draft; the writer still refuses the save.
func (m *model) reopenKept(key draftKey) bool {
	if _, ok := m.drafts[key]; !ok {
		return false
	}
	m.openDraft(key, "")
	return true
}

func (m *model) startAnswer() {
	it, ok := m.selected()
	if !ok {
		m.say("answer: the selected run has no open question")
		return
	}
	// An answer kept from a question since answered or replaced comes
	// back first, still bound to its own question: it is never moved to
	// another. Discarding it lets a reach the open one.
	if d := m.staleAnswer(it); d != nil {
		m.openDraft(d.key, "")
		return
	}
	if it.Question == nil {
		m.say("answer: the selected run has no open question")
		return
	}
	m.openDraft(draftKey{project: it.Project, run: it.Run, purpose: "answer", question: it.Question.ID}, "")
}

func (m *model) startEditBody() {
	it, ok := m.selected()
	if !ok {
		m.say("edit: select an open idea or intent")
		return
	}
	m.startEditBodyOf(&it)
}

func (m *model) editKey(k Key) effect {
	if k.Code == KeyShiftEnter {
		k.Code = KeyEnter
	}
	d := m.active
	switch k.Code {
	case KeyEsc:
		m.active, m.mode = nil, modeNormal
		if !d.dirty() {
			m.dropDraft(d.key)
		}
		return effNone
	case KeyCtrlC:
		return m.quit()
	case KeyCtrlX:
		m.ask("Discard this "+draftWhat(d.key)+"?", func(m *model) {
			m.active = nil
			m.dropDraft(d.key)
			m.say("draft discarded")
		})
		return effNone
	case KeyCtrlS:
		m.submitDraft(d)
		return effNone
	case KeyTab, KeyBackTab, KeyPgUp, KeyPgDn:
		return effNone
	}
	if !d.ed.key(k) {
		if k.Code == KeyRune || k.Code == KeyPaste || k.Code == KeyEnter {
			m.say(fmt.Sprintf("text limit reached (%d KiB); nothing was inserted", maxText>>10))
		}
	}
	if k.Code == KeyPaste && k.Truncated {
		m.say(fmt.Sprintf("paste longer than %d KiB: only its start was inserted", maxPaste>>10))
	}
	return effNone
}

func draftWhat(k draftKey) string {
	switch k.purpose {
	case "note":
		return "note for " + k.project + "/" + k.run
	case "answer":
		return fmt.Sprintf("answer to question #%d on %s/%s", k.question, k.project, k.run)
	case "body":
		return "edit of " + k.project + "/" + k.run
	case "new-idea":
		return "new idea " + k.project + "/" + k.run
	case "new-intent":
		return "new intent " + k.project + "/" + k.run
	}
	return "draft"
}

// isIdea reports whether a draft's saved result is an idea the save
// dialogue may also tag.
func (m *model) isIdea(k draftKey) bool {
	if k.purpose == "new-idea" {
		return true
	}
	for _, it := range m.board.Items {
		if it.Project == k.project && it.Run == k.run && it.Kind == ItemIdea {
			return it.Editable
		}
	}
	return false
}

func (m *model) submitDraft(d *draft) {
	if m.busy != nil {
		m.say("busy: " + m.busy.label + " — wait for its outcome")
		return
	}
	if strings.TrimSpace(d.ed.String()) == "" {
		m.say("nothing to save")
		return
	}
	if d.uncertain != nil && !d.confirmed {
		d.confirmed = true
		m.say("the last save of this draft has an unknown outcome (see details); Ctrl-S again to submit anyway")
		return
	}
	if m.isIdea(d.key) {
		m.saveDraft, m.mode = d, modeSave
		return
	}
	m.runDraft(d, "", false)
}

// saveKey answers the save dialogue. Any other key leaves it open.
func (m *model) saveKey(k Key) effect {
	d := m.saveDraft
	switch {
	case k.Code == KeyEnter || k.Code == KeyRune && k.Rune == 's':
		m.saveDraft = nil
		m.runDraft(d, "", false)
	case k.Code == KeyRune && k.Rune == 'g':
		m.saveDraft = nil
		m.runDraft(d, "sdlc", false)
	case k.Code == KeyRune && k.Rune == 'd':
		m.saveDraft = nil
		m.runDraft(d, "sdlc", true)
	case k.Code == KeyEsc || k.Code == KeyCtrlC:
		m.saveDraft, m.mode = nil, modeEdit
	}
	return effNone
}

// runDraft queues the draft's save, then (tag != "") a tag of the same
// idea. The draft stays until the save succeeds.
func (m *model) runDraft(d *draft, tag string, designOnly bool) {
	k := d.key
	a := Action{Project: k.project, Run: k.run, Text: d.ed.String()}
	switch k.purpose {
	case "note":
		a.Kind = ActNote
	case "answer":
		a.Kind, a.QuestionID = ActAnswer, k.question
	case "body":
		a.Kind, a.Expected = ActEdit, d.base
	case "new-idea", "new-intent":
		a.Kind, a.Workflow = ActCreate, strings.TrimPrefix(k.purpose, "new-")
	}
	j := job{label: draftWhat(k), steps: []Action{a}, draft: &k, target: itemKey{project: k.project, run: k.run}}
	if tag != "" {
		j.steps = append(j.steps, Action{Kind: ActTag, Project: k.project, Run: k.run, Workflow: tag, DesignOnly: designOnly})
		j.label += " + " + tagLabel(tag, designOnly)
	}
	d.uncertain, d.refused, d.confirmed = nil, nil, false
	m.active, m.mode = nil, modeNormal
	m.start(j)
}

func tagLabel(workflow string, designOnly bool) string {
	switch {
	case workflow == "":
		return "untag"
	case designOnly:
		return "tag " + workflow + " (design only)"
	}
	return "tag " + workflow
}

func (m *model) start(j job) {
	if m.busy != nil {
		m.say("busy: " + m.busy.label + " — wait for its outcome")
		return
	}
	m.busy, m.progress = &j, 0
	m.say(j.label + "…")
	m.queue <- j
}

// finished applies a worker result.
func (m *model) finished(r jobResult) {
	if r.progress {
		for i := m.progress; i < len(r.results); i++ {
			m.resultEvent(r.job.steps[i], r.results[i])
		}
		m.progress = len(r.results)
		return
	}
	m.busy = nil
	j := r.job
	var lines []string
	var msgs []string
	for i, res := range r.results {
		step := j.steps[i]
		msgs = append(msgs, res.Message)
		if i >= m.progress {
			m.resultEvent(step, res)
		}
		lines = append(lines, res.Lines...)
		if res.RequestID != "" {
			key := step.Project + "/" + step.Run + " " + step.Control
			if res.Uncertain {
				step.RequestID = res.RequestID
				m.retry[key] = step
			} else {
				delete(m.retry, key)
			}
		}
		if i == 0 && j.draft != nil {
			m.settleDraft(*j.draft, res)
		}
		if step.Kind == ActReveal && len(res.Choices) > 0 {
			m.choices, m.choiceFor = res.Choices, step
			if m.mode == modeNormal {
				m.mode = modeChoice
			} else {
				m.say("terminal choice ready for " + step.Project + "/" + step.Run + "; return to navigation and press t to choose")
			}
		}
		if step.Kind == ActCreate && res.OK {
			if m.raw.ProjectFilter != "" && m.raw.ProjectFilter != step.Project || m.raw.WorkflowFilter != "" && m.raw.WorkflowFilter != step.Workflow {
				msg := step.Project + "/" + step.Run + " was created; hidden by current filters"
				msgs = append(msgs, msg)
				m.say(msg)
			} else {
				m.pendingSel = &itemKey{project: step.Project, run: step.Run}
			}
		}
	}
	if len(r.results) == 1 && len(j.steps) == 2 {
		msgs = append(msgs, "not tagged: the save did not succeed")
		m.say("not tagged: the save did not succeed")
	}
	if len(r.results) == 2 && !r.results[1].OK {
		t := j.steps[1]
		retry := "saved " + t.Project + "/" + t.Run + "; select that idea and retry with :" + tagCommand(t.Workflow, t.DesignOnly) + " (tag only)"
		msgs = append(msgs, retry)
		m.say(retry)
	}
	msgs = slices.DeleteFunc(msgs, func(s string) bool { return s == "" })
	m.lastResult[j.target.project+"/"+j.target.run] = append(msgs, lines...)
	m.progress = 0
	m.applyBoard()
}

func (m *model) resultEvent(step Action, res Result) {
	text := outcomeWord(res) + ": " + step.describe()
	if res.Message != "" {
		text += " — " + res.Message
	}
	if len(res.Lines) > 0 {
		text += "\n" + strings.Join(res.Lines, "\n")
	}
	m.say(text)
}

func tagCommand(workflow string, designOnly bool) string {
	switch {
	case workflow == "":
		return "untag"
	case designOnly:
		return "tag design"
	}
	return "tag sdlc"
}

func (m *model) settleDraft(k draftKey, res Result) {
	d := m.drafts[k]
	if d == nil {
		return
	}
	switch {
	case res.OK:
		m.dropDraft(k)
	case res.Uncertain:
		d.uncertain = append([]string{res.Message}, res.Lines...)
	default:
		d.uncertain, d.refused = nil, append([]string{res.Message}, res.Lines...)
	}
}

// --- commands ---

func (m *model) openCommand() {
	m.completion = nil
	m.cmd, m.mode = newEditor("", true), modeCommand
	if it, ok := m.selected(); ok {
		m.cmdTarget = &it
	} else {
		m.cmdTarget = nil
	}
}

func (m *model) commandKey(k Key) effect {
	if k.Code == KeyTab || k.Code == KeyBackTab {
		m.completeCommand(k.Code == KeyBackTab)
		return effNone
	}
	m.completion = nil
	switch k.Code {
	case KeyEsc:
		m.mode, m.cmd = modeNormal, nil
		return effNone
	case KeyCtrlC:
		m.mode, m.cmd = modeNormal, nil
		return effNone
	case KeyEnter:
		line := m.cmd.String()
		m.mode, m.cmd = modeNormal, nil
		return m.runCommand(line, m.cmdTarget)
	}
	if !m.cmd.key(k) && (k.Code == KeyRune || k.Code == KeyPaste) {
		m.say("command text limit reached; nothing inserted")
	}
	if k.Code == KeyPaste && k.Truncated {
		m.say("command paste truncated at 256 KiB")
	}
	return effNone
}

// commandHint is what the typed command would do to its target, shown
// beside the prompt before Enter.
func commandHint(line string, target *Item) string {
	verb := strings.Fields(line)
	if len(verb) == 0 {
		if target == nil {
			return "no selection"
		}
		return "→ " + target.Label()
	}
	switch verb[0] {
	case "finish":
		if target != nil {
			return "→ " + target.Label() + ": ships this run with !! from its next boundary"
		}
	case "pause":
		if target != nil {
			return "→ " + target.Label() + ": holds it once current work settles"
		}
	case "handoff":
		if target != nil {
			return "→ " + target.Label() + ": holds it for you to pick its conversation up"
		}
	case "park":
		if target != nil {
			return "→ " + target.Label() + ": its current owner yields once"
		}
	case "resume":
		if target != nil {
			return "→ " + target.Label() + ": releases the hold; starts nothing"
		}
	case "close":
		if target != nil {
			return "→ " + target.Label() + ": retires it (asks first)"
		}
	case "tag", "untag", "edit", "terminal":
		if target != nil {
			return "→ " + target.Label()
		}
	case "new":
		return "creates on save"
	}
	if target == nil {
		return "no selection"
	}
	return "→ " + target.Label()
}

func (m *model) runCommand(line string, target *Item) effect {
	f := strings.Fields(line)
	if len(f) == 0 {
		return effNone
	}
	need := func(ok bool, why string) bool {
		if !ok {
			label := f[0]
			if target != nil {
				label += " (" + target.Label() + ")"
			}
			m.say(label + ": " + why)
		}
		return ok
	}
	switch verb := f[0]; verb {
	case "help":
		if !need(len(f) == 1, "takes no arguments") {
			return effNone
		}
		m.mode, m.detailTop = modeHelp, 0
	case "quit", "q":
		if !need(len(f) == 1, "takes no arguments") {
			return effNone
		}
		return m.quit()
	case "pause", "handoff", "park", "finish", "resume":
		if !need(len(f) == 1, "takes no arguments") ||
			!need(target != nil && target.Controls, "select a live operator-paced run") {
			return effNone
		}
		key := target.Label() + " " + verb
		// A retry resends the original request whole — its id and the
		// owner and hold it was bound to — so the writer reads back
		// that request rather than acting on whatever replaced them.
		a, retrying := m.retry[key]
		if !retrying {
			if verb == "park" && !need(target.Generation != "", "no live owner answered; there is nothing to park") {
				return effNone
			}
			a = Action{Kind: ActControl, Project: target.Project, Run: target.Run, Control: verb,
				Generation: target.Generation, HoldRevision: target.HoldRevision}
		}
		label := verb + " " + target.Label()
		if retrying {
			label = "retrying " + label + " (" + a.RequestID + ")"
		}
		m.start(job{label: label, steps: []Action{a}, target: keyOf(*target)})
	case "new":
		if !need(len(f) >= 2 && (f[1] == "idea" || f[1] == "intent"), "usage: :new idea <project>/<slug> or :new intent <project>/<slug>") {
			return effNone
		}
		if !need(len(f) == 3, "usage: :new "+f[1]+" <project>/<slug>") {
			return effNone
		}
		p, slug, ok := strings.Cut(f[2], "/")
		if !need(ok && p != "" && validSlug(slug), "want <project>/<slug> with a lowercase-kebab slug") {
			return effNone
		}
		m.openDraft(draftKey{project: p, run: slug, purpose: "new-" + f[1]}, "# "+slug+"\n")
	case "edit":
		if need(len(f) == 1, "takes no arguments") {
			m.startEditBodyOf(target)
		}
	case "close":
		if !need(len(f) == 1, "takes no arguments") || !need(target != nil && target.Editable, "select an open idea or intent") {
			return effNone
		}
		t := *target
		what := map[ItemKind]string{ItemIdea: "idea", ItemIntent: "intent"}[t.Kind]
		m.ask("Close "+what+" "+t.Label()+"? Its history stays", func(m *model) {
			m.start(job{label: "close " + t.Label(), steps: []Action{{Kind: ActClose, Project: t.Project, Run: t.Run}}, target: keyOf(t)})
		})
	case "tag", "untag":
		if !need(target != nil && target.Kind == ItemIdea && target.Editable, "select an open idea") {
			return effNone
		}
		a := Action{Kind: ActTag, Project: target.Project, Run: target.Run}
		switch {
		case verb == "untag" && len(f) == 1:
		case verb == "tag" && len(f) == 2 && f[1] == "sdlc":
			a.Workflow = "sdlc"
		case verb == "tag" && len(f) == 2 && f[1] == "design":
			a.Workflow, a.DesignOnly = "sdlc", true
		default:
			m.say("usage: tag sdlc | tag design | untag")
			return effNone
		}
		m.start(job{label: tagLabel(a.Workflow, a.DesignOnly) + " " + target.Label(), steps: []Action{a}, target: keyOf(*target)})
	case "terminal":
		if !need(len(f) == 1, "takes no arguments") {
			return effNone
		}
		m.revealOf(target, "")
	default:
		m.say("unknown command " + quote(verb) + " — ? lists them")
	}
	return effNone
}

func (m *model) startEditBodyOf(target *Item) {
	if target != nil && m.reopenKept(draftKey{project: target.Project, run: target.Run, purpose: "body"}) {
		return
	}
	if target == nil || !target.Editable {
		m.say("edit: select an open idea or intent")
		return
	}
	m.openDraft(draftKey{project: target.Project, run: target.Run, purpose: "body"}, target.Body)
}

func validSlug(s string) bool {
	if s == "" || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

func quote(s string) string { return "\"" + sanitize(s) + "\"" }

// --- terminal reveal ---

func (m *model) reveal(client string) {
	it, ok := m.selected()
	if !ok {
		m.say("terminal: select a run reporting a Rex terminal")
		return
	}
	m.revealOf(&it, client)
}

func (m *model) revealOf(target *Item, client string) {
	if target == nil || !target.Terminal {
		m.say("terminal: the selected run reports no Rex terminal")
		return
	}
	if client == "" && len(m.choices) > 0 && m.choiceFor.Project == target.Project && m.choiceFor.Run == target.Run {
		m.mode = modeChoice
		return
	}
	a := Action{Kind: ActReveal, Project: target.Project, Run: target.Run, Generation: target.Generation, Client: client}
	m.start(job{label: "reveal " + target.Label(), steps: []Action{a}, target: keyOf(*target)})
}

func (m *model) choiceKey(k Key) effect {
	if k.Code == KeyRune && k.Rune >= '1' && k.Rune <= '9' {
		if i := int(k.Rune - '1'); i < len(m.choices) {
			a := m.choiceFor
			a.Client = m.choices[i].ID
			m.choices, m.mode = nil, modeNormal
			m.start(job{label: "reveal " + a.Project + "/" + a.Run, steps: []Action{a}, target: itemKey{project: a.Project, run: a.Run}})
		}
		return effNone
	}
	if k.Code == KeyEsc || k.Code == KeyCtrlC {
		m.choices, m.mode = nil, modeNormal
		m.say("terminal reveal cancelled")
	}
	return effNone
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// staleAnswer is a kept answer draft for it whose question is no longer
// the open one, the oldest question first.
func (m *model) staleAnswer(it Item) *draft {
	var best *draft
	for _, d := range m.drafts {
		k := d.key
		if k.project != it.Project || k.run != it.Run || k.purpose != "answer" ||
			it.Question != nil && k.question == it.Question.ID {
			continue
		}
		if best == nil || k.question < best.key.question {
			best = d
		}
	}
	return best
}

// targetClosed reports whether k is a body or note draft whose item no
// longer accepts that write.
func (m *model) targetClosed(k draftKey) bool {
	for _, it := range m.board.Items {
		if it.Project == k.project && it.Run == k.run {
			return k.purpose == "body" && !it.Editable || k.purpose == "note" && !it.Notes
		}
	}
	return false
}

// answerClosed reports whether k is an answer to a question its run no
// longer has open.
func (m *model) answerClosed(k draftKey) bool {
	if k.purpose != "answer" {
		return false
	}
	for _, it := range m.board.Items {
		if it.Project == k.project && it.Run == k.run {
			return it.Question == nil || it.Question.ID != k.question
		}
	}
	return false
}
