// Package control owns client permissions, approvals and durable request history.
package control

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sync"
	"time"
)

type Operation struct {
	Kind   string            `json:"kind"`
	Params map[string]string `json:"params"`
}
type Preview struct {
	Before     map[string]string `json:"before"`
	After      map[string]string `json:"after"`
	Sensitive  bool              `json:"sensitive"`
	Reversible bool              `json:"reversible"`
	Summary    string            `json:"summary"`
}
type Executor interface {
	Describe(context.Context, Operation) (Preview, error)
	Execute(context.Context, string, Operation, time.Time) (bool, error)
	Confirm(context.Context, string) error
	Rollback(context.Context, string) error
	Result(context.Context, string) (string, error)
}
type Client struct {
	ID      string               `json:"id"`
	Name    string               `json:"name"`
	Queries map[string]bool      `json:"queries"`
	Grants  map[string]time.Time `json:"grants"`
	Limit   int                  `json:"limit"`
	Revoked bool                 `json:"revoked"`
}
type Request struct {
	ID              string    `json:"id"`
	Client          string    `json:"client"`
	Operation       Operation `json:"operation"`
	Preview         Preview   `json:"preview"`
	Digest          string    `json:"digest"`
	Status          string    `json:"status"`
	CreatedAt       time.Time `json:"created_at"`
	ExpiresAt       time.Time `json:"expires_at"`
	ConfirmDeadline time.Time `json:"confirm_deadline"`
	Error           string    `json:"error,omitempty"`
}
type Event struct {
	Time   time.Time `json:"time"`
	Client string    `json:"client"`
	Action string    `json:"action"`
	Result string    `json:"result"`
}
type State struct {
	Paused   bool      `json:"paused"`
	Clients  []Client  `json:"clients"`
	Requests []Request `json:"requests"`
	Audit    []Event   `json:"audit"`
	Now      time.Time `json:"now"`
}

// A Manager has a single owning daemon. Its lock spans the durable claim and
// platform operation so two administrators cannot execute the same request.
type Manager struct {
	mu    sync.Mutex
	dir   string
	ex    Executor
	now   func() time.Time
	state State
	fatal error
	hits  map[string][]time.Time
}

// The checksum lets startup distinguish a valid empty state from an incomplete
// but syntactically valid JSON file. File permissions protect its integrity.
type record struct {
	Version  int             `json:"version"`
	State    json.RawMessage `json:"state"`
	Checksum string          `json:"sha256"`
}

var validID = regexp.MustCompile("^[A-Za-z0-9_-]{1,40}$")

