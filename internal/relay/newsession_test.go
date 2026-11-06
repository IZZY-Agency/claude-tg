package relay

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/IZZY-Agency/claude-tg/internal/relay/relaytest"
	"github.com/IZZY-Agency/claude-tg/internal/session"
	"github.com/IZZY-Agency/claude-tg/internal/state"
	"github.com/IZZY-Agency/claude-tg/internal/telegram"
	"github.com/IZZY-Agency/claude-tg/internal/term"
)

const newPane = "N1" // the agterm id the fake terminal gives a new session

type startCall struct {
	panes           []term.Pane
	dir, name, sess string
}

// starter is the owner's terminal and Claude Code for /new: Start opens a
// pane, and when comesUp is set Claude Code lists the new session.
type starter struct {
	f       *fixture
	root    string
	calls   []startCall
	err     error
	comesUp bool
	onStart func()
}

func (s *starter) start(_ context.Context, panes []term.Pane, dir, name, id string) (term.Pane, error) {
	s.calls = append(s.calls, startCall{panes, dir, name, id})
	if s.err != nil {
		return term.Pane{}, s.err
	}
	idle, _ := os.ReadFile("../term/testdata/idle-screen.txt")
	s.f.term.Screens[newPane] = string(idle)
	if s.onStart != nil {
		s.onStart()
	}
	return term.Pane{Backend: "agterm", Target: newPane}, nil
}

func newStarter(t *testing.T) *starter {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"client/acme-shop/.git", "client/blog/.git", "tools/acme-admin/.git", "notes"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f := newFixture(t)
	s := &starter{f: f, root: root, comesUp: true}
	base := f.r.Agents
	f.r.Agents = func(ctx context.Context) ([]session.Agent, error) {
		agents, err := base(ctx)
		if s.comesUp {
			for _, c := range s.calls {
				agents = append(agents, session.Agent{SessionID: c.sess, Name: "claude-new", Kind: "interactive", Status: "idle", Cwd: c.dir})
			}
		}
		return agents, err
	}
	f.r.ProjectsDir = root
	f.r.Start = s.start
	return s
}

// seen makes session id a recent one in dir, age ago.
func (s *starter) seen(t *testing.T, id, dir string, age time.Duration) {
	t.Helper()
	if _, err := s.f.store.UpdateSession(id, func(sess *state.Session) {
		sess.Cwd, sess.LastSeen = dir, s.f.now.Add(-age)
	}); err != nil {
		t.Fatal(err)
	}
}

func (s *starter) dir(rel string) string { return filepath.Join(s.root, rel) }

// run handles u and waits for the session it may have started.
func (s *starter) run(t *testing.T, u telegram.Update) {
	t.Helper()
	s.f.handle(t, u)
	s.f.r.WaitStarts()
}

func buttonTexts(kb [][]telegram.Button) []string {
	var out []string
	for _, row := range kb {
		for _, b := range row {
			out = append(out, b.Text)
		}
	}
	return out
}

func (s *starter) lastEdit(t *testing.T) relaytest.Edit {
	t.Helper()
	if len(s.f.api.Edits) == 0 {
		t.Fatal("no edit")
	}
	return s.f.api.Edits[len(s.f.api.Edits)-1]
}

// Starting programs from the phone is off until the owner names a folder.
func TestNewIsOffWithoutProjectsDir(t *testing.T) {
	s := newStarter(t)
	s.f.r.ProjectsDir = ""
	s.run(t, s.f.msg("/new blog", 0))
	last := s.f.lastSent(t)
	if !strings.Contains(last.Text, "projects_dir") || len(last.InlineKeyboard) != 0 || len(s.calls) != 0 {
		t.Fatalf("last = %+v, calls = %+v", last, s.calls)
	}
}

func TestNewListsRecentProjects(t *testing.T) {
	s := newStarter(t)
	s.seen(t, sidShop, s.dir("client/acme-shop"), time.Hour)
	s.seen(t, sidShopF, s.dir("client/acme-shop/.claude/worktrees/fix"), 5*time.Minute) // the same project, more recent
	s.seen(t, sidBlog, s.dir("notes"), 2*time.Hour)                                     // a plain folder counts once used
	s.seen(t, sidOld, "/elsewhere/project", time.Minute)                                // outside the folder
	s.seen(t, "5e55a000-0000-4000-8000-000000000009", s.dir("gone"), time.Minute)       // deleted since
	s.run(t, s.f.msg("/new", 0))
	last := s.f.lastSent(t)
	if last.Text != "New session in which project?" || last.ReplyToMessageID != 500 {
		t.Fatalf("last = %+v", last)
	}
	want := []string{"client/acme-shop", "notes", "✖️ Cancel"}
	if got := buttonTexts(last.InlineKeyboard); !reflect.DeepEqual(got, want) {
		t.Fatalf("buttons = %q, want %q", got, want)
	}
	for _, row := range last.InlineKeyboard {
		if len(row[0].Data) > maxCallbackData {
			t.Fatalf("data too long: %q", row[0].Data)
		}
	}
	if len(s.calls) != 0 {
		t.Fatalf("started without a tap: %+v", s.calls)
	}
}

