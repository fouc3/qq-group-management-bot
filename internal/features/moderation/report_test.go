package moderation

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/fouc3/qq-group-management-bot/internal/config"
	"github.com/fouc3/qq-group-management-bot/internal/feature"
)

// reportHarness builds the feature with both a cache and a stub model, which is
// what the whole path needs: a window to find and a judgement to come back.
func reportHarness(t *testing.T, stub *modelStub, extra string) (*handler, string) {
	t.Helper()
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("set TEST_REDIS_ADDR to run the report tests against a real Redis " +
			"(the bot's own is at 127.0.0.1:6380)")
	}
	server := stub.start(t)
	group := "G-" + strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())
	section := `
enabled: true
cache_hours: 6
context_before: 10
context_after: 10
chain_minutes: 10
default_mute: "5m"
model:
  base_url: "` + server.URL + `/v1"
  api_key: "test-key"
  name: "stub-model"
  timeout_seconds: 5
categories:
  ad:
    label: "广告"
    mute: "10m"
  fraud:
    label: "诈骗"
` + extra

	var document yaml.Node
	if err := yaml.Unmarshal([]byte(section), &document); err != nil {
		t.Fatalf("decoding the section: %v", err)
	}
	if len(document.Content) == 0 {
		t.Fatal("the section produced no node")
	}
	instance, err := New(*document.Content[0], feature.Deps{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Redis:  config.Redis{Addr: addr, Prefix: "qgb-test", DialTimeoutSeconds: 3},
	})
	if err != nil {
		t.Fatalf("building the feature: %v", err)
	}
	h := instance.(*handler)
	t.Cleanup(func() {
		ctx := context.Background()
		set, index := h.cache.keys(group)
		h.cache.client.Del(ctx, set, index)
		h.cache.Close()
	})
	return h, group
}

// cacheChain writes a window into the cache, the quoted message in the middle.
func cacheChain(t *testing.T, h *handler, group string, texts ...string) string {
	t.Helper()
	ctx := context.Background()
	chain := chainOf(texts...)
	quoted := chain[len(chain)/2].Idx
	for _, message := range chain {
		if err := h.cache.Record(ctx, group, message); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	return quoted
}

// TestAReportBecomesAVerdict covers the ordinary path end to end: a cached
// window, a model that finds an advertisement, and everything a caller needs in
// order to act.
func TestAReportBecomesAVerdict(t *testing.T) {
	stub := &modelStub{answer: `{"reason":"测试理由","verdict":"violation","category":"ad",` +
		`"reason":"卖号广告","confidence":0.9}`}
	h, group := reportHarness(t, stub, "")
	quoted := cacheChain(t, h, group, "正常聊天", "加群送皮肤 私聊我", "谁在发广告")

	report, err := h.JudgeQuoted(context.Background(), group, quoted, "", "REPORTER-1")
	if err != nil {
		t.Fatalf("JudgeQuoted: %v", err)
	}
	if report.Category != "ad" {
		t.Fatalf("category = %q, want ad", report.Category)
	}
	// The label is the configuration's, which is what keeps the group's side of
	// the reply free of anything the model wrote.
	if report.Label != "广告" {
		t.Errorf("label = %q, want the configured one", report.Label)
	}
	if report.MuteSeconds != 600 {
		t.Errorf("mute = %d seconds, want 600", report.MuteSeconds)
	}
	// The subject is the quoted message's author, and the quoted message is what a
	// recall would take back -- not the newest message in the window.
	if report.SubjectOpenID == "" || report.QuotedMessageID == "" {
		t.Errorf("report = %+v, want a subject and a message to recall", report)
	}
	if len(report.JudgedMessageIDs) != 3 {
		t.Errorf("judged %d messages, want all three", len(report.JudgedMessageIDs))
	}
	if report.Reason == "" || report.Model != "stub-model" {
		t.Errorf("report = %+v, want the model's reason and name for the record", report)
	}
}

// TestACategoryWithoutItsOwnDurationFallsBack covers the configuration the plan
// calls "one fixed time for every type": fraud names no duration here.
func TestACategoryWithoutItsOwnDurationFallsBack(t *testing.T) {
	stub := &modelStub{answer: `{"reason":"测试理由","verdict":"violation","category":"fraud",` +
		`"confidence":0.9}`}
	h, group := reportHarness(t, stub, "")
	quoted := cacheChain(t, h, group, "先交押金")

	report, err := h.JudgeQuoted(context.Background(), group, quoted, "", "REPORTER-1")
	if err != nil {
		t.Fatalf("JudgeQuoted: %v", err)
	}
	if report.MuteSeconds != 300 {
		t.Errorf("mute = %d seconds, want the 5m default", report.MuteSeconds)
	}
	if report.Label != "诈骗" {
		t.Errorf("label = %q, want the configured one", report.Label)
	}
}

// TestACleanVerdictHasNoPunishment covers the case that decides whether anybody
// is silenced: nothing found means nothing served, and the reporter's fate is the
// caller's business.
func TestACleanVerdictHasNoPunishment(t *testing.T) {
	stub := &modelStub{answer: `{"reason":"测试理由","verdict":"ok","confidence":0.9}`}
	h, group := reportHarness(t, stub, "")
	quoted := cacheChain(t, h, group, "这条没问题")

	report, err := h.JudgeQuoted(context.Background(), group, quoted, "", "REPORTER-1")
	if err != nil {
		t.Fatalf("JudgeQuoted: %v", err)
	}
	if report.Category != "" || report.MuteSeconds != 0 || report.Label != "" {
		t.Errorf("report = %+v, want nothing to act on", report)
	}
}

// TestNoJudgementIsAnError covers every way of not reaching one. None may look
// like a clean verdict, or a real report would be silently dropped.
func TestNoJudgementIsAnError(t *testing.T) {
	cases := map[string]struct {
		stub    *modelStub
		extra   string
		quoted  string
		noModel bool
	}{
		"a message nobody cached": {
			stub: &modelStub{answer: `{"reason":"测试理由","verdict":"ok"}`}, quoted: "NEVER-SEEN"},
		"an empty index": {
			stub: &modelStub{answer: `{"reason":"测试理由","verdict":"ok"}`}, quoted: ""},
		"a model that answered nothing readable": {
			stub: &modelStub{answer: "我不知道"}, quoted: "MID"},
		"a server error": {
			stub: &modelStub{status: 500}, quoted: "MID"},
		"no model configured": {
			stub: &modelStub{answer: `{"reason":"测试理由","verdict":"ok"}`}, quoted: "MID", noModel: true},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			h, group := reportHarness(t, testCase.stub, testCase.extra)
			quoted := testCase.quoted
			if quoted == "MID" {
				quoted = cacheChain(t, h, group, "正常聊天", "加群送皮肤")
			}
			if testCase.noModel {
				h.cfg.Model.Name = ""
			}
			_, err := h.JudgeQuoted(context.Background(), group, quoted, "", "REPORTER-1")
			if !errors.Is(err, ErrUnjudged) {
				t.Fatalf("err = %v, want ErrUnjudged", err)
			}
		})
	}
}

