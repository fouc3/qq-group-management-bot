package joinrequest

import (
	"io"
	"log/slog"
	"testing"

	"github.com/fouc3/qq-group-management-bot/internal/feature"
)

// TestABarredApplicantGetsTheConfiguredOutcome covers the choice the section
// offers: refused outright, or quietly left to a person.
//
// It never covers "admitted". Being on the list is not the same as being
// approved, and that boundary is the reason this setting has two values rather
// than three.
func TestABarredApplicantGetsTheConfiguredOutcome(t *testing.T) {
	cases := map[string]struct {
		section string
		want    string
	}{
		"the default refuses them": {"enabled: true\naction: approve\n", ActionDecline},
		"ignore leaves it to a person": {
			"enabled: true\naction: approve\nbarred: ignore\n", ""},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, testCase.section)
			h.feature.blacklist = &stubBlacklist{barred: true}

			if err := h.request(); err != nil {
				t.Fatalf("onJoinRequest: %v", err)
			}
			if got := h.answered(); got != testCase.want {
				t.Errorf("answered %q, want %q", got, testCase.want)
			}
		})
	}
}

// TestApproveIsNotAValueForBarred covers the boundary at the configuration
// layer, so a section asking for it is refused at startup rather than quietly
// admitting the people it lists.
func TestApproveIsNotAValueForBarred(t *testing.T) {
	_, err := New(sectionNode(t, "enabled: true\nbarred: approve\n"),
		feature.Deps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err == nil {
		t.Error("barred: approve must be refused")
	}
}

// TestBarredIgnoreStillRefusesNobodyElse covers that the setting only reaches
// the people on the list: everybody else still follows action.
func TestBarredIgnoreStillRefusesNobodyElse(t *testing.T) {
	h := newHarness(t, "enabled: true\naction: approve\nbarred: ignore\n")

	if err := h.request(); err != nil {
		t.Fatalf("onJoinRequest: %v", err)
	}
	if got := h.answered(); got != ActionApprove {
		t.Errorf("answered %q, want %q for an applicant who is not on the list",
			got, ActionApprove)
	}
}
