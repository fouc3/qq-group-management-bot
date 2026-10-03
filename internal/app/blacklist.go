package app

import (
	"context"
	"time"

	"github.com/fouc3/qq-group-management-bot/internal/features/admincmd"
	"github.com/fouc3/qq-group-management-bot/internal/features/joinrequest"
	"github.com/fouc3/qq-group-management-bot/internal/store"
)

// joinBarrierAware is a feature that takes the list of barred applicants.
type joinBarrierAware interface {
	SetBlacklist(joinrequest.Blacklist)
}

// blacklistAdminAware is a feature that takes the blacklist to manage.
//
// Two interfaces rather than one, and both are declared here, because a feature
// can only have one method called SetBlacklist: the two features want different
// things from the same list, and the compiler is what tells them apart. The
// interfaces name a feature package each, which is why they live in the
// composition root beside the adapter.
type blacklistAdminAware interface {
	SetBlacklist(admincmd.Blacklist)
}

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
