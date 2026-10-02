// Package onebot talks to an OneBot v11 implementation over its HTTP API.
//
// It exists for one job the official bot cannot do at all: removing a member
// from a group. The official platform answers 40012010 "应用无接口访问权限" for
// the removal endpoint even when the bot is a group administrator, so the
// removal is handed to an OneBot account that is an administrator too.
//
// The two sides identify people differently and there is no API that converts
// between them: the official bot only ever sees an app scoped openid, while
// OneBot only ever sees a QQ number. The bridge used here is the join time,
// which both sides report as a Unix timestamp for the same event.
package onebot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Errors reported by the caller, separate from a transport failure.
var (
	// ErrNotMatched reports that no member matched a join time.
	ErrNotMatched = errors.New("onebot: no member matched the join time")
	// ErrAmbiguous reports that more than one member matched, so guessing
	// would risk removing the wrong person.
	ErrAmbiguous = errors.New("onebot: more than one member matched the join time")
)

// DefaultTimeout bounds one HTTP call.
const DefaultTimeout = 10 * time.Second

// Client is an OneBot HTTP API client.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// Options configures a Client.
type Options struct {
	// BaseURL is the OneBot HTTP address, for example
	// http://127.0.0.1:3000.
	BaseURL string
	// AccessToken is sent as a bearer token when it is not empty.
	AccessToken string
	// Timeout bounds one call. Zero means DefaultTimeout.
	Timeout time.Duration
	// HTTPClient replaces the default client.
	HTTPClient *http.Client
}

// New returns a client for one OneBot implementation.
func New(options Options) (*Client, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(options.BaseURL), "/")
	if baseURL == "" {
		return nil, errors.New("onebot: no base url configured")
	}
	httpClient := options.HTTPClient
	if httpClient == nil {
		timeout := options.Timeout
		if timeout == 0 {
			timeout = DefaultTimeout
		}
		httpClient = &http.Client{Timeout: timeout}
	}
	return &Client{baseURL: baseURL, token: options.AccessToken, http: httpClient}, nil
}

// envelope is the shape every OneBot HTTP action answers with.
type envelope struct {
	Status  string          `json:"status"`
	Retcode int             `json:"retcode"`
	Data    json.RawMessage `json:"data"`
	Message string          `json:"message"`
	Wording string          `json:"wording"`
}

// Error is a failure reported by OneBot rather than by the network.
type Error struct {
	// Action is the API action that failed.
	Action string
	// Retcode is the implementation's code.
	Retcode int
	// Status is the implementation's status word.
	Status string
	// Detail is the message or wording field, whichever was set.
	Detail string
}

// Error implements error.
func (e *Error) Error() string {
	return fmt.Sprintf("onebot: %s failed: status=%s retcode=%d: %s",
		e.Action, e.Status, e.Retcode, e.Detail)
}

// call performs one API action and decodes its data.
func (c *Client) call(ctx context.Context, action string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("onebot: encoding the %s request: %w", action, err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/"+action, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("onebot: building the %s request: %w", action, err)
	}
	request.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		request.Header.Set("Authorization", "Bearer "+c.token)
	}

	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("onebot: calling %s: %w", action, err)
	}
	defer func() { _ = response.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("onebot: reading the %s answer: %w", action, err)
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("onebot: %s answered http %d: %s",
			action, response.StatusCode, strings.TrimSpace(string(raw)))
	}

	var result envelope
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("onebot: decoding the %s answer: %w", action, err)
	}
	if result.Status != "ok" && result.Retcode != 0 {
		detail := result.Message
		if detail == "" {
			detail = result.Wording
		}
		return &Error{Action: action, Retcode: result.Retcode, Status: result.Status, Detail: detail}
	}
	if out == nil || len(result.Data) == 0 || string(result.Data) == "null" {
		return nil
	}
	if err := json.Unmarshal(result.Data, out); err != nil {
		return fmt.Errorf("onebot: decoding the %s data: %w", action, err)
	}
	return nil
}

// Member is one group member as OneBot reports them.
//
// Only the fields this program uses are declared; OneBot returns many more.
type Member struct {
	// UserID is the QQ number, the identity OneBot acts on.
	UserID int64 `json:"user_id"`
	// Nickname is the member's display name.
	Nickname string `json:"nickname"`
	// Card is the group specific name, which many members set instead.
	Card string `json:"card"`
	// Role is owner, admin or member.
	Role string `json:"role"`
	// JoinTime is when they joined, as a Unix timestamp. It is the bridge to
	// the official bot's event timestamp.
	JoinTime int64 `json:"join_time"`
	// ShutUpTimestamp is when their mute ends, as a Unix timestamp, or zero
	// when they are not muted.
	ShutUpTimestamp int64 `json:"shut_up_timestamp"`
	// IsRobot reports whether the account is a bot.
	IsRobot bool `json:"is_robot"`
}

// DisplayName is the name to show an operator.
func (m Member) DisplayName() string {
	if strings.TrimSpace(m.Card) != "" {
		return m.Card
	}
	return m.Nickname
}

