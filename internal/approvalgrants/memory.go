package approvalgrants

import (
	"context"
	"slices"
	"strconv"
	"sync"
)

// MemoryStore is an in-process Store, for tests.
type MemoryStore struct {
	mu      sync.Mutex
	records map[string]Record
	version int
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore { return &MemoryStore{records: map[string]Record{}} }

// Get implements Store.
func (m *MemoryStore) Get(_ context.Context, agent Agent) (Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.records[agent.UID]
	if !ok {
		return Record{Agent: agent}, nil
	}
	r.Grants = slices.Clone(r.Grants)
	return r, nil
}

// ListByNamespace implements Store.
func (m *MemoryStore) ListByNamespace(_ context.Context, namespace string) ([]Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Record
	for _, r := range m.records {
		if r.Agent.Namespace == namespace {
			r.Grants = slices.Clone(r.Grants)
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b Record) int {
		if a.Agent.Name < b.Agent.Name {
			return -1
		}
		if a.Agent.Name > b.Agent.Name {
			return 1
		}
		return 0
	})
	return out, nil
}

// Replace implements Store.
func (m *MemoryStore) Replace(_ context.Context, r Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.records[r.Agent.UID]
	if (ok && current.Version != r.Version) || (!ok && r.Version != "") {
		return ErrConflict
	}
	if len(r.Grants) == 0 {
		delete(m.records, r.Agent.UID)
		return nil
	}
	m.version++
	r.Version = strconv.Itoa(m.version)
	r.Grants = slices.Clone(r.Grants)
	m.records[r.Agent.UID] = r
	return nil
}

// Delete implements Store.
func (m *MemoryStore) Delete(_ context.Context, agentUID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.records, agentUID)
	return nil
}

// Put stores grants for an agent directly, for tests.
func (m *MemoryStore) Put(agent Agent, grants ...Grant) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.version++
	m.records[agent.UID] = Record{Agent: agent, Grants: slices.Clone(grants), Version: strconv.Itoa(m.version)}
}
