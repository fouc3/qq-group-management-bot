package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/fouc3/qq-group-management-bot/internal/command"
	"github.com/fouc3/qq-group-management-bot/internal/config"
	"github.com/fouc3/qq-group-management-bot/internal/feature"
	"github.com/fouc3/qq-group-management-bot/internal/features/joinverify"
	"github.com/fouc3/qq-group-management-bot/internal/humanverify"
	"github.com/fouc3/qq-group-management-bot/internal/logring"
	"github.com/fouc3/qq-group-management-bot/internal/store"
)

// The tests here are about the two things this package really does: deciding who
// may ask, and turning what the bot already knows into what a browser can read.
//
// Everything else it answers comes from a function that is tested where it lives,
// and repeating those tests through an HTTP handler would only test the handler.
// What is tested is what would be wrong silently: a secret that leaks, a hold that
// gets dropped because its settings could not be parsed, a three-state switch
// flattened into two, an entry that bars every group counted once per group.

// --- the fakes -------------------------------------------------------------

// fakeStore answers only what a test asks of it.
//
// The embedded interface is nil on purpose: the store is a broad contract, and a
// test that spelled out all of it would be fifteen methods saying nothing. A call
// to one that was not overridden panics, which is what a test wants -- it says
// which method the handler reached for.
type fakeStore struct {
	store.Store
	pending    []store.Pending
	blacklist  map[string][]store.Barred
	watches    []store.Watch
	judgements map[string][]store.Judgement
	events     []store.MemberEvent
}

func (f fakeStore) Pending() store.PendingStore { return fakePending{entries: f.pending} }

type fakePending struct {
	store.PendingStore
	entries []store.Pending
}

func (f fakePending) Load(context.Context, time.Time) ([]store.Pending, error) {
	return f.entries, nil
}

func (f fakeStore) Blacklist() store.BlacklistStore { return fakeBlacklist{byGroup: f.blacklist} }

type fakeBlacklist struct {
	store.BlacklistStore
	byGroup map[string][]store.Barred
}

func (f fakeBlacklist) List(_ context.Context, groupOpenID string, _ int) ([]store.Barred, error) {
	return f.byGroup[groupOpenID], nil
}

func (f fakeStore) Watches() store.WatchStore { return fakeWatches{entries: f.watches} }

type fakeWatches struct {
	store.WatchStore
	entries []store.Watch
}

func (f fakeWatches) List(context.Context, time.Time) ([]store.Watch, error) {
	return f.entries, nil
}

func (f fakeStore) Judgements() store.JudgementStore {
	return fakeJudgements{byGroup: f.judgements}
}

type fakeJudgements struct {
	store.JudgementStore
	byGroup map[string][]store.Judgement
}

func (f fakeJudgements) RecentInGroup(_ context.Context, groupOpenID string,
	_ time.Time, _ int) ([]store.Judgement, error) {
	return f.byGroup[groupOpenID], nil
}

func (f fakeStore) MemberEvents() store.MemberEventStore { return fakeEvents{entries: f.events} }

type fakeEvents struct {
	store.MemberEventStore
	entries []store.MemberEvent
}

func (f fakeEvents) List(context.Context, int) ([]store.MemberEvent, error) {
	return f.entries, nil
}

// fakeConfig is a configuration that does not move.
type fakeConfig struct{ cfg *config.Config }

func (f fakeConfig) Current() *config.Config { return f.cfg }

// fakeFeatures reports what a test says is running.
type fakeFeatures struct {
	running map[string]bool
	live    map[string]feature.Feature
	// commands is what the running features offer together; nil is a bot with no
	// command to show, which is the state most of these tests are in.
	commands []command.Def
}

func (f fakeFeatures) Running(name string) bool { return f.running[name] }

func (f fakeFeatures) Instance(name string) feature.Feature { return f.live[name] }

func (f fakeFeatures) CommandDefs() []command.Def { return f.commands }

// fakeReload answers with the outcome a test names.
type fakeReload struct {
	err   error
	calls int
}

func (f *fakeReload) Reload(string) error {
	f.calls++
	return f.err
}

// --- the fixtures ----------------------------------------------------------

// testConfig is a configuration with a secret in every corner it has one.
//
// The secrets are the point: what the console answers must never contain them,
// and the only way to test that is to put some in.
func testConfig() *config.Config {
	cfg := &config.Config{}
	cfg.Bot.AppID = "appid-should-never-appear"
	cfg.Bot.ClientSecret = "client-secret-should-never-appear"
	cfg.Bot.AccessToken = "access-token-should-never-appear"
	cfg.Bot.QQ = 2152595244
	cfg.Bot.Groups = config.Groups{
		{OpenID: "FABED0ACCA31B997DFF11DC30658D252", QQGroupID: 123456789},
		{OpenID: "A1B2C3D4E5F6A1B2C3D4E5F6A1B2C3D4", QQGroupID: 987654321},
	}
	cfg.Database.Driver = "sqlite"
	cfg.Database.DSN = t0Database
	cfg.Redis.Addr = "127.0.0.1:6380"
	cfg.Redis.Password = "redis-password-should-never-appear"
	cfg.Redis.Prefix = "qgb"
	cfg.Log.Level = "info"
	cfg.Log.Format = "text"
	cfg.Admin = config.Admin{Enabled: true, Listen: "127.0.0.1:8787"}
	cfg.Features = map[string]yaml.Node{}
	return cfg
}

// t0Database is a path that does not exist, so the size reported for it is zero.
//
// A test does not need a database file; it needs the answer to be a number, and
// a path that is not there is the shortest way to say so.
const t0Database = "/nonexistent/for-a-test/bot.db"

// withFeature adds a feature section to a configuration.
func withFeature(t *testing.T, cfg *config.Config, name, body string) {
	t.Helper()
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(body), &node); err != nil {
		t.Fatalf("building the %s section: %v", name, err)
	}
	// The document node wraps the mapping; the mapping is what a section is.
	if len(node.Content) > 0 {
		cfg.Features[name] = *node.Content[0]
	}
}

// newTestServer builds a server with the fakes a test needs.
func newTestServer(t *testing.T, opts Options) *Server {
	t.Helper()
	if opts.Config == nil {
		opts.Config = fakeConfig{testConfig()}
	}
	if opts.Store == nil {
		opts.Store = fakeStore{}
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	opts.Started = time.Now()
	server, err := New(opts)
	if err != nil {
		t.Fatalf("building the server: %v", err)
	}
	return server
}

// get runs one request and returns the recorder.
func get(t *testing.T, server *Server, target string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
	return recorder
}

// decode reads a response body into a value.
func decode(t *testing.T, recorder *httptest.ResponseRecorder, into any) {
	t.Helper()
	if recorder.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200; body: %s", recorder.Code, recorder.Body.String())
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), into); err != nil {
		t.Fatalf("reading the response: %v\n%s", err, recorder.Body.String())
	}
}