// GroupMemberList returns every member of one group.
//
// The parameter is the QQ group number, not the official bot's group openid:
// the two are different identifiers for the same group.
func (c *Client) GroupMemberList(ctx context.Context, groupID int64) ([]Member, error) {
	request := struct {
		GroupID int64 `json:"group_id"`
	}{GroupID: groupID}

	var members []Member
	if err := c.call(ctx, "get_group_member_list", request, &members); err != nil {
		return nil, err
	}
	return members, nil
}

// KickGroupMember removes one member from one group.
//
// rejectAddRequest asks OneBot to refuse their future join requests, which this
// program leaves off so that a member who was removed for not verifying can
// come back and try again.
func (c *Client) KickGroupMember(ctx context.Context, groupID, userID int64, rejectAddRequest bool) error {
	request := struct {
		GroupID          int64 `json:"group_id"`
		UserID           int64 `json:"user_id"`
		RejectAddRequest bool  `json:"reject_add_request"`
	}{GroupID: groupID, UserID: userID, RejectAddRequest: rejectAddRequest}

	return c.call(ctx, "set_group_kick", request, nil)
}

// Match is one resolved member together with how well it matched.
type Match struct {
	// Member is the member found.
	Member Member
	// DeltaSeconds is how far the member's join time was from the target.
	// It is worth reporting so an operator can tighten the tolerance.
	DeltaSeconds int64
}

// MatchByJoinTime finds the member of a group who joined at the given time.
//
// Both sides report the same join as a Unix timestamp, but not necessarily the
// same second: the official event time is when the platform emitted the event,
// while OneBot reports the recorded join time. A tolerance covers that, and the
// match must be unique, because removing the wrong member is much worse than
// reporting that the member could not be identified.
func MatchByJoinTime(members []Member, joinTime, toleranceSeconds int64) (Match, error) {
	if toleranceSeconds < 0 {
		toleranceSeconds = -toleranceSeconds
	}
	var (
		best       *Match
		secondBest *Match
	)
	for _, member := range members {
		if member.JoinTime == 0 {
			continue
		}
		delta := member.JoinTime - joinTime
		if delta < 0 {
			delta = -delta
		}
		if delta > toleranceSeconds {
			continue
		}
		candidate := Match{Member: member, DeltaSeconds: delta}
		switch {
		case best == nil || delta < best.DeltaSeconds:
			secondBest = best
			best = &candidate
		case secondBest == nil || delta < secondBest.DeltaSeconds:
			secondBest = &candidate
		}
	}

	switch {
	case best == nil:
		return Match{}, fmt.Errorf("%w: no member joined within %d seconds of %d",
			ErrNotMatched, toleranceSeconds, joinTime)
	case secondBest != nil && secondBest.DeltaSeconds == best.DeltaSeconds:
		// Two members joined equally close, so either could be the one being
		// tracked. Guessing risks removing the wrong person.
		return Match{}, fmt.Errorf("%w: %s (%d) and %s (%d) are both %d second(s) away",
			ErrAmbiguous,
			best.Member.DisplayName(), best.Member.UserID,
			secondBest.Member.DisplayName(), secondBest.Member.UserID,
			best.DeltaSeconds)
	}
	return *best, nil
}

// Segment is one part of an OneBot message.
//
// A mention arrives as a segment of type "at" whose data carries the QQ number,
// which is how an openid becomes a QQ number: the official bot mentions the
// member and the platform resolves the mention on the way out.
type Segment struct {
	// Type is text, at, markdown, image and so on.
	Type string `json:"type"`
	// Data holds the segment's own fields.
	Data map[string]any `json:"data"`
}

// GroupMessage is one group message as OneBot reports it.
type GroupMessage struct {
	// MessageSeq orders messages within a group.
	MessageSeq int64 `json:"message_seq"`
	// MessageID is OneBot's own id, which is unrelated to the official id.
	MessageID int64 `json:"message_id"`
	// UserID is the sender's QQ number.
	UserID int64 `json:"user_id"`
	// RawMessage is the message with OneBot's CQ codes, which makes a mention
	// readable as [CQ:at,qq=...].
	RawMessage string `json:"raw_message"`
	// Message is the parsed form, where a mention is its own segment.
	Message []Segment `json:"message"`
	// Time is when it was sent, as a Unix timestamp.
	Time int64 `json:"time"`
}

// Mentions lists the QQ numbers mentioned in the message, in order.
func (m GroupMessage) Mentions() []int64 {
	mentions := make([]int64, 0, len(m.Message))
	for _, segment := range m.Message {
		if segment.Type != "at" {
			continue
		}
		value, ok := segment.Data["qq"]
		if !ok {
			continue
		}
		if number, ok := toQQ(value); ok {
			mentions = append(mentions, number)
		}
	}
	return mentions
}

