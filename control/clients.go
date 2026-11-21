package control

import (
	"errors"
	"maps"
	"slices"
	"time"
	"unicode/utf8"
)

func queryGroup(group string) bool {
	switch group {
	case "discovery", "system", "network", "devices", "logs":
		return true
	}
	return false
}

func (m *Manager) client(id string) (*Client, error) {
	if m.fatal != nil {
		return nil, m.fatal
	}
	for i := range m.state.Clients {
		c := &m.state.Clients[i]
		if c.ID == id && !c.Revoked {
			return c, nil
		}
	}
	return nil, errors.New("unknown or revoked client")
}

func (m *Manager) ImportClient(c Client) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fatal != nil {
		return m.fatal
	}
	if !validID.MatchString(c.ID) {
		return errors.New("invalid client id")
	}
	for _, existing := range m.state.Clients {
		if existing.ID == c.ID {
			return nil
		}
	}
	if c.Limit == 0 {
		c.Limit = 60
	}
	if c.Limit < 1 || c.Limit > 600 || len(c.Name) > 256 || !utf8.ValidString(c.Name) {
		return errors.New("invalid client configuration")
	}
	for group := range c.Queries {
		if !queryGroup(group) {
			return errors.New("invalid query group")
		}
	}
	c.Queries = maps.Clone(c.Queries)
	if c.Queries == nil {
		c.Queries = make(map[string]bool)
	}
	c.Grants = make(map[string]time.Time)
	m.state.Clients = append(m.state.Clients, c)
	m.event(c.ID, "client", "imported")
	return m.save()
}
func (m *Manager) CheckClient(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, err := m.client(id)
	return err
}

func (m *Manager) rate(c *Client) error {
	now := m.now()
	cutoff := now.Add(-time.Minute)
	hits := slices.DeleteFunc(m.hits[c.ID], func(t time.Time) bool { return !t.After(cutoff) })
	m.hits[c.ID] = hits
	if len(hits) >= c.Limit {
		return errors.New("client rate limit exceeded")
	}
	m.hits[c.ID] = append(hits, now)
	return nil
}

func (m *Manager) AuthorizeQuery(id, group string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, err := m.client(id)
	if err != nil {
		return err
	}
	if err = m.rate(c); err != nil {
		return err
	}
	if !queryGroup(group) {
		return errors.New("invalid query group")
	}
	allowed := group == "discovery" || c.Queries[group]
	result := "allowed"
	if !allowed {
		result = "denied"
	}
	// Coalesce repeated query checks to one audit entry per group/result/minute.
	cutoff := m.now().Add(-time.Minute)
	action := "query " + group
	logged := false
	for _, e := range slices.Backward(m.state.Audit) {
		if e.Time.Before(cutoff) {
			break
		}
		if e.Client == id && e.Action == action && e.Result == result {
			logged = true
			break
		}
	}
	if !logged {
		m.event(id, action, result)
		if err = m.save(); err != nil {
			return err
		}
	}
	if !allowed {
		return errors.New("query group disabled")
	}
	return nil
}

func (m *Manager) SetQuery(id, group string, enabled bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, err := m.client(id)
	if err != nil {
		return err
	}
	if !queryGroup(group) || group == "discovery" {
		return errors.New("query group cannot be changed")
	}
	c.Queries[group] = enabled
	result := "disabled"
	if enabled {
		result = "enabled"
	}
	m.event(id, "query permission "+group, result)
	return m.save()
}
func (m *Manager) SetLimit(id string, limit int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, err := m.client(id)
	if err != nil {
		return err
	}
	if limit < 1 || limit > 600 {
		return errors.New("limit must be 1 to 600 per minute")
	}
	c.Limit = limit
	m.event(id, "rate limit", "updated")
	return m.save()
}
func ordinary(kind string) bool { return kind == "node" || kind == "mode" }

func (m *Manager) RevokeGrant(id, kind string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, err := m.client(id)
	if err != nil {
		return err
	}
	if !ordinary(kind) {
		return errors.New("invalid operation grant")
	}
	delete(c.Grants, kind)
	m.event(id, "grant "+kind, "revoked")
	return m.save()
}
func (m *Manager) Grant(id, kind string, duration time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, err := m.client(id)
	if err != nil {
		return err
	}
	if m.state.Paused {
		return errors.New("operations paused")
	}
	if !ordinary(kind) || duration < time.Minute || duration > time.Hour {
		return errors.New("invalid operation grant or duration")
	}
	c.Grants[kind] = m.now().UTC().Add(duration)
	m.event(id, "grant "+kind, "enabled")
	return m.save()
}
func (m *Manager) cancelPending(id string) {
	for i := range m.state.Requests {
		r := &m.state.Requests[i]
		if r.Status == "pending" && (id == "" || r.Client == id) {
			m.outcome(r, "cancelled", "")
		}
	}
}
func (m *Manager) Revoke(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, err := m.client(id)
	if err != nil {
		return err
	}
	c.Revoked = true
	clear(c.Grants)
	delete(m.hits, id)
	m.cancelPending(id)
	m.event(id, "client", "revoked")
	return m.save()
}
func (m *Manager) Pause() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fatal != nil {
		return m.fatal
	}
	m.state.Paused = true
	for i := range m.state.Clients {
		clear(m.state.Clients[i].Grants)
	}
	m.cancelPending("")
	m.event("", "operations", "paused")
	return m.save()
}
func (m *Manager) Resume() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fatal != nil {
		return m.fatal
	}
	m.state.Paused = false
	m.event("", "operations", "resumed")
	return m.save()
}