// --- who may ask -----------------------------------------------------------

// TestNoTokenIsAcceptedOnItsOwn covers the deployment the configuration allows:
// a loopback address with no token, which is checked when the file is read rather
// than here.
func TestNoTokenIsAcceptedOnItsOwn(t *testing.T) {
	server := newTestServer(t, Options{})
	if got := get(t, server, "/api/v1/status").Code; got != http.StatusOK {
		t.Fatalf("a tokenless deployment refused its own console: status %d", got)
	}
}

// TestTokenIsRequiredAndCompared covers the deployment that is reachable from the
// network, where the file has already insisted on a token.
func TestTokenIsRequiredAndCompared(t *testing.T) {
	cfg := testConfig()
	cfg.Admin.Token = "the-right-token"
	server := newTestServer(t, Options{Admin: cfg.Admin,
		Config: fakeConfig{cfg}})

	for _, test := range []struct {
		name   string
		header string
		want   int
	}{
		{"no header", "", http.StatusUnauthorized},
		{"the wrong token", "Bearer not-the-token", http.StatusUnauthorized},
		{"a token that is a prefix of the right one", "Bearer the-right", http.StatusUnauthorized},
		{"the right token", "Bearer the-right-token", http.StatusOK},
		{"the right token with room around it", "Bearer   the-right-token  ", http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
			if test.header != "" {
				request.Header.Set("Authorization", test.header)
			}
			recorder := httptest.NewRecorder()
			server.Handler().ServeHTTP(recorder, request)
			if recorder.Code != test.want {
				t.Fatalf("got status %d, want %d", recorder.Code, test.want)
			}
		})
	}
}

// TestThePageItselfNeedsNoToken covers the order a browser works in: it has to
// load the application before it can have anywhere to put a token.
func TestThePageItselfNeedsNoToken(t *testing.T) {
	cfg := testConfig()
	cfg.Admin.Token = "the-right-token"
	server := newTestServer(t, Options{
		Admin:  cfg.Admin,
		Config: fakeConfig{cfg},
		Web:    fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<html>控制台</html>")}},
	})
	if got := get(t, server, "/").Code; got != http.StatusOK {
		t.Fatalf("the shell was refused without a token: status %d", got)
	}
}

// --- what must never leave -------------------------------------------------

// TestSecretsNeverLeaveInTheConfiguration is the one test that would be a security
// bug rather than a wrong answer: the console reports whether a secret is set,
// never what it is.
func TestSecretsNeverLeaveInTheConfiguration(t *testing.T) {
	server := newTestServer(t, Options{})
	recorder := get(t, server, "/api/v1/config")
	body := recorder.Body.String()

	for _, secret := range []string{
		"access-token-should-never-appear",
		"client-secret-should-never-appear",
		"appid-should-never-appear",
		"redis-password-should-never-appear",
	} {
		if strings.Contains(body, secret) {
			t.Fatalf("the console answered with %q:\n%s", secret, body)
		}
	}

	var response configResponse
	decode(t, recorder, &response)
	if !response.Bot.CredentialsConfigured {
		t.Fatal("credentials are set but the console says they are not")
	}
	if response.Redis.Addr != "127.0.0.1:6380" {
		t.Fatalf("the cache address was not reported: %q", response.Redis.Addr)
	}
}

// TestSecretsNeverLeaveInStatus covers the same thing on the screen that is
// loaded first, which is the one nobody thinks of as carrying configuration.
func TestSecretsNeverLeaveInStatus(t *testing.T) {
	body := get(t, newTestServer(t, Options{}), "/api/v1/status").Body.String()
	for _, secret := range []string{
		"access-token-should-never-appear",
		"client-secret-should-never-appear",
		"redis-password-should-never-appear",
	} {
		if strings.Contains(body, secret) {
			t.Fatalf("status answered with %q:\n%s", secret, body)
		}
	}
}

// fakeHumanVerify answers what a test says about the online verification service.
type fakeHumanVerify struct{ connected bool }

func (f fakeHumanVerify) HumanVerifyConnected() bool { return f.connected }

// TestStatusTellsTheTwoAnswersAboutOnlineVerificationApart covers the pair that has to
// stay distinct: whether the file names a service, and whether its connection could
// deliver a result right now.
//
// A deployment with the section but no connection hands out nothing at all, so every
// group that verifies online is falling back to its button -- and until this says
// otherwise it reads like a deployment that verifies online.
func TestStatusTellsTheTwoAnswersAboutOnlineVerificationApart(t *testing.T) {
	var none statusResponse
	decode(t, get(t, newTestServer(t, Options{}), "/api/v1/status"), &none)
	if none.HumanVerification.Configured || none.HumanVerification.Connected {
		t.Fatalf("a deployment with no service was reported as having one: %+v", none.HumanVerification)
	}

	cfg := testConfig()
	cfg.HumanVerification.BaseURL = "https://verify.example"

	var offline statusResponse
	server := newTestServer(t, Options{
		Config:      fakeConfig{cfg},
		HumanVerify: fakeHumanVerify{connected: false},
	})
	decode(t, get(t, server, "/api/v1/status"), &offline)
	if !offline.HumanVerification.Configured {
		t.Fatal("the service the file names was not reported as configured")
	}
	if offline.HumanVerification.Connected {
		t.Fatal("a connection that is down was reported as up")
	}

	var online statusResponse
	server = newTestServer(t, Options{
		Config:      fakeConfig{cfg},
		HumanVerify: fakeHumanVerify{connected: true},
	})
	decode(t, get(t, server, "/api/v1/status"), &online)
	if !online.HumanVerification.Connected {
		t.Fatal("a connection that is up was not reported")
	}
}