// TestNoCategoryDurationMeansNoPunishment covers a category the configuration
// knows but gives no duration to, and no default either: the finding is reported
// without anybody being silenced.
func TestNoCategoryDurationMeansNoPunishment(t *testing.T) {
	var cfg Config
	cfg.DefaultMute = ""
	cfg.Categories = map[string]Category{"ad": {Label: "广告"}}
	if err := cfg.applyDefaults(); err != nil {
		t.Fatalf("applyDefaults: %v", err)
	}
	if seconds, known := cfg.MuteFor("ad"); !known || seconds != 0 {
		t.Errorf("MuteFor = %d, %v; want 0, true", seconds, known)
	}
	if _, known := cfg.MuteFor("nothing-such"); known {
		t.Error("a category that is not configured must not be known")
	}
}

// groupSection is a groups: block naming the group this test's harness uses.
//
// The harness names its group after the test, so a test can address its own
// without the two agreeing on a constant.
func groupSection(t *testing.T, body string) string {
	t.Helper()
	return "groups:\n  \"G-" + t.Name() + "\":\n" + body
}

// TestTheAllowListDoesNotExemptAnything records why the content exemption was
// removed instead of repaired.
//
// Both of these were reported as working, and both did work. The second is the
// first with one character inserted, which is all it takes when the rule is about
// text: an exemption keyed on what a message says is satisfied by what the message
// says.
func TestTheAllowListDoesNotExemptAnything(t *testing.T) {
	cases := map[string]string{
		"the allowed word carried as a shield": "deepseek0.01x https://q1.1110103.xyz/（意思是ds模型中转站0.01倍率）\n防屏蔽：api.mcapple.top",
		"the same, with the advertisement's own domain broken up so the " +
			"extractor cannot see it": "deepseek0.01x https://q1删.1110103删.删xyz/\n防屏蔽：api.mcapple.top",
	}
	cfg := Config{
		Categories: map[string]Category{"ad": {Label: "广告", Mute: "10m"}},
		Groups: map[string]GroupOverride{
			"G-1": {Allow: []string{"api.mcapple.top"}},
		},
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			if matched, allowed := cfg.allowedIn("G-1", text); allowed {
				t.Errorf("exempted by %q, which is how an advertisement gets past "+
					"the judge:\n%s", matched, text)
			}
		})
	}
}

