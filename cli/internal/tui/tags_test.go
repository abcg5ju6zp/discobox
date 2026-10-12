package tui

import (
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func taggedSandboxes() []Sandbox {
	boxes := testSandboxes()
	boxes[0].Tags = []string{"ticket=ENG-12", "wip"}
	boxes[1].Tags = []string{"wip"}
	return boxes
}

func headerLine(m *Model) string { return ansi.Strip(m.viewHeaderLeft()) }

// tagRows are the tags the filter card offers, as it reads them.
func tagRows(m *Model) []string {
	var out []string
	for _, row := range m.filterDialog().filter.rows() {
		if row.group == "Tag" {
			out = append(out, row.label)
		}
	}
	return out
}

// A project nobody tags offers no tags: a group that can only say "all tags"
// is a group spent saying nothing.
func TestThereAreNoTagsToPickUntilSomethingIsTagged(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	if got := tagRows(m); len(got) != 0 {
		t.Fatalf("the card offers tags %q with nothing tagged", got)
	}
}

// Once a discobox is tagged the card offers every tag after the folders, each
// with how many boxes carry it, and marking one narrows the list to the boxes
// carrying it.
func TestATagNarrowsTheList(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeSource(taggedSandboxes()...))
	filterTo(t, m, "Folder: "+m.session.folder().label)
	if got := tagRows(m); len(got) != 3 || got[0] != allTags || got[1] != "#ticket=ENG-12" || got[2] != "#wip" {
		t.Fatalf("tags = %q, want every box then each tag in order", got)
	}
	view := m.filterDialog().view(m.st, &m.zones, 120, 40)
	for _, want := range []string{allTags, "#ticket=ENG-12", "#wip", "2 boxes", "ticket set to this value"} {
		if !strings.Contains(view, want) {
			t.Errorf("the card is missing %q:\n%s", want, view)
		}
	}
	rows := func() []string {
		var ids []string
		for _, s := range m.list.rows() {
			ids = append(ids, s.ID)
		}
		return ids
	}
	if got := rows(); len(got) != 2 {
		t.Fatalf("rows = %v, want both of this folder's boxes", got)
	}

	filterTo(t, m, "#wip")
	if got := rows(); !slices.Equal(m.list.tags, []string{"wip"}) || len(got) != 2 {
		t.Fatalf("tags %q list %v, want both boxes tagged wip", m.list.tags, got)
	}
	filterTo(t, m, "#ticket=ENG-12")
	if !slices.Equal(m.list.tags, []string{"ticket=ENG-12", "wip"}) || !strings.Contains(headerLine(m), "#ticket=ENG-12 #wip") {
		t.Fatalf("tags = %q, header = %q, want both tags", m.list.tags, headerLine(m))
	}
	if got := rows(); len(got) != 1 || got[0] != "sbx_one" {
		t.Fatalf("rows = %v, want only the box carrying both tags", got)
	}
	filterTo(t, m, allTags)
	if len(m.list.tags) != 0 || strings.Contains(headerLine(m), "#") {
		t.Fatalf("tags = %q, header = %q, want every box and no tag named", m.list.tags, headerLine(m))
	}
}

// A tag that goes while the filter is on it stays the filter's choice rather
// than vanishing from under it, so what the header says is still what is shown.
func TestAChosenTagOutlivesItsLastBox(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeSource(taggedSandboxes()...))
	m.list.tags = []string{"wip"}
	untagged := testSandboxes()
	m.list.setAll(untagged)
	if got := tagRows(m); !slices.Contains(got, "#wip") || !strings.Contains(headerLine(m), "#wip") {
		t.Fatalf("header = %q, tags = %q, want the filter still naming #wip", headerLine(m), got)
	}
	if len(m.list.rows()) != 0 {
		t.Fatalf("rows = %d, want none: nothing carries the tag now", len(m.list.rows()))
	}
}

// Space turns a tag on and off again, so several can be marked; Enter only
// ever turns the row it is on on, so the key that applies the card never drops
// the tag under the cursor.
func TestSpaceTogglesTagsAndEnterOnlyMarks(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeSource(taggedSandboxes()...))
	m.dialog = m.filterDialog()
	p := m.dialog.filter
	marked := func() []string { return p.list.tags }

	markFilter(t, m, "#wip")
	markFilter(t, m, "#ticket=ENG-12")
	if got := marked(); !slices.Equal(got, []string{"ticket=ENG-12", "wip"}) {
		t.Fatalf("marked %q, want both tags", got)
	}
	if got := dialogText(m); !strings.Contains(got, "■ #wip") || !strings.Contains(got, "□ "+allTags) {
		t.Fatalf("the tags are not drawn as boxes, marked:\n%s", got)
	}
	markFilter(t, m, "#wip")
	if got := marked(); !slices.Equal(got, []string{"ticket=ENG-12"}) {
		t.Fatalf("marked %q after Space on a marked tag, want it let go", got)
	}
	markFilter(t, m, "#ticket=ENG-12")
	if got := marked(); len(got) != 0 {
		t.Fatalf("marked %q, want none", got)
	}

	markFilter(t, m, "#wip")
	send(t, m, keyPress("enter"))
	if !slices.Equal(m.list.tags, []string{"wip"}) {
		t.Fatalf("tags = %q after Enter on a marked tag, want it kept", m.list.tags)
	}

	m.dialog = m.filterDialog()
	markFilter(t, m, "#ticket=ENG-12")
	markFilter(t, m, allTags)
	if got := m.dialog.filter.list.tags; len(got) != 0 {
		t.Fatalf("marked %q after all tags, want none", got)
	}
}

