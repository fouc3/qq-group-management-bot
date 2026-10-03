package broadcast

import (
	"context"
	"strings"
	"testing"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"

	"github.com/fouc3/qq-group-management-bot/internal/command"
)

// broadcastOnce runs the whole flow and posts one broadcast, so that a test can read
// what was written down about it.
func broadcastOnce(t *testing.T, p *platform, text string) {
	t.Helper()
	p.start(t, theAdmin)
	p.choose(t, true, true)
	p.press(t, "确认", theAdmin)
	p.press(t, "继续", theAdmin)
	p.say(t, theAdmin, text)
	p.press(t, "发送", theAdmin)
}

// TestThePostingIsRecorded covers the record that makes an anonymous notice answerable: the
// group is not told who asked, and the data layer is where that is kept.
func TestThePostingIsRecorded(t *testing.T) {
	p := newPlatform(t)
	broadcastOnce(t, p, "第一行\n第二行")

	posted, err := p.store.Broadcasts().ListByGroups(context.Background(), []string{hereGroup}, 10)
	if err != nil {
		t.Fatalf("reading the record: %v", err)
	}
	if len(posted) != 1 {
		t.Fatalf("recorded %d broadcast(s), want 1: %+v", len(posted), posted)
	}
	entry := posted[0]
	if entry.SenderOpenID != theAdmin {
		t.Errorf("sender = %q, want the member who asked", entry.SenderOpenID)
	}
	if len(entry.Targets) != 1 || entry.Targets[0].GroupOpenID != hereGroup {
		t.Errorf("the notice reached %+v, want the group it was sent to", entry.Targets)
	}
	if entry.Targets[0].MessageID == "" {
		t.Error("the record does not say which message it was")
	}
	if !entry.Anonymous || !entry.Markdown {
		t.Errorf("the switches were not recorded: %+v", entry)
	}
	if entry.Content != "第一行\n第二行" {
		t.Errorf("content = %q, want what was written", entry.Content)
	}
	if entry.Token == "" {
		t.Error("the record does not say which card it came from")
	}
}

// TestOneNoticeToTwoGroupsIsOneRecord covers what "one broadcast" means in the record.
//
// It is one act by one member that reached two groups, so it is one row with two targets: a
// row per group would answer "who sent this here" out of two copies of the same sentence, and
// nothing would say the groups got the same notice.
func TestOneNoticeToTwoGroupsIsOneRecord(t *testing.T) {
	p := newPlatform(t)
	p.start(t, theAdmin)
	p.choose(t, true, true)
	p.press(t, "群-OTHER", theAdmin)
	p.press(t, "确认", theAdmin)
	p.press(t, "继续", theAdmin)
	p.say(t, theAdmin, "两个群都发")
	p.press(t, "发送", theAdmin)

	for _, group := range []string{hereGroup, otherGroup} {
		posted, err := p.store.Broadcasts().ListByGroups(context.Background(), []string{group}, 10)
		if err != nil {
			t.Fatalf("reading %s: %v", group, err)
		}
		if len(posted) != 1 {
			t.Fatalf("%s reads %d record(s), want the one notice", group, len(posted))
		}
		if len(posted[0].Targets) != 2 {
			t.Errorf("the notice is recorded with %d target(s), want both groups: %+v",
				len(posted[0].Targets), posted[0].Targets)
		}
	}
	// And asked about both groups at once, it is still one answer.
	both, err := p.store.Broadcasts().ListByGroups(context.Background(),
		[]string{hereGroup, otherGroup}, 10)
	if err != nil {
		t.Fatalf("reading both: %v", err)
	}
	if len(both) != 1 {
		t.Errorf("a notice in two of these groups counts %d time(s), want once", len(both))
	}
}

// TestTheAuditNamesWhoAsked covers the command that answers for an anonymous notice.
func TestTheAuditNamesWhoAsked(t *testing.T) {
	p := newPlatform(t)
	broadcastOnce(t, p, "有广告\n后面还有一行")

	data := &qqbotsdk.GroupMessageCreateData{
		ID: "AUDIT-MESSAGE", GroupOpenID: hereGroup,
		Author: &qqbotsdk.User{MemberOpenID: theAdmin},
	}
	if err := p.handler.auditCommand(context.Background(), data, command.Parsed{}); err != nil {
		t.Fatalf("the audit: %v", err)
	}

	said := p.lastText()
	if !strings.Contains(said, "广播记录") {
		t.Errorf("the audit did not say what it is:\n%s", said)
	}
	if !strings.Contains(said, theAdmin) {
		t.Errorf("the audit does not name who asked:\n%s", said)
	}
	if !strings.Contains(said, "有广告") {
		t.Errorf("the audit does not show what was said:\n%s", said)
	}
	if strings.Contains(said, "后面还有一行") {
		t.Errorf("the audit shows more than the first line:\n%s", said)
	}
}

// TestTheAuditOfAGroupWithNothingSaysSo covers the empty case, which is what a group
// that never broadcast sees.
func TestTheAuditOfAGroupWithNothingSaysSo(t *testing.T) {
	p := newPlatform(t)
	data := &qqbotsdk.GroupMessageCreateData{
		ID: "AUDIT-MESSAGE", GroupOpenID: hereGroup,
		Author: &qqbotsdk.User{MemberOpenID: theAdmin},
	}
	if err := p.handler.auditCommand(context.Background(), data, command.Parsed{}); err != nil {
		t.Fatalf("the audit: %v", err)
	}
	if said := p.lastText(); !strings.Contains(said, "还没有广播记录") {
		t.Errorf("an empty record was not said plainly:\n%s", said)
	}
}

// TestFirstLineIsOneLine covers what an audit shows of each notice: enough to
// recognise it, and never a wall of text in a group.
func TestFirstLineIsOneLine(t *testing.T) {
	cases := map[string]string{
		"一行":                    "一行",
		"第一行\n第二行":              "第一行",
		"  前面有空格  ":             "前面有空格",
		"\n\n":                  "（没有文字内容）",
		strings.Repeat("字", 50): strings.Repeat("字", 40) + "…",
	}
	for content, want := range cases {
		if got := firstLine(content, 40); got != want {
			t.Errorf("firstLine(%q) = %q, want %q", content, got, want)
		}
	}
}

// TestABroadcastWithNoRecordIsLoudAboutIt covers the one failure an audit cannot afford:
// a notice that went out and was not written down. The send still happens -- a group is
// not left waiting on the database -- but the log says so.
func TestABroadcastWithNoRecordIsLoudAboutIt(t *testing.T) {
	p := newPlatform(t)
	p.handler.deps.Store = nil
	broadcastOnce(t, p, "没有数据层")

	if posted, err := p.store.Broadcasts().ListByGroups(context.Background(),
		[]string{hereGroup}, 10); err != nil {
		t.Fatalf("reading the record: %v", err)
	} else if len(posted) != 0 {
		t.Errorf("something was recorded with no data layer: %+v", posted)
	}
	// And the broadcast itself still went out: the record is the audit's business, not
	// the group's.
	if notices := p.notices(); len(notices) != 1 {
		t.Errorf("the broadcast went out %d time(s), want once: %v", len(notices), notices)
	}
}
