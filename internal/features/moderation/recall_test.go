package moderation

import "testing"

// TestTheJudgesRecallListIsCheckedBeforeItIsUsed covers the safety of letting a
// model name what to take down.
//
// A number out of a model is a pointer, and pointers need checking: a number it
// invented points at nothing, and one that lands on somebody else's message is not
// a reason to take that message down. The report is about one sender.
func TestTheJudgesRecallListIsCheckedBeforeItIsUsed(t *testing.T) {
	chain := []CachedMessage{
		{ID: "M-1", Idx: "IDX-1", User: "SUBJECT", Text: "第一条"},
		{ID: "M-2", Idx: "IDX-2", User: "SOMEONE-ELSE", Text: "别人说的话"},
		{ID: "M-3", Idx: "IDX-3", User: "SUBJECT", Text: "第二条广告"},
		{ID: "M-4", Idx: "IDX-4", User: "SUBJECT", Text: "第三条广告"},
	}

	t.Run("the offending sender's own messages", func(t *testing.T) {
		ids, numbers := resolveRecall(chain, "SUBJECT", []int{1, 3, 4}, "M-1", "IDX-1")
		if len(ids) != 3 || ids[0] != "M-1" || ids[2] != "M-4" {
			t.Errorf("recall = %v, want the three of the subject's own", ids)
		}
		if len(numbers) != 3 || numbers[2] != 4 {
			t.Errorf("numbers = %v, want the numbers the judge was shown", numbers)
		}
	})

	t.Run("numbers that point at nothing", func(t *testing.T) {
		ids, _ := resolveRecall(chain, "SUBJECT", []int{0, 99, -3}, "M-1", "IDX-1")
		// Nothing usable was named, so the reported message stands rather than the
		// violation leaving the advertisement in place.
		if len(ids) != 1 || ids[0] != "M-1" {
			t.Errorf("recall = %v, want only the reported message", ids)
		}
	})

	t.Run("a number pointing at somebody else", func(t *testing.T) {
		ids, numbers := resolveRecall(chain, "SUBJECT", []int{2}, "M-1", "IDX-1")
		if len(ids) != 1 || ids[0] != "M-1" {
			t.Errorf("recall = %v, want the bystander's message left alone", ids)
		}
		if len(numbers) != 1 || numbers[0] != 1 {
			t.Errorf("numbers = %v, want the reported message's number", numbers)
		}
	})

	t.Run("the same message named twice", func(t *testing.T) {
		ids, _ := resolveRecall(chain, "SUBJECT", []int{3, 3}, "M-1", "IDX-1")
		if len(ids) != 1 || ids[0] != "M-3" {
			t.Errorf("recall = %v, want one entry", ids)
		}
	})
}