// TestConfigDescribesOnlineVerificationWithoutItsTokens covers the section's own
// answers: the business marker resolved to the one actually in use -- a link signed
// with an unwritten marker and one signed with an empty string are not the same link
// -- and the two secrets reported as present rather than printed.
func TestConfigDescribesOnlineVerificationWithoutItsTokens(t *testing.T) {
	cfg := testConfig()
	cfg.HumanVerification = config.HumanVerification{
		BaseURL:        "https://verify.example",
		IssueToken:     "issue-token-should-never-appear",
		WSToken:        "ws-token-should-never-appear",
		TTLSeconds:     600,
		TimeoutSeconds: 5,
	}
	server := newTestServer(t, Options{Config: fakeConfig{cfg}})

	recorder := get(t, server, "/api/v1/config")
	for _, secret := range []string{
		"issue-token-should-never-appear",
		"ws-token-should-never-appear",
	} {
		if strings.Contains(recorder.Body.String(), secret) {
			t.Fatalf("the console answered with %q:\n%s", secret, recorder.Body.String())
		}
	}

	var response configResponse
	decode(t, recorder, &response)
	human := response.HumanVerification
	if !human.Configured || human.BaseURL != "https://verify.example" {
		t.Fatalf("the service was not described: %+v", human)
	}
	if human.Cdata != humanverify.BusinessMarker {
		t.Fatalf("an unwritten business marker was not resolved to the one in use: %q", human.Cdata)
	}
	if human.TTLSeconds != 600 || human.TimeoutSeconds != 5 {
		t.Fatalf("the link lifetime or the timeout was not passed on: %+v", human)
	}
	if !human.IssueTokenConfigured || !human.WSTokenConfigured {
		t.Fatalf("the tokens were not reported as present: %+v", human)
	}
}

// --- the conversions -------------------------------------------------------

// TestGroupOverrideIsThreeStates covers the field that cannot be two states.
//
// "this group turned it off" and "this group did not say" are different answers,
// and a console that showed the second as the first would tell an operator they
// had decided something they had not.
func TestGroupOverrideIsThreeStates(t *testing.T) {
	cfg := testConfig()
	withFeature(t, cfg, "join_verification", `
enabled: true
groups:
  FABED0ACCA31B997DFF11DC30658D252:
    enabled: false
  A1B2C3D4E5F6A1B2C3D4E5F6A1B2C3D4:
    enabled: true
`)
	server := newTestServer(t, Options{Config: fakeConfig{cfg}})

	var response groupsResponse
	decode(t, get(t, server, "/api/v1/groups"), &response)
	if len(response.Groups) != 2 {
		t.Fatalf("got %d groups, want 2", len(response.Groups))
	}

	first := response.Groups[0].Features[0]
	if first.GroupOverride == nil || *first.GroupOverride {
		t.Fatalf("a group that turned it off was not reported as off: %+v", first)
	}
	if first.Effective {
		t.Fatal("a group that turned it off is still effective")
	}
	if !first.GlobalEnabled {
		t.Fatal("the feature's own switch was lost")
	}

	second := response.Groups[1].Features[0]
	if second.GroupOverride == nil || !*second.GroupOverride {
		t.Fatalf("a group that turned it on was not reported as on: %+v", second)
	}
	if !second.Effective {
		t.Fatal("a group that turned it on is not effective")
	}

	// A group the section does not name says nothing, and its answer is the
	// default rather than a decision.
	withFeature(t, cfg, "join_verification", "enabled: true")
	var unnamed groupsResponse
	decode(t, get(t, server, "/api/v1/groups"), &unnamed)
	if got := unnamed.Groups[0].Features[0].GroupOverride; got != nil {
		t.Fatalf("a group that said nothing was reported as saying %v", *got)
	}
}

// TestPendingSettingsAreDecoded covers the opaque column.
//
// What is stored is the verification feature's own structure as JSON. The console
// has to read it, and a row it cannot read must still be returned: a member being
// held by a row the console refuses to show is a member nobody can release.
func TestPendingSettingsAreDecoded(t *testing.T) {
	settings, err := json.Marshal(map[string]any{
		"Enabled": true, "MuteMode": "pseudo", "DeadlineHours": 24,
		"PseudoMuteAfter": 3, "OnlineVerify": true,
	})
	if err != nil {
		t.Fatalf("building the settings: %v", err)
	}

	data := fakeStore{pending: []store.Pending{
		{Token: "token-a", GroupOpenID: "group-a", MemberOpenID: "member-a",
			JoinedAt: 100, Deadline: 200, Settings: string(settings)},
		{Token: "token-b", GroupOpenID: "group-a", MemberOpenID: "member-b",
			JoinedAt: 100, Deadline: 200, Settings: "{ this is not json"},
		{Token: "token-c", GroupOpenID: "group-a", MemberOpenID: "member-c",
			JoinedAt: 100, Deadline: 200},
	}}
	server := newTestServer(t, Options{Store: data})

	var response pendingResponse
	decode(t, get(t, server, "/api/v1/pending"), &response)
	if len(response.Pending) != 3 {
		t.Fatalf("got %d holds, want 3: an unreadable row was dropped", len(response.Pending))
	}

	readable := response.Pending[0]
	if readable.MuteMode != "pseudo" {
		t.Fatalf("the hold mode was not decoded: %q", readable.MuteMode)
	}
	if readable.DeadlineHours != 24 {
		t.Fatalf("the deadline was not decoded: %d", readable.DeadlineHours)
	}
	if readable.LeftToTalk != 3 {
		t.Fatalf("the pseudo-mute allowance was not decoded: %d", readable.LeftToTalk)
	}
	// How this member is asked to prove they are human is part of the settings, so
	// the console reads it from there rather than from the group's current rule.
	if readable.OnlineVerify == nil || !*readable.OnlineVerify {
		t.Fatalf("the verification channel was not decoded: %v", readable.OnlineVerify)
	}
	if response.Pending[1].MuteMode != "" || response.Pending[1].Token != "token-b" {
		t.Fatalf("an unreadable row was not returned as itself: %+v", response.Pending[1])
	}
	// A record that cannot be read is not a record that says "the button": null and
	// false are different answers, and only one of them is a decision.
	if response.Pending[1].OnlineVerify != nil || response.Pending[2].OnlineVerify != nil {
		t.Fatalf("a row whose settings cannot be read named a verification channel: %v / %v",
			response.Pending[1].OnlineVerify, response.Pending[2].OnlineVerify)
	}
}

// TestBlacklistCountsAnEntryThatBarsEveryGroupOnce covers the arithmetic that is
// wrong in a way nobody notices: an entry with no group of its own is in every
// group's answer, and adding the answers up would count it once per group.
func TestBlacklistCountsAnEntryThatBarsEveryGroupOnce(t *testing.T) {
	cfg := testConfig()
	everyone := store.Barred{ID: "bars-everyone", Reason: "广告"}
	data := fakeStore{blacklist: map[string][]store.Barred{
		"FABED0ACCA31B997DFF11DC30658D252": {everyone, {ID: "only-here"}},
		"A1B2C3D4E5F6A1B2C3D4E5F6A1B2C3D4": {everyone},
	}}
	server := newTestServer(t, Options{Config: fakeConfig{cfg}, Store: data})

	var response overviewResponse
	decode(t, get(t, server, "/api/v1/overview"), &response)
	if response.Blacklist.Total != 2 {
		t.Fatalf("the barred list counted %d entries, want 2 (one of them bars every group)",
			response.Blacklist.Total)
	}
	if len(response.Blacklist.ByGroup) != 2 {
		t.Fatalf("got %d groups, want 2", len(response.Blacklist.ByGroup))
	}
}

