package app

import (
	"context"
	"io"
	"log/slog"
	"testing"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"
	"github.com/redis/go-redis/v9"
	"gopkg.in/yaml.v3"

	"github.com/fouc3/qq-group-management-bot/internal/config"
	"github.com/fouc3/qq-group-management-bot/internal/feature"
	"github.com/fouc3/qq-group-management-bot/internal/features/joinrequest"
	"github.com/fouc3/qq-group-management-bot/internal/features/joinverify"
	"github.com/fouc3/qq-group-management-bot/internal/features/memberlog"
	"github.com/fouc3/qq-group-management-bot/internal/features/moderation"
)

// silentRedis is the logger installed over the cache client's own.
//
// The cache is not what this test is about, and the address the feature is handed
// answers nothing on purpose, so the client would otherwise narrate every retry on
// stderr and bury the assertion. The library keeps one logger for the process and
// offers no getter, so it stays silent for the rest of this package's tests -- none of
// which talk to a cache.
type silentRedis struct{}

func (silentRedis) Printf(context.Context, string, ...any) {}

// TestCloseLeavesNoEventHandlerBehind covers what replacing a feature requires.
//
// A feature is built, stopped and built again while the bot serves: a configuration
// change does exactly that, and so does a wide one. The instance that was stopped has
// to stop answering, and the only way to be sure of that is that it left nothing
// registered on the connection -- two instances of one feature answer every event
// twice, which is how one advertisement came to be withdrawn twice and its author
// silenced three times in three minutes.
//
// It lives in app because this is the only package that sees every feature: a feature
// cannot check this about another one. A feature that registers through
// command.Router gets it from Router.Stop, so the table below is the ones that
// register on the connection directly -- and the ones that do it correctly are in it
// too, so that the check is not merely a list of the three that are wrong.
//
// Every event type a feature owns is named rather than counted, because one shared
// dispatcher is deliberately left alone: the button router belongs to the bot's one
// connection, and cancelling it with a feature's own registrations would take the
// other features' presses with it.
func TestCloseLeavesNoEventHandlerBehind(t *testing.T) {
	ctx := context.Background()
	// The cache client narrates its retries on stderr; nothing in this test is about
	// the cache, so it is told to keep quiet.
	redis.SetLogger(silentRedis{})

	cases := []struct {
		name   string
		build  func(yaml.Node, feature.Deps) (feature.Feature, error)
		events []string
	}{
		{"member_log", memberlog.New, []string{
			qqbotsdk.EventGroupMemberAdd,
			qqbotsdk.EventGroupMemberRemove,
		}},
		{"join_request", joinrequest.New, []string{
			qqbotsdk.EventGroupJoinRequest,
		}},
		{"join_verification", joinverify.New, []string{
			qqbotsdk.EventGroupMemberAdd,
			qqbotsdk.EventGroupMessageCreate,
			qqbotsdk.EventGroupAtMessageCreate,
		}},
		{"moderation", moderation.New, []string{
			qqbotsdk.EventGroupAtMessageCreate,
			qqbotsdk.EventGroupMessageCreate,
		}},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			// A connection of its own per feature: a bot shares one dispatcher among
			// its features, so a shared one would count the others' handlers as well.
			client := qqbotsdk.NewClient("", "")
			deps := feature.Deps{
				Client: client,
				Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
				// Nothing answers at this address, which is what a test wants: the
				// cache is not what this is about, and a failure to reach it is
				// reported and lived with rather than being fatal.
				Redis: config.Redis{
					Addr: "127.0.0.1:1", Prefix: "qgb-test", DialTimeoutSeconds: 1,
				},
			}

			instance, err := test.build(section(t), deps)
			if err != nil {
				t.Fatalf("building the feature: %v", err)
			}
			if err := instance.Register(ctx); err != nil {
				t.Fatalf("registering the feature: %v", err)
			}

			for _, event := range test.events {
				if registered := client.Dispatcher().Handlers(event); registered == 0 {
					t.Fatalf("%s registered nothing for %s, so this test is not "+
						"looking at what it thinks it is", test.name, event)
				}
			}

			if err := instance.Close(ctx); err != nil {
				t.Fatalf("closing the feature: %v", err)
			}

			for _, event := range test.events {
				if registered := client.Dispatcher().Handlers(event); registered != 0 {
					t.Errorf("after Close, %s still has %d handler(s) for %s: the "+
						"instance that replaces it would answer every %s a second "+
						"time, and the one that was replaced goes on acting with the "+
						"configuration it was built from",
						test.name, registered, event, event)
				}
			}
		})
	}
}