// A discobox has one value for a key, so marking another value of a key
// already marked takes its place rather than narrowing to what nothing carries.
func TestATagReplacesAnotherValueOfItsKey(t *testing.T) {
	t.Parallel()
	var l sandboxList
	l.addTag("wip")
	l.addTag("ticket=ENG-12")
	l.addTag("ticket=ENG-13")
	if want := []string{"ticket=ENG-13", "wip"}; !slices.Equal(l.tags, want) {
		t.Fatalf("tags = %q, want %q", l.tags, want)
	}
	l.addTag("ticket")
	if want := []string{"ticket", "wip"}; !slices.Equal(l.tags, want) {
		t.Fatalf("tags = %q, want a bare key in place of its value: %q", l.tags, want)
	}
}

// An `issue=` or `pr=` tag on a discobox cut from a GitHub repository is the
// number of an issue or pull request there; any other tag, or one of those on
// a discobox with no GitHub repository to number it in, points nowhere.
func TestATagThatNumbersGitHubWorkLinksToIt(t *testing.T) {
	t.Parallel()
	box := Sandbox{Repository: "https://github.com/acme/foo"}
	for _, tc := range []struct{ tag, want string }{
		{"issue=4", "https://github.com/acme/foo/issues/4"},
		{"pr=123", "https://github.com/acme/foo/pull/123"},
		{"issue", ""},
		{"issue=", ""},
		{"issue=04", ""},
		{"issue=ENG-12", ""},
		{"pr=-1", ""},
		{"ticket=4", ""},
		{"wip", ""},
	} {
		if got := box.tagURL(tc.tag); got != tc.want {
			t.Errorf("tagURL(%q) = %q, want %q", tc.tag, got, tc.want)
		}
	}
	if got := (Sandbox{}).tagURL("issue=4"); got != "" {
		t.Errorf("tagURL on a discobox with no GitHub repository = %q, want nothing", got)
	}
}

// githubTaggedSandboxes is the listing with the first discobox, cut from a
// GitHub repository, working on an issue and a pull request in it.
func githubTaggedSandboxes() []Sandbox {
	boxes := testSandboxes()
	boxes[0].Name = "reaper"
	boxes[0].Repository = "https://github.com/acme/foo"
	boxes[0].Tags = []string{"issue=4", "pr=9", "wip"}
	return boxes
}

// On the list, the tag is the link: a plain click on `#issue=4` opens that
// discobox's workspace with the issue as a tab beside it, as the workspace
// header's link does, rather than only pointing at the row or going out to a
// browser. It is drawn as the OSC 8 a terminal's own Ctrl-click follows, and
// that click is left to the terminal.
func TestClickingAnIssueTagOnTheListOpensTheIssue(t *testing.T) {
	t.Parallel()
	ds := pullSource()
	d, m := openList(t, ds)
	// Wide enough for the tags beside the name: they give way before it does.
	d.dispatch(tea.WindowSizeMsg{Width: 180, Height: 40})
	d.wait("the tags", func() bool { return strings.Contains(plainFrame(m), "#issue=4 #pr=9 #wip") })

	opened := make(chan string, 4)
	m.openOS = func(url string) error { opened <- url; return nil }

	if frame := rawFrame(m); !strings.Contains(frame, "https://github.com/acme/foo/issues/4") ||
		!strings.Contains(frame, "https://github.com/acme/foo/pull/9") {
		t.Fatalf("the issue and pull request tags are not links:\n%q", frame)
	}

	// Ctrl-click is the terminal's: it follows the link, and the window does
	// nothing of its own.
	x, y := at(t, m, "#pr=9")
	send(t, m,
		tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft, Mod: tea.ModCtrl},
		tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft, Mod: tea.ModCtrl},
	)
	if m.inPanes() || issueTabOf(m) != nil {
		t.Fatalf("a Ctrl-click on #pr=9 opened the workspace, want it left to the terminal")
	}

	// A tag that numbers nothing is the row, as it was.
	x, y = at(t, m, "#wip")
	tap(t, m, x, y)
	select {
	case got := <-opened:
		t.Errorf("clicking #wip opened %q, want nothing opened", got)
	default:
	}
	// Nor has the wheel: over a linked tag it scrolls the list, as it does
	// anywhere else on the row, and opens nothing.
	m.list.cursor = 0
	x, y = at(t, m, "#issue=4")
	send(t, m, tea.MouseWheelMsg{X: x, Y: y, Button: tea.MouseWheelDown})
	if m.list.cursor == 0 {
		t.Errorf("the wheel over #issue=4 did not move the list on")
	}
	select {
	case got := <-opened:
		t.Errorf("the wheel over #issue=4 opened %q, want nothing opened", got)
	default:
	}

	// The right button has nothing to do with a link, so it is the row's menu
	// there as anywhere else on the row.
	x, y = at(t, m, "#issue=4")
	send(t, m,
		tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseRight},
		tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseRight},
	)
	if m.dialog == nil {
		t.Errorf("right-clicking #issue=4 opened no menu, want the row's actions")
	}
	d.key("esc")

	// The plain click: the workspace, and the pull request beside it.
	x, y = at(t, m, "#pr=9")
	slowClock(m)
	tap(t, m, x, y)
	d.wait("the pull request tab", func() bool { return strings.Contains(plainFrame(m), "Reap only stopped boxes") })
	p := issueTabOf(m)
	if !m.inPanes() || p == nil || p.name() != "PR #9" || m.focusedPane() != p || m.paneBox.Name != "reaper" {
		t.Errorf("clicking #pr=9 did not open reaper's workspace on the pull request:\n%s", plainFrame(m))
	}
	select {
	case got := <-opened:
		t.Errorf("clicking #pr=9 opened %q in a browser, want the tab", got)
	default:
	}
}

