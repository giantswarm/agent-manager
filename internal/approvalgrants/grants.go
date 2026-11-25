// Package approvalgrants keeps the approval grants of agents: a connector
// admin's Allow over that connector's Ask, for one entry of one agent's tool
// list (giantswarm/giantswarm#38902, slice 3).
//
// Grants are their own records behind their own API, never part of an
// agent's values, labels or annotations: values can be written past
// agent-manager (GitOps, a hand edit), and nothing written there may lift a
// connector admin's Ask. The records are kept with agent-manager's own
// ServiceAccount, one ConfigMap per agent in agent-manager's namespace, in a
// versioned document (APIVersion, Kind). Store is the only way agent-manager
// reaches them, so a later owner (a connector manager,
// giantswarm/bumblebee-plans#237) can take the records over by reading the
// same objects, or by putting its own Store behind the interface, without a
// migration of agent values. See docs/approval-grants.md.
package approvalgrants

import (
	"context"
	"errors"
	"time"
)

// The document's version and kind.
const (
	APIVersion = "approval.agent-manager.giantswarm.io/v1"
	Kind       = "ApprovalGrants"
)

// ErrConflict is a Replace whose record changed since it was read.
var ErrConflict = errors.New("the agent's approval grants changed since they were read")

// Agent names the agent a record belongs to. UID is its HelmRelease's
// metadata.uid: a recreated agent has a new UID and none of the old grants.
type Agent struct {
	UID       string `json:"uid"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// Key is the agent as <namespace>/<name>, as muster's binding names it.
func (a Agent) Key() string { return a.Namespace + "/" + a.Name }

// Grant is one connector admin's Allow over the connector's Ask for one
// entry of the agent's tool list (or its default).
type Grant struct {
	// Entry is the tool list's selector, or "default".
	Entry string `json:"entry"`
	// Server is the connector's muster server key.
	Server string `json:"server"`
	// ConnectorNamespace and ConnectorUID are the MCPServer's namespace and
	// metadata.uid.
	ConnectorNamespace string `json:"namespace"`
	ConnectorUID       string `json:"connectorUid"`
	// GrantedBy is who saved it: email, else subject.
	GrantedBy string    `json:"grantedBy"`
	GrantedAt time.Time `json:"grantedAt"`
	// Generation is the HelmRelease metadata.generation the save that
	// recorded it wrote. A release whose generation moved on was written
	// past agent-manager: the grant no longer holds.
	Generation int64 `json:"generation"`
}

// Record is one agent's grants. Version is the stored object's version, ""
// for a record that does not exist yet; Replace writes only over it.
type Record struct {
	Agent   Agent   `json:"agent"`
	Grants  []Grant `json:"grants"`
	Version string  `json:"-"`
}

// Store keeps the grants.
type Store interface {
	// Get returns the agent's record; an empty one (Version "") when it has
	// none.
	Get(ctx context.Context, agent Agent) (Record, error)
	// ListByNamespace returns the records of the agents of one namespace.
	ListByNamespace(ctx context.Context, namespace string) ([]Record, error)
	// Replace writes the record over the version it was read at (ErrConflict
	// when it moved); a record without grants is deleted.
	Replace(ctx context.Context, r Record) error
	// Delete removes the agent's record; a missing one is no error.
	Delete(ctx context.Context, agentUID string) error
}
