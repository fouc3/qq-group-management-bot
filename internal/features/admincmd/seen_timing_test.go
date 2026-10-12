package admincmd

import (
	"testing"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"
)

// TestACommandIsAnsweredWhenThePlainDeliveryArrivesFirst covers the order of the two
// questions asked before a command runs: whether this message is one the bot was meant
// to read, and whether it has already been handled.
//
// The same message arrives twice in a group set to receive everything -- once as an
// ordinary message and once as a mention event -- and which of the two arrives first is
// not something the bot is told. Only one of them counts as a mention under
// require_mention, so recording the first sighting before that question is asked makes
// the command answer nothing at all whenever the order goes the wrong way: silently, and
// about half the time.
//
// The group that is not configured yet has always asked the question in this order, for
// this reason; this pins the other path to it.
func TestACommandIsAnsweredWhenThePlainDeliveryArrivesFirst(t *testing.T) {
	h := newHarness(t, baseSection)

	// The plain delivery, which does not count as a mention: the content carries no
	// mention markup, and a mention event is the only thing that says the bot was
	// addressed.
	h.deliverAs("MESSAGE-1", qqbotsdk.EventGroupMessageCreate,
		"/禁言 30m", testAdmin, testGroupOpenID, testTarget)
	// The same message again, this time as the mention event.
	h.deliverAs("MESSAGE-1", qqbotsdk.EventGroupAtMessageCreate,
		"/禁言 30m", testAdmin, testGroupOpenID, testTarget)

	if h.muteCount() != 1 {
		t.Errorf("mute calls = %d, want 1: the delivery that mentioned the bot was "+
			"dropped as a duplicate of one that had been discarded for not mentioning "+
			"it, so the command was never answered at all", h.muteCount())
	}
}