func hash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func New(dir string, ex Executor) (*Manager, error) {
	if dir == "" || ex == nil {
		return nil, errors.New("state directory and executor required")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("invalid state directory")
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return nil, err
	}
	m := &Manager{dir: dir, ex: ex, now: time.Now, hits: make(map[string][]time.Time),
		state: State{Clients: []Client{}, Requests: []Request{}, Audit: []Event{}}}
	path := filepath.Join(dir, "state.json")
	info, err = os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		// A durable marker distinguishes first use from loss of a
		// previously initialized permission database.
		if err = os.Mkdir(filepath.Join(dir, ".initialized"), 0700); err != nil {
			return nil, fmt.Errorf("cannot initialize missing permission state: %w", err)
		}
		if err = m.save(); err != nil {
			return nil, err
		}
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("invalid state file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var stored record
	if err = json.Unmarshal(data, &stored); err != nil {
		return nil, err
	}
	if stored.Version != 1 || hash(stored.State) != stored.Checksum {
		return nil, errors.New("invalid state checksum or version")
	}
	var state State
	dec := json.NewDecoder(bytes.NewReader(stored.State))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&state); err != nil {
		return nil, err
	}
	if state.Clients == nil || state.Requests == nil || state.Audit == nil {
		return nil, errors.New("incomplete state")
	}
	if err = os.Mkdir(filepath.Join(dir, ".initialized"), 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	m.state = state
	for i := range m.state.Clients {
		m.state.Clients[i].Grants = map[string]time.Time{}
	}
	for i := range m.state.Requests {
		r := &m.state.Requests[i]
		switch r.Status {
		case "executing":
			// A crash can follow the platform journal write and precede our
			// own waiting transition. Never retry the consumed execution.
			if r.Preview.Sensitive && r.Preview.Reversible {
				result, err := m.ex.Result(context.Background(), r.ID)
				if err == nil && (result == "kept" || result == "rolledback") {
					m.outcome(r, result, "")
					continue
				}
				if !errors.Is(err, os.ErrNotExist) {
					m.requireRollback(r)
					continue
				}
			}
			m.outcome(r, "cancelled", "execution interrupted")
		case "pending":
			m.outcome(r, "cancelled", "daemon restarted")
		}
	}
	if err = m.save(); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *Manager) save() error {
	if m.fatal != nil {
		return m.fatal
	}
	m.trimAudit()
	if err := m.write(); err != nil {
		m.fatal = fmt.Errorf("permission state unavailable until restart: %w", err)
		return m.fatal
	}
	return nil
}
func (m *Manager) write() error {
	body, err := json.Marshal(m.state)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(m.dir, ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err = json.NewEncoder(tmp).Encode(record{1, body, hash(body)}); err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(tmp.Name(), filepath.Join(m.dir, "state.json")); err != nil {
		return err
	}
	parent, err := os.Open(m.dir)
	if err != nil {
		return err
	}
	defer parent.Close()
	return parent.Sync()
}

func (m *Manager) trimAudit() bool {
	const maxEvents = 8192 // Bounded IDs, actions and results keep JSON well below 16 MiB.
	before := len(m.state.Audit)
	cutoff := m.now().Add(-7 * 24 * time.Hour)
	m.state.Audit = slices.DeleteFunc(m.state.Audit, func(e Event) bool { return !e.Time.After(cutoff) })
	if len(m.state.Audit) > maxEvents {
		m.state.Audit = slices.Delete(m.state.Audit, 0, len(m.state.Audit)-maxEvents)
	}
	return before != len(m.state.Audit)
}
func (m *Manager) event(client, action, result string) {
	m.state.Audit = append(m.state.Audit, Event{m.now().UTC(), client, action, result})
}
func (m *Manager) outcome(r *Request, status, detail string) {
	r.Status = status
	r.Error = detail
	m.event(r.Client, r.Operation.Kind+" "+r.ID, status)
}
func (r Request) clone() Request {
	r.Operation.Params = maps.Clone(r.Operation.Params)
	r.Preview.Before = maps.Clone(r.Preview.Before)
	r.Preview.After = maps.Clone(r.Preview.After)
	return r
}
func (m *Manager) Snapshot(ctx context.Context) (State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fatal != nil {
		return State{}, m.fatal
	}
	changed := m.expire()
	for i := range m.state.Requests {
		r := &m.state.Requests[i]
		if r.Status == "waiting" && m.reconcile(ctx, r) {
			changed = true
		}
	}
	if m.trimAudit() {
		changed = true
	}
	if changed {
		if err := m.save(); err != nil {
			return State{}, err
		}
	}
	s := m.state
	s.Now = m.now().UTC()
	s.Clients = slices.Clone(s.Clients)
	for i := range s.Clients {
		c := &s.Clients[i]
		c.Queries = maps.Clone(c.Queries)
		c.Grants = maps.Clone(c.Grants)
	}
	s.Requests = slices.Clone(s.Requests)
	for i := range s.Requests {
		s.Requests[i] = s.Requests[i].clone()
	}
	s.Audit = slices.Clone(s.Audit)
	return s, nil
}
func (m *Manager) expire() bool {
	now := m.now()
	changed := false
	for i := range m.state.Clients {
		c := &m.state.Clients[i]
		for kind, end := range c.Grants {
			if !now.Before(end) {
				delete(c.Grants, kind)
				m.event(c.ID, "grant "+kind, "expired")
				changed = true
			}
		}
	}
	for i := range m.state.Requests {
		r := &m.state.Requests[i]
		if r.Status == "pending" && !now.Before(r.ExpiresAt) {
			m.outcome(r, "expired", "")
			changed = true
		}
	}
	return changed
}

