package command

import "testing"

// TestSeenReportsEveryLaterDelivery covers the redelivery the record exists for:
// the same message arriving twice is acted on once.
func TestSeenReportsEveryLaterDelivery(t *testing.T) {
	seen := NewSeen()
	if !seen.First("MESSAGE-1") {
		t.Error("the first sighting of a message was refused")
	}
	if seen.First("MESSAGE-1") {
		t.Error("the second sighting of a message was acted on again")
	}
	if !seen.First("MESSAGE-2") {
		t.Error("another message was taken for a redelivery")
	}
	// A message the platform gave no id cannot be recognised, and answering it a
	// second time is better than dropping the only delivery there is.
	if !seen.First("") || !seen.First("") {
		t.Error("a message with no id was dropped")
	}
}