// TestJudgingCanBeTurnedOffForOneGroup covers one group opting out while the
// feature stays on everywhere else.
func TestJudgingCanBeTurnedOffForOneGroup(t *testing.T) {
	stub := &modelStub{answer: `{"reason":"测试理由","verdict":"ok","confidence":0.9}`}
	h, group := reportHarness(t, stub, groupSection(t, "    enabled: false\n"))
	quoted := cacheChain(t, h, group, "正常聊天")

	if _, err := h.JudgeQuoted(context.Background(), group, quoted, "", "REPORTER-1"); !errors.Is(err, ErrUnjudged) {
		t.Fatalf("err = %v, want ErrUnjudged", err)
	}
	if len(stub.requests) != 0 {
		t.Error("the model was asked about a group where judging is turned off")
	}
}

// TestAGroupCanHaveItsOwnDurations covers the per-group override: the same
// category is punished for different lengths in different groups.
func TestAGroupCanHaveItsOwnDurations(t *testing.T) {
	stub := &modelStub{answer: `{"reason":"测试理由","verdict":"violation","category":"ad",` +
		`"confidence":0.9}`}
	h, group := reportHarness(t, stub,
		groupSection(t, "    categories:\n      ad: \"30m\"\n"))
	quoted := cacheChain(t, h, group, "加群送皮肤 私聊我")

	report, err := h.JudgeQuoted(context.Background(), group, quoted, "", "REPORTER-1")
	if err != nil {
		t.Fatalf("JudgeQuoted: %v", err)
	}
	if report.MuteSeconds != 1800 {
		t.Errorf("mute = %d seconds, want the group's own 30 minutes", report.MuteSeconds)
	}
	if report.Label != "广告" {
		t.Errorf("label = %q, want the configured one", report.Label)
	}
}

// TestAnEmptyAllowEntryIsRefusedAtStartup covers the typo that would otherwise
// silently accept every message in the group.
func TestAnEmptyAllowEntryIsRefusedAtStartup(t *testing.T) {
	cfg := Config{
		Categories: map[string]Category{"ad": {Label: "广告", Mute: "10m"}},
		Groups:     map[string]GroupOverride{"G-1": {Allow: []string{" "}}},
	}
	if err := cfg.applyDefaults(); err == nil {
		t.Error("an empty allow entry must be refused: it would match every message")
	}
}

// TestASenderTheGroupTrustsIsNotJudged covers the one exemption the code makes.
//
// It is about identity, and that is the whole reason it is sound: there is no
// sentence somebody can write to become the group's own account, which is exactly
// what could not be said of the content versions.
func TestASenderTheGroupTrustsIsNotJudged(t *testing.T) {
	stub := &modelStub{answer: `{"reason":"测试理由","verdict":"violation","category":"ad",` +
		`"confidence":0.99}`}
	h, group := reportHarness(t, stub,
		groupSection(t, "    allow_senders: [\"MEMBER-1\"]\n"))
	// Every message in the window carries the same sender, which is the one the
	// group declared.
	quoted := cacheChain(t, h, group, "本群公告", "官方说明")

	report, err := h.JudgeQuoted(context.Background(), group, quoted, "", "REPORTER-1")
	if err != nil {
		t.Fatalf("JudgeQuoted: %v", err)
	}
	if report.Category != "" {
		t.Errorf("report = %+v, want nothing to act on", report)
	}
	if len(stub.requests) != 0 {
		t.Error("the model was asked about a sender the group had put beyond judging")
	}
}

// TestTheGroupsOwnListGoesIntoTheInstructions covers where the list belongs.
//
// The instructions are the part of the prompt a member cannot write into, which
// is what makes them the right home for something a message must not be able to
// satisfy. The data block is where a member's text goes, and the list must not be
// there.
func TestTheGroupsOwnListGoesIntoTheInstructions(t *testing.T) {
	stub := &modelStub{answer: `{"reason":"测试理由","verdict":"ok","confidence":0.9}`}
	h, group := reportHarness(t, stub,
		groupSection(t, "    allow: [\"api.mcapple.top\"]\n"))
	quoted := cacheChain(t, h, group, "正常聊天")

	if _, err := h.JudgeQuoted(context.Background(), group, quoted, "", "REPORTER-1"); err != nil {
		t.Fatalf("JudgeQuoted: %v", err)
	}
	request := stub.lastRequest(t)

	system := systemContent(t, request)
	if !strings.Contains(system, "api.mcapple.top") {
		t.Errorf("the group's own list is not in the instructions:\n%s", system)
	}
	// The instructions also say what the list is worth, because the trick is to
	// wear it: the point is not to protect the listed content but to stop it being
	// used as a shield.
	if !strings.Contains(system, "免罪牌") {
		t.Error("the instructions do not warn that the list can be worn as a disguise")
	}
	if strings.Contains(userContent(t, request), "api.mcapple.top") {
		t.Error("the group's list reached the part of the prompt a member writes")
	}
}
