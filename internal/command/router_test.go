package command

import (
	"context"
	"encoding/json"
	"testing"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"
)

// counting returns a handler that writes down that it was asked to answer.
func counting(seen *int) EventHandler {
	return func(context.Context, *qqbotsdk.Event) error {
		*seen++
		return nil
	}
}

// message hands one raw group message to the client's dispatcher.
func message(t *testing.T, client *qqbotsdk.Client, id string) {
	t.Helper()
	messageFrom(t, client, id, "MEMBER-OPENID")
}

// messageFrom hands one raw group message from a named member to the dispatcher.
func messageFrom(t *testing.T, client *qqbotsdk.Client, id, memberOpenID string) {
	t.Helper()
	dispatch(t, client, `{"id": "`+id+`", "group_openid": "GROUP-OPENID",
		"author": {"member_openid": "`+memberOpenID+`"}, "content": "hello"}`)
}

// dispatch hands one raw group message body to the client's dispatcher.
func dispatch(t *testing.T, client *qqbotsdk.Client, body string) {
	t.Helper()
	payload := &qqbotsdk.Payload{
		ID:   "EVENT-ID",
		Op:   qqbotsdk.OpDispatch,
		Type: qqbotsdk.EventGroupMessageCreate,
		Data: json.RawMessage(body),
	}
	if err := client.Dispatcher().DispatchSync(context.Background(),
		qqbotsdk.NewEvent(payload, "test")); err != nil {
		t.Fatalf("dispatching: %v", err)
	}
}

// TestAMemberBeingHeldBackIsAnsweredNothing covers the half of a take-back that
// the verification cannot do itself.
//
// The message is taken out of the group by the feature that holds the member, and
// the answer to it would be posted by the feature whose handler this is: an answer
// would be a message about something nobody else can read, and the answer itself is
// not taken back.
func TestAMemberBeingHeldBackIsAnsweredNothing(t *testing.T) {
	client := testClient(t)
	var answered int
	router := NewRouter("commands", nil)
	router.SetHeldBack(func(_ string, memberOpenID string) bool {
		return memberOpenID == "MEMBER-HELD"
	})
	if err := router.Register(client, Handlers{Group: counting(&answered)}); err != nil {
		t.Fatalf("registering: %v", err)
	}

	messageFrom(t, client, "HELD-1", "MEMBER-HELD")
	if answered != 0 {
		t.Errorf("a message from a member who is being held back was answered %d "+
			"time(s), want none", answered)
	}

	// And the hold is the only reason: the same message from somebody who is not
	// being held is answered as it always was.
	messageFrom(t, client, "OTHER-1", "MEMBER-OTHER")
	if answered != 1 {
		t.Errorf("answered %d of a message from somebody who is not held, want 1",
			answered)
	}
}

// TestNothingHeldBackAnswersAsBefore covers the default a bot without the
// verification runs with: nobody is held back, so nothing is dropped.
func TestNothingHeldBackAnswersAsBefore(t *testing.T) {
	client := testClient(t)
	var answered int
	router := NewRouter("commands", nil)
	if err := router.Register(client, Handlers{Group: counting(&answered)}); err != nil {
		t.Fatalf("registering: %v", err)
	}

	message(t, client, "MESSAGE-1")
	if answered != 1 {
		t.Errorf("answered %d of a message with nothing held back, want 1", answered)
	}
}

// TestAMessageWithNoSenderStillReachesTheHandler covers the fallback: an event that
// says nothing about who sent it cannot be matched against a hold, and the handler
// is the place that knows what to say about it. Dropping it here would take the
// complaint away from the only place equipped to make it.
func TestAMessageWithNoSenderStillReachesTheHandler(t *testing.T) {
	client := testClient(t)
	var answered int
	router := NewRouter("commands", nil)
	router.SetHeldBack(func(string, string) bool { return true })
	if err := router.Register(client, Handlers{Group: counting(&answered)}); err != nil {
		t.Fatalf("registering: %v", err)
	}

	dispatch(t, client, `{"id": "NO-AUTHOR", "group_openid": "GROUP-OPENID"}`)
	if answered != 1 {
		t.Errorf("answered %d of a message that names no sender, want the handler to "+
			"have seen it", answered)
	}
}