// toQQ reads a QQ number from a segment field, which OneBot sends as a string
// and sometimes as a number.
func toQQ(value any) (int64, bool) {
	switch typed := value.(type) {
	case string:
		var number int64
		if _, err := fmt.Sscan(typed, &number); err != nil {
			return 0, false
		}
		return number, true
	case float64:
		return int64(typed), true
	case json.Number:
		number, err := typed.Int64()
		return number, err == nil
	default:
		return 0, false
	}
}

// SendGroupMessage posts one plain text group message.
func (c *Client) SendGroupMessage(ctx context.Context, groupID int64, message string) (int64, error) {
	request := struct {
		GroupID int64  `json:"group_id"`
		Message string `json:"message"`
	}{GroupID: groupID, Message: message}

	var sent struct {
		MessageID int64 `json:"message_id"`
	}
	if err := c.call(ctx, "send_group_msg", request, &sent); err != nil {
		return 0, err
	}
	return sent.MessageID, nil
}

// GroupMessageHistory returns the most recent messages of one group, oldest
// first as OneBot reports them.
func (c *Client) GroupMessageHistory(ctx context.Context, groupID int64, count int) ([]GroupMessage, error) {
	if count <= 0 {
		count = 20
	}
	request := struct {
		GroupID int64 `json:"group_id"`
		Count   int   `json:"count"`
	}{GroupID: groupID, Count: count}

	var history struct {
		Messages []GroupMessage `json:"messages"`
	}
	if err := c.call(ctx, "get_group_msg_history", request, &history); err != nil {
		return nil, err
	}
	return history.Messages, nil
}

// ErrMentionNotFound reports that no message carrying the marker mentioned
// anybody.
var ErrMentionNotFound = errors.New("onebot: no message carrying the marker mentioned anybody")

// ErrMentionNotFromSender reports that a message carried the marker but came
// from another account.
//
// It is what a forged marker looks like: anybody in the group can read a
// message the bot sent, copy its marker, and mention whoever they like. Acting
// on such a message would remove the wrong person, so it is refused and
// reported separately from a plain miss.
var ErrMentionNotFromSender = errors.New(
	"onebot: a message carried the marker but came from another account")

// FindMentionedQQ returns the QQ number mentioned by the newest message whose
// raw text contains marker and which the expected sender posted.
//
// Only the newest match is considered, because a retry writes a new message and
// an older one may already have served its purpose.
func FindMentionedQQ(messages []GroupMessage, marker string, senderQQ int64) (int64, error) {
	var forged bool
	for i := len(messages) - 1; i >= 0; i-- {
		message := messages[i]
		if !strings.Contains(message.RawMessage, marker) {
			continue
		}
		mentions := message.Mentions()
		if len(mentions) == 0 {
			continue
		}
		if senderQQ != 0 && message.UserID != senderQQ {
			// Keep looking: the genuine one may still be further back, but
			// remember that somebody tried.
			forged = true
			continue
		}
		return mentions[0], nil
	}
	if forged {
		return 0, fmt.Errorf("%w: marker %q", ErrMentionNotFromSender, marker)
	}
	return 0, fmt.Errorf("%w: marker %q", ErrMentionNotFound, marker)
}

// ResolveRequest describes one attempt to turn a mention into a QQ number.
type ResolveRequest struct {
	// GroupID is the QQ group number.
	GroupID int64
	// SenderQQ is the official bot's own QQ number. Every candidate message
	// must come from it, because anyone in the group can copy a marker and
	// mention whoever they like.
	SenderQQ int64
	// Marker ties a message back to the member it was sent for.
	Marker string
	// HistoryCount is how many recent messages to read. Zero means 20.
	HistoryCount int
	// Wait is how long to let the message come back before reading. Zero
	// means two seconds.
	Wait time.Duration
}

// WaitForMentionedQQ waits for a message carrying a marker to come back, reads
// the group, and returns the QQ number the platform resolved its mention into.
//
// This is the bridge between the two halves of the system: the official bot only
// ever sees an app scoped openid, OneBot only ever sees a QQ number, and neither
// can be converted into the other by an API. The platform converts one when the
// member is mentioned, so the mention has to be sent by the bot whose openid is
// being resolved, and read back here.
//
// It deliberately does not send anything: a message sent through OneBot would
// carry the official mention tag as plain text, because OneBot does not know it,
// and no conversion would happen.
func (c *Client) WaitForMentionedQQ(ctx context.Context, request ResolveRequest) (int64, error) {
	if request.GroupID == 0 {
		return 0, errors.New("onebot: WaitForMentionedQQ needs a group id")
	}
	if strings.TrimSpace(request.Marker) == "" {
		return 0, errors.New("onebot: WaitForMentionedQQ needs a marker")
	}

	wait := request.Wait
	if wait <= 0 {
		wait = 2 * time.Second
	}
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-time.After(wait):
	}

	messages, err := c.GroupMessageHistory(ctx, request.GroupID, request.HistoryCount)
	if err != nil {
		return 0, err
	}
	return FindMentionedQQ(messages, request.Marker, request.SenderQQ)
}