// --- the answers that are not a list ---------------------------------------

// TestJudgementsSayWhyTheyCannotAnswer covers the honest empty answer.
//
// The store answers this question one group at a time, so a deployment that
// manages every group it is in has no list to walk. An empty list would read as
// "nothing was judged", which is a different and false statement.
func TestJudgementsSayWhyTheyCannotAnswer(t *testing.T) {
	cfg := testConfig()
	cfg.Bot.Groups = nil
	server := newTestServer(t, Options{Config: fakeConfig{cfg}})

	var response judgementsResponse
	recorder := get(t, server, "/api/v1/judgements")
	decode(t, recorder, &response)
	if !response.GroupRequired {
		t.Fatal("a deployment with no named groups did not say why it cannot answer")
	}
	if len(response.Judgements) != 0 {
		t.Fatalf("an unanswerable question produced %d judgements", len(response.Judgements))
	}
}

// TestJudgementsMergeEveryGroup covers the merge, which is the part of this
// package that is its own: no store query reads every group.
func TestJudgementsMergeEveryGroup(t *testing.T) {
	cfg := testConfig()
	groupA, groupB := cfg.Bot.Groups[0].OpenID, cfg.Bot.Groups[1].OpenID
	data := fakeStore{judgements: map[string][]store.Judgement{
		groupA: {{ID: "older", GroupOpenID: groupA, Verdict: "violation", CreatedAt: 100}},
		groupB: {{ID: "newer", GroupOpenID: groupB, Verdict: "ok", CreatedAt: 200}},
	}}
	server := newTestServer(t, Options{Config: fakeConfig{cfg}, Store: data})

	var response judgementsResponse
	decode(t, get(t, server, "/api/v1/judgements"), &response)
	if len(response.Judgements) != 2 {
		t.Fatalf("got %d judgements, want 2", len(response.Judgements))
	}
	if response.Judgements[0].ID != "newer" {
		t.Fatalf("the newest judgement is not first: %s", response.Judgements[0].ID)
	}
	if response.Judgements[0].Verdict != store.JudgementOK {
		t.Fatalf("the verdict was not passed through: %q", response.Judgements[0].Verdict)
	}
}

// TestCategoriesKeepTheirOwnNameWhenNothingCanLabelThem covers the fallback: the
// word a group is told belongs to the moderation feature, and a deployment
// without it shows the category as written rather than inventing one.
func TestCategoriesKeepTheirOwnNameWhenNothingCanLabelThem(t *testing.T) {
	cfg := testConfig()
	group := cfg.Bot.Groups[0].OpenID
	data := fakeStore{judgements: map[string][]store.Judgement{
		group: {{ID: "one", GroupOpenID: group, Category: "ad", Verdict: "violation"}},
	}}
	server := newTestServer(t, Options{Config: fakeConfig{cfg}, Store: data})

	var response judgementsResponse
	decode(t, get(t, server, "/api/v1/judgements"), &response)
	if response.Judgements[0].CategoryLabel != "ad" {
		t.Fatalf("an unlabellable category was not shown as itself: %q",
			response.Judgements[0].CategoryLabel)
	}
}

// TestRecallsAreNeverNull covers the shape a front end maps over: an empty list
// and a missing one are different things in JSON, and the second is how a page
// ends up with a crash instead of an empty row.
func TestRecallsAreNeverNull(t *testing.T) {
	cfg := testConfig()
	group := cfg.Bot.Groups[0].OpenID
	data := fakeStore{judgements: map[string][]store.Judgement{
		group: {{ID: "one", GroupOpenID: group, Verdict: "ok"}},
	}}
	body := get(t, newTestServer(t, Options{Config: fakeConfig{cfg}, Store: data}),
		"/api/v1/judgements").Body.String()
	if strings.Contains(body, `"recalls":null`) || strings.Contains(body, `"message_ids":null`) {
		t.Fatalf("a list came back as null:\n%s", body)
	}
}

// --- the one thing that is not a read --------------------------------------

// TestReloadReportsWhatHappened covers the answer that matters most: a console
// that says "done" when the file does not parse leaves an operator believing a
// change is in force when it is not.
func TestReloadReportsWhatHappened(t *testing.T) {
	reloader := &fakeReload{err: errors.New("config: parsing: line 12: bad indent")}
	server := newTestServer(t, Options{Reload: reloader})

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder,
		httptest.NewRequest(http.MethodPost, "/api/v1/config/reload", nil))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("a failed reload answered %d, want 500", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "bad indent") {
		t.Fatalf("the reason the file failed was not passed through:\n%s", recorder.Body.String())
	}

	reloader.err = nil
	recorder = httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder,
		httptest.NewRequest(http.MethodPost, "/api/v1/config/reload", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("a successful reload answered %d, want 200", recorder.Code)
	}
	if reloader.calls != 2 {
		t.Fatalf("the reloader was called %d times, want 2", reloader.calls)
	}
}

// --- the front end ---------------------------------------------------------

// TestFrontEndNotBuiltSaysSo covers a checkout that has not been built, where the
// missing file has exactly one cause and a 404 would hide it.
func TestFrontEndNotBuiltSaysSo(t *testing.T) {
	server := newTestServer(t, Options{Web: fstest.MapFS{}})
	recorder := get(t, server, "/")
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("an unbuilt front end answered %d, want 503", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "pnpm build") {
		t.Fatalf("the answer does not say how to build it:\n%s", recorder.Body.String())
	}
}

// TestFrontEndServesTheShellForAClientRoute covers the refresh: a client-side
// route is not a file, and it is answered with the shell rather than a 404.
func TestFrontEndServesTheShellForAClientRoute(t *testing.T) {
	web := fstest.MapFS{
		"index.html":          &fstest.MapFile{Data: []byte("<html>控制台</html>")},
		"assets/index-abc.js": &fstest.MapFile{Data: []byte("console.log(1)")},
	}
	server := newTestServer(t, Options{Web: web})

	shell := get(t, server, "/groups/FABED0ACCA31B997DFF11DC30658D252")
	if shell.Code != http.StatusOK {
		t.Fatalf("a client route answered %d, want 200", shell.Code)
	}
	if !strings.Contains(shell.Body.String(), "控制台") {
		t.Fatalf("a client route did not answer with the shell:\n%s", shell.Body.String())
	}
	if got := shell.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("the shell may be cached: %q", got)
	}

	asset := get(t, server, "/assets/index-abc.js")
	if asset.Code != http.StatusOK {
		t.Fatalf("a built asset answered %d, want 200", asset.Code)
	}
	if !strings.Contains(asset.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("a hashed asset is not cacheable: %q", asset.Header().Get("Cache-Control"))
	}
}

