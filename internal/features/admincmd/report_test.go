package admincmd

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"

	"github.com/fouc3/qq-group-management-bot/internal/feature"
)

// stubJudge stands in for the moderation feature.
type stubJudge struct {
	verdict    feature.ModerationVerdict
	err        error
	dryRun     bool
	judged     int
	lastGroup  string
	lastQuoted string
	// release holds the judgement until the test lets it go, so that the line the
	// group sees first can be asserted without racing the verdict.
	release chan struct{}
	// penalty is how long the configuration silences a reporter whose report found
	// nothing, zero when it does not ask for that.
	penalty int64
	// reporter, outcome and outcomeMute are what the command told the stub, so a
	// test can see the two halves of a judgement line up. quotedText is the locator
	// the quote carried, which must reach the judgement and must never be judged.
	reporter    string
	quotedText  string
	outcome     string
	outcomeMute int64
}

func (s *stubJudge) JudgeQuoted(_ context.Context, groupOpenID,
	quotedIndex, quotedText, reporter string) (feature.ModerationVerdict, error) {
	if s.release != nil {
		<-s.release
	}
	s.judged++
	s.lastGroup, s.lastQuoted = groupOpenID, quotedIndex
	s.quotedText, s.reporter = quotedText, reporter
	if s.err != nil {
		return feature.ModerationVerdict{}, s.err
	}
	return s.verdict, nil
}

func (s *stubJudge) DryRun() bool { return s.dryRun }

func (s *stubJudge) ReportPenaltySeconds() int64 { return s.penalty }

// JudgingEnabled is true because a stub is only ever built where a judge would be:
// the harnesses that need one without say so by leaving it out entirely.
func (s *stubJudge) JudgingEnabled() bool { return true }

// RecordOutcome keeps what it was told, so that the record and the act can be
// asserted to have happened together.
func (s *stubJudge) RecordOutcome(_ context.Context, judgementID, action string,
	muteSeconds int64) error {
	s.outcome = action
	s.outcomeMute = muteSeconds
	return nil
}

// reportHarness builds a harness with a stub judge behind the command.
func reportHarness(t *testing.T, judge *stubJudge) *harness {
	t.Helper()
	h := newHarness(t, baseSection)
	h.handler.SetModeration(judge)
	return h
}

// quotedReport builds the message a member sends when reporting, quoting
// somebody else's message.
func quotedReport(quotedIndex string) *qqbotsdk.GroupMessageCreateData {
	data := &qqbotsdk.GroupMessageCreateData{
		ID:          "REPORT-MESSAGE",
		GroupOpenID: testGroupOpenID,
		Content:     "/违规举报",
		Author:      &qqbotsdk.User{MemberOpenID: testAdmin},
	}
	if quotedIndex != "" {
		data.MessageScene = &qqbotsdk.MessageScene{
			Ext: []string{"msg_idx=REPORT-MESSAGE", "ref_msg_idx=" + quotedIndex},
		}
	}
	return data
}

// TestAnyMemberMayReport covers the gate the report command has to sit above.
//
// It used to sit below it, so an ordinary member was answered "你没有权限使用管理
// 命令。" -- and none of the report tests caught that, because every one of them
// reported as an administrator. What a member who forgot the quote should get is the
// instruction to quote, which is what this asserts.
func TestAnyMemberMayReport(t *testing.T) {
	h := reportHarness(t, &stubJudge{})
	h.send("/违规举报", "MEMBER-OPENID", testGroupOpenID)

	reply := h.lastReply()
	if strings.Contains(reply, "没有权限") {
		t.Errorf("an ordinary member was refused: %q", reply)
	}
	if !strings.Contains(reply, "引用") {
		t.Errorf("reply = %q, want the instruction to quote a message", reply)
	}
}

// TestATemporaryQuoteReachesTheJudgement covers the quote whose index the cache can
// never hold.
//
// A quote of a message that is itself a quote comes with a temporary index, and no
// ordinary message event ever carries one, so the judgement has to find the message by
// the text the quote showed instead. The report must therefore reach the judgement --
// an earlier version refused it outright with an explanation, which left the reporter
// unable to report anything that had been quoted before.
func TestATemporaryQuoteReachesTheJudgement(t *testing.T) {
	judge := &stubJudge{verdict: feature.ModerationVerdict{
		Category: "ad", Label: "广告", MuteSeconds: 600, SubjectOpenID: "SUBJECT-1",
	}}
	h := reportHarness(t, judge)

	if err := h.handler.reportCommand(context.Background(),
		quotedReport("TMP_94e31996-8fcd-4b2e-bdd3-09e9d23bb5e4"),
		parsedCommand{}); err != nil {
		t.Fatalf("reportCommand: %v", err)
	}

	// The judgement runs on its own goroutine, so the verdict is the reply to wait for.
	reply := waitForReply(t, h, "广告")
	if judge.judged != 1 {
		t.Errorf("the judgement was asked for %d times, want once", judge.judged)
	}
	if !strings.Contains(reply, "已撤回") && !strings.Contains(reply, "已禁言") {
		t.Errorf("reply = %q, want what was done about it", reply)
	}
}

