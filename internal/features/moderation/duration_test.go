package moderation

import (
	"testing"
	"time"
)

// TestHumanDuration is the mirror of TestHumanDuration in admincmd.
//
// The two renderers are separate copies on purpose (see the comment on this one's
// implementation), so what keeps them the same is the same cases written twice: a change to
// one that this file does not expect fails here, in the package that changed, rather than in
// a group where a member reads back a duration the bot never meant.
func TestHumanDuration(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{0, "0秒"},
		{30 * time.Second, "30秒"},
		{90 * time.Second, "1分30秒"},
		{10 * time.Minute, "10分钟"},
		{100 * time.Minute, "1小时40分钟"},
		{2 * time.Hour, "2小时"},
		{25 * time.Hour, "1天1小时"},
		{time.Hour + time.Minute + time.Second, "1小时1分1秒"},
		{29 * 24 * time.Hour, "29天"},
	}
	for _, one := range cases {
		if got := humanDuration(one.in); got != one.want {
			t.Errorf("humanDuration(%s) = %q, want %q", one.in, got, one.want)
		}
	}
}
