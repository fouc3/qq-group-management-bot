package command

import (
	"context"
	"encoding/json"
	"testing"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"
)

// testClient is a client with a dispatcher and no connection: what these tests
// exercise is the routing, and a dispatch runs the handlers without one.
func testClient(t *testing.T) *qqbotsdk.Client {
	t.Helper()
	client, err := qqbotsdk.NewClientFromConfig(qqbotsdk.Config{
		AccessToken: "test-token",
		BaseURL:     "http://127.0.0.1:1",
	})
	if err != nil {
		t.Fatalf("building the client: %v", err)
	}
	return client
}

// groupPressBody is one press in a group, as the platform sends it.
func groupPressBody(buttonData string) string {
	return pressBody(`"scene": "group", "chat_type": 1,
		"group_openid": "GROUP-OPENID", "group_member_openid": "MEMBER-OPENID",`,
		buttonData)
}

// privatePressBody is one press in a single chat.
func privatePressBody(buttonData string) string {
	return pressBody(`"scene": "c2c", "chat_type": 2,
		"user_openid": "USER-OPENID",`, buttonData)
}

func pressBody(where, buttonData string) string {
	encoded, _ := json.Marshal(buttonData)
	return `{"id": "INTERACTION-1", "type": 11, ` + where +
		` "data": {"type": 11, "resolved": {"button_data": ` + string(encoded) + `}}}`
}

// press hands one raw press to the client's dispatcher.
func press(t *testing.T, client *qqbotsdk.Client, body string) {
	t.Helper()
	payload := &qqbotsdk.Payload{
		ID:   "EVENT-ID",
		Op:   qqbotsdk.OpDispatch,
		Type: qqbotsdk.EventInteractionCreate,
		Data: json.RawMessage(body),
	}
	if err := client.Dispatcher().DispatchSync(context.Background(),
		qqbotsdk.NewEvent(payload, "test")); err != nil {
		t.Fatalf("dispatching: %v", err)
	}
}

// recorder is a claim's handler that writes down what it was asked to answer.
func recorder(seen *[]Press) ButtonHandler {
	return func(_ context.Context, press Press) error {
		*seen = append(*seen, press)
		return nil
	}
}

// claim sets one feature's buttons, which is what its Register does.
func claim(t *testing.T, buttons *Buttons, owner string, claims ...ButtonClaim) {
	t.Helper()
	if err := buttons.Set(owner, claims); err != nil {
		t.Fatalf("claiming %s: %v", owner, err)
	}
}

// TestOnePressReachesOneFeature covers what the registry is for: a press carries
// nothing but its button's data, and exactly one feature is asked to answer it.
//
// The dispatcher is declared twice, which is what every feature does when it
// registers: the second one must not route the same press a second time.
func TestOnePressReachesOneFeature(t *testing.T) {
	buttons := NewButtons()
	var verification, receipt []Press
	claim(t, buttons, "join_verification",
		ButtonClaim{Namespace: "v:", Scenes: InGroup, Handle: recorder(&verification)})
	claim(t, buttons, "admin_commands",
		ButtonClaim{Namespace: "qgb-receipt-detail:", Scenes: InGroup | InPrivate,
			Handle: recorder(&receipt)})

	client := testClient(t)
	buttons.Register(client)
	buttons.Register(client)

	press(t, client, groupPressBody("v:TOKEN"))
	press(t, client, groupPressBody("qgb-receipt-detail:RECEIPT"))

	if len(verification) != 1 {
		t.Fatalf("the verification feature answered %d press(es), want 1", len(verification))
	}
	if len(receipt) != 1 {
		t.Fatalf("the receipt feature answered %d press(es), want 1", len(receipt))
	}
	// What is handed over is the button's payload, not the whole data: the
	// namespace is the routing's business and has no meaning to the feature.
	if verification[0].Payload != "TOKEN" {
		t.Errorf("verification payload = %q, want TOKEN", verification[0].Payload)
	}
	if receipt[0].Payload != "RECEIPT" {
		t.Errorf("receipt payload = %q, want RECEIPT", receipt[0].Payload)
	}
	// A feature that answers with a message needs the event's own id to reply to.
	if receipt[0].EventID != "EVENT-ID" {
		t.Errorf("event id = %q, want EVENT-ID", receipt[0].EventID)
	}
}

