// Command whois connects to the gateway and prints the identifiers an operator
// needs to configure a group for the first time.
//
// The official platform never lists the groups a bot is in, and it only ever
// names a member by an app scoped openid, so both have to be read from a live
// event. Running this while somebody sends one message in the group prints the
// group openid and that member's openid, ready to paste into the configuration.
//
// Usage:
//
//	whois
//
// It reads the same environment variables as the bot:
//
//	QQBOT_APPID, QQBOT_SECRET, QQBOT_BOT_QQ (optional)
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"

	"github.com/fouc3/qq-group-management-bot/internal/config"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client, err := buildClient()
	if err != nil {
		fail("%v", err)
	}

	// Every group event carries the group openid, and most carry the member.
	for _, eventType := range []string{
		qqbotsdk.EventGroupAddRobot,
		qqbotsdk.EventGroupMessageCreate,
		qqbotsdk.EventGroupAtMessageCreate,
		qqbotsdk.EventGroupMemberAdd,
		qqbotsdk.EventGroupJoinRequest,
	} {
		client.RegisterFunc(eventType, report)
	}

	gateway, err := client.GetGateway(ctx)
	if err != nil {
		fail("getting the gateway: %v", err)
	}
	client.UseTransport(qqbotsdk.NewWebSocketTransport(gateway.URL,
		qqbotsdk.WithIntents(qqbotsdk.IntentGroupAndC2CEvent|qqbotsdk.IntentGroupMemberEvent)))
	if err := client.Start(ctx); err != nil {
		fail("starting: %v", err)
	}
	fmt.Println("listening; send one message in the group you want to configure")
	fmt.Println()

	<-ctx.Done()
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	_ = client.Stop(stopCtx)
}

// report prints the identifiers one event carries.
func report(_ context.Context, event *qqbotsdk.Event) error {
	value, err := event.Decode()
	if err != nil {
		fmt.Printf("%-28s (could not decode: %v)\n", event.Type, err)
		return nil
	}

	var groupOpenID, memberOpenID, username, extra string
	switch data := value.(type) {
	case *qqbotsdk.GroupMessageCreateData:
		groupOpenID = data.GroupOpenID
		if data.Author != nil {
			memberOpenID = data.Author.MemberOpenID
			username = data.Author.Username
		}
		// The mentions are the useful part when another client mentions a QQ
		// number: the platform resolves it into the openid the bot must use.
		extra = fmt.Sprintf("content=%q mentions=[%s]", data.Content, mentionList(data.Mentions))
	case *qqbotsdk.GroupAddRobotData:
		groupOpenID = data.GroupOpenID
		memberOpenID = data.OpMemberOpenID
	case *qqbotsdk.GroupMemberAddData:
		groupOpenID = data.GroupOpenID
		memberOpenID = data.MemberOpenID
	case *qqbotsdk.GroupJoinRequestData:
		groupOpenID = data.GroupOpenID
		memberOpenID = data.MemberOpenID
		username = data.Username
		extra = fmt.Sprintf("request=%s", data.JoinRequestID)
	}

	fmt.Printf("%-28s group=%s member=%s name=%q %s\n",
		event.Type, orDash(groupOpenID), orDash(memberOpenID), username, extra)
	return nil
}

// mentionList renders the members a message mentioned, which is how a QQ number
// becomes the openid the bot has to use.
func mentionList(mentions []qqbotsdk.User) string {
	parts := make([]string, 0, len(mentions))
	for _, mention := range mentions {
		parts = append(parts, fmt.Sprintf("%s(name=%q)", orDash(mention.MemberOpenID), mention.Username))
	}
	return strings.Join(parts, " ")
}

// buildClient builds a client from the same environment variables the bot uses.
func buildClient() (*qqbotsdk.Client, error) {
	cfg, err := config.Load(configPath())
	if err != nil {
		return nil, err
	}
	return qqbotsdk.NewClientFromConfig(qqbotsdk.Config{
		AppID:        cfg.Bot.AppID,
		ClientSecret: cfg.Bot.ClientSecret,
		AccessToken:  cfg.Bot.AccessToken,
		BaseURL:      cfg.Bot.ResolvedBaseURL(),
	})
}

// configPath is the file the credentials are read from.
func configPath() string {
	if path := os.Getenv("QQBOT_CONFIG"); path != "" {
		return path
	}
	return config.DefaultConfigPath
}

// orDash keeps a missing identifier visible rather than blank.
func orDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "whois: "+format+"\n", args...)
	os.Exit(1)
}
