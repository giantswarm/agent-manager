// Package approvalgrants keeps the approval grants of agents: a connector
// admin's Allow over that connector's Ask, for one entry of one agent's tool
// list (giantswarm/giantswarm#38902, slice 3).
//
// Grants are their own records behind their own API, never part of an
// agent's values, labels or annotations: values can be written past
// agent-manager (GitOps, a hand edit), and nothing written there may lift a
// connector admin's Ask. The records are kept with agent-manager's own
// ServiceAccount, one ConfigMap per agent in agent-manager's namespace, in a
// versioned document (APIVersion, Kind), in a namespace that holds nothing
// else (the chart's grants namespace): whoever may write ConfigMaps there
// may lift any connector's Ask for any agent, so only agent-manager's
// ServiceAccount should. A record is checked on read (its object name, its
// labels and the agent it names agree) and one that does not is skipped.
// Store is the only way agent-manager
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
	// recorded it wrote. A release whose generation moved on (any change of
	// its spec: values, chart version, suspend) was written past
	// agent-manager: the grant no longer holds.
	Generation int64 `json:"generation"`
	// Tools are the server's own names of the tools the grant covered when
	// it was given; muster lifts the connector's Ask for those only.
	Tools []string `json:"tools"`
}

// Record is one agent's grants. Version is the stored object's version, ""
// for a record that does not exist yet; Replace writes only over it.
// PendingSessionSync marks a record whose grants muster has not yet been told
// about: a drop that did not reach the agent's running Sessions. It is sent
// again on the agent's next bind, save or read, and the record is kept for
// it even without grants (an agent that was deleted included).
type Record struct {
	Agent              Agent   `json:"agent"`
	Grants             []Grant `json:"grants"`
	PendingSessionSync bool    `json:"pendingSessionSync,omitempty"`
	Version            string  `json:"-"`
}

// Store keeps the grants.
type Store interface {
	// Get returns the agent's record; an empty one (Version "") when it has
	// none.
	Get(ctx context.Context, agent Agent) (Record, error)
	// ListByNamespace returns the records of the agents of one namespace;
	// a record that fails its checks is skipped (and logged).
	ListByNamespace(ctx context.Context, namespace string) ([]Record, error)
	// Replace writes the record over the version it was read at (ErrConflict
	// when it moved); a record without grants and without a pending sync is
	// deleted.
	Replace(ctx context.Context, r Record) error
	// Delete removes the agent's record; a missing one is no error.
	Delete(ctx context.Context, agentUID string) error
}