func TestNewListStopsAtEightProjects(t *testing.T) {
	s := newStarter(t)
	for i := range 10 {
		name := fmt.Sprintf("site-%d", i)
		if err := os.MkdirAll(s.dir(name), 0o755); err != nil {
			t.Fatal(err)
		}
		s.seen(t, fmt.Sprintf("5e55a000-0000-4000-8000-00000000001%d", i), s.dir(name), time.Duration(i+1)*time.Minute)
	}
	s.run(t, s.f.msg("/new", 0))
	got := buttonTexts(s.f.lastSent(t).InlineKeyboard)
	if len(got) != 9 || got[0] != "site-0" || got[7] != "site-7" {
		t.Fatalf("buttons = %q", got)
	}
}

func TestNewWithNoRecentProjectSaysToNameOne(t *testing.T) {
	s := newStarter(t)
	s.run(t, s.f.msg("/new", 0))
	last := s.f.lastSent(t)
	if !strings.Contains(last.Text, "/new") || len(last.InlineKeyboard) != 0 {
		t.Fatalf("last = %+v", last)
	}
}

// The whole way, as the phone sees it: one match starts at once, the
// session gets a record it can be reached by, and the next message goes
// to it.
func TestNewByNameStartsTheOneMatch(t *testing.T) {
	s := newStarter(t)
	s.run(t, s.f.msg("/new blog", 0))
	if len(s.calls) != 1 {
		t.Fatalf("calls = %+v", s.calls)
	}
	c := s.calls[0]
	if c.dir != s.dir("client/blog") || c.name != "blog" {
		t.Fatalf("call = %+v", c)
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(c.sess) {
		t.Fatalf("session id %q", c.sess)
	}
	if len(c.panes) != 3 || c.panes[0] != (term.Pane{Backend: "agterm", Target: "A1"}) {
		t.Fatalf("panes = %+v", c.panes)
	}
	if last := s.f.lastSent(t); last.Text != "⏳ Starting a session in <b>client/blog</b>…" || last.ReplyToMessageID != 500 {
		t.Fatalf("sent = %+v", last)
	}
	sameEdit(t, s.lastEdit(t), relaytest.Edit{MessageID: 1001, Text: "✅ <b>blog</b> is ready. Next messages go to it."})
	sess, err := s.f.store.LoadSession(c.sess)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Pane() != (term.Pane{Backend: "agterm", Target: newPane}) || sess.Cwd != c.dir || sess.Name != "blog" {
		t.Fatalf("record = %+v", sess)
	}
	if p := s.f.storedPick(t); p.SessionID != c.sess {
		t.Fatalf("pick = %+v", p)
	}
	s.f.handle(t, s.f.msg("hello", 0))
	s.f.typedOnce(t, newPane, " hello\n")
}

// A folder used before counts for search too, repository or not.
func TestNewByNameFindsAUsedFolder(t *testing.T) {
	s := newStarter(t)
	s.seen(t, sidShop, s.dir("notes"), time.Hour)
	s.run(t, s.f.msg("/new notes", 0))
	if len(s.calls) != 1 || s.calls[0].dir != s.dir("notes") {
		t.Fatalf("calls = %+v", s.calls)
	}
}

func TestNewByNameOffersSeveralMatches(t *testing.T) {
	s := newStarter(t)
	s.run(t, s.f.msg("/new acme", 0))
	last := s.f.lastSent(t)
	want := []string{"client/acme-shop", "tools/acme-admin", "✖️ Cancel"}
	if got := buttonTexts(last.InlineKeyboard); last.Text != "2 projects match" || !reflect.DeepEqual(got, want) {
		t.Fatalf("text %q, buttons %q", last.Text, got)
	}
	if len(s.calls) != 0 {
		t.Fatalf("started without a tap: %+v", s.calls)
	}
	s.run(t, tapUpdate(last.InlineKeyboard[0][0].Data, 1001))
	if len(s.calls) != 1 || s.calls[0].dir != s.dir("client/acme-shop") {
		t.Fatalf("calls = %+v", s.calls)
	}
	sameEdit(t, s.f.editAt(t, 0), relaytest.Edit{MessageID: 1001, Text: "⏳ Starting a session in <b>client/acme-shop</b>…"})
	sameEdit(t, s.lastEdit(t), relaytest.Edit{MessageID: 1001, Text: "✅ <b>acme-shop</b> is ready. Next messages go to it."})
	s.f.toastsAre(t, "starting")
	if s.f.parked(t) != 0 {
		t.Fatal("the list stayed parked")
	}
}

func TestNewByNameSaysWhenThereAreMoreThanEight(t *testing.T) {
	s := newStarter(t)
	for i := range 10 {
		if err := os.MkdirAll(s.dir(fmt.Sprintf("site-%d/.git", i)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	s.run(t, s.f.msg("/new site", 0))
	last := s.f.lastSent(t)
	if last.Text != "10 projects match, here are the first 8: add more of the name" || len(last.InlineKeyboard) != 9 {
		t.Fatalf("last = %+v", last)
	}
}

func TestNewByNameNoMatch(t *testing.T) {
	s := newStarter(t)
	s.run(t, s.f.msg("/new <nothing>", 0))
	last := s.f.lastSent(t)
	if last.Text != "no project matches <b>&lt;nothing&gt;</b>" || len(s.calls) != 0 {
		t.Fatalf("last = %+v, calls = %+v", last, s.calls)
	}
}

func TestNewByNameUnreadableFolder(t *testing.T) {
	s := newStarter(t)
	s.f.r.ProjectsDir = s.dir("none")
	s.run(t, s.f.msg("/new blog", 0))
	if last := s.f.lastSent(t); !strings.HasPrefix(last.Text, "cannot read the projects folder: ") || len(s.calls) != 0 {
		t.Fatalf("last = %+v", last)
	}
}

func (s *starter) listed(t *testing.T) (first, cancel string) {
	t.Helper()
	s.run(t, s.f.msg("/new acme", 0))
	kb := s.f.lastSent(t).InlineKeyboard
	return kb[0][0].Data, kb[len(kb)-1][0].Data
}

func TestNewCancel(t *testing.T) {
	s := newStarter(t)
	_, cancel := s.listed(t)
	s.run(t, tapUpdate(cancel, 1001))
	sameEdit(t, s.lastEdit(t), relaytest.Edit{MessageID: 1001, Text: "✖️ not started"})
	s.f.toastsAre(t, "cancelled")
	if len(s.calls) != 0 || s.f.parked(t) != 0 {
		t.Fatalf("calls = %+v, parked = %d", s.calls, s.f.parked(t))
	}
}

func TestNewExpired(t *testing.T) {
	s := newStarter(t)
	first, _ := s.listed(t)
	s.f.now = s.f.now.Add(pendingTTL + time.Minute)
	s.run(t, tapUpdate(first, 1001))
	sameEdit(t, s.lastEdit(t), relaytest.Edit{MessageID: 1001, Text: "expired; send /new again"})
	s.f.toastsAre(t, "expired, send /new again")
	if len(s.calls) != 0 {
		t.Fatalf("started from an expired list: %+v", s.calls)
	}
}

// Two taps on a client that still shows the buttons start one session.
func TestNewTappedTwiceStartsOnce(t *testing.T) {
	s := newStarter(t)
	first, _ := s.listed(t)
	s.run(t, tapUpdate(first, 1001))
	edits := len(s.f.api.Edits)
	s.run(t, tapUpdate(first, 1001))
	if len(s.calls) != 1 {
		t.Fatalf("calls = %+v", s.calls)
	}
	if len(s.f.api.Edits) != edits {
		t.Fatalf("the second tap edited the outcome: %+v", s.f.api.Edits)
	}
	s.f.toastsAre(t, "starting", "expired, send /new again")
}

func TestNewUnknownButton(t *testing.T) {
	for _, data := range []string{"n:", "n:k1", "n:k1:9", "n:k1:-1", "n:k1:x", "n:k1:0:0"} {
		t.Run(data, func(t *testing.T) {
			s := newStarter(t)
			if err := s.f.store.PutPending("k1", state.Parked{Projects: []string{s.dir("client/blog")}, Created: s.f.now}); err != nil {
				t.Fatal(err)
			}
			s.run(t, tapUpdate(data, 1001))
			s.f.toastsAre(t, "unknown button")
			if len(s.calls) != 0 {
				t.Fatalf("calls = %+v", s.calls)
			}
		})
	}
}

// What a tap starts is checked again, whatever the list said: the folder
// may be gone, or have become a link, since.
func TestNewRefusesAFolderOutsideProjectsDir(t *testing.T) {
	outside := t.TempDir()
	for name, dir := range map[string]func(s *starter) string{
		"outside": func(*starter) string { return outside },
		"gone":    func(s *starter) string { return s.dir("gone") },
		"root":    func(s *starter) string { return s.root },
		"link": func(s *starter) string {
			link := s.dir("link")
			if err := os.Symlink(outside, link); err != nil {
				t.Fatal(err)
			}
			return link
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := newStarter(t)
			if err := s.f.store.PutPending("k1", state.Parked{Projects: []string{dir(s)}, Created: s.f.now}); err != nil {
				t.Fatal(err)
			}
			s.run(t, tapUpdate("n:k1:0", 1001))
			if len(s.calls) != 0 {
				t.Fatalf("started: %+v", s.calls)
			}
			if got := s.lastEdit(t).Text; !strings.HasPrefix(got, "✗ ") {
				t.Fatalf("edit = %q", got)
			}
		})
	}
}

func TestNewTapFromAStrangerIsDropped(t *testing.T) {
	s := newStarter(t)
	first, _ := s.listed(t)
	u := tapUpdate(first, 1001)
	u.CallbackQuery.From = &telegram.User{ID: me + 1}
	s.run(t, u)
	if len(s.calls) != 0 || s.f.parked(t) != 1 {
		t.Fatalf("calls = %+v, parked = %d", s.calls, s.f.parked(t))
	}
}

func TestNewStartFails(t *testing.T) {
	s := newStarter(t)
	s.err = errors.New("agtermctl session: no <window> open")
	s.run(t, s.f.msg("/new blog", 0))
	sameEdit(t, s.lastEdit(t), relaytest.Edit{MessageID: 1001, Text: "✗ could not start a session in <b>client/blog</b>: agtermctl session: no &lt;window&gt; open"})
	if p := s.f.storedPick(t); p != (state.Pick{}) {
		t.Fatalf("pick = %+v", p)
	}
}

// With no live session to go by, the terminal is left to choose.
func TestNewWithNoLiveSessionHasNoLikePane(t *testing.T) {
	s := newStarter(t)
	s.f.r.Agents = relaytest.AgentsOf()
	s.comesUp = false
	s.run(t, s.f.msg("/new blog", 0))
	if len(s.calls) != 1 || len(s.calls[0].panes) != 0 {
		t.Fatalf("calls = %+v", s.calls)
	}
}

// Without the list tg cannot tell which tabs hold a session, so it starts
// nothing: Ctrl-C and a launch could reach one of them.
func TestNewRefusesWhenSessionsCannotBeListed(t *testing.T) {
	s := newStarter(t)
	s.f.r.Agents = func(context.Context) ([]session.Agent, error) { return nil, errors.New("claude agents timed out") }
	s.run(t, s.f.msg("/new blog", 0))
	if len(s.calls) != 0 {
		t.Fatalf("started: %+v", s.calls)
	}
	want := "✗ could not start a session in <b>client/blog</b>: cannot list sessions: claude agents timed out"
	if got := s.lastEdit(t).Text; got != want {
		t.Fatalf("edit = %q, want %q", got, want)
	}
}

// A tap can start a new watch of a pane before the one that asked has ended:
// the first to end must not free the pane for the other.
func TestPaneStaysTakenUntilEveryWatchEnds(t *testing.T) {
	r := &Router{}
	p := term.Pane{Backend: "agterm", Target: newPane}
	r.holdPane(p)
	r.holdPane(p)
	r.freePane(p)
	if got := r.startingPanes(); len(got) != 1 || got[0] != p {
		t.Fatalf("panes = %+v", got)
	}
	r.freePane(p)
	if got := r.startingPanes(); len(got) != 0 {
		t.Fatalf("panes = %+v", got)
	}
}

// The session runs but tg could not keep its record, so it cannot reach it:
// the owner is told it is running.
func TestNewRecordFails(t *testing.T) {
	s := newStarter(t)
	sessions := filepath.Join(s.f.store.Dir, "sessions")
	s.onStart = func() {
		if err := os.Chmod(sessions, 0o500); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = os.Chmod(sessions, 0o700) })
	s.run(t, s.f.msg("/new blog", 0))
	got := s.lastEdit(t).Text
	if !strings.HasPrefix(got, "✗ <b>client/blog</b> started, but tg cannot reach it: ") {
		t.Fatalf("edit = %q", got)
	}
}

func TestNewReadyWhenThePickCannotBeSaved(t *testing.T) {
	s := newStarter(t)
	dir := s.f.store.Dir
	s.onStart = func() {
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	s.run(t, s.f.msg("/new blog", 0))
	sameEdit(t, s.lastEdit(t), relaytest.Edit{MessageID: 1001, Text: "✅ <b>blog</b> is ready. Pick it with /sessions."})
}

func TestNewSessionClosesBeforeItIsUp(t *testing.T) {
	s := newStarter(t)
	s.comesUp = false
	s.onStart = func() { s.f.term.Missing[newPane] = true }
	s.run(t, s.f.msg("/new blog", 0))
	sameEdit(t, s.lastEdit(t), relaytest.Edit{MessageID: 1001, Text: "✗ <b>client/blog</b> closed before it was up"})
	if p := s.f.storedPick(t); p != (state.Pick{}) {
		t.Fatalf("pick = %+v", p)
	}
}

// One read that finds no tab proves nothing: agterm's tree can time out on a
// busy Mac, and Exists says no for that too.
func TestNewOneFailedReadIsNotAClosedTab(t *testing.T) {
	s := newStarter(t)
	s.comesUp = false
	s.onStart = func() {
		s.f.term.Missing[newPane] = true
		s.f.r.Sleep = func(_ context.Context, d time.Duration) {
			s.f.now = s.f.now.Add(d)
			s.f.term.Missing[newPane] = false
			s.comesUp = true
		}
	}
	s.run(t, s.f.msg("/new blog", 0))
	if got := s.lastEdit(t).Text; !strings.HasPrefix(got, "✅ ") {
		t.Fatalf("edit = %q", got)
	}
}

// A session that never comes up shows what its pane shows: a dialog of a
// kind tg does not know is the usual reason.
func TestNewSessionDoesNotComeUp(t *testing.T) {
	s := newStarter(t)
	s.f.r.Secrets = []string{"123456:ABCDEFsecretbot"}
	s.comesUp = false
	s.onStart = func() {
		s.f.term.Screens[newPane] = strings.Repeat("old line\n", 20) + "Allow <this> server?\n\n 123456:ABCDEFsecretbot\n" + strings.Repeat("x", 200) + "\n\n"
	}
	s.run(t, s.f.msg("/new blog", 0))
	got := s.lastEdit(t).Text
	if !strings.HasPrefix(got, "✗ <b>client/blog</b> did not come up within a minute. Its pane shows:\n<pre>") || !strings.HasSuffix(got, "</pre>") {
		t.Fatalf("edit = %q", got)
	}
	if !strings.Contains(got, "Allow &lt;this&gt; server?") || strings.Contains(got, "secret") {
		t.Fatalf("edit = %q", got)
	}
	if strings.Count(got, "old line") != startTail-3 || strings.Contains(got, strings.Repeat("x", 100)) {
		t.Fatalf("edit = %q", got)
	}
	if s.f.slept < startWait {
		t.Fatalf("waited %s", s.f.slept)
	}
	if p := s.f.storedPick(t); p != (state.Pick{}) {
		t.Fatalf("pick = %+v", p)
	}
}

// agterm opens no terminal while the Mac's display sleeps, and that is when
// the phone is in use: the display is woken, once.
func TestNewWakesASleepingDisplay(t *testing.T) {
	s := newStarter(t)
	s.comesUp = false
	s.onStart = func() { s.f.term.ScreenErrs = map[string]error{newPane: term.ErrNotRealized} }
	woken := 0
	s.f.r.Wake = func(context.Context) {
		woken++
		s.f.term.ScreenErrs = nil
		s.comesUp = true
	}
	s.run(t, s.f.msg("/new blog", 0))
	if woken != 1 {
		t.Fatalf("woken %d times", woken)
	}
	sameEdit(t, s.lastEdit(t), relaytest.Edit{MessageID: 1001, Text: "✅ <b>blog</b> is ready. Next messages go to it."})
}

func TestNewDisplayStaysAsleep(t *testing.T) {
	s := newStarter(t)
	s.comesUp = false
	s.onStart = func() { s.f.term.ScreenErrs = map[string]error{newPane: term.ErrNotRealized} }
	woken := 0
	s.f.r.Wake = func(context.Context) { woken++ }
	s.run(t, s.f.msg("/new blog", 0))
	if woken != 1 {
		t.Fatalf("woken %d times", woken)
	}
	sameEdit(t, s.lastEdit(t), relaytest.Edit{MessageID: 1001, Text: "✗ <b>client/blog</b> did not come up within a minute: the Mac's display stayed asleep. It starts when the display wakes."})
}

// Without a way to wake it, the wait and the message stay the same.
func TestNewDisplayAsleepWithNoWake(t *testing.T) {
	s := newStarter(t)
	s.comesUp = false
	s.onStart = func() { s.f.term.ScreenErrs = map[string]error{newPane: term.ErrNotRealized} }
	s.run(t, s.f.msg("/new blog", 0))
	if got := s.lastEdit(t).Text; !strings.Contains(got, "display stayed asleep") {
		t.Fatalf("edit = %q", got)
	}
}

// The relay stopping ends the wait without a word: the session may still
// come up, and "did not come up" would be wrong.
func TestNewWaitEndsWithTheRelay(t *testing.T) {
	s := newStarter(t)
	s.comesUp = false
	ctx, cancel := context.WithCancel(context.Background())
	s.onStart = cancel
	if err := s.f.r.Handle(ctx, s.f.msg("/new blog", 0)); err != nil {
		t.Fatal(err)
	}
	s.f.r.WaitStarts()
	if len(s.f.api.Edits) != 0 {
		t.Fatalf("edits = %+v", s.f.api.Edits)
	}
}

// A tap carries only its message's date: the parked list's age decides, so
// a list older than the stale limit still gets its expired outcome.
func TestNewTapIsNotDroppedAsStale(t *testing.T) {
	now := time.Date(2026, 10, 22, 21, 0, 0, 0, time.UTC)
	old := &telegram.Message{Date: now.Add(-20 * time.Minute).Unix()}
	ups := []telegram.Update{{UpdateID: 1, CallbackQuery: &telegram.CallbackQuery{Data: "n:0a1b2c3d:0", Message: old}}}
	if fresh, _ := PartitionStale(ups, now, 10*time.Minute); len(fresh) != 1 {
		t.Fatal("a /new tap was dropped as stale")
	}
}

func TestNewIsInTheMenuAndTheHelp(t *testing.T) {
	found := false
	for _, c := range Commands {
		found = found || c.Command == "new"
	}
	if !found || !strings.Contains(helpText, "/new") {
		t.Fatalf("menu has /new: %v; help = %q", found, helpText)
	}
}

// trusting makes Start open a pane on Claude Code's question about a new
// folder. Down moves the highlight to Yes, and Enter on Yes brings the
// session up; on No, or after Esc, it closes.
func (s *starter) trusting(t *testing.T) {
	t.Helper()
	read := func(name string) string {
		b, err := os.ReadFile("../term/testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	asking := func(name string) string {
		return strings.ReplaceAll(read(name), "/Users/alex/code/acme-shop", s.dir("client/blog"))
	}
	no, yes, idle := asking("trust-dialog.txt"), asking("trust-dialog-yes.txt"), read("idle-screen.txt")
	s.comesUp = false
	s.onStart = func() { s.f.term.Screens[newPane] = no }
	s.f.term.OnType = func(id, text string) {
		if id != newPane {
			return
		}
		switch text {
		case term.Down:
			s.f.term.Screens[newPane] = yes
		case term.Enter:
			if s.f.term.Screens[newPane] == yes {
				s.f.term.Screens[newPane] = idle
				s.comesUp = true
			} else {
				s.f.term.Missing[newPane] = true
			}
		case term.Escape:
			s.f.term.Missing[newPane] = true
		}
	}
}

// keysTyped are the keys that reached the new pane, in order.
func (s *starter) keysTyped() []string {
	out := []string{}
	for _, k := range s.f.term.Typed {
		if k.ID == newPane {
			out = append(out, k.Text)
		}
	}
	return out
}

// asked runs /new as far as the question about trusting the folder, and
// returns its two buttons.
func (s *starter) asked(t *testing.T) (yes, cancel string) {
	t.Helper()
	s.trusting(t)
	s.run(t, s.f.msg("/new blog", 0))
	e := s.lastEdit(t)
	if len(e.Keyboard) != 1 || len(e.Keyboard[0]) != 2 {
		t.Fatalf("edit = %+v", e)
	}
	return e.Keyboard[0][0].Data, e.Keyboard[0][1].Data
}

// Claude Code asks before it works in a folder it has not opened, and most
// of a projects folder is that. The owner answers: tg never does on its own.
func TestNewAsksBeforeTrustingAFolder(t *testing.T) {
	s := newStarter(t)
	s.trusting(t)
	s.run(t, s.f.msg("/new blog", 0))
	e := s.lastEdit(t)
	if e.Text != "🔐 Claude Code has not opened <b>client/blog</b> before and asks if you trust it: it could read, edit and run the files there." {
		t.Fatalf("edit = %q", e.Text)
	}
	if got := buttonTexts(e.Keyboard); !reflect.DeepEqual(got, []string{"✅ Yes, trust", "✖️ Cancel"}) {
		t.Fatalf("buttons = %q", got)
	}
	if got := s.keysTyped(); len(got) != 0 {
		t.Fatalf("typed %q without a tap", got)
	}
	if s.f.slept >= startWait {
		t.Fatalf("the wait went on for %s", s.f.slept)
	}
	if p := s.f.storedPick(t); p != (state.Pick{}) {
		t.Fatalf("pick = %+v", p)
	}
}

func TestNewTrustYes(t *testing.T) {
	s := newStarter(t)
	yes, _ := s.asked(t)
	s.run(t, tapUpdate(yes, 1001))
	if got := s.keysTyped(); !reflect.DeepEqual(got, []string{term.Down, term.Enter}) {
		t.Fatalf("keys = %q", got)
	}
	sameEdit(t, s.lastEdit(t), relaytest.Edit{MessageID: 1001, Text: "✅ <b>blog</b> is ready. Next messages go to it."})
	if p := s.f.storedPick(t); p.SessionID != s.calls[0].sess {
		t.Fatalf("pick = %+v", p)
	}
	s.f.toastsAre(t, "starting")
}

// A tab outlives a session: the question on it can be a later session's,
// about another folder, and an old button must not answer that one.
func TestNewOldTrustButtonLeavesAnotherFoldersQuestion(t *testing.T) {
	for _, tap := range []int{0, 1} {
		s := newStarter(t)
		yes, cancel := s.asked(t)
		s.f.term.Screens[newPane] = strings.ReplaceAll(s.f.term.Screens[newPane], s.dir("client/blog"), s.dir("tools/acme-admin"))
		s.run(t, tapUpdate([]string{yes, cancel}[tap], 1001))
		if got := s.keysTyped(); len(got) != 0 {
			t.Fatalf("keys = %q", got)
		}
	}
}

// Nor is another folder's question put to the owner as this one's.
func TestNewDoesNotAskForAnotherFoldersQuestion(t *testing.T) {
	s := newStarter(t)
	s.trusting(t)
	on := s.onStart
	s.onStart = func() {
		on()
		s.f.term.Screens[newPane] = strings.ReplaceAll(s.f.term.Screens[newPane], s.dir("client/blog"), s.dir("tools/acme-admin"))
	}
	s.run(t, s.f.msg("/new blog", 0))
	if got := s.lastEdit(t); len(got.Keyboard) != 0 || !strings.HasPrefix(got.Text, "✗ ") {
		t.Fatalf("edit = %+v", got)
	}
}

// A second /new while the first session comes up must not reach its tab:
// Claude Code does not list that session yet.
func TestNewTellsTheTerminalWhichPanesAreComingUp(t *testing.T) {
	s := newStarter(t)
	s.comesUp = false
	hold, arrived := make(chan struct{}), make(chan struct{}, 2)
	s.f.r.Sleep = func(context.Context, time.Duration) {
		select {
		case arrived <- struct{}{}:
		default:
		}
		<-hold
	}
	s.f.handle(t, s.f.msg("/new blog", 0))
	<-arrived // the first watch waits: nothing reads the fixture now
	s.f.handle(t, s.f.msg("/new acme-admin", 0))
	<-arrived
	s.comesUp = true
	close(hold)
	s.f.r.WaitStarts()
	if len(s.calls) != 2 {
		t.Fatalf("calls = %+v", s.calls)
	}
	found := false
	for _, p := range s.calls[1].panes {
		found = found || p.Target == newPane
	}
	if !found {
		t.Fatalf("the second start was not told about %s: %+v", newPane, s.calls[1].panes)
	}
	if got := s.f.r.startingPanes(); len(got) != 0 {
		t.Fatalf("still starting: %+v", got)
	}
}

// The dialog's default is No: Enter before the highlight has moved would
// refuse what the owner accepted, so Enter waits for it.
func TestNewTrustYesWaitsForTheHighlight(t *testing.T) {
	s := newStarter(t)
	yes, _ := s.asked(t)
	no := s.f.term.Screens[newPane]
	reads := 0
	s.f.term.NextScreen = func(_ int, id string) (string, bool) {
		if id != newPane || len(s.keysTyped()) != 1 {
			return "", false
		}
		if reads++; reads < 3 {
			return no, true // Claude Code has not redrawn yet
		}
		return "", false
	}
	s.run(t, tapUpdate(yes, 1001))
	if reads < 3 {
		t.Fatalf("Enter came after %d reads", reads)
	}
	if got := s.lastEdit(t).Text; !strings.HasPrefix(got, "✅ ") {
		t.Fatalf("edit = %q", got)
	}
}

func TestNewTrustHighlightNeverMoves(t *testing.T) {
	s := newStarter(t)
	yes, _ := s.asked(t)
	no := s.f.term.Screens[newPane]
	s.f.term.NextScreen = func(_ int, id string) (string, bool) { return no, id == newPane }
	s.run(t, tapUpdate(yes, 1001))
	if got := s.keysTyped(); !reflect.DeepEqual(got, []string{term.Down}) {
		t.Fatalf("keys = %q", got)
	}
	if got := s.lastEdit(t).Text; !strings.HasPrefix(got, "✗ ") {
		t.Fatalf("edit = %q", got)
	}
}

func TestNewTrustCancel(t *testing.T) {
	s := newStarter(t)
	_, cancel := s.asked(t)
	s.run(t, tapUpdate(cancel, 1001))
	if got := s.keysTyped(); !reflect.DeepEqual(got, []string{term.Escape}) {
		t.Fatalf("keys = %q", got)
	}
	sameEdit(t, s.lastEdit(t), relaytest.Edit{MessageID: 1001, Text: "✖️ not started"})
	s.f.toastsAre(t, "cancelled")
}

// A late Yes is no Yes: the folder is not trusted on an answer that old, and
// the tab that waited closes.
func TestNewTrustExpired(t *testing.T) {
	s := newStarter(t)
	yes, _ := s.asked(t)
	s.f.now = s.f.now.Add(pendingTTL + time.Minute)
	s.run(t, tapUpdate(yes, 1001))
	if got := s.keysTyped(); !reflect.DeepEqual(got, []string{term.Escape}) {
		t.Fatalf("keys = %q", got)
	}
	sameEdit(t, s.lastEdit(t), relaytest.Edit{MessageID: 1001, Text: newExpired})
	s.f.toastsAre(t, "expired, send /new again")
}

// Answered at the Mac meanwhile: a key typed now would land in the session's
// prompt, so none is, and the watch reports how it went.
func TestNewTrustAnsweredAtTheMac(t *testing.T) {
	for _, tap := range []int{0, 1} {
		s := newStarter(t)
		yes, cancel := s.asked(t)
		idle, _ := os.ReadFile("../term/testdata/idle-screen.txt")
		s.f.term.Screens[newPane] = string(idle)
		s.comesUp = true
		s.run(t, tapUpdate([]string{yes, cancel}[tap], 1001))
		if got := s.keysTyped(); len(got) != 0 {
			t.Fatalf("keys = %q", got)
		}
		if got := s.lastEdit(t).Text; !strings.HasPrefix(got, "✅ ") {
			t.Fatalf("edit = %q", got)
		}
	}
}

func TestNewTrustSessionClosedMeanwhile(t *testing.T) {
	s := newStarter(t)
	yes, _ := s.asked(t)
	s.f.term.Missing[newPane] = true
	s.run(t, tapUpdate(yes, 1001))
	if got := s.keysTyped(); len(got) != 0 {
		t.Fatalf("keys = %q", got)
	}
	sameEdit(t, s.lastEdit(t), relaytest.Edit{MessageID: 1001, Text: "✗ <b>client/blog</b> closed before it was up"})
}

func TestNewTrustTappedTwice(t *testing.T) {
	s := newStarter(t)
	yes, _ := s.asked(t)
	s.run(t, tapUpdate(yes, 1001))
	s.run(t, tapUpdate(yes, 1001))
	if got := s.keysTyped(); !reflect.DeepEqual(got, []string{term.Down, term.Enter}) {
		t.Fatalf("keys = %q", got)
	}
	s.f.toastsAre(t, "starting", "expired, send /new again")
}

func TestNewTrustUnknownButton(t *testing.T) {
	s := newStarter(t)
	yes, _ := s.asked(t)
	s.run(t, tapUpdate(strings.TrimSuffix(yes, newTrust)+"0", 1001))
	if got := s.keysTyped(); len(got) != 0 {
		t.Fatalf("keys = %q", got)
	}
	s.f.toastsAre(t, "unknown button")
}

// What starts is the folder as checked, links resolved: a link swapped
// afterwards changes nothing.
func TestNewStartsInTheResolvedFolder(t *testing.T) {
	s := newStarter(t)
	link := s.dir("link")
	if err := os.Symlink(s.dir("client/blog"), link); err != nil {
		t.Fatal(err)
	}
	if err := s.f.store.PutPending("k1", state.Parked{Projects: []string{link}, Created: s.f.now}); err != nil {
		t.Fatal(err)
	}
	s.run(t, tapUpdate("n:k1:0", 1001))
	if len(s.calls) != 1 || s.calls[0].dir != s.dir("client/blog") || s.calls[0].name != "blog" {
		t.Fatalf("calls = %+v", s.calls)
	}
}

// The check is the last thing before the start: listing the sessions takes
// a while, and the folder may go meanwhile.
func TestNewChecksTheFolderAfterListingSessions(t *testing.T) {
	s := newStarter(t)
	base := s.f.r.Agents
	s.f.r.Agents = func(ctx context.Context) ([]session.Agent, error) {
		if err := os.RemoveAll(s.dir("client/blog")); err != nil {
			t.Fatal(err)
		}
		return base(ctx)
	}
	if err := s.f.store.PutPending("k1", state.Parked{Projects: []string{s.dir("client/blog")}, Created: s.f.now}); err != nil {
		t.Fatal(err)
	}
	s.run(t, tapUpdate("n:k1:0", 1001))
	if len(s.calls) != 0 {
		t.Fatalf("started: %+v", s.calls)
	}
}

// A list taken just before /new picks its session must not count that
// session as ended: the owner was told the next messages go to it.
func TestPickMadeWhileListingStands(t *testing.T) {
	for name, send := range map[string]func(s *starter, t *testing.T){
		"a text": func(s *starter, t *testing.T) {
			s.f.handle(t, s.f.msg("hello", 0))
			s.f.typedOnce(t, newPane, " hello\n")
		},
		"/sessions": func(s *starter, t *testing.T) {
			s.f.handle(t, s.f.msg("/sessions", 0))
			if last := s.f.lastSent(t); !strings.Contains(last.Text, "📌 now: <b>blog</b>") {
				t.Fatalf("last = %+v", last)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := newStarter(t)
			s.run(t, s.f.msg("/new blog", 0))
			pick := s.f.storedPick(t)
			if err := s.f.store.SavePick(state.Pick{}); err != nil {
				t.Fatal(err)
			}
			base := s.f.r.Agents
			calls := 0
			s.f.r.Agents = func(ctx context.Context) ([]session.Agent, error) {
				agents, err := base(ctx)
				if calls++; calls > 1 {
					return agents, err
				}
				// This list is the older one: the pick lands right after it.
				if err := s.f.store.SavePick(pick); err != nil {
					t.Fatal(err)
				}
				return agents[:len(agents)-1], err
			}
			send(s, t)
			if p := s.f.storedPick(t); p.SessionID != pick.SessionID {
				t.Fatalf("pick = %+v, want %+v", p, pick)
			}
		})
	}
}

// A list that failed says nothing about the session: the pick stays, and the
// message is refused rather than sent elsewhere.
func TestPickStandsWhenTheSecondListFails(t *testing.T) {
	for name, wantSent := range map[string]string{"hello": "cannot list sessions: ", "/sessions": "acme-shop-92"} {
		t.Run(name, func(t *testing.T) {
			s := newStarter(t)
			s.run(t, s.f.msg("/new blog", 0))
			pick := s.f.storedPick(t)
			base := s.f.r.Agents
			calls := 0
			s.f.r.Agents = func(ctx context.Context) ([]session.Agent, error) {
				agents, err := base(ctx)
				if calls++; calls > 1 {
					return nil, errors.New("claude agents timed out")
				}
				return agents[:len(agents)-1], err // older than the pick
			}
			s.f.term.Typed = nil
			s.f.handle(t, s.f.msg(name, 0))
			if p := s.f.storedPick(t); p.SessionID != pick.SessionID {
				t.Fatalf("pick = %+v, want %+v", p, pick)
			}
			if len(s.f.term.Typed) != 0 {
				t.Fatalf("typed = %+v", s.f.term.Typed)
			}
			if last := s.f.lastSent(t); !strings.Contains(last.Text, wantSent) {
				t.Fatalf("last = %+v", last)
			}
		})
	}
}

// A session that really ended still loses its pick.
func TestPickOfASessionGoneFromBothListsIsDropped(t *testing.T) {
	s := newStarter(t)
	s.run(t, s.f.msg("/new blog", 0))
	s.comesUp = false
	s.f.handle(t, s.f.msg("hello", 0))
	if p := s.f.storedPick(t); p != (state.Pick{}) {
		t.Fatalf("pick = %+v", p)
	}
}

// tg relay --once ends after its batch: a session on its way up still gets
// its outcome.
func TestRunOnceWaitsForTheNewSession(t *testing.T) {
	s := newStarter(t)
	s.comesUp = false
	// The loop reads the clock too, so this sleep leaves the fixture's clock
	// alone: it runs beside the loop, unlike every other test's.
	s.onStart = func() {
		s.f.r.Sleep = func(context.Context, time.Duration) { s.comesUp = true }
	}
	l := &Loop{Poll: &fakePoller{batches: [][]telegram.Update{{s.f.msg("/new blog", 0)}}}, Router: s.f.r, Store: s.f.store, Once: true, Log: func(string, ...any) {}}
	if err := l.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := s.lastEdit(t).Text; !strings.HasPrefix(got, "✅ ") {
		t.Fatalf("edit = %q", got)
	}
}

// The tail is cut after masking: a secret cut first would no longer match,
// and most of it would go out.
func TestNewTailMasksASecretAtTheCut(t *testing.T) {
	const secret = "123456:ABCDEFsecretbotABCDEFsecretbotABCDEF"
	s := newStarter(t)
	s.f.r.Secrets = []string{secret}
	s.comesUp = false
	s.onStart = func() { s.f.term.Screens[newPane] = strings.Repeat("a", 60) + secret + "\n" }
	s.run(t, s.f.msg("/new blog", 0))
	if got := s.lastEdit(t).Text; strings.Contains(got, secret[:12]) {
		t.Fatalf("part of the secret went out: %q", got)
	}
}

// Cancel that did not reach the dialog must not say it did: the tab still
// waits there.
func TestNewTrustCancelFails(t *testing.T) {
	s := newStarter(t)
	_, cancel := s.asked(t)
	s.f.term.TypeErr = errors.New("agtermctl session: not responding")
	s.run(t, tapUpdate(cancel, 1001))
	got := s.lastEdit(t).Text
	if !strings.HasPrefix(got, "✗ could not answer for <b>client/blog</b>: ") || !strings.HasSuffix(got, "Answer it at the machine.") {
		t.Fatalf("edit = %q", got)
	}
	s.f.toastsAre(t, "could not answer")
}

// The question must reach the phone: the tab waits on it.
func TestNewTrustQuestionSentAnewWhenTheEditFails(t *testing.T) {
	s := newStarter(t)
	s.trusting(t)
	on := s.onStart
	s.onStart = func() { on(); s.f.api.EditErr = errors.New("telegram is away") }
	s.run(t, s.f.msg("/new blog", 0))
	last := s.f.lastSent(t)
	if !strings.HasPrefix(last.Text, "🔐 ") || len(last.InlineKeyboard) != 1 {
		t.Fatalf("last = %+v", last)
	}
}