// A second tag clicked on the row while its workspace is still attaching joins
// the first tab: the workspace is not opened over again, which would close it.
func TestASecondTagClickedWhileAttachingKeepsTheFirstTab(t *testing.T) {
	t.Parallel()
	ds := pullSource()
	_, m := openList(t, ds)
	gen := m.wsGen
	// Both presses in one go, so nothing of the attach has landed between.
	m.press(hit{kind: hitPull, idx: 9, url: "https://github.com/acme/foo/pull/9", box: m.list.rows()[0].ID}, 1)
	opened := m.wsGen
	m.press(hit{kind: hitIssue, idx: 4, url: "https://github.com/acme/foo/issues/4", box: m.list.rows()[0].ID}, 1)
	if opened == gen || m.wsGen != opened {
		t.Fatalf("the workspace generation went %d, %d, %d; want it opened once", gen, opened, m.wsGen)
	}
	var names []string
	for _, p := range m.shells.all() {
		if p.issue != nil {
			names = append(names, p.name())
		}
	}
	if len(names) != 2 {
		t.Errorf("tabs = %q, want the pull request and the issue both", names)
	}
}

// The issue tag opens the issue the same way.
func TestClickingAnIssueTagOnTheListOpensTheIssueTab(t *testing.T) {
	t.Parallel()
	ds := issueSource()
	d, m := openList(t, ds)
	d.dispatch(tea.WindowSizeMsg{Width: 180, Height: 40})
	d.wait("the tags", func() bool { return strings.Contains(plainFrame(m), "#issue=4 #pr=9 #wip") })
	x, y := at(t, m, "#issue=4")
	tap(t, m, x, y)
	d.wait("the issue tab", func() bool { p := issueTabOf(m); return p != nil && p.issue.issue != nil })
	if p := issueTabOf(m); !m.inPanes() || p.name() != "issue #4" || m.focusedPane() != p {
		t.Errorf("clicking #issue=4 did not open the workspace on the issue:\n%s", plainFrame(m))
	}
}

// The workspace says which issue and pull request it is working on at the top,
// each a link to it. A plain click on either opens it beside the workspace
// instead (issue_test.go, pull_test.go); the link is still what Ctrl-click
// follows.
func TestTheWorkspaceHeaderLinksTheIssueAndPullRequest(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(githubTaggedSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the header", func() bool { return strings.Contains(plainFrame(m), "issue #4 · PR #9") })

	if _, y := at(t, m, "issue #4"); y != 0 {
		t.Fatalf("issue #4 is drawn on row %d, want the header", y)
	}
	if frame := rawFrame(m); !strings.Contains(frame, "https://github.com/acme/foo/issues/4") {
		t.Errorf("issue #4 is not a link:\n%q", frame)
	}
	if _, y := at(t, m, "PR #9"); y != 0 {
		t.Fatalf("PR #9 is drawn on row %d, want the header", y)
	}
	if frame := rawFrame(m); !strings.Contains(frame, "https://github.com/acme/foo/pull/9") {
		t.Errorf("PR #9 is not a link:\n%q", frame)
	}
}

// Without a GitHub repository, `issue=4` is only a tag: nothing in the
// workspace header claims to know whose issue it is.
func TestTheWorkspaceHeaderLeavesAnUnnumberedIssueOut(t *testing.T) {
	t.Parallel()
	boxes := githubTaggedSandboxes()
	boxes[0].Repository = ""
	if field := workField(newStyles(false), boxes[0]); !field.empty() {
		t.Fatalf("workField = %q, want nothing without a repository", field.text())
	}
}
