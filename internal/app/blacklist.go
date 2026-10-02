package app

import (
	"context"
	"time"

	"github.com/fouc3/qq-group-management-bot/internal/features/joinrequest"
	"github.com/fouc3/qq-group-management-bot/internal/store"
)

// barredFromJoining answers the join-request feature from the blacklist in the
// data layer.
//
// The adapter lives here, in the composition root, because this is the only
// place allowed to know both sides: the feature declares what it needs from a
// list, and the data layer declares what it can answer. Neither imports the
// other, which is what keeps the feature testable without a database.
type barredFromJoining struct {
	blacklist store.BlacklistStore
}

// Barred implements joinrequest.Blacklist.
//
// The list is only about join requests. Nothing here reaches into a group to
// remove somebody, and nothing here says anything about a member who is already
// in one.
func (b barredFromJoining) Barred(ctx context.Context, applicant joinrequest.Applicant) (bool, error) {
	return b.blacklist.Barred(ctx, applicant.MemberOpenID, applicant.UnionOpenID, time.Now())
}
