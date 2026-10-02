package onebot

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// newServer starts a stub OneBot that records the last request.
func newServer(t *testing.T, answer string) (*Client, *recorded) {
	t.Helper()
	captured := &recorded{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.action = r.URL.Path
		captured.authorization = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&captured.body)
		if captured.answer != "" {
			w.WriteHeader(captured.status)
			_, _ = w.Write([]byte(captured.answer))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(answer))
	}))
	t.Cleanup(server.Close)

	client, err := New(Options{BaseURL: server.URL, AccessToken: "secret-token"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client, captured
}

// recorded holds what a stub OneBot received.
type recorded struct {
	action        string
	authorization string
	body          map[string]any
	// answer and status let one test override the reply.
	answer string
	status int
}

// TestGroupMemberListParsesTheDocumentedFields checks the fields the program
// depends on, since the real payload carries many more.
func TestGroupMemberListParsesTheDocumentedFields(t *testing.T) {
	client, captured := newServer(t, `{"status":"ok","retcode":0,"data":[
		{"user_id":3884506253,"nickname":"btrfs","card":"","role":"member",
		 "join_time":1790926337,"shut_up_timestamp":1793406337,"is_robot":false},
		{"user_id":4016438750,"nickname":"FouC3的Bot","role":"admin",
		 "join_time":1790878923,"is_robot":true}
	]}`)

	members, err := client.GroupMemberList(context.Background(), 1015779284)
	if err != nil {
		t.Fatalf("GroupMemberList: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("members = %d, want 2", len(members))
	}
	first := members[0]
	if first.UserID != 3884506253 || first.Nickname != "btrfs" {
		t.Errorf("first = %+v", first)
	}
	if first.JoinTime != 1790926337 {
		t.Errorf("JoinTime = %d", first.JoinTime)
	}
	if first.ShutUpTimestamp != 1793406337 {
		t.Errorf("ShutUpTimestamp = %d", first.ShutUpTimestamp)
	}
	if !members[1].IsRobot {
		t.Error("the second member should be flagged as a robot")
	}
	if first.DisplayName() != "btrfs" {
		t.Errorf("DisplayName = %q, want the nickname when no card is set", first.DisplayName())
	}
	named := Member{Nickname: "btrfs", Card: "群名片"}
	if named.DisplayName() != "群名片" {
		t.Errorf("DisplayName = %q, want the card when it is set", named.DisplayName())
	}

	if captured.action != "/get_group_member_list" {
		t.Errorf("action = %q", captured.action)
	}
	if got := captured.body["group_id"]; got != float64(1015779284) {
		t.Errorf("group_id = %v", got)
	}
	if captured.authorization != "Bearer secret-token" {
		t.Errorf("authorization = %q", captured.authorization)
	}
}

// TestKickGroupMemberSendsTheDocumentedBody covers the removal call.
func TestKickGroupMemberSendsTheDocumentedBody(t *testing.T) {
	client, captured := newServer(t, `{"status":"ok","retcode":0,"data":null}`)

	if err := client.KickGroupMember(context.Background(), 1015779284, 3884506253, false); err != nil {
		t.Fatalf("KickGroupMember: %v", err)
	}
	if captured.action != "/set_group_kick" {
		t.Errorf("action = %q", captured.action)
	}
	if captured.body["user_id"] != float64(3884506253) {
		t.Errorf("user_id = %v", captured.body["user_id"])
	}
	if captured.body["reject_add_request"] != false {
		t.Errorf("reject_add_request = %v, want false so a removed member may ask again",
			captured.body["reject_add_request"])
	}
}

// TestClientSurfacesOneBotFailures covers both a reported failure and a
// transport level one.
func TestClientSurfacesOneBotFailures(t *testing.T) {
	client, _ := newServer(t, `{"status":"failed","retcode":1400,"wording":"bad request"}`)
	err := client.KickGroupMember(context.Background(), 1, 2, false)
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want an *Error", err)
	}
	if apiErr.Retcode != 1400 || apiErr.Detail != "bad request" || apiErr.Action != "set_group_kick" {
		t.Errorf("apiErr = %+v", apiErr)
	}

	client, captured := newServer(t, `{}`)
	captured.answer = "boom"
	captured.status = http.StatusInternalServerError
	if err := client.KickGroupMember(context.Background(), 1, 2, false); err == nil {
		t.Error("an HTTP 500 must be reported")
	}
}

// TestNewRejectsAnEmptyAddress covers the guard that keeps a misconfiguration
// from being reported as a network failure later.
func TestNewRejectsAnEmptyAddress(t *testing.T) {
	if _, err := New(Options{BaseURL: "  "}); err == nil {
		t.Error("New with an empty url must fail")
	}
}

// TestMatchByJoinTime covers the bridge between the two sides.
//
// The rule that matters most is refusing to guess: removing the wrong person is
// worse than reporting that the member could not be identified.
func TestMatchByJoinTime(t *testing.T) {
	members := []Member{
		{UserID: 1, Nickname: "early", JoinTime: 1000},
		{UserID: 2, Nickname: "close", JoinTime: 1008},
		{UserID: 3, Nickname: "late", JoinTime: 1100},
		{UserID: 4, Nickname: "unknown", JoinTime: 0},
	}

	t.Run("unique match inside the tolerance", func(t *testing.T) {
		match, err := MatchByJoinTime(members, 1005, 15)
		if err != nil {
			t.Fatalf("MatchByJoinTime: %v", err)
		}
		if match.Member.UserID != 2 {
			t.Errorf("matched %d, want the closest member 2", match.Member.UserID)
		}
		if match.DeltaSeconds != 3 {
			t.Errorf("DeltaSeconds = %d, want 3", match.DeltaSeconds)
		}
	})

	t.Run("nothing inside the tolerance", func(t *testing.T) {
		_, err := MatchByJoinTime(members, 5000, 15)
		if !errors.Is(err, ErrNotMatched) {
			t.Fatalf("err = %v, want ErrNotMatched", err)
		}
	})

	t.Run("two members equally close is refused", func(t *testing.T) {
		two := []Member{
			{UserID: 1, Nickname: "a", JoinTime: 1000},
			{UserID: 2, Nickname: "b", JoinTime: 1000},
		}
		_, err := MatchByJoinTime(two, 1000, 15)
		if !errors.Is(err, ErrAmbiguous) {
			t.Fatalf("err = %v, want ErrAmbiguous", err)
		}
	})

	t.Run("an exact match beats a near one", func(t *testing.T) {
		match, err := MatchByJoinTime(members, 1100, 200)
		if err != nil {
			t.Fatalf("MatchByJoinTime: %v", err)
		}
		if match.Member.UserID != 3 || match.DeltaSeconds != 0 {
			t.Errorf("match = %+v, want the exact member 3", match)
		}
	})

	t.Run("a member with no recorded join time is skipped", func(t *testing.T) {
		only := []Member{{UserID: 4, Nickname: "unknown"}}
		if _, err := MatchByJoinTime(only, 1000, 60); !errors.Is(err, ErrNotMatched) {
			t.Fatalf("err = %v, want ErrNotMatched", err)
		}
	})

	t.Run("a negative tolerance is treated as its magnitude", func(t *testing.T) {
		if _, err := MatchByJoinTime(members, 1005, -15); err != nil {
			t.Errorf("err = %v, want a match", err)
		}
	})
}

// TestFindMentionedQQRequiresTheRightSender covers the guard against a forged
// marker: anybody in the group can read a message the bot sent, copy its
// marker, and mention whoever they like.
func TestFindMentionedQQRequiresTheRightSender(t *testing.T) {
	const botQQ = int64(4016438750)
	mention := func(sender int64, raw string) GroupMessage {
		return GroupMessage{
			UserID:     sender,
			RawMessage: raw,
			Message: []Segment{
				{Type: "at", Data: map[string]any{"qq": "3884506253"}},
				{Type: "text", Data: map[string]any{"text": "未通过验证"}},
			},
		}
	}

	t.Run("the marker from the bot resolves", func(t *testing.T) {
		messages := []GroupMessage{mention(botQQ, "MARKER-1 未通过验证")}
		qq, err := FindMentionedQQ(messages, "MARKER-1", botQQ)
		if err != nil {
			t.Fatalf("FindMentionedQQ: %v", err)
		}
		if qq != 3884506253 {
			t.Errorf("qq = %d", qq)
		}
	})

	t.Run("a marker copied by a member is refused", func(t *testing.T) {
		messages := []GroupMessage{mention(3866370858, "MARKER-2 未通过验证")}
		_, err := FindMentionedQQ(messages, "MARKER-2", botQQ)
		if !errors.Is(err, ErrMentionNotFromSender) {
			t.Fatalf("err = %v, want ErrMentionNotFromSender", err)
		}
	})

	t.Run("the genuine message wins over a forged one before it", func(t *testing.T) {
		messages := []GroupMessage{
			mention(botQQ, "MARKER-3 未通过验证"),
			mention(3866370858, "MARKER-3 未通过验证"),
		}
		qq, err := FindMentionedQQ(messages, "MARKER-3", botQQ)
		if err != nil {
			t.Fatalf("FindMentionedQQ: %v", err)
		}
		if qq != 3884506253 {
			t.Errorf("qq = %d", qq)
		}
	})

	t.Run("a marker nobody carried is a miss", func(t *testing.T) {
		_, err := FindMentionedQQ([]GroupMessage{mention(botQQ, "别的消息")}, "MARKER-4", botQQ)
		if !errors.Is(err, ErrMentionNotFound) {
			t.Fatalf("err = %v, want ErrMentionNotFound", err)
		}
	})

	t.Run("a message with no mention is skipped", func(t *testing.T) {
		plain := GroupMessage{UserID: botQQ, RawMessage: "MARKER-5 未通过验证"}
		if _, err := FindMentionedQQ([]GroupMessage{plain}, "MARKER-5", botQQ); !errors.Is(err, ErrMentionNotFound) {
			t.Fatalf("err = %v, want ErrMentionNotFound", err)
		}
	})
}

// TestMentionsReadsQQNumbers covers the two shapes OneBot uses for a QQ number.
func TestMentionsReadsQQNumbers(t *testing.T) {
	message := GroupMessage{Message: []Segment{
		{Type: "at", Data: map[string]any{"qq": "3884506253"}},
		{Type: "at", Data: map[string]any{"qq": float64(2152595244)}},
		{Type: "at", Data: map[string]any{"qq": "not-a-number"}},
		{Type: "text", Data: map[string]any{"text": "hi"}},
	}}
	mentions := message.Mentions()
	if len(mentions) != 2 {
		t.Fatalf("mentions = %v, want the two readable ones", mentions)
	}
	if mentions[0] != 3884506253 || mentions[1] != 2152595244 {
		t.Errorf("mentions = %v", mentions)
	}
}

// TestWaitForMentionedQQReadsTheGroup covers the reading half of the bridge: the
// group is read back and the QQ number is picked out of the reply.
func TestWaitForMentionedQQReadsTheGroup(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/get_group_msg_history":
			_, _ = w.Write([]byte(`{"status":"ok","retcode":0,"data":{"messages":[
				{"user_id":4016438750,"raw_message":"MARKER 未通过验证",
				 "message":[{"type":"at","data":{"qq":"3884506253"}}]}
			]}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	client, err := New(Options{BaseURL: server.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	qq, err := client.WaitForMentionedQQ(context.Background(), ResolveRequest{
		GroupID:  1015779284,
		SenderQQ: 4016438750,
		Marker:   "MARKER",
		Wait:     time.Millisecond,
	})
	if err != nil {
		t.Fatalf("ResolveMentionedQQ: %v", err)
	}
	if qq != 3884506253 {
		t.Errorf("qq = %d", qq)
	}
}

// TestResolveMentionedQQNeedsItsArguments covers the guards.
func TestWaitForMentionedQQNeedsItsArguments(t *testing.T) {
	client, _ := newServer(t, `{"status":"ok","retcode":0,"data":{}}`)
	if _, err := client.WaitForMentionedQQ(context.Background(), ResolveRequest{Marker: "M"}); err == nil {
		t.Error("a missing group id must be refused")
	}
	if _, err := client.WaitForMentionedQQ(context.Background(), ResolveRequest{GroupID: 1}); err == nil {
		t.Error("a missing marker must be refused")
	}
}
