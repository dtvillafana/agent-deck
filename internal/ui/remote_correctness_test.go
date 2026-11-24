// Remote-deck correctness (#2170): a stale fleet poll must not undo an
// action, a broken config must not wipe the remotes, and a refused rename
// must revert with a message instead of snapping back silently.

package ui

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

func remoteTitles(h *Home, remote string) []string {
	h.remoteSessionsMu.RLock()
	defer h.remoteSessionsMu.RUnlock()
	var out []string
	for _, s := range h.remoteSessions[remote] {
		out = append(out, s.Title)
	}
	return out
}

func TestRemoteFetch_StaleResultIsIgnored(t *testing.T) {
	home := newTestHomeWithItems(100, 30, nil)
	fresh := map[string][]session.RemoteSessionInfo{"box": {{ID: "a", Title: "kept"}}}
	stale := map[string][]session.RemoteSessionInfo{"box": {{ID: "a", Title: "kept"}, {ID: "b", Title: "deleted-meanwhile"}}}

	model, _ := home.Update(remoteSessionsFetchedMsg{gen: 5, sessions: fresh})
	h := model.(*Home)
	model, _ = h.Update(remoteSessionsFetchedMsg{gen: 4, sessions: stale})
	h = model.(*Home)

	if got := remoteTitles(h, "box"); len(got) != 1 || got[0] != "kept" {
		t.Fatalf("a fetch older than the last applied one must be ignored; sessions = %v", got)
	}
	if h.remotesFetchActive {
		t.Fatal("a stale result must still release the in-flight guard")
	}
}

func TestRemoteFetch_ConfigErrorKeepsRemotes(t *testing.T) {
	home := newTestHomeWithItems(100, 30, nil)
	model, _ := home.Update(remoteSessionsFetchedMsg{gen: 1, sessions: map[string][]session.RemoteSessionInfo{"box": {{ID: "a", Title: "kept"}}}})
	h := model.(*Home)
	model, _ = h.Update(remoteSessionsFetchedMsg{gen: 2, configErr: errors.New("toml: line 3: bad key")})
	h = model.(*Home)

	if got := remoteTitles(h, "box"); len(got) != 1 {
		t.Fatalf("an unreadable config must not drop cached remotes; sessions = %v", got)
	}
	if h.err == nil {
		t.Fatal("an unreadable config must be reported in the footer")
	}
}

func TestRemoteRename_RefusedRevertsTitle(t *testing.T) {
	home := newTestHomeWithItems(100, 30, nil)
	home.remoteSessions = map[string][]session.RemoteSessionInfo{"box": {{ID: "a", Title: "old"}}}

	if old := home.setRemoteSessionTitle("box", "a", "new"); old != "old" {
		t.Fatalf("setRemoteSessionTitle returned %q, want the previous title", old)
	}
	model, _ := home.Update(remoteRenameResultMsg{remoteName: "box", sessionID: "a", oldTitle: "old", newTitle: "new", err: errors.New("title already taken")})
	h := model.(*Home)

	if got := remoteTitles(h, "box"); got[0] != "old" {
		t.Fatalf("a refused rename must revert the cached title; got %v", got)
	}
	if h.err == nil {
		t.Fatal("a refused rename must be reported")
	}
}

func TestRemoteCreate_QueuedIsNoticeNotError(t *testing.T) {
	home := newTestHomeWithItems(100, 30, nil)
	msg := remoteCreateOutcomeMsg("box", session.RemoteAddOptions{}, "",
		&session.RemoteSessionQueuedError{ID: "x", Title: "job", Group: "misc"}).(remoteSessionCreatedMsg)
	if msg.err != nil || msg.created == nil || msg.created.ID != "x" ||
		msg.created.Status != string(session.StatusQueued) || msg.created.Group != "misc" {
		t.Fatalf("expected a confirmed queued row, got %+v", msg)
	}
	home.Update(msg)
	if !isFooterNotice(home.err) || !strings.Contains(home.err.Error(), "group 'misc'") {
		t.Fatalf("expected a queue notice naming the group, got %v", home.err)
	}
	if got := remoteTitles(home, "box"); len(got) != 1 || got[0] != "job" {
		t.Fatalf("expected the queued row immediately, got %v", got)
	}
}

func TestRemoteQueuedRowDoesNotAttach(t *testing.T) {
	for _, tc := range []struct {
		name     string
		embedded bool
		key      tea.KeyMsg
	}{
		{"classic Enter", false, tea.KeyMsg{Type: tea.KeyEnter}},
		{"embedded Enter", true, tea.KeyMsg{Type: tea.KeyEnter}},
		{"embedded Alt+Enter", true, tea.KeyMsg{Type: tea.KeyEnter, Alt: true}},
		{"classic Shift+Enter", false, *ParseCSIu([]byte("\x1b[13;2u"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rs := session.RemoteSessionInfo{ID: "x", Title: "job", Group: "misc", Status: string(session.StatusQueued)}
			home := newTestHomeWithItems(100, 30, []session.Item{{Type: session.ItemTypeRemoteSession, RemoteName: "box", RemoteSession: &rs}})
			home.embeddedLayout = tc.embedded
			_, cmd := home.handleMainKey(tc.key)
			if cmd != nil || home.isAttaching.Load() || home.insertMode {
				t.Fatal("queued row tried to attach before it had a pane")
			}
			if !isFooterNotice(home.err) || !strings.Contains(home.err.Error(), "group 'misc'") {
				t.Fatalf("expected a queue notice naming the group, got %v", home.err)
			}
		})
	}
}

// A session the remote just confirmed is drawn the moment the attach
// returns, not at the next fetch (the maintainer saw the row arrive "after
// some time" when detaching from a freshly created remote session).
func TestRemoteCreate_ConfirmedRowIsDrawnAtOnce(t *testing.T) {
	home := newTestHomeWithItems(100, 30, nil)
	home.remoteSessions = map[string][]session.RemoteSessionInfo{"box": {{ID: "a", Title: "old", Group: "work"}}}

	model, cmd := home.Update(remoteSessionCreatedMsg{created: &session.RemoteSessionInfo{ID: "new", Title: "fresh", Tool: "claude", RemoteName: "box"}})
	h := model.(*Home)

	got := remoteTitles(h, "box")
	if len(got) != 2 || got[1] != "fresh" {
		t.Fatalf("confirmed session must be in the cache at once; titles = %v", got)
	}
	h.remoteSessionsMu.RLock()
	group := h.remoteSessions["box"][1].Group
	h.remoteSessionsMu.RUnlock()
	if group != session.DefaultGroupPath {
		t.Fatalf("an ungrouped session lands in the default group like the remote does; got %q", group)
	}
	if cmd == nil {
		t.Fatal("the create return must still schedule the reconciling fetch")
	}
}
