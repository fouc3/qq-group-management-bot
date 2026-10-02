package admincmd

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/fouc3/qq-group-management-bot/internal/store"
)

// stubBlacklist stands in for the list the data layer would provide.
//
// It records what it was asked to do as well as what it holds, because the thing
// worth asserting is often that nothing was asked at all.
type stubBlacklist struct {
	mu      sync.Mutex
	entries []store.Barred
	added   []store.Barred
	removed []string
	fail    bool
}

func (s *stubBlacklist) Add(_ context.Context, entry store.Barred) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return errStub
	}
	s.added = append(s.added, entry)
	s.entries = append(s.entries, entry)
	return nil
}

func (s *stubBlacklist) Remove(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return errStub
	}
	s.removed = append(s.removed, key)
	return nil
}

func (s *stubBlacklist) List(_ context.Context, _ int) ([]store.Barred, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return nil, errStub
	}
	return append([]store.Barred(nil), s.entries...), nil
}

func (s *stubBlacklist) snapshot() (added []store.Barred, removed []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]store.Barred(nil), s.added...), append([]string(nil), s.removed...)
}

// TestBlacklistAddTakesAMention covers the common case: the operator mentions
// somebody in the group.
func TestBlacklistAddTakesAMention(t *testing.T) {
	h := newHarness(t, baseSection)
	list := &stubBlacklist{}
	h.handler.SetBlacklist(list)

	h.send("<@BOT-OPENID> /黑名单 add <@TARGET-OPENID> 刷屏", testAdmin,
		testGroupOpenID, "BOT-OPENID")

	added, _ := list.snapshot()
	if len(added) != 1 {
		t.Fatalf("added %d entries, want 1", len(added))
	}
	if added[0].MemberOpenID != testTarget {
		t.Errorf("added %q, want the mentioned member", added[0].MemberOpenID)
	}
	if added[0].Reason != "刷屏" {
		t.Errorf("reason = %q, want the words that followed", added[0].Reason)
	}
	if added[0].ID == "" {
		t.Error("the entry has no id, so nothing could refer to it later")
	}
	if added[0].AddedBy == "" {
		t.Error("the entry does not say who added it")
	}
}

// TestBlacklistAddTakesABareOpenid covers the other case, and the reason both
// are accepted: somebody who never applied here has an openid from elsewhere and
// no way to be mentioned in the group.
func TestBlacklistAddTakesABareOpenid(t *testing.T) {
	h := newHarness(t, baseSection)
	list := &stubBlacklist{}
	h.handler.SetBlacklist(list)

	h.send("<@BOT-OPENID> /黑名单 add 82A40FDE81459B3F19850E543A4FCF4F", testAdmin,
		testGroupOpenID, "BOT-OPENID")

	added, _ := list.snapshot()
	if len(added) != 1 || added[0].MemberOpenID != "82A40FDE81459B3F19850E543A4FCF4F" {
		t.Fatalf("added %+v, want the openid that was written", added)
	}
}

// TestBlacklistRemoveRefusesWhatIsNotThere covers the refusal that keeps the
// answer honest: reporting a removal that never happened would leave an
// administrator believing the list had changed.
func TestBlacklistRemoveRefusesWhatIsNotThere(t *testing.T) {
	h := newHarness(t, baseSection)
	list := &stubBlacklist{}
	h.handler.SetBlacklist(list)

	h.send("<@BOT-OPENID> /黑名单 remove 82A40FDE81459B3F19850E543A4FCF4F", testAdmin,
		testGroupOpenID, "BOT-OPENID")

	_, removed := list.snapshot()
	if len(removed) != 0 {
		t.Errorf("removed %v, want nothing to be removed", removed)
	}
	if reply := h.lastReply(); !strings.Contains(reply, "没有") {
		t.Errorf("reply = %q, want it to say the entry is not there", reply)
	}
}

// TestBlacklistRemoveTakesWhatIsThere covers the successful removal.
func TestBlacklistRemoveTakesWhatIsThere(t *testing.T) {
	h := newHarness(t, baseSection)
	list := &stubBlacklist{entries: []store.Barred{{
		ID: "ENTRY-1", MemberOpenID: "82A40FDE81459B3F19850E543A4FCF4F",
	}}}
	h.handler.SetBlacklist(list)

	h.send("<@BOT-OPENID> /黑名单 remove 82A40FDE81459B3F19850E543A4FCF4F", testAdmin,
		testGroupOpenID, "BOT-OPENID")

	_, removed := list.snapshot()
	if len(removed) != 1 || removed[0] != "82A40FDE81459B3F19850E543A4FCF4F" {
		t.Errorf("removed %v, want the identity that was named", removed)
	}
}

// TestBlacklistListRendersWhatIsThere covers the listing, including an entry
// that has expired: it stays in the list and is marked, rather than vanishing.
func TestBlacklistListRendersWhatIsThere(t *testing.T) {
	h := newHarness(t, baseSection)
	list := &stubBlacklist{entries: []store.Barred{
		{ID: "ENTRY-1", MemberOpenID: "MEMBER-1", Reason: "刷屏", AddedAt: 1},
		{ID: "ENTRY-2", UnionOpenID: "UNION-2", ExpiresAt: 1},
	}}
	h.handler.SetBlacklist(list)

	h.send("<@BOT-OPENID> /黑名单 list", testAdmin, testGroupOpenID, "BOT-OPENID")

	reply := h.lastReply()
	for _, want := range []string{"MEMBER-1", "刷屏", "union:UNION-2", "已过期"} {
		if !strings.Contains(reply, want) {
			t.Errorf("reply = %q, want it to contain %q", reply, want)
		}
	}
}

// TestBlacklistIsAdministratorsOnly covers the boundary: the list is a management
// action, so the ordinary-member refusal has to reach it too.
func TestBlacklistIsAdministratorsOnly(t *testing.T) {
	h := newHarness(t, baseSection)
	list := &stubBlacklist{}
	h.handler.SetBlacklist(list)

	h.send("<@BOT-OPENID> /黑名单 add <@TARGET-OPENID>", "SOMEONE-ELSE",
		testGroupOpenID, "BOT-OPENID")

	added, removed := list.snapshot()
	if len(added) != 0 || len(removed) != 0 {
		t.Errorf("the list was touched by a member: added %v, removed %v", added, removed)
	}
	if reply := h.lastReply(); !strings.Contains(reply, "没有权限") {
		t.Errorf("reply = %q, want a refusal", reply)
	}
}

// TestBlacklistWithoutADataLayerSaysSo covers a deployment that has no list: the
// command has to say so rather than fail in a way nobody can read.
func TestBlacklistWithoutADataLayerSaysSo(t *testing.T) {
	h := newHarness(t, baseSection)

	h.send("<@BOT-OPENID> /黑名单 list", testAdmin, testGroupOpenID, "BOT-OPENID")

	if reply := h.lastReply(); !strings.Contains(reply, "数据层") {
		t.Errorf("reply = %q, want it to explain that there is no list", reply)
	}
}
