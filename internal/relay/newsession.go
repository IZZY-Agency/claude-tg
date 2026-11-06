package relay

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/IZZY-Agency/claude-tg/internal/projects"
	"github.com/IZZY-Agency/claude-tg/internal/render"
	"github.com/IZZY-Agency/claude-tg/internal/send"
	"github.com/IZZY-Agency/claude-tg/internal/state"
	"github.com/IZZY-Agency/claude-tg/internal/telegram"
	"github.com/IZZY-Agency/claude-tg/internal/term"
)

const (
	// newData starts the callback data of the buttons of /new:
	// "n:<key>:<i>" starts a session in project i of the list parked under
	// key, and newCancel in place of i drops the list. A button names a
	// place in a list the relay made, never a path: the phone cannot ask
	// for a folder the relay did not offer. On the question about trusting
	// a folder, newTrust answers yes and newCancel no.
	newData   = "n:"
	newCancel = "-"
	newTrust  = "y"
	// maxNewButtons caps the projects /new offers at once.
	maxNewButtons = 8
	// startWait is how long a new session has to show up in Claude Code's
	// list, startPoll how often the list is read meanwhile.
	startWait = time.Minute
	startPoll = 2 * time.Second
	// startGone is how many reads in a row must miss a pane for it to count
	// as closed.
	startGone = 2
	// startTail and maxTailLine bound what a session that did not come up
	// shows of its pane: lines, and UTF-16 code units of each.
	startTail   = 6
	maxTailLine = 80
	// findWait bounds the search for projects, which runs in the update
	// loop.
	findWait = 5 * time.Second
	// trustWait bounds the wait for the highlight to reach Yes.
	trustWait = 3 * time.Second
	trustPoll = 200 * time.Millisecond
)

// The copy of /new that needs no argument (HTML).
const (
	newOff       = "starting sessions from here is off: set <code>projects_dir</code> in tg's config, see the README"
	newNoRecent  = "no recent project yet: name one with <code>/new name</code>"
	newWhich     = "New session in which project?"
	newCancelled = "✖️ not started"
	newExpired   = "expired; send /new again"
)

// newSession is /new: without a name it offers the recent projects, with one
// the projects matching it; a single match starts at once.
func (r *Router) newSession(ctx context.Context, m *telegram.Message, name string) error {
	if r.ProjectsDir == "" || r.Start == nil {
		return r.reply(ctx, m, newOff)
	}
	recent := r.recentProjects()
	if name == "" {
		if len(recent) == 0 {
			return r.reply(ctx, m, newNoRecent)
		}
		return r.askProject(ctx, m, newWhich, recent)
	}
	fctx, cancel := context.WithTimeout(ctx, findWait)
	defer cancel()
	found, err := projects.Find(fctx, r.ProjectsDir)
	if err != nil {
		return r.reply(ctx, m, "cannot read the projects folder: "+render.EscapeHTML(err.Error()))
	}
	dirs := projects.Match(withRecent(found, recent), r.ProjectsDir, name)
	switch n := len(dirs); {
	case n == 0:
		return r.reply(ctx, m, "no project matches "+bold(name))
	case n == 1:
		id, err := r.send(ctx, m.MessageID, startingText(r.shownProject(dirs[0])), nil)
		if err != nil {
			return err
		}
		r.startIn(ctx, dirs[0], id)
		return nil
	case n > maxNewButtons:
		return r.askProject(ctx, m, fmt.Sprintf("%d projects match, here are the first %d: add more of the name", n, maxNewButtons), dirs)
	default:
		return r.askProject(ctx, m, fmt.Sprintf("%d projects match", n), dirs)
	}
}

// withRecent adds the recent folders the search did not find: a folder that
// is no repository counts once a session has used it.
func withRecent(found, recent []string) []string {
	seen := make(map[string]bool, len(found))
	for _, dir := range found {
		seen[dir] = true
	}
	out := found
	for _, dir := range recent {
		if !seen[dir] {
			out = append(out, dir)
		}
	}
	return out
}

