package admincmd

import "testing"

// TestAMuteWhoseCapCannotBeReadStillAnswersTheGroup covers the last line of defence
// behind a command that always has to answer.
//
// A cap that cannot be read cannot reach a running feature: the section is checked when
// the feature is built, and this feature has no way to adopt a new one afterwards. The
// branch below is therefore unreachable in production -- which is exactly why it is
// worth a test. It was the one place in this command that returned its error to the
// dispatcher instead of saying it to the group, and an error returned to the dispatcher
// is a log line nobody in the group can see: the administrator who typed the command is
// met with silence, which is the one report that cannot be acted on.
//
// The cap is put in place after the feature is built, because that is the only way to
// reach the branch at all.
func TestAMuteWhoseCapCannotBeReadStillAnswersTheGroup(t *testing.T) {
	h := newHarness(t, baseSection)
	h.handler.cfg.Groups[testGroupOpenID] = GroupConfig{
		Admins:  []string{testAdmin},
		MaxMute: "100d",
	}

	h.send("/禁言 30m", testAdmin, testGroupOpenID, testTarget)

	if reply := h.lastReply(); reply == "" {
		t.Error("the group was told nothing: the cap could not be read, and the " +
			"administrator has to be told so rather than met with silence")
	}
	if h.muteCount() != 0 {
		t.Errorf("mute calls = %d, want 0: no mute may be applied on top of a cap that "+
			"could not be read", h.muteCount())
	}
}
