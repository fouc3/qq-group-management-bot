package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/fouc3/qq-group-management-bot/internal/config"
	"github.com/fouc3/qq-group-management-bot/internal/store"
	"github.com/fouc3/qq-group-management-bot/web"
)

// This file is the one test here that uses the real data layer and the real front
// end.
//
// It exists for the failure the fakes cannot catch: this package's structs are a
// second description of tables the store already describes, and the two can
// disagree -- a column renamed, a list that is nil where the front end expects
// [], a time that is a string in one place and a number in the other. A fake
// answers whatever the test told it to, which is exactly the thing that cannot be
// wrong.
//
// The database is a file in the test's own temporary directory, removed by the
// testing package when the test ends. It is never the deployment's database: that
// one is found from the user's state directory, and nothing here reads it.

// TestTheConsoleOverARealDatabase runs the whole thing: a real store, the
// compiled front end, and one HTTP request per screen the console has.
func TestTheConsoleOverARealDatabase(t *testing.T) {
	ctx := context.Background()
	now := time.Now()

	database, err := store.Open(ctx, store.Config{
		Driver: "sqlite",
		DSN:    filepath.Join(t.TempDir(), "console.db"),
	})
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	const group = "FABED0ACCA31B997DFF11DC30658D252"
	const other = "A1B2C3D4E5F6A1B2C3D4E5F6A1B2C3D4"

	// Everything is written through the store's own methods, which is the point: a
	// fixture inserted with raw SQL would prove the queries work against this
	// test's idea of the schema rather than against the store's.
	if err := database.Pending().Put(ctx, store.Pending{
		Token:        "a-token",
		GroupOpenID:  group,
		MemberOpenID: "MEMBER0000000000000000000000001",
		JoinedAt:     now.Add(-time.Hour).Unix(),
		Deadline:     now.Add(23 * time.Hour).Unix(),
		HeldUntil:    now.Add(23 * time.Hour).Unix(),
		Settings:     `{"Enabled":true,"MuteMode":"pseudo","DeadlineHours":24,"PseudoMuteAfter":3}`,
	}); err != nil {
		t.Fatalf("recording a hold: %v", err)
	}
	if err := database.Blacklist().Add(ctx, store.Barred{
		ID: "a-bar", GroupOpenID: group, MemberOpenID: "MEMBER0000000000000000000000002",
		Reason: "广告", AddedAt: now.Unix(), AddedBy: "2152595244",
	}); err != nil {
		t.Fatalf("barring an applicant: %v", err)
	}
	if err := database.Watches().Add(ctx, store.Watch{
		ID: "a-watch", GroupOpenID: group, MemberOpenID: "MEMBER0000000000000000000000003",
		Reason: "可疑", AddedAt: now.Unix(), AddedBy: "2152595244",
		ExpiresAt: now.Add(3 * time.Hour).Unix(),
	}); err != nil {
		t.Fatalf("marking a member: %v", err)
	}
	if err := database.Judgements().Record(ctx, store.Judgement{
		ID: "a-judgement", GroupOpenID: group, SubjectOpenID: "MEMBER0000000000000000000000004",
		ReporterOpenID: "2152595244", Category: "ad", Verdict: store.JudgementViolation,
		Model: "gpt-4o-mini", MessageIDs: []string{"884213901"},
		Reason: "推广文案", Action: "recall+mute", MuteSeconds: 3600,
		Recalls:   []store.RecallOutcome{{ID: "884213901", Text: "买它", Recalled: true}},
		CreatedAt: now.Unix(),
	}); err != nil {
		t.Fatalf("recording a judgement: %v", err)
	}
	if err := database.MemberEvents().Record(ctx, store.MemberEvent{
		GroupOpenID: group, GroupName: "开发测试群", GroupQQID: 123456789,
		MemberOpenID: "MEMBER0000000000000000000000005", Kind: store.MemberJoined,
		EventAt: now.Unix(),
	}); err != nil {
		t.Fatalf("recording a member event: %v", err)
	}
	if err := database.Broadcasts().Record(ctx, store.Broadcast{
		Token: "a-broadcast", SenderOpenID: "2152595244", Anonymous: true, Markdown: true,
		Content: "群通知：今晚维护", SentAt: now.Unix(),
		Targets: []store.Target{{GroupOpenID: group, MessageID: "msg-1"}},
	}); err != nil {
		t.Fatalf("recording a broadcast: %v", err)
	}

	cfg := &config.Config{}
	cfg.Bot.QQ = 2152595244
	cfg.Bot.Groups = config.Groups{
		{OpenID: group, QQGroupID: 123456789},
		{OpenID: other, QQGroupID: 987654321},
	}
	cfg.Database.Driver = "sqlite"
	cfg.Database.DSN = "test.db"
	cfg.Log.Level = "info"
	// The two sections that are read for something other than their own switch:
	// the prefix a command is typed with, and the model name.
	cfg.Features = map[string]yaml.Node{}
	for name, body := range map[string]string{
		"admin_commands": `enabled: true
prefix: "/"
groups:
  FABED0ACCA31B997DFF11DC30658D252:
    enabled: false`,
		"moderation": `enabled: true
model:
  name: gpt-4o-mini`,
	} {
		var node yaml.Node
		if err := yaml.Unmarshal([]byte(body), &node); err != nil {
			t.Fatalf("building the %s section: %v", name, err)
		}
		cfg.Features[name] = *node.Content[0]
	}

	server, err := New(Options{
		Admin:      config.Admin{Enabled: true, Listen: "127.0.0.1:8787"},
		Config:     fakeConfig{cfg},
		ConfigPath: "/tmp/for-a-test/config.yaml",
		Store:      database,
		// The real front end, as the binary carries it. A deployment serves this
		// exact file system, so a test that built its own would not test what
		// ships.
		Web:     web.Dist(),
		Started: now,
	})
	if err != nil {
		t.Fatalf("building the console: %v", err)
	}

	// Every read the console has, against the real tables.
	for _, path := range []string{
		"/api/v1/status",
		"/api/v1/overview",
		"/api/v1/groups",
		"/api/v1/pending",
		"/api/v1/member-events",
		"/api/v1/judgements",
		"/api/v1/blacklist",
		"/api/v1/high-risk",
		"/api/v1/broadcasts",
		"/api/v1/commands",
		"/api/v1/config",
	} {
		t.Run(path, func(t *testing.T) {
			recorder := get(t, server, path)
			if recorder.Code != http.StatusOK {
				t.Fatalf("got status %d: %s", recorder.Code, recorder.Body.String())
			}
			if !json.Valid(recorder.Body.Bytes()) {
				t.Fatalf("the answer is not JSON: %s", recorder.Body.String())
			}
		})
	}

	// The data that was written is the data that comes back, through the store and
	// through this package's own description of it.
	t.Run("what was written comes back", func(t *testing.T) {
		var overview overviewResponse
		decode(t, get(t, server, "/api/v1/overview"), &overview)
		if overview.Pending != 1 {
			t.Errorf("the overview says %d holds, want 1", overview.Pending)
		}
		if overview.HighRisk != 1 {
			t.Errorf("the overview says %d marks, want 1", overview.HighRisk)
		}
		if overview.Blacklist.Total != 1 {
			t.Errorf("the overview says %d barred entries, want 1", overview.Blacklist.Total)
		}
		if overview.Judgements.Violations != 1 {
			t.Errorf("the overview says %d violations, want 1", overview.Judgements.Violations)
		}
		if overview.MemberEvents.Joined != 1 {
			t.Errorf("the overview says %d joins, want 1", overview.MemberEvents.Joined)
		}
	})

	t.Run("the hold keeps its settings", func(t *testing.T) {
		var pending pendingResponse
		decode(t, get(t, server, "/api/v1/pending"), &pending)
		if len(pending.Pending) != 1 {
			t.Fatalf("got %d holds, want 1", len(pending.Pending))
		}
		if pending.Pending[0].MuteMode != "pseudo" || pending.Pending[0].LeftToTalk != 3 {
			t.Errorf("the settings column did not survive the round trip: %+v", pending.Pending[0])
		}
	})

	t.Run("a per-group switch is read from the file", func(t *testing.T) {
		var groups groupsResponse
		decode(t, get(t, server, "/api/v1/groups"), &groups)
		if len(groups.Groups) != 2 {
			t.Fatalf("got %d groups, want 2", len(groups.Groups))
		}
		found := false
		for _, feature := range groups.Groups[0].Features {
			if feature.Name != "admin_commands" {
				continue
			}
			found = true
			if feature.GroupOverride == nil || *feature.GroupOverride {
				t.Errorf("the group's own switch was not read: %+v", feature)
			}
			if feature.Effective {
				t.Error("the group turned the feature off and it is still effective")
			}
			if !feature.GlobalEnabled {
				t.Error("the feature's own switch was lost")
			}
		}
		if !found {
			t.Fatal("the commands feature is not in the answer at all")
		}
	})

	t.Run("a console with no features answers an empty table", func(t *testing.T) {
		// Nothing is wired into this console at all, so there is no table to read
		// and nobody offering a command. Worth pinning that an empty table is well
		// formed rather than a failure: it is also the state the real console is in
		// when every feature that answers a command is off.
		var commands commandsResponse
		decode(t, get(t, server, "/api/v1/commands"), &commands)
		if commands.Prefix != "/" {
			t.Errorf("got prefix %q, want /", commands.Prefix)
		}
		if commands.Commands == nil {
			t.Error("an empty table came back as null rather than []")
		}
	})

	t.Run("the model name is read from the file", func(t *testing.T) {
		var status statusResponse
		decode(t, get(t, server, "/api/v1/status"), &status)
		if status.JudgedModel != "gpt-4o-mini" {
			t.Errorf("got model %q, want gpt-4o-mini", status.JudgedModel)
		}
		if status.GroupsManaged != 2 {
			t.Errorf("got %d managed groups, want 2", status.GroupsManaged)
		}
	})

	// The front end as it ships: the shell is served, and it is the built one
	// rather than the placeholder a checkout without a build gets.
	t.Run("the built front end is served", func(t *testing.T) {
		recorder := get(t, server, "/")
		if recorder.Code != http.StatusOK {
			t.Fatalf("the shell answered %d: %s", recorder.Code, recorder.Body.String())
		}
		body := recorder.Body.String()
		if !strings.Contains(body, "<script") {
			t.Fatalf("the shell carries no script:\n%s", body)
		}
		if strings.Contains(body, "前端尚未构建") {
			t.Fatal("the placeholder was served instead of the built front end")
		}
	})
}