// TestAPressNobodyClaimedIsIgnored covers the buttons of a feature that is not
// running, and an event that is not a button press at all.
func TestAPressNobodyClaimedIsIgnored(t *testing.T) {
	buttons := NewButtons()
	var seen []Press
	claim(t, buttons, "join_verification",
		ButtonClaim{Namespace: "v:", Scenes: InGroup, Handle: recorder(&seen)})
	client := testClient(t)
	buttons.Register(client)

	press(t, client, groupPressBody("qgb-receipt-detail:RECEIPT"))
	press(t, client, groupPressBody(""))
	press(t, client, groupPressBody("v"))

	if len(seen) != 0 {
		t.Errorf("%d press(es) reached a claim, want none: %+v", len(seen), seen)
	}
}

// TestAClaimIsOnlyAnsweredWhereItSays covers the scene, and the identifiers each
// scene needs: without them there is nothing to check a permission against and
// nowhere to answer, so a press nobody can answer is not one to act on.
func TestAClaimIsOnlyAnsweredWhereItSays(t *testing.T) {
	buttons := NewButtons()
	var inGroup, inPrivate []Press
	claim(t, buttons, "join_verification",
		ButtonClaim{Namespace: "v:", Scenes: InGroup, Handle: recorder(&inGroup)})
	claim(t, buttons, "admin_commands",
		ButtonClaim{Namespace: "qgb-receipt-detail:", Scenes: InPrivate,
			Handle: recorder(&inPrivate)})
	client := testClient(t)
	buttons.Register(client)

	// A verification button reported from a single chat, and a receipt button
	// reported from a group: neither is where that feature answers.
	press(t, client, privatePressBody("v:TOKEN"))
	press(t, client, groupPressBody("qgb-receipt-detail:RECEIPT"))
	if len(inGroup) != 0 || len(inPrivate) != 0 {
		t.Errorf("a press answered outside its scene: %d group, %d private",
			len(inGroup), len(inPrivate))
	}

	// And the same two presses where they belong.
	press(t, client, groupPressBody("v:TOKEN"))
	press(t, client, privatePressBody("qgb-receipt-detail:RECEIPT"))
	if len(inGroup) != 1 || len(inPrivate) != 1 {
		t.Errorf("the presses where they belong were not answered: %d group, %d private",
			len(inGroup), len(inPrivate))
	}
}

// TestAPressMissingWhatItsSceneNeedsIsIgnored covers the half of a press that is
// about the platform rather than about the feature.
func TestAPressMissingWhatItsSceneNeedsIsIgnored(t *testing.T) {
	for _, body := range []string{
		// A group press with no member to check a permission against or to answer.
		`{"id": "INTERACTION-1", "type": 11, "scene": "group", "chat_type": 1,
			"group_openid": "GROUP-OPENID",
			"data": {"type": 11, "resolved": {"button_data": "v:TOKEN"}}}`,
		// A group press naming no group.
		`{"id": "INTERACTION-1", "type": 11, "scene": "group", "chat_type": 1,
			"group_member_openid": "MEMBER-OPENID",
			"data": {"type": 11, "resolved": {"button_data": "v:TOKEN"}}}`,
		// A press in a chat that is neither of the two a button can be in.
		`{"id": "INTERACTION-1", "type": 11, "scene": "channel", "chat_type": 3,
			"group_openid": "GROUP-OPENID", "group_member_openid": "MEMBER-OPENID",
			"data": {"type": 11, "resolved": {"button_data": "v:TOKEN"}}}`,
		// A press whose data is not there at all.
		`{"id": "INTERACTION-1", "type": 11, "scene": "group", "chat_type": 1,
			"group_openid": "GROUP-OPENID", "group_member_openid": "MEMBER-OPENID"}`,
	} {
		buttons := NewButtons()
		var seen []Press
		claim(t, buttons, "join_verification",
			ButtonClaim{Namespace: "v:", Scenes: InGroup | InPrivate,
				Handle: recorder(&seen)})
		client := testClient(t)
		buttons.Register(client)

		press(t, client, body)
		if len(seen) != 0 {
			t.Errorf("%s reached a claim, want silence", body)
		}
	}
}

// TestOverlappingNamespacesAreRefused covers the mistake a claim exists to catch.
//
// Resolving an overlap instead would make routing depend on which feature claimed
// first, and the buttons of one would answer as those of the other.
func TestOverlappingNamespacesAreRefused(t *testing.T) {
	handle := func(context.Context, Press) error { return nil }
	for _, pair := range [][2]string{
		{"v:", "v:"},
		{"v:", "v:deeper"},
		{"v:deeper", "v:"},
		{"qgb-receipt-detail:", "qgb-receipt-"},
	} {
		buttons := NewButtons()
		claim(t, buttons, "first", ButtonClaim{Namespace: pair[0], Scenes: InGroup,
			Handle: handle})
		err := buttons.Set("second", []ButtonClaim{{Namespace: pair[1],
			Scenes: InGroup, Handle: handle}})
		if err == nil {
			t.Errorf("%q was accepted beside %q", pair[1], pair[0])
		}
	}
}