// recentProjects returns the projects under ProjectsDir that sessions used,
// the latest first, a worktree counting as its project. A folder that has
// gone since is left out.
func (r *Router) recentProjects() []string {
	sessions, err := r.Store.ListSessions()
	if err != nil {
		r.logf("recent projects: %v", err)
		return nil
	}
	sort.SliceStable(sessions, func(i, j int) bool { return sessions[i].LastSeen.After(sessions[j].LastSeen) })
	seen := map[string]bool{}
	var out []string
	for _, s := range sessions {
		dir := projects.Home(s.Cwd)
		if dir == "" || seen[dir] {
			continue
		}
		seen[dir] = true
		if projects.Under(r.ProjectsDir, dir) {
			out = append(out, dir)
		}
	}
	return out
}

func (r *Router) shownProject(dir string) string {
	return r.shown(projects.Shown(r.ProjectsDir, dir))
}

func startingText(shown string) string {
	return "⏳ Starting a session in " + bold(shown) + "…"
}

// askProject parks the first maxNewButtons of dirs and offers them under
// heading (HTML), with ✖️ Cancel.
func (r *Router) askProject(ctx context.Context, m *telegram.Message, heading string, dirs []string) error {
	if len(dirs) > maxNewButtons {
		dirs = dirs[:maxNewButtons]
	}
	key := newKey()
	if err := r.Store.PutPending(key, state.Parked{Projects: dirs, MessageID: m.MessageID, Created: r.now()}); err != nil {
		return r.reply(ctx, m, "cannot keep the list of projects: "+render.EscapeHTML(err.Error()))
	}
	var kb [][]telegram.Button
	for i, dir := range dirs {
		kb = append(kb, []telegram.Button{{Text: r.shownProject(dir), Data: newData + key + ":" + strconv.Itoa(i)}})
	}
	kb = append(kb, []telegram.Button{{Text: "✖️ Cancel", Data: newData + key + ":" + newCancel}})
	_, err := r.send(ctx, m.MessageID, heading, kb)
	return err
}

// newTap is a button of /new. The tap takes the parked list, so two taps can
// never start two sessions. The caller has checked the sender.
func (r *Router) newTap(ctx context.Context, cq *telegram.CallbackQuery) error {
	parts := strings.Split(cq.Data, ":")
	if len(parts) != 3 || parts[1] == "" {
		r.answerCallback(ctx, cq, "unknown button")
		return nil
	}
	id := cq.Message.MessageID
	edit := func(html string) {
		if err := r.edit(ctx, id, html); err != nil {
			r.logf("edit: %v", err)
		}
	}
	p, ok := r.Store.TakePending(parts[1], r.now(), pendingTTL)
	if p.SessionID != "" {
		r.trustTap(ctx, cq, p, parts[2], ok)
		return nil
	}
	if !ok {
		// Nothing parked means an earlier tap took the list: its outcome
		// stays on the message (ruling R16).
		if !p.Created.IsZero() {
			edit(newExpired)
		}
		r.answerCallback(ctx, cq, "expired, send /new again")
		return nil
	}
	if parts[2] == newCancel {
		edit(newCancelled)
		r.answerCallback(ctx, cq, "cancelled")
		return nil
	}
	i, err := strconv.Atoi(parts[2])
	if err != nil || i < 0 || i >= len(p.Projects) {
		r.answerCallback(ctx, cq, "unknown button")
		return nil
	}
	dir := p.Projects[i]
	edit(startingText(r.shownProject(dir)))
	r.answerCallback(ctx, cq, "starting")
	r.startIn(ctx, dir, id)
	return nil
}

// startJob is a session on its way up: message id shows how it goes.
type startJob struct {
	sessionID string
	dir       string
	shown     string // the project as the phone shows it
	pane      term.Pane
	id        int64
}