// TestStopStopsThisFeatureAnswering covers what makes a feature rebuildable
// rather than merely startable.
//
// Two features run on the one connection, which is the ordinary case: stopping
// one of them has to stop that one and leave the other alone.
func TestStopStopsThisFeatureAnswering(t *testing.T) {
	client := testClient(t)
	var first, second int
	firstRouter := NewRouter("first", nil)
	secondRouter := NewRouter("second", nil)
	for _, router := range []*Router{firstRouter, secondRouter} {
		var counter *int
		if router == firstRouter {
			counter = &first
		} else {
			counter = &second
		}
		if err := router.Register(client, Handlers{Group: counting(counter)}); err != nil {
			t.Fatalf("registering %s: %v", router.owner, err)
		}
	}

	message(t, client, "MESSAGE-1")
	if first != 1 || second != 1 {
		t.Fatalf("before stopping: first answered %d, second %d; want 1 each", first, second)
	}

	firstRouter.Stop()
	message(t, client, "MESSAGE-2")

	if first != 1 {
		t.Errorf("a feature that stopped answered a later message %d time(s)", first-1)
	}
	if second != 2 {
		t.Errorf("stopping one feature stopped another: it answered %d of 2", second)
	}
}

// TestStopTwiceIsSafe covers the shutdown path: Close may run after a Register
// that failed, and after a Stop that already happened.
func TestStopTwiceIsSafe(t *testing.T) {
	client := testClient(t)
	var seen int
	router := NewRouter("one", nil)
	if err := router.Register(client, Handlers{Group: counting(&seen)}); err != nil {
		t.Fatal(err)
	}
	router.Stop()
	router.Stop()
	message(t, client, "MESSAGE-1")
	if seen != 0 {
		t.Errorf("a stopped feature answered %d message(s)", seen)
	}
}

// TestStopGivesUpTheButtonsWhenNothingTakesOver covers a feature that stops and
// is not built again -- removed from the configuration, say.
//
// Its buttons have to reach nobody then, rather than an instance that has already
// stopped and is still holding the namespace.
func TestStopGivesUpTheButtonsWhenNothingTakesOver(t *testing.T) {
	client := testClient(t)
	var seen []Press
	router := NewRouter("admin_commands", NewButtons())
	claims := []ButtonClaim{{Namespace: "qgb-receipt-detail:", Scenes: InGroup,
		Handle: recorder(&seen)}}
	if err := router.Register(client, Handlers{Buttons: claims}); err != nil {
		t.Fatal(err)
	}

	press(t, client, groupPressBody("qgb-receipt-detail:ONE"))
	if len(seen) != 1 {
		t.Fatalf("the feature answered %d press(es) before stopping, want 1", len(seen))
	}

	router.Stop()
	press(t, client, groupPressBody("qgb-receipt-detail:TWO"))
	if len(seen) != 1 {
		t.Errorf("a press was answered after the feature that owned the button stopped")
	}
}

// TestStopGivesTheButtonsBack covers the other half of a rebuild: the second
// instance claims exactly what the first claimed, and only it answers.
func TestStopGivesTheButtonsBack(t *testing.T) {
	client := testClient(t)
	buttons := NewButtons()
	var first, second []Press

	firstRouter := NewRouter("admin_commands", buttons)
	claims := []ButtonClaim{{Namespace: "qgb-receipt-detail:", Scenes: InGroup,
		Handle: recorder(&first)}}
	if err := firstRouter.Register(client, Handlers{Buttons: claims}); err != nil {
		t.Fatalf("registering: %v", err)
	}
	press(t, client, groupPressBody("qgb-receipt-detail:ONE"))
	if len(first) != 1 {
		t.Fatalf("the first instance answered %d press(es), want 1", len(first))
	}

	firstRouter.Stop()

	// The same feature, built again, on the same connection: the namespace is
	// free, and the dispatcher is not declared a second time.
	secondRouter := NewRouter("admin_commands", buttons)
	claims = []ButtonClaim{{Namespace: "qgb-receipt-detail:", Scenes: InGroup,
		Handle: recorder(&second)}}
	if err := secondRouter.Register(client, Handlers{Buttons: claims}); err != nil {
		t.Fatalf("a rebuilt feature could not claim its own namespace: %v", err)
	}

	press(t, client, groupPressBody("qgb-receipt-detail:TWO"))
	if len(first) != 1 {
		t.Errorf("the instance that stopped answered a later press")
	}
	if len(second) != 1 {
		t.Errorf("the rebuilt instance answered %d press(es), want 1", len(second))
	}
}