// TestUnknownAPIPathIsNotFound covers the mistake made in a script rather than in
// a browser: answering it with the front end would send the caller looking in the
// wrong place.
func TestUnknownAPIPathIsNotFound(t *testing.T) {
	server := newTestServer(t, Options{
		Web: fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<html></html>")}},
	})
	recorder := get(t, server, "/api/v1/nonsense")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("an unknown endpoint answered %d, want 404", recorder.Code)
	}
	var body errorBody
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("the failure was not JSON: %v", err)
	}
	if body.Error.Code != "unknown_endpoint" {
		t.Fatalf("got error code %q", body.Error.Code)
	}
}

// TestMethodIsCheckedByTheRouter covers a rule that is declared rather than
// written: the patterns carry their methods, so a write sent to a read does not
// reach the read at all.
func TestMethodIsCheckedByTheRouter(t *testing.T) {
	server := newTestServer(t, Options{})
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder,
		httptest.NewRequest(http.MethodDelete, "/api/v1/status", nil))
	if recorder.Code == http.StatusOK {
		t.Fatal("a DELETE reached the read")
	}
}

// --- the pure conversions --------------------------------------------------

// TestScenesOf lists a command's scenes once each, whichever panel placements
// name it.
func TestScenesOf(t *testing.T) {
	for _, test := range []struct {
		name string
		def  command.Def
		want []string
	}{
		{"group only", command.Def{Panels: []command.PanelPlacement{
			{Scene: command.InGroup}}}, []string{"group"}},
		{"both", command.Def{Panels: []command.PanelPlacement{
			{Scene: command.InGroup}, {Scene: command.InPrivate}}}, []string{"group", "private"}},
		{"neither", command.Def{}, []string{}},
		{"a combined placement", command.Def{Panels: []command.PanelPlacement{
			{Scene: command.InGroup | command.InPrivate}}}, []string{"group", "private"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := scenesOf(test.def)
			if strings.Join(got, ",") != strings.Join(test.want, ",") {
				t.Fatalf("got %v, want %v", got, test.want)
			}
		})
	}
}

// TestAffectedSecondsAreZeroForNever covers a column whose zero is not 1970: it
// means "never", and a real instant in front of an operator is a worse answer
// than a blank.
func TestAffectedSecondsAreZeroForNever(t *testing.T) {
	if got := seconds(time.Time{}); got != 0 {
		t.Fatalf("the zero time became %d", got)
	}
	at := time.Unix(1700000000, 0)
	if got := seconds(at); got != 1700000000 {
		t.Fatalf("a real time became %d", got)
	}
}

// TestQueryBoundsAreApplied covers the limits, which are the only thing standing
// between a typo in a URL and a table scan.
func TestQueryBoundsAreApplied(t *testing.T) {
	for _, test := range []struct {
		query string
		want  int
	}{
		{"", defaultLimit},
		{"?limit=5", 5},
		{"?limit=0", defaultLimit},
		{"?limit=-1", defaultLimit},
		{"?limit=not-a-number", defaultLimit},
		{"?limit=999999", maxLimit},
	} {
		t.Run(test.query, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/api/v1/pending"+test.query, nil)
			if got := limitOf(request, defaultLimit); got != test.want {
				t.Fatalf("got %d, want %d", got, test.want)
			}
		})
	}
}

// TestSinceIsClamped covers the window: a caller may not ask for the beginning of
// time, and a window that starts in the future is not a window.
func TestSinceIsClamped(t *testing.T) {
	server := newTestServer(t, Options{})
	now := time.Unix(1_700_000_000, 0)

	for _, test := range []struct {
		name  string
		query string
		want  time.Duration
	}{
		{"nothing asked for", "", defaultWindow},
		{"an hour", "?since=1699996400", time.Hour},
		{"the future", "?since=1800000000", 0},
		{"the beginning of time", "?since=1", maxWindow},
		{"not a number", "?since=soon", defaultWindow},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/api/v1/pending"+test.query, nil)
			got := now.Sub(server.sinceOf(request, now))
			if got != test.want {
				t.Fatalf("got a window of %v, want %v", got, test.want)
			}
		})
	}
}

// TestGroupOverrideReadsOnlyTheGroupsShape covers a section that has no overrides
// at all, which every feature's section starts as.
func TestGroupOverrideReadsOnlyTheGroupsShape(t *testing.T) {
	var node yaml.Node
	if err := yaml.Unmarshal([]byte("enabled: true\ndebug: false\n"), &node); err != nil {
		t.Fatalf("building the section: %v", err)
	}
	if got := groupOverride(*node.Content[0], "any-group"); got != nil {
		t.Fatalf("a section with no groups answered %v", *got)
	}
}

// TestIsLoopbackIsStrict covers the check the configuration relies on to decide
// whether a token is required. A typo has to fail closed: binding somewhere
// unexpected without a token is the outcome this exists to prevent.
func TestIsLoopbackIsStrict(t *testing.T) {
	for _, test := range []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:8787", true},
		{"[::1]:8787", true},
		{"localhost:8787", true},
		{"0.0.0.0:8787", false},
		{":8787", false},
		{"192.168.1.10:8787", false},
		{"127.0.0.1", false},
		{"nonsense", false},
	} {
		if got := config.IsLoopback(test.addr); got != test.want {
			t.Errorf("IsLoopback(%q) = %v, want %v", test.addr, got, test.want)
		}
	}
}

// --- the log ---------------------------------------------------------------

// logServer builds a server over a buffer that has already been logged to.
func logServer(t *testing.T, lines ...string) *Server {
	t.Helper()
	buffer := logring.New(64)
	logger := slog.New(buffer.Wrap(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug})))
	for _, line := range lines {
		logger.Info(line, "component", "bot")
	}
	return newTestServer(t, Options{Logs: buffer})
}