// waitForReply waits for a reply that mentions something, because the judgement
// runs on its own goroutine and the test must not race it.
func waitForReply(t *testing.T, h *harness, contains string) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		reply := h.lastReply()
		if strings.Contains(reply, contains) {
			return reply
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no reply mentioned %q within the wait; the last one was %q",
		contains, h.lastReply())
	return ""
}

// TestAReportWithoutAQuoteTeachesHow covers the one mistake worth answering
// rather than guessing at: nothing says which message is being reported.
func TestAReportWithoutAQuoteTeachesHow(t *testing.T) {
	judge := &stubJudge{}
	h := reportHarness(t, judge)

	if err := h.handler.reportCommand(context.Background(), quotedReport(""), parsedCommand{}); err != nil {
		t.Fatalf("reportCommand: %v", err)
	}
	if judge.judged != 0 {
		t.Error("the judge was asked about a report that named no message")
	}
	if reply := h.lastReply(); !strings.Contains(reply, "引用") {
		t.Errorf("reply = %q, want it to say the message must be quoted", reply)
	}
}

// TestAReportWithoutModerationSaysSo covers a deployment with nothing behind the
// command: saying so is better than a command that looks broken.
func TestAReportWithoutModerationSaysSo(t *testing.T) {
	h := newHarness(t, baseSection)

	if err := h.handler.reportCommand(context.Background(), quotedReport("IDX-1"),
		parsedCommand{}); err != nil {
		t.Fatalf("reportCommand: %v", err)
	}
	if reply := h.lastReply(); !strings.Contains(reply, "没有配置") {
		t.Errorf("reply = %q, want it to explain that nothing is configured", reply)
	}
}

// TestAViolationIsChangedAndSaid covers the whole outcome: the quoted message is
// taken back, the author is silenced, and the group is told what it was.
func TestAViolationIsChangedAndSaid(t *testing.T) {
	release := make(chan struct{})
	judge := &stubJudge{
		release: release,
		verdict: feature.ModerationVerdict{
			JudgementID:     "JUDGE-1",
			Category:        "ad",
			Label:           "广告",
			MuteSeconds:     600,
			SubjectOpenID:   "SUBJECT-OPENID",
			QuotedMessageID: "QUOTED-MESSAGE",
			// The judge names the messages to take back, and the group is told which
			// ones they were by the numbers the judge was shown.
			RecallMessageIDs: []string{"QUOTED-MESSAGE", "SECOND-MESSAGE"},
			RecallNumbers:    []int{2, 5},
			Reason:           "卖号广告", // the model's words, which must not be published
			Model:            "stub-model",
		},
	}
	h := reportHarness(t, judge)

	if err := h.handler.reportCommand(context.Background(), quotedReport("IDX-QUOTED"),
		parsedCommand{}); err != nil {
		t.Fatalf("reportCommand: %v", err)
	}
	// Held at the gate, so this is the line the group sees first, and seeing it
	// does not depend on how fast the stub would have answered.
	if reply := h.lastReply(); !strings.Contains(reply, "请稍候") {
		t.Errorf("reply = %q, want the waiting line", reply)
	}
	if judge.judged != 0 {
		t.Error("the judgement was reached before the waiting line was sent")
	}

	close(release)
	reply := waitForReply(t, h, "判定为")

	if !strings.Contains(reply, "广告") {
		t.Errorf("reply = %q, want the configured label", reply)
	}
	if strings.Contains(reply, "卖号广告") {
		t.Error("the model's own words were published in the group")
	}
	if !strings.Contains(reply, "已禁言") {
		t.Errorf("reply = %q, want the mute reported", reply)
	}
	if !strings.Contains(reply, "已撤回 2 条消息") {
		t.Errorf("reply = %q, want the number of messages taken back", reply)
	}
	// How many, never which ones: the judgement's numbers are positions in the
	// window that was sent for judging, and the group never saw that list. Printing
	// them makes a reader count and come up with a different answer.
	if strings.Contains(reply, "第 ") {
		t.Errorf("reply = %q, want no window positions in it", reply)
	}
	if count := h.muteCount(); count == 0 {
		t.Error("no mute was actually sent")
	}
	if judge.lastQuoted != "IDX-QUOTED" {
		t.Errorf("judged %q, want the quoted index", judge.lastQuoted)
	}
	if judge.reporter != testAdmin {
		t.Errorf("the judgement was recorded against %q, want the reporter",
			judge.reporter)
	}
	// What was done is written back onto the judgement, which is the only way the
	// record answers "and then what" rather than "and then nothing".
	if judge.outcome == "" || judge.outcomeMute != 600 {
		t.Errorf("outcome = %q, %d; want the actions that were taken",
			judge.outcome, judge.outcomeMute)
	}
}

