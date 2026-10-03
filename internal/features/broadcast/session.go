package broadcast

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"

	"github.com/fouc3/qq-group-management-bot/internal/messaging"
)

// sessionLifetime is how long a card stays usable.
//
// Generous, because a broadcast is written by a person rather than by a command: it
// has to survive somebody being called away in the middle of typing it. What the
// limit is for is the map -- a card nobody ever finishes must not be kept for ever
// -- rather than the moment it stops working.
const sessionLifetime = 30 * time.Minute

// chosen is one option on the card.
//
// It starts unset, and a press turns it on and then off again. The unset state is
// the point: a card where nothing was touched is not ready to continue, so an
// administrator says what they want rather than taking whatever the default
// happened to be.
type chosen uint8

const (
	unset chosen = iota
	on
	off
)

// words is what the card calls each state.
func (c chosen) words() string {
	switch c {
	case on:
		return "开"
	case off:
		return "关"
	default:
		return "未选"
	}
}

// groupChoice is one group a broadcast can go to.
type groupChoice struct {
	openID string
	// name is what the group is called, as the platform reports it, or empty when
	// it could not be read.
	name string
	// selected is whether this broadcast goes there. Nothing is selected to begin
	// with: which groups a message goes to is the decision being made.
	selected bool
}

// place is where a card is: a group, or a single chat with the bot.
//
// A broadcast is written by one person rather than by a group, so the card can be
// worked on in private and posted into the groups that person administers. Which of
// the two it is decides the endpoint every message of the flow goes to, and it is
// carried rather than passed around so that nothing can send half a card to the wrong
// place.
type place struct {
	groupOpenID string
	userOpenID  string
}

// message is one message on its way to this place.
func (p place) message(text string, keyboard *qqbotsdk.Keyboard) messaging.Message {
	return messaging.Message{
		GroupOpenID: p.groupOpenID,
		UserOpenID:  p.userOpenID,
		Text:        text,
		Keyboard:    keyboard,
	}
}

// inGroup reports whether the card is in a group rather than in a single chat.
func (p place) inGroup() bool { return p.groupOpenID != "" }

// String is how a place is named in a log line, where one field has to say which of
// the two it was.
func (p place) String() string {
	if p.inGroup() {
		return "group " + p.groupOpenID
	}
	return "chat " + p.userOpenID
}

// matches reports whether an interaction happened in this place.
func (p place) matches(data *qqbotsdk.InteractionCreateData) bool {
	if data == nil {
		return false
	}
	if p.inGroup() {
		return data.Scene == qqbotsdk.InteractionSceneGroup &&
			data.GroupOpenID == p.groupOpenID
	}
	return data.Scene == qqbotsdk.InteractionSceneC2C && data.UserOpenID == p.userOpenID
}

// presser is who pressed, which each scene names in its own field.
func presser(data *qqbotsdk.InteractionCreateData) string {
	if data.Scene == qqbotsdk.InteractionSceneC2C {
		return data.UserOpenID
	}
	return data.GroupMemberOpenID
}

// session is one broadcast being written: the card a member is looking at, what
// they have chosen on it, and the text once they have written it.
type session struct {
	token string
	// where is the group or single chat the card lives in, and where everything
	// about this broadcast happens.
	where place
	// starter is the member who may work the card.
	//
	// Nobody else can, whatever the buttons say: the platform's own administrator
	// gate is about QQ's administrators, and the list this bot obeys is a different
	// list.
	starter string

	// replyTo and sequence are where the next card answers: the message the member
	// sent, and how many answers it has had. The platform treats two answers to one
	// message carrying the same number as duplicates of each other.
	replyTo  string
	sequence int

	// cardMessageID is the card showing now, so that the next one can take its
	// place and the group is not left reading a column of cards.
	cardMessageID string

	markdown  chosen
	anonymous chosen
	groups    []groupChoice

	// content is what the broadcast will say, once the member has written it.
	content string
	// waiting says that the next message the starter sends is that text rather than
	// anything else.
	waiting bool
	// previewMessageID is the message showing what the broadcast will look like.
	previewMessageID string

	// updated is when the card was last worked on, which is what the lifetime is
	// measured from.
	updated time.Time

	// mu serializes the presses on one card, so that two of them cannot read the same
	// state and send two cards for it. It is per session rather than per feature:
	// one member's card has no reason to wait behind another's.
	mu chan struct{}
}

// lock takes the session, and returns the function that gives it back.
//
// A channel rather than a mutex, because the lock is held across the network calls
// that answer a press, and a channel makes the wait visible at every call site: a
// press is rare, and one waiting for the other is what should happen.
func (s *session) lock() func() {
	s.mu <- struct{}{}
	return func() { <-s.mu }
}

// selectedGroups are the groups this broadcast is going to.
func (s *session) selectedGroups() []string {
	var chosen []string
	for _, group := range s.groups {
		if group.selected {
			chosen = append(chosen, group.openID)
		}
	}
	return chosen
}

// missing is what still has to be decided before the broadcast can be written.
//
// The platform-can't-do-it option is left out on purpose: 富文本 is shown so that
// an administrator knows it is not available, and a card that waited for it to be
// chosen would never be finished.
func (s *session) missing() []string {
	var waiting []string
	if s.markdown == unset {
		waiting = append(waiting, "Markdown 渲染")
	}
	if s.anonymous == unset {
		waiting = append(waiting, "匿名发送")
	}
	if len(s.selectedGroups()) == 0 {
		waiting = append(waiting, "至少一个群")
	}
	return waiting
}

// newToken is what a card's buttons carry back.
//
// A token rather than a message id: a message id is about a hundred characters of
// opaque text, and how much a button's data field holds is not something to find
// out in production.
func newToken() (string, error) {
	buffer := make([]byte, 8)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("generating a broadcast token: %w", err)
	}
	return hex.EncodeToString(buffer), nil
}

// remember records an open card.
func (h *handler) remember(s *session) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.dropExpired()
	h.open[s.token] = s
}

// lookup returns an open card.
func (h *handler) lookup(token string) (*session, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.dropExpired()
	s, found := h.open[token]
	return s, found
}

// touch extends a card's life, which is what working on it does.
func (h *handler) touch(s *session) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s.updated = time.Now()
}

// forget closes a card, so that a press on an old one cannot start it again.
func (h *handler) forget(s *session) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.open, s.token)
}

// dropExpired closes the cards nobody came back to.
//
// Called under the lock, from both the write and the read, because a map that is
// only pruned on the way in grows with every card a group ever opened.
func (h *handler) dropExpired() {
	now := time.Now()
	for token, s := range h.open {
		if now.Sub(s.updated) > sessionLifetime {
			delete(h.open, token)
		}
	}
}
