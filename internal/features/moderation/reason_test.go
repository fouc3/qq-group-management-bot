package moderation

import (
	"errors"
	"strings"
	"testing"
)

// TestTheRefusalReasonIsShortenedTheSameWay is the mirror of the test of that name in
// admincmd.
//
// This package's copy is called recallReason and the other one is shortReason; the cases
// are the same on purpose, because a refusal an operator reads in a record here and one a
// member reads in a group there have to recognise each other.
func TestTheRefusalReasonIsShortenedTheSameWay(t *testing.T) {
	// Long enough to be cut, and carrying a trace id nobody should be shown.
	long := errors.New("qqbotsdk: POST https://api.bot.qq.com/x: http 400: err_code 306004: " +
		strings.Repeat("参数不对", 60) + " [trace_id=" + strings.Repeat("a", 32) + "]")
	reason := recallReason(long)
	if !strings.HasSuffix(reason, "…") {
		t.Errorf("a refusal this long was not cut short: %q", reason)
	}
	if got := len([]rune(reason)); got != 121 {
		t.Errorf("the shortened refusal is %d characters, want 121 (120 and the ellipsis): %q",
			got, reason)
	}
	if strings.Contains(reason, "https://") || strings.Contains(reason, "trace_id") {
		t.Errorf("the shortened refusal still carries the request line or the trace id: %q",
			reason)
	}

	// The same refusal, with the trace id written against the text before it rather than
	// after a space. It is still the trace id, and it still must not be recorded.
	joined := recallReason(errors.New(
		"qqbotsdk: POST https://x: http 400: err_code 306004: 请求参数错误[trace_id=abc]"))
	if strings.Contains(joined, "trace_id") {
		t.Errorf("a trace id with no space in front of it was left in: %q", joined)
	}
	if !strings.Contains(joined, "请求参数错误") {
		t.Errorf("the platform's own wording was dropped with the trace id: %q", joined)
	}
}