// startIn starts a session in dir and watches it come up, reporting on
// message id. tg chooses the session's id, so it needs no answer from the
// session to know it, and writes its record itself: the hooks write one
// only after the session's first turn, and until then the relay would know
// no pane to type into.
func (r *Router) startIn(ctx context.Context, dir string, id int64) {
	shown := r.shownProject(dir)
	fail := func(why string) {
		if err := r.edit(ctx, id, "✗ could not start a session in "+bold(shown)+": "+render.EscapeHTML(why)); err != nil {
			r.logf("edit: %v", err)
		}
	}
	if r.Start == nil || r.ProjectsDir == "" {
		fail("starting sessions is off")
		return
	}
	// Without the list there is no telling which tabs hold a session.
	live, err := r.live(ctx)
	if err != nil {
		fail("cannot list sessions: " + err.Error())
		return
	}
	var panes []term.Pane
	for _, l := range live {
		if !l.Pane.IsZero() {
			panes = append(panes, l.Pane)
		}
	}
	panes = append(panes, r.startingPanes()...)
	sessionID, err := newSessionID()
	if err != nil {
		fail(err.Error())
		return
	}
	// Last before the start, and the session opens in what this resolved.
	dir, ok := projects.Resolve(r.ProjectsDir, dir)
	if !ok {
		fail("it is not a folder under projects_dir any more")
		return
	}
	name := filepath.Base(dir)
	pane, err := r.Start(ctx, panes, dir, name, sessionID)
	if err != nil {
		r.logf("new session in %s: %v", shown, err)
		fail(err.Error())
		return
	}
	now := r.now()
	if _, err := r.Store.UpdateSession(sessionID, func(s *state.Session) {
		s.Name, s.Cwd, s.LastSeen = name, dir, now
		s.SetPane(pane)
	}); err != nil {
		r.logf("new session in %s: %v", shown, err)
		if err := r.edit(ctx, id, "✗ "+bold(shown)+" started, but tg cannot reach it: "+render.EscapeHTML(err.Error())); err != nil {
			r.logf("edit: %v", err)
		}
		return
	}
	r.logf("started session %s in %s", sessionID, shown)
	r.watch(ctx, startJob{sessionID: sessionID, dir: dir, shown: shown, pane: pane, id: id})
}

// watch runs watchStart beside the update loop, which must stay free for
// the owner's taps. Its pane counts as taken meanwhile: Claude Code's list
// does not show the session yet, and the terminal may not either.
func (r *Router) watch(ctx context.Context, job startJob) {
	r.holdPane(job.pane)
	r.startWG.Add(1)
	go func() {
		defer r.startWG.Done()
		defer r.freePane(job.pane)
		r.watchStart(ctx, job)
	}()
}

// holdPane and freePane count the watches of a pane: a tap on the question
// about trusting a folder can start one before the watch that asked has ended.
func (r *Router) holdPane(p term.Pane) {
	r.startMu.Lock()
	defer r.startMu.Unlock()
	if r.starting == nil {
		r.starting = map[term.Pane]int{}
	}
	r.starting[p]++
}

func (r *Router) freePane(p term.Pane) {
	r.startMu.Lock()
	defer r.startMu.Unlock()
	if r.starting[p]--; r.starting[p] <= 0 {
		delete(r.starting, p)
	}
}

// startingPanes are the panes of the sessions being watched.
func (r *Router) startingPanes() []term.Pane {
	r.startMu.Lock()
	defer r.startMu.Unlock()
	var out []term.Pane
	for p := range r.starting {
		out = append(out, p)
	}
	return out
}

// trustOn reads the pane's question about trusting a folder, when it is about
// dir: a tab outlives a session, so the question on it may be another
// session's, for another folder.
func trustOn(screen, dir string) (open, yes bool) {
	folder, open, yes := term.TrustDialog(screen)
	if !open || folder != dir {
		return false, false
	}
	return true, yes
}

// WaitStarts blocks until every session being watched has come up, failed,
// or the relay stopped.
func (r *Router) WaitStarts() { r.startWG.Wait() }

