package agents

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/giantswarm/agent-manager/internal/approvalgrants"
	"github.com/giantswarm/agent-manager/internal/identity"
	"github.com/giantswarm/agent-manager/internal/muster"
)

// Revocation reaching running Sessions (M2-S3-C-04, S-04). A grant leaves an
// agent's running Sessions only when muster is told: PATCH
// /session-bindings/grants with the grants left (keep). agent-manager records
// first that muster has to be told (the record's PendingSessionSync), then
// tells it, retrying a few times; only a confirmed call clears the mark. A
// mark left set is sent again on the agent's next bind, save or read (any
// caller's request carrying a person's token: muster takes the call only with
// one), so a failure delays the revocation instead of losing it, and the
// caller's result says so.

// syncAttempts and syncBackoff bound the retries of one sync.
var (
	syncAttempts = 3
	syncBackoff  = 200 * time.Millisecond
	// syncSleep waits between attempts; tests replace it.
	syncSleep = func(ctx context.Context, d time.Duration) {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
		case <-t.C:
		}
	}
)

// SessionSync is what a write reports about telling muster: whether the
// agent's running Sessions now carry exactly its grants.
type SessionSync struct {
	// SessionsUpdated is false when muster could not be told; nil when no
	// call was needed.
	SessionsUpdated *bool `json:"sessionsUpdated,omitempty"`
	// Warnings say what did not reach muster and when it is sent again.
	Warnings []string `json:"warnings,omitempty"`
}

// keepMarks are the marks muster keeps: the grants left.
func keepMarks(grants []approvalgrants.Grant) []muster.Grant {
	keep := make([]muster.Grant, 0, len(grants))
	for _, g := range grants {
		keep = append(keep, muster.Grant{Entry: g.Entry, Server: g.Server, UID: g.ConnectorUID, Tools: g.Tools})
	}
	return keep
}

// syncSessions tells muster the agent's grants (rec, as stored with
// PendingSessionSync set) and clears the pending mark once muster confirmed
// it, unless the grants changed meanwhile (that save syncs its own). A record
// without grants whose agent is gone is deleted once synced.
func (s *Service) syncSessions(ctx context.Context, tool string, rec approvalgrants.Record, agentGone bool) SessionSync {
	dropper, ok := s.cfg.Bindings.(GrantDropper)
	attrs := s.approvalLogAttrs(ctx, tool, rec.Agent.Key())
	if !ok || s.cfg.Bindings == nil {
		return SessionSync{}
	}
	token, _ := identity.TokenFromContext(ctx)
	keep := keepMarks(rec.Grants)
	var err error
	for attempt := 0; attempt < syncAttempts; attempt++ {
		if attempt > 0 {
			syncSleep(ctx, syncBackoff<<(attempt-1))
		}
		if err = dropper.DropGrants(ctx, token, rec.Agent.Key(), keep); err == nil {
			break
		}
	}
	if err != nil {
		s.log.WarnContext(ctx, "approval grants removed from running sessions", append(attrs, "kept", len(keep), "attempts", syncAttempts, "error", err.Error())...)
		updated := false
		return SessionSync{SessionsUpdated: &updated, Warnings: []string{fmt.Sprintf(
			"muster was not told which grants agent %s keeps (%v): its running sessions keep the grants they were bound with until agent-manager tells muster on the agent's next start, save or read", rec.Agent.Key(), err)}}
	}
	s.log.InfoContext(ctx, "approval grants removed from running sessions", append(attrs, "kept", len(keep))...)
	s.clearPending(ctx, rec, agentGone)
	updated := true
	return SessionSync{SessionsUpdated: &updated}
}

// clearPending clears the record's pending mark when its grants are still
// the ones muster was told.
func (s *Service) clearPending(ctx context.Context, sent approvalgrants.Record, agentGone bool) {
	if s.cfg.Grants == nil {
		return
	}
	for attempt := 0; attempt < 3; attempt++ {
		rec, err := s.cfg.Grants.Get(ctx, sent.Agent)
		if err != nil || !rec.PendingSessionSync || !sameGrants(rec.Grants, sent.Grants) {
			return
		}
		rec.PendingSessionSync = false
		if agentGone {
			rec.Grants = nil
		}
		err = s.cfg.Grants.Replace(ctx, rec)
		if !errors.Is(err, approvalgrants.ErrConflict) {
			return
		}
	}
}

// sameGrants compares grant sets by what muster is told of them.
func sameGrants(a, b []approvalgrants.Grant) bool {
	ka, kb := keepMarks(a), keepMarks(b)
	if len(ka) != len(kb) {
		return false
	}
	for _, m := range ka {
		if !slices.ContainsFunc(kb, func(o muster.Grant) bool {
			return o.Entry == m.Entry && o.Server == m.Server && o.UID == m.UID && slices.Equal(o.Tools, m.Tools)
		}) {
			return false
		}
	}
	return true
}

// resendPendingIn sends every pending sync of a namespace's records again,
// those of agents that no longer exist (live holds the agents' release
// UIDs) included: a deleted agent's empty record is removed once muster has
// it.
func (s *Service) resendPendingIn(ctx context.Context, tool string, records []approvalgrants.Record, live map[string]bool) {
	for _, r := range records {
		if r.PendingSessionSync {
			s.syncSessions(ctx, tool, r, !live[r.Agent.UID])
		}
	}
}