// TestTheLogAnswersWhatWasLoggedWithTheFieldsInIt covers the whole point of the
// page: the newest lines, in order, with the level and the structured fields.
func TestTheLogAnswersWhatWasLoggedWithTheFieldsInIt(t *testing.T) {
	server := logServer(t, "the first line", "the second line")

	var response logsResponse
	decode(t, get(t, server, "/api/v1/logs"), &response)

	if len(response.Records) != 2 {
		t.Fatalf("answered with %d records, want 2", len(response.Records))
	}
	if response.Records[0].Message != "the first line" {
		t.Errorf("the oldest record is %q, want the oldest first", response.Records[0].Message)
	}
	if response.Records[1].Seq <= response.Records[0].Seq {
		t.Errorf("sequence numbers do not increase: %d then %d",
			response.Records[0].Seq, response.Records[1].Seq)
	}
	if response.Records[0].Level != "INFO" {
		t.Errorf("level = %q, want INFO", response.Records[0].Level)
	}
	if len(response.Records[0].Attrs) != 1 || response.Records[0].Attrs[0].Key != "component" {
		t.Errorf("attrs = %+v, want component in them", response.Records[0].Attrs)
	}
	if response.Capacity != 64 || response.Kept != 2 {
		t.Errorf("capacity = %d, kept = %d, want 64 and 2", response.Capacity, response.Kept)
	}
	if response.NextSeq != response.Records[1].Seq {
		t.Errorf("next_seq = %d, want the newest sequence number %d",
			response.NextSeq, response.Records[1].Seq)
	}
}

// TestTheLogSaysHowManyRecordsItLost covers the disclosure that keeps a wrapped
// buffer from looking like a quiet bot.
func TestTheLogSaysHowManyRecordsItLost(t *testing.T) {
	buffer := logring.New(2)
	logger := slog.New(buffer.Wrap(slog.NewTextHandler(io.Discard, nil)))
	for _, line := range []string{"one", "two", "three"} {
		logger.Info(line)
	}
	server := newTestServer(t, Options{Logs: buffer})

	var response logsResponse
	decode(t, get(t, server, "/api/v1/logs"), &response)
	if response.Dropped != 1 {
		t.Errorf("dropped = %d, want 1", response.Dropped)
	}
	if response.Kept != 2 {
		t.Errorf("kept = %d, want 2", response.Kept)
	}
}

// TestTheLogEchoesTheFilterItApplied covers a request that asks for a level this
// side does not know: the answer says what was really applied rather than letting
// the caller believe the filter took effect.
func TestTheLogEchoesTheFilterItApplied(t *testing.T) {
	buffer := logring.New(16)
	logger := slog.New(buffer.Wrap(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug})))
	logger.Debug("quiet")
	logger.Info("ordinary")
	logger.Error("loud")
	server := newTestServer(t, Options{Logs: buffer})

	for _, test := range []struct {
		query     string
		wantLevel string
		want      int
	}{
		{"", "INFO", 2},
		{"?level=debug", "DEBUG", 3},
		{"?level=ERROR", "ERROR", 1},
		{"?level=nonsense", "INFO", 2},
	} {
		t.Run(test.query, func(t *testing.T) {
			var response logsResponse
			decode(t, get(t, server, "/api/v1/logs"+test.query), &response)
			if response.Level != test.wantLevel {
				t.Errorf("level = %q, want %q", response.Level, test.wantLevel)
			}
			if len(response.Records) != test.want {
				t.Errorf("answered with %d records, want %d", len(response.Records), test.want)
			}
		})
	}
}

// TestTheLogCanBeAskedForOnlyWhatIsNew covers the reconnect: a reader says which
// sequence number it already has and gets the gap, not the whole buffer again.
func TestTheLogCanBeAskedForOnlyWhatIsNew(t *testing.T) {
	server := logServer(t, "one", "two", "three")

	var all logsResponse
	decode(t, get(t, server, "/api/v1/logs"), &all)
	if len(all.Records) != 3 {
		t.Fatalf("answered with %d records, want 3", len(all.Records))
	}
	seen := all.Records[0].Seq
	var gap logsResponse
	decode(t, get(t, server, "/api/v1/logs?after="+strconv.FormatUint(seen, 10)), &gap)
	if len(gap.Records) != 2 {
		t.Fatalf("answered with %d records after %d, want 2", len(gap.Records), seen)
	}
	if gap.Records[0].Seq != seen+1 {
		t.Errorf("the first record after %d has sequence %d", seen, gap.Records[0].Seq)
	}
}

// TestTheLogAnswersAnEmptyBufferAsAnEmptyList covers the shape a front end maps
// over: [] and null are different answers.
func TestTheLogAnswersAnEmptyBufferAsAnEmptyList(t *testing.T) {
	server := newTestServer(t, Options{Logs: logring.New(8)})
	body := get(t, server, "/api/v1/logs").Body.String()
	if !strings.Contains(body, `"records":[]`) {
		t.Fatalf("an empty buffer answered %s, want an empty records array", body)
	}
}

// TestTheLogAnswersWithoutABufferRatherThanFailing covers the build that has no
// buffer: the page still loads and says what it has, which is nothing.
func TestTheLogAnswersWithoutABufferRatherThanFailing(t *testing.T) {
	server := newTestServer(t, Options{})
	var response logsResponse
	decode(t, get(t, server, "/api/v1/logs"), &response)
	if response.Capacity != 0 || len(response.Records) != 0 {
		t.Fatalf("a bufferless deployment answered %+v, want nothing", response)
	}
	// The stream is the one that cannot pretend: there is nothing to stream from.
	if got := get(t, server, "/api/v1/logs/stream").Code; got != http.StatusServiceUnavailable {
		t.Errorf("the stream answered %d, want 503", got)
	}
}

// TestTheLogStreamSendsWhatIsLoggedAfterItOpened covers the live half: the
// response is an event stream, it says which filter it is applying, and a record
// logged afterwards arrives on it.
func TestTheLogStreamSendsWhatIsLoggedAfterItOpened(t *testing.T) {
	buffer := logring.New(16)
	logger := slog.New(buffer.Wrap(slog.NewTextHandler(io.Discard, nil)))
	server := newTestServer(t, Options{Logs: buffer})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/logs/stream", nil).WithContext(ctx)
	recorder := &streamRecorder{header: http.Header{}}
	opened := make(chan struct{})
	recorder.onFlush = func() {
		select {
		case <-opened:
		default:
			close(opened)
		}
	}

	served := make(chan struct{})
	go func() {
		defer close(served)
		server.Handler().ServeHTTP(recorder, request)
	}()

	// Wait for the opening event before logging, so the test is about a record
	// arriving after the stream opened rather than about one that was already
	// there.
	select {
	case <-opened:
	case <-time.After(2 * time.Second):
		t.Fatal("the stream did not open within two seconds")
	}

	logger.Warn("something happened", "group", "FABED0AC")
	body := waitFor(t, recorder, "something happened")
	cancel()
	select {
	case <-served:
	case <-time.After(2 * time.Second):
		t.Fatal("the stream did not end when its caller left")
	}

	if !strings.HasPrefix(body, "retry:") {
		t.Errorf("the stream did not start with a retry hint:\n%s", body)
	}
	if !strings.Contains(body, `event: open`) || !strings.Contains(body, `"level":"INFO"`) {
		t.Errorf("the opening event did not name the filter:\n%s", body)
	}
	if !strings.Contains(body, `"level":"WARN"`) {
		t.Errorf("the record's level was not in the event:\n%s", body)
	}
	if !strings.Contains(body, `{"key":"group","value":"FABED0AC"}`) {
		t.Errorf("the record's fields were not in the event:\n%s", body)
	}
}