// watchStart waits for the new session to show up in Claude Code's list and
// then points the next messages at it. Ending ctx ends the wait without a
// word: the session may still come up. A pane with no terminal means the
// display sleeps; it is woken once.
func (r *Router) watchStart(ctx context.Context, job startJob) {
	edit := func(html string) {
		if err := r.edit(ctx, job.id, html); err != nil {
			r.logf("edit: %v", err)
		}
	}
	deadline := r.now().Add(startWait)
	woken, asleep, screen := false, false, ""
	gone := 0
	for ctx.Err() == nil {
		if live, err := r.live(ctx); err == nil {
			if l := bySession(live, job.sessionID); l != nil {
				edit(r.startReady(l))
				return
			}
		}
		// One read proves nothing: Exists also says no when the backend is
		// too busy to answer.
		if !r.Term.Exists(ctx, job.pane) {
			if gone++; gone >= startGone {
				if ctx.Err() == nil {
					edit("✗ " + bold(job.shown) + " closed before it was up")
				}
				return
			}
			r.sleep(ctx, startPoll)
			continue
		}
		gone = 0
		s, err := r.Term.Screen(ctx, job.pane, screenLines)
		asleep = errors.Is(err, term.ErrNotRealized)
		if err == nil {
			screen = s
			if open, _ := trustOn(s, job.dir); open {
				r.askTrust(ctx, job)
				return
			}
		}
		if asleep && !woken && r.Wake != nil {
			woken = true
			r.Wake(ctx)
		}
		if !r.now().Before(deadline) {
			edit(r.notUp(job.shown, asleep, screen))
			return
		}
		r.sleep(ctx, startPoll)
	}
}

// askTrust puts Claude Code's question about a folder it has not opened to
// the owner. Only a tap answers it, and the watch ends here: the question
// waits as long as it takes.
func (r *Router) askTrust(ctx context.Context, job startJob) {
	key := newKey()
	if err := r.Store.PutPending(key, state.Parked{SessionID: job.sessionID, Header: job.shown, Projects: []string{job.dir}, Created: r.now()}); err != nil {
		if err := r.edit(ctx, job.id, "✗ "+bold(job.shown)+" asks to be trusted, and tg cannot keep the question: "+render.EscapeHTML(err.Error())+". Answer it at the machine."); err != nil {
			r.logf("edit: %v", err)
		}
		return
	}
	html := "🔐 Claude Code has not opened " + bold(job.shown) + " before and asks if you trust it: it could read, edit and run the files there."
	kb := [][]telegram.Button{{
		{Text: "✅ Yes, trust", Data: newData + key + ":" + newTrust},
		{Text: "✖️ Cancel", Data: newData + key + ":" + newCancel},
	}}
	if err := send.EditHTML(ctx, r.API, r.ChatID, job.id, r.scrub(html), r.cleanKeyboard(kb)); err != nil {
		// The tab waits on this question, so it goes out as a message of its
		// own rather than not at all.
		r.logf("edit: %v", err)
		if _, err := r.send(ctx, 0, html, kb); err != nil {
			r.logf("send the question about %s: %v", job.shown, err)
		}
	}
}