// TestOneOwnerCannotOverlapItself covers the same mistake made inside one
// feature's own set: two of its namespaces that a press cannot be told apart by.
func TestOneOwnerCannotOverlapItself(t *testing.T) {
	handle := func(context.Context, Press) error { return nil }
	err := NewButtons().Set("one", []ButtonClaim{
		{Namespace: "qgb-receipt-", Scenes: InGroup, Handle: handle},
		{Namespace: "qgb-receipt-detail:", Scenes: InGroup, Handle: handle},
	})
	if err == nil {
		t.Error("a feature was allowed to claim two namespaces a press cannot tell apart")
	}
}

// TestARebuiltFeatureClaimsWhatItClaimedBefore covers the case that makes a
// feature rebuildable: the second build claims exactly what the first did.
//
// Adding instead of replacing would refuse that as a conflict with the feature
// itself, and the rebuilt feature would never answer a press again.
func TestARebuiltFeatureClaimsWhatItClaimedBefore(t *testing.T) {
	buttons := NewButtons()
	var first, second []Press
	claim(t, buttons, "admin_commands",
		ButtonClaim{Namespace: "qgb-receipt-detail:", Scenes: InGroup,
			Handle: recorder(&first)})
	// The same feature, built again.
	claim(t, buttons, "admin_commands",
		ButtonClaim{Namespace: "qgb-receipt-detail:", Scenes: InGroup,
			Handle: recorder(&second)})

	client := testClient(t)
	buttons.Register(client)
	press(t, client, groupPressBody("qgb-receipt-detail:RECEIPT"))

	if len(first) != 0 {
		t.Errorf("the instance that stopped answered %d press(es)", len(first))
	}
	if len(second) != 1 {
		t.Fatalf("the instance that is running answered %d press(es), want 1", len(second))
	}
}

// TestReleaseGivesTheNamespaceBack covers stopping: the press is answered by
// nobody, and the namespace is free for the build that comes after.
func TestReleaseGivesTheNamespaceBack(t *testing.T) {
	buttons := NewButtons()
	var seen []Press
	claim(t, buttons, "join_verification",
		ButtonClaim{Namespace: "v:", Scenes: InGroup, Handle: recorder(&seen)})
	client := testClient(t)
	buttons.Register(client)

	press(t, client, groupPressBody("v:TOKEN"))
	if len(seen) != 1 {
		t.Fatalf("the feature answered %d press(es) before stopping, want 1", len(seen))
	}

	buttons.Release("join_verification")
	press(t, client, groupPressBody("v:TOKEN"))
	if len(seen) != 1 {
		t.Errorf("a press was answered after the feature gave its buttons back")
	}

	// And the namespace is free again, which is what lets the feature be built
	// once more.
	var rebuilt []Press
	claim(t, buttons, "join_verification",
		ButtonClaim{Namespace: "v:", Scenes: InGroup, Handle: recorder(&rebuilt)})
	press(t, client, groupPressBody("v:TOKEN"))
	if len(rebuilt) != 1 {
		t.Errorf("the rebuilt feature answered %d press(es), want 1", len(rebuilt))
	}
}

// TestAClaimMustSayWhatItAnswers covers the three things a claim cannot leave out.
func TestAClaimMustSayWhatItAnswers(t *testing.T) {
	handle := func(context.Context, Press) error { return nil }
	for why, claims := range map[string][]ButtonClaim{
		"no namespace": {{Scenes: InGroup, Handle: handle}},
		"none at all":  {{}},
		"no scenes":    {{Namespace: "v:", Handle: handle}},
		"no handler":   {{Namespace: "v:", Scenes: InGroup}},
	} {
		if err := NewButtons().Set("one", claims); err == nil {
			t.Errorf("a claim with %s was accepted", why)
		}
	}
	if err := NewButtons().Set("", []ButtonClaim{{Namespace: "v:", Scenes: InGroup,
		Handle: handle}}); err == nil {
		t.Error("buttons were accepted with no owner to give them back to")
	}
}

// TestButtonsOrOwn covers the difference between a feature built by a bot and one
// built on its own: the first shares the registry, the second gets its own.
func TestButtonsOrOwn(t *testing.T) {
	shared := NewButtons()
	if got := ButtonsOrOwn(shared); got != shared {
		t.Error("a registry handed over was not the one used")
	}
	if got := ButtonsOrOwn(nil); got == nil {
		t.Error("a feature built without a bot got no registry")
	} else if got == shared {
		t.Error("a feature built without a bot was given somebody else's registry")
	}
}