// TestTheLogStreamReplaysTheGapOnReconnect covers what a reconnect must not do:
// lose the lines written while the connection was down.
func TestTheLogStreamReplaysTheGapOnReconnect(t *testing.T) {
	buffer := logring.New(16)
	logger := slog.New(buffer.Wrap(slog.NewTextHandler(io.Discard, nil)))
	for _, line := range []string{"while away one", "while away two"} {
		logger.Info(line)
	}
	server := newTestServer(t, Options{Logs: buffer})

	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodGet, "/api/v1/logs/stream?after=0", nil).WithContext(ctx)
	recorder := &streamRecorder{header: http.Header{}}
	served := make(chan struct{})
	go func() {
		defer close(served)
		server.Handler().ServeHTTP(recorder, request)
	}()
	// The replay is written before the loop starts, so waiting for the records
	// themselves is what this test can wait for.
	waitFor(t, recorder, "while away two")
	cancel()
	<-served
}

// waitFor waits until the stream has written something containing needle, and
// returns everything written so far.
//
// A test cannot just log and then look: the handler is a select, and a record
// that is ready and a cancelled context that is ready are equally likely to be
// chosen, so waiting for the write is what makes the test say what it means.
func waitFor(t *testing.T, recorder *streamRecorder, needle string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		body := recorder.snapshot()
		if strings.Contains(body, needle) {
			return body
		}
		if time.Now().After(deadline) {
			t.Fatalf("%q never arrived:\n%s", needle, body)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// streamRecorder is a response writer that keeps what was written and reports
// every flush, so a test can tell a stream from a buffered answer.
type streamRecorder struct {
	header  http.Header
	body    strings.Builder
	status  int
	onFlush func()
	mu      sync.Mutex
}

func (r *streamRecorder) Header() http.Header { return r.header }

func (r *streamRecorder) WriteHeader(status int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status = status
}

func (r *streamRecorder) Write(payload []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body.Write(payload)
}

// snapshot is what has been written so far. It takes the lock the writer holds,
// because a test reads this while the handler is still running.
func (r *streamRecorder) snapshot() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body.String()
}

func (r *streamRecorder) Flush() {
	if r.onFlush != nil {
		r.onFlush()
	}
}

// --- the three things an operator does to somebody waiting to verify ---------

// fakeVerifier records what it was asked to do, and answers what the test said.
//
// The embedded interface is nil, like the store fakes: a test that spelled out
// all of feature.Feature would be four methods saying nothing.
type fakeVerifier struct {
	feature.Feature
	resend  []string
	approve []string
	reject  []string
	err     error
}

func (f *fakeVerifier) Resend(_ context.Context, group, member string) error {
	f.resend = append(f.resend, group+"/"+member)
	return f.err
}

func (f *fakeVerifier) Approve(_ context.Context, group, member string) error {
	f.approve = append(f.approve, group+"/"+member)
	return f.err
}

func (f *fakeVerifier) Reject(_ context.Context, group, member string) error {
	f.reject = append(f.reject, group+"/"+member)
	return f.err
}

// verificationStore is a store that records what was written to it.
type verificationStore struct {
	store.Store
	pending []store.Pending
	added   []store.Barred
}

func (s *verificationStore) Pending() store.PendingStore {
	return verificationPending{store: s}
}

func (s *verificationStore) Blacklist() store.BlacklistStore {
	return &verificationBlacklist{store: s}
}

// verificationPending answers the queue the test set up.
type verificationPending struct {
	store.PendingStore
	store *verificationStore
}

func (p verificationPending) Load(_ context.Context, _ time.Time) ([]store.Pending, error) {
	return p.store.pending, nil
}

// verificationBlacklist records what was barred.
type verificationBlacklist struct {
	store.BlacklistStore
	store *verificationStore
}

func (b *verificationBlacklist) Add(_ context.Context, entry store.Barred) error {
	b.store.added = append(b.store.added, entry)
	return nil
}

// post sends a request with an optional body.
func post(t *testing.T, server *Server, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(http.MethodPost, target, reader)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	return recorder
}

// waitingVerification is the queue one of these tests acts on.
func waitingVerification() []store.Pending {
	return []store.Pending{{
		Token:        "TOKEN-A",
		GroupOpenID:  "FABED0ACCA31B997DFF11DC30658D252",
		MemberOpenID: "D4E5F6A1B2C3D4E5F6A1B2C3D4E5F6A1",
		JoinedAt:     1791837564,
		Deadline:     1791837564 + 48*3600,
		HeldUntil:    0,
	}}
}

// verificationServer builds a console with a verifier and a queue.
func verificationServer(t *testing.T, verifier *fakeVerifier, data *verificationStore) *Server {
	t.Helper()
	features := fakeFeatures{
		running: map[string]bool{joinverify.Name: true},
		live:    map[string]feature.Feature{joinverify.Name: verifier},
	}
	return newTestServer(t, Options{Store: data, Features: features})
}

// TestTheThreeVerificationActionsReachTheFeature covers the whole point: the
// console asks the feature that owns verification, and reports what it said.
func TestTheThreeVerificationActionsReachTheFeature(t *testing.T) {
	data := &verificationStore{pending: waitingVerification()}
	verifier := &fakeVerifier{}
	server := verificationServer(t, verifier, data)

	var resent verificationActionResponse
	decode(t, post(t, server, "/api/v1/verifications/TOKEN-A/resend", ""), &resent)
	if len(verifier.resend) != 1 {
		t.Fatalf("resend reached the feature %d times, want 1", len(verifier.resend))
	}
	if resent.Action != "resend" || resent.MemberOpenID != "D4E5F6A1B2C3D4E5F6A1B2C3D4E5F6A1" {
		t.Errorf("resend answered %+v", resent)
	}
	if resent.Pending != 1 {
		t.Errorf("pending = %d, want the queue counted after the action", resent.Pending)
	}

	var approved verificationActionResponse
	decode(t, post(t, server, "/api/v1/verifications/TOKEN-A/approve", ""), &approved)
	if len(verifier.approve) != 1 {
		t.Fatalf("approve reached the feature %d times, want 1", len(verifier.approve))
	}

	var rejected verificationActionResponse
	decode(t, post(t, server, "/api/v1/verifications/TOKEN-A/reject", `{"blacklist":true,"reason":"广告号"}`), &rejected)
	if len(verifier.reject) != 1 {
		t.Fatalf("reject reached the feature %d times, want 1", len(verifier.reject))
	}
	if !rejected.Blacklisted {
		t.Errorf("reject answered %+v, want the blacklist write reported", rejected)
	}
	if len(data.added) != 1 {
		t.Fatalf("the blacklist received %d entries, want 1", len(data.added))
	}
	entry := data.added[0]
	if entry.MemberOpenID != "D4E5F6A1B2C3D4E5F6A1B2C3D4E5F6A1" ||
		entry.GroupOpenID != "FABED0ACCA31B997DFF11DC30658D252" ||
		entry.Reason != "广告号" || entry.AddedBy == "" || entry.ID == "" {
		t.Errorf("the entry written is %+v", entry)
	}
}

// TestRejectingWithoutBarringWritesNothing covers the ordinary case: the body is
// optional, and no body must mean "just turn them away".
func TestRejectingWithoutBarringWritesNothing(t *testing.T) {
	data := &verificationStore{pending: waitingVerification()}
	server := verificationServer(t, &fakeVerifier{}, data)

	var response verificationActionResponse
	decode(t, post(t, server, "/api/v1/verifications/TOKEN-A/reject", ""), &response)
	if response.Blacklisted || len(data.added) != 0 {
		t.Errorf("a rejection with no body wrote %+v", data.added)
	}
}

// TestATokenThatIsNotInTheQueueIsAnsweredAsSuch covers the ordinary end of a
// verification: the page asked about somebody who is no longer waiting.
func TestATokenThatIsNotInTheQueueIsAnsweredAsSuch(t *testing.T) {
	data := &verificationStore{}
	server := verificationServer(t, &fakeVerifier{}, data)

	recorder := post(t, server, "/api/v1/verifications/GONE/approve", "")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("got status %d, want 404; body: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "verification_not_found") {
		t.Errorf("the refusal did not name itself: %s", recorder.Body.String())
	}
}

// TestTheFeatureLettingGoIsAConflictRatherThanAFailure covers the race the
// console cannot avoid: the feature released the member between the page loading
// and the button being pressed.
func TestTheFeatureLettingGoIsAConflictRatherThanAFailure(t *testing.T) {
	data := &verificationStore{pending: waitingVerification()}
	verifier := &fakeVerifier{err: &joinverify.NotHeld{Message: "该成员不在验证中，无法放行"}}
	server := verificationServer(t, verifier, data)

	recorder := post(t, server, "/api/v1/verifications/TOKEN-A/approve", "")
	if recorder.Code != http.StatusConflict {
		t.Fatalf("got status %d, want 409; body: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "不在验证中") {
		t.Errorf("the feature's own sentence was not passed on: %s", recorder.Body.String())
	}
}

// TestTheConsoleSaysSoWhenVerificationIsNotRunning covers a deployment with the
// console and without the feature: the button must answer, not crash.
func TestTheConsoleSaysSoWhenVerificationIsNotRunning(t *testing.T) {
	data := &verificationStore{pending: waitingVerification()}
	server := newTestServer(t, Options{Store: data})
	if got := post(t, server, "/api/v1/verifications/TOKEN-A/approve", "").Code; got != http.StatusServiceUnavailable {
		t.Fatalf("got status %d, want 503", got)
	}
}

// TestABadRejectBodyIsRefusedWithoutReachingTheFeature covers the request that
// cannot be understood: nothing must happen to the member.
func TestABadRejectBodyIsRefusedWithoutReachingTheFeature(t *testing.T) {
	data := &verificationStore{pending: waitingVerification()}
	verifier := &fakeVerifier{}
	server := verificationServer(t, verifier, data)

	recorder := post(t, server, "/api/v1/verifications/TOKEN-A/reject", `{"blacklist":`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("got status %d, want 400; body: %s", recorder.Code, recorder.Body.String())
	}
	if len(verifier.reject) != 0 {
		t.Errorf("the feature was asked to reject anyway: %v", verifier.reject)
	}
}

// TestTheCommandTableListsWhatEveryFeatureAnswers covers the console's half of a
// mistake worth naming: the group's table is kept by one feature while the commands
// in it are offered by several. Asking the keeper for its own sources answered an
// empty list on a bot whose table was full, so the console asks the running set as a
// whole -- this pins that the answer is rendered from what it is handed.
func TestTheCommandTableListsWhatEveryFeatureAnswers(t *testing.T) {
	features := fakeFeatures{
		running: map[string]bool{"admin_commands": true},
		live:    map[string]feature.Feature{},
		commands: []command.Def{{
			Name:     "群广播",
			Aliases:  []string{"广播", "群通报"},
			Usage:    "{prefix}群广播 —— 私聊里打开广播卡片",
			Desc:     "打开广播卡片",
			Audience: command.Admins,
			Panels:   []command.PanelPlacement{{Scene: command.InPrivate}, {Scene: command.InGroup}},
		}},
	}
	server := newTestServer(t, Options{Features: features})

	var response commandsResponse
	decode(t, get(t, server, "/api/v1/commands"), &response)
	if response.Prefix != "/" {
		t.Errorf("got prefix %q, want /", response.Prefix)
	}
	if len(response.Commands) != 1 {
		t.Fatalf("got %d commands, want 1", len(response.Commands))
	}
	entry := response.Commands[0]
	if entry.Name != "群广播" {
		t.Errorf("got name %q, want 群广播", entry.Name)
	}
	if entry.Usage != "/群广播 —— 私聊里打开广播卡片" {
		t.Errorf("got usage %q, want the configured prefix filled in", entry.Usage)
	}
	if len(entry.Aliases) != 2 || entry.Aliases[0] != "广播" || entry.Aliases[1] != "群通报" {
		t.Errorf("got aliases %v, want [广播 群通报]", entry.Aliases)
	}
	if entry.Desc != "打开广播卡片" {
		t.Errorf("got desc %q", entry.Desc)
	}
	if entry.Audience != "admins" {
		t.Errorf("got audience %q, want admins", entry.Audience)
	}
	if len(entry.Scenes) != 2 || entry.Scenes[0] != "private" || entry.Scenes[1] != "group" {
		t.Errorf("got scenes %v, want [private group]", entry.Scenes)
	}
}

// --- helpers the assertions need -------------------------------------------