// trustTap answers that question from its buttons. fresh is TakePending's:
// an answer that old is taken as no. The pane is read first: when the
// question is gone a key would land in the session's prompt, and when the
// pane asks about another folder the answer would be for that one, so no key
// is typed and the watch reports how it went.
func (r *Router) trustTap(ctx context.Context, cq *telegram.CallbackQuery, p state.Parked, action string, fresh bool) {
	edit := func(html string) {
		if err := r.edit(ctx, cq.Message.MessageID, html); err != nil {
			r.logf("edit: %v", err)
		}
	}
	if action != newTrust && action != newCancel {
		r.answerCallback(ctx, cq, "unknown button")
		return
	}
	sess, err := r.Store.LoadSession(p.SessionID)
	if err != nil {
		edit("✗ cannot read the session of " + bold(p.Header) + ": " + render.EscapeHTML(err.Error()))
		r.answerCallback(ctx, cq, "cannot read the session")
		return
	}
	if len(p.Projects) != 1 {
		r.answerCallback(ctx, cq, "unknown button")
		return
	}
	job := startJob{sessionID: p.SessionID, dir: p.Projects[0], shown: p.Header, pane: sess.Pane(), id: cq.Message.MessageID}
	couldNotAnswer := func(err error) {
		r.logf("new session in %s: %v", job.shown, err)
		edit("✗ could not answer for " + bold(job.shown) + ": " + render.EscapeHTML(err.Error()) + ". Answer it at the machine.")
		r.answerCallback(ctx, cq, "could not answer")
	}
	screen, err := r.Term.Screen(ctx, job.pane, screenLines)
	open := false
	if err == nil {
		open, _ = trustOn(screen, job.dir)
	}
	switch {
	case !open:
		edit(startingText(job.shown))
		r.answerCallback(ctx, cq, "starting")
		r.watch(ctx, job)
	case !fresh || action == newCancel:
		if err := r.Term.Type(ctx, job.pane, term.Escape); err != nil {
			couldNotAnswer(err)
			return
		}
		if fresh {
			edit(newCancelled)
			r.answerCallback(ctx, cq, "cancelled")
		} else {
			edit(newExpired)
			r.answerCallback(ctx, cq, "expired, send /new again")
		}
	default:
		if err := r.acceptTrust(ctx, job.pane, job.dir); err != nil {
			couldNotAnswer(err)
			return
		}
		edit(startingText(job.shown))
		r.answerCallback(ctx, cq, "starting")
		r.watch(ctx, job)
	}
}

// acceptTrust moves the highlight to Yes and confirms. Enter waits until the
// pane shows Yes highlighted: on the default it would refuse.
func (r *Router) acceptTrust(ctx context.Context, pane term.Pane, dir string) error {
	if err := r.Term.Type(ctx, pane, term.Down); err != nil {
		return err
	}
	deadline := r.now().Add(trustWait)
	for {
		screen, err := r.Term.Screen(ctx, pane, screenLines)
		if err != nil {
			return err
		}
		switch open, yes := trustOn(screen, dir); {
		case !open:
			return errors.New("the question went away")
		case yes:
			return r.Term.Type(ctx, pane, term.Enter)
		}
		if !r.now().Before(deadline) {
			return errors.New("the highlight did not reach Yes")
		}
		r.sleep(ctx, trustPoll)
	}
}

// startReady picks the session that came up and returns the message saying
// so (HTML).
func (r *Router) startReady(l *Live) string {
	if err := r.Store.SavePick(state.Pick{SessionID: l.SessionID, At: r.now()}); err != nil {
		r.logf("pick the new session: %v", err)
		return "✅ " + bold(l.Name) + " is ready. Pick it with /sessions."
	}
	return "✅ " + bold(l.Name) + " is ready. Next messages go to it."
}

// notUp is the message for a session that did not come up in time (HTML):
// what its pane shows is the likely reason, a dialog tg does not know.
func (r *Router) notUp(shown string, asleep bool, screen string) string {
	html := "✗ " + bold(shown) + " did not come up within a minute"
	if asleep {
		return html + ": the Mac's display stayed asleep. It starts when the display wakes."
	}
	if tail := r.screenTail(screen); tail != "" {
		return html + ". Its pane shows:\n<pre>" + render.EscapeHTML(tail) + "</pre>"
	}
	return html + "."
}

// screenTail is the last startTail lines of screen that are not blank, each
// cut to maxTailLine. A line is cut only after the secrets are masked and
// credentials redacted: a cut secret would no longer match, and the outbound
// scrub would let the rest of it through.
func (r *Router) screenTail(screen string) string {
	var lines []string
	for _, l := range strings.Split(screen, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, clipUTF16(render.Redact(r.scrub(l)), maxTailLine))
		}
	}
	if len(lines) > startTail {
		lines = lines[len(lines)-startTail:]
	}
	return strings.Join(lines, "\n")
}

// newSessionID makes a random UUID, which is what Claude Code takes as a
// session id.
func newSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40 // version 4
	b[8] = b[8]&0x3f | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