// TestDryRunTouchesNobody covers the setting that makes the first days safe.
func TestDryRunTouchesNobody(t *testing.T) {
	judge := &stubJudge{
		dryRun: true,
		verdict: feature.ModerationVerdict{
			Category: "ad", Label: "广告", MuteSeconds: 600,
			SubjectOpenID: "SUBJECT-OPENID", QuotedMessageID: "QUOTED-MESSAGE",
		},
	}
	h := reportHarness(t, judge)

	if err := h.handler.reportCommand(context.Background(), quotedReport("IDX-QUOTED"),
		parsedCommand{}); err != nil {
		t.Fatalf("reportCommand: %v", err)
	}
	reply := waitForReply(t, h, "试运行")

	if !strings.Contains(reply, "广告") {
		t.Errorf("reply = %q, want it to say what was found", reply)
	}
	if count := h.muteCount(); count != 0 {
		t.Errorf("a dry run sent %d mutes", count)
	}
}

// TestNoJudgementTouchesNobody covers a model that could not be read: nobody is
// punished, and the group is told rather than left waiting.
func TestNoJudgementTouchesNobody(t *testing.T) {
	judge := &stubJudge{err: errors.New("the model did not answer")}
	h := reportHarness(t, judge)

	if err := h.handler.reportCommand(context.Background(), quotedReport("IDX-QUOTED"),
		parsedCommand{}); err != nil {
		t.Fatalf("reportCommand: %v", err)
	}
	reply := waitForReply(t, h, "送检失败")

	if !strings.Contains(reply, "未采取任何处理") {
		t.Errorf("reply = %q, want it to say nothing was done", reply)
	}
	if count := h.muteCount(); count != 0 {
		t.Errorf("an unjudged report sent %d mutes", count)
	}
}

// TestTheRateLimitStopsFlooding covers the one person who reports everything: the
// model costs money per call, and the limit is what keeps one member from paying
// it out.
//
// The limiter is exercised directly rather than through the command, because the
// command starts a goroutine: counting calls that must not happen is exactly the
// kind of assertion a goroutine makes flaky.
func TestTheRateLimitStopsFlooding(t *testing.T) {
	h := newHarness(t, baseSection)

	for attempt := 1; attempt <= reportRateLimitPerHour; attempt++ {
		if !h.handler.allowReport(testAdmin) {
			t.Fatalf("report %d was refused, but the limit is %d",
				attempt, reportRateLimitPerHour)
		}
	}
	if h.handler.allowReport(testAdmin) {
		t.Errorf("report %d was allowed, want the limit to hold",
			reportRateLimitPerHour+1)
	}

	// One member's reports are not another's.
	if !h.handler.allowReport("SOMEONE-ELSE") {
		t.Error("one member's reports counted against another's")
	}

	// And the window slides: a report from two hours ago does not count.
	h.handler.mu.Lock()
	h.handler.reports[testAdmin] = []time.Time{time.Now().Add(-2 * time.Hour)}
	h.handler.mu.Unlock()
	if !h.handler.allowReport(testAdmin) {
		t.Error("a report from two hours ago still counted against the limit")
	}
}

// TestAnUnfoundedReportCanCostTheReporter covers the setting that makes a member
// think before reporting: nothing was found, so the report was wrong.
//
// The verdict carries no subject, which is what makes this unambiguous: any mute
// here can only be the reporter's.
func TestAnUnfoundedReportCanCostTheReporter(t *testing.T) {
	judge := &stubJudge{penalty: 300}
	h := reportHarness(t, judge)

	if err := h.handler.reportCommand(context.Background(), quotedReport("IDX-1"),
		parsedCommand{}); err != nil {
		t.Fatalf("reportCommand: %v", err)
	}
	reply := waitForReply(t, h, "未发现违规")

	if !strings.Contains(reply, "禁言举报者") {
		t.Errorf("reply = %q, want it to say the reporter was silenced", reply)
	}
	if count := h.muteCount(); count != 1 {
		t.Errorf("sent %d mutes, want exactly the reporter's", count)
	}
}

// TestTheReporterIsNotPunishedByDefault covers the default the plan asked for:
// with no penalty configured, a report that finds nothing costs nothing.
func TestTheReporterIsNotPunishedByDefault(t *testing.T) {
	judge := &stubJudge{}
	h := reportHarness(t, judge)

	if err := h.handler.reportCommand(context.Background(), quotedReport("IDX-1"),
		parsedCommand{}); err != nil {
		t.Fatalf("reportCommand: %v", err)
	}
	reply := waitForReply(t, h, "未发现违规")

	if strings.Contains(reply, "禁言") {
		t.Errorf("reply = %q, want nobody silenced", reply)
	}
	if count := h.muteCount(); count != 0 {
		t.Errorf("sent %d mutes with no penalty configured", count)
	}
}
