package control

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// Value semantics and router-specific allowlists are checked by Describe.
// Keep the persistent operation format closed to credentials and extra fields.
var parameterNames = map[string][]string{
	"node": {"group", "node"}, "mode": {"mode"}, "restart": {},
	"config_set": {"key", "value"}, "dhcp_lease": {"section", "ip", "mac", "hostname"},
}

func validate(op Operation) error {
	names, ok := parameterNames[op.Kind]
	if !ok || len(op.Params) > 16 || len(op.Params) != len(names) {
		return errors.New("invalid operation")
	}
	for k, v := range op.Params {
		if len(k) > 128 || v == "" || len(v) > 2048 || !utf8.ValidString(v) || strings.ContainsAny(v, "\x00\r\n") || !slices.Contains(names, k) {
			return errors.New("invalid operation parameter")
		}
	}
	return nil
}
func digest(r *Request) string {
	// All fields have a total JSON representation. encoding/json sorts map keys.
	body, _ := json.Marshal(struct {
		ID, Client           string
		CreatedAt, ExpiresAt time.Time
		Operation            Operation
		Preview              Preview
	}{r.ID, r.Client, r.CreatedAt, r.ExpiresAt, r.Operation, r.Preview})
	return hash(body)
}

func (m *Manager) find(id string) (*Request, error) {
	if m.fatal != nil {
		return nil, m.fatal
	}
	for i := range m.state.Requests {
		if m.state.Requests[i].ID == id {
			return &m.state.Requests[i], nil
		}
	}
	return nil, errors.New("unknown request")
}
func (m *Manager) room(id string, pending bool) error {
	if pending {
		count := 0
		for _, r := range m.state.Requests {
			if r.Status == "pending" && r.Client == id {
				count++
			}
		}
		if count >= 10 {
			return errors.New("client already has ten pending requests")
		}
	}
	if len(m.state.Requests) < 256 {
		return nil
	}
	idx := slices.IndexFunc(m.state.Requests, func(r Request) bool {
		switch r.Status {
		case "done", "kept", "rolledback", "rejected", "cancelled", "expired", "failed":
			return true
		}
		return false
	})
	if idx < 0 {
		return errors.New("request history is full of active requests")
	}
	m.state.Requests = slices.Delete(m.state.Requests, idx, idx+1)
	return nil
}
func (m *Manager) prepare(ctx context.Context, id string, op Operation, direct bool) (Request, error) {
	c, err := m.client(id)
	if err != nil {
		return Request{}, err
	}
	if m.state.Paused {
		return Request{}, errors.New("operations paused")
	}
	if err = m.rate(c); err != nil {
		return Request{}, err
	}
	if err = validate(op); err != nil {
		return Request{}, err
	}
	if m.expire() {
		if err = m.save(); err != nil {
			return Request{}, err
		}
	}
	if direct && (!ordinary(op.Kind) || !m.now().Before(c.Grants[op.Kind])) {
		return Request{}, errors.New("temporary grant required")
	}
	op.Params = maps.Clone(op.Params)
	preview, err := m.ex.Describe(ctx, op)
	if err != nil {
		return Request{}, err
	}
	if direct && (preview.Sensitive || !m.now().Before(c.Grants[op.Kind])) {
		return Request{}, errors.New("operation requires approval")
	}
	if err = ctx.Err(); err != nil {
		return Request{}, err
	}
	var idBytes [16]byte
	rand.Read(idBytes[:])
	now := m.now().UTC()
	r := Request{ID: hex.EncodeToString(idBytes[:]), Client: id, Operation: op, Preview: preview, Status: "pending", CreatedAt: now, ExpiresAt: now.Add(5 * time.Minute)}
	r = r.clone()
	r.Digest = digest(&r)
	if err = m.room(id, !direct); err != nil {
		return Request{}, err
	}
	return r, nil
}
func (m *Manager) Submit(ctx context.Context, id string, op Operation) (Request, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, err := m.prepare(ctx, id, op, false)
	if err != nil {
		return Request{}, err
	}
	m.state.Requests = append(m.state.Requests, r)
	m.event(id, r.Operation.Kind+" "+r.ID, "submitted")
	if err = m.save(); err != nil {
		return Request{}, err
	}
	return r.clone(), nil
}
func (m *Manager) ExecuteGranted(ctx context.Context, id string, op Operation) (Request, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, err := m.prepare(ctx, id, op, true)
	if err != nil {
		return Request{}, err
	}
	m.state.Requests = append(m.state.Requests, r)
	m.event(id, r.Operation.Kind+" "+r.ID, "temporary grant")
	c, _ := m.client(id) // prepare checked the client under the same lock.
	return m.execute(ctx, &m.state.Requests[len(m.state.Requests)-1], c.Grants[op.Kind])
}
func (m *Manager) Approve(ctx context.Context, id, approvedDigest string) (Request, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, err := m.find(id)
	if err != nil {
		return Request{}, err
	}
	if m.expire() {
		if err = m.save(); err != nil {
			return Request{}, err
		}
	}
	if _, err = m.client(r.Client); err != nil {
		return Request{}, err
	}
	if m.state.Paused || r.Status != "pending" {
		return Request{}, errors.New("request no longer approvable")
	}
	if r.Digest != approvedDigest || digest(r) != r.Digest {
		return Request{}, errors.New("approval digest mismatch")
	}
	current, err := m.ex.Describe(ctx, r.clone().Operation)
	if err != nil {
		return Request{}, err
	}
	check := *r
	check.Preview = current
	if digest(&check) != r.Digest {
		m.outcome(r, "cancelled", "preview changed")
		if err = m.save(); err != nil {
			return Request{}, err
		}
		return r.clone(), errors.New("preview changed; submit a new request")
	}
	if !m.now().Before(r.ExpiresAt) {
		m.outcome(r, "expired", "")
		if err = m.save(); err != nil {
			return Request{}, err
		}
		return r.clone(), errors.New("request expired")
	}
	if err = ctx.Err(); err != nil {
		return Request{}, err
	}
	m.event(r.Client, r.Operation.Kind+" "+r.ID, "approved")
	return m.execute(ctx, r, r.ExpiresAt)
}
func (m *Manager) execute(ctx context.Context, r *Request, authorizedUntil time.Time) (Request, error) {
	r.ConfirmDeadline = m.now().UTC().Add(120 * time.Second)
	m.outcome(r, "executing", "")
	if err := m.save(); err != nil {
		return Request{}, err
	}
	if !m.now().Before(authorizedUntil) {
		m.outcome(r, "expired", "authorization expired before execution")
		r.ConfirmDeadline = time.Time{}
		if err := m.save(); err != nil {
			return Request{}, err
		}
		return r.clone(), errors.New("authorization expired before execution")
	}
	var waiting bool
	var execErr error
	if approved, ok := m.ex.(interface {
		ExecuteApproved(context.Context, string, Operation, Preview, time.Time) (bool, error)
	}); ok {
		waiting, execErr = approved.ExecuteApproved(ctx, r.ID, r.clone().Operation, r.clone().Preview, r.ConfirmDeadline)
	} else {
		waiting, execErr = m.ex.Execute(ctx, r.ID, r.clone().Operation, r.ConfirmDeadline)
	}
	switch {
	case waiting && execErr != nil:
		m.requireRollback(r)
	case waiting:
		m.outcome(r, "waiting", "")
	case execErr != nil:
		m.outcome(r, "failed", "platform execution failed")
	default:
		m.outcome(r, "done", "")
	}
	if !waiting {
		r.ConfirmDeadline = time.Time{}
	}
	if err := m.save(); err != nil {
		return Request{}, err
	}
	return r.clone(), execErr
}

// An interrupted protected change may be partially applied. A zero deadline
// immediately triggers rollback and permanently excludes confirmation.
func (m *Manager) requireRollback(r *Request) {
	r.ConfirmDeadline = time.Time{}
	m.outcome(r, "waiting", "execution interrupted; rollback required")
}

func (m *Manager) Reject(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, err := m.find(id)
	if err != nil {
		return err
	}
	if m.expire() {
		if err = m.save(); err != nil {
			return err
		}
	}
	if r.Status != "pending" {
		return errors.New("request is not pending")
	}
	m.outcome(r, "rejected", "")
	return m.save()
}

func (m *Manager) recordResult(ctx context.Context, r *Request) (string, bool) {
	status, err := m.ex.Result(ctx, r.ID)
	if err == nil {
		switch status {
		case "kept", "rolledback":
			m.outcome(r, status, "")
			return status, true
		case "waiting":
			changed := r.Error != ""
			r.Error = ""
			return status, changed
		}
	}
	changed := r.Error != "recovery status unavailable"
	r.Error = "recovery status unavailable"
	return "", changed
}
func (m *Manager) reconcile(ctx context.Context, r *Request) bool {
	status, changed := m.recordResult(ctx, r)
	if status == "waiting" && !m.now().Before(r.ConfirmDeadline) {
		if err := m.ex.Rollback(ctx, r.ID); err != nil {
			changed = changed || r.Error != "rollback pending"
			r.Error = "rollback pending"
			return changed
		}
		_, updated := m.recordResult(ctx, r)
		return changed || updated
	}
	return changed
}

func (m *Manager) Confirm(ctx context.Context, id string) error  { return m.recover(ctx, id, true) }
func (m *Manager) Rollback(ctx context.Context, id string) error { return m.recover(ctx, id, false) }
func (m *Manager) recover(ctx context.Context, id string, keep bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, err := m.find(id)
	if err != nil {
		return err
	}
	if r.Status != "waiting" {
		return errors.New("request is not waiting for confirmation")
	}
	status, changed := m.recordResult(ctx, r)
	if changed {
		if err = m.save(); err != nil {
			return err
		}
	}
	if status != "waiting" {
		return errors.New("recovery status no longer permits this action")
	}
	expired := !m.now().Before(r.ConfirmDeadline)
	target := "rolledback"
	if keep && !expired {
		target = "kept"
		err = m.ex.Confirm(ctx, id)
	} else {
		err = m.ex.Rollback(ctx, id)
	}
	if err != nil {
		return err
	}
	status, _ = m.recordResult(ctx, r)
	if err = m.save(); err != nil {
		return err
	}
	if status != target {
		return errors.New("platform has not verified the recovery outcome")
	}
	if keep && expired {
		return errors.New("confirmation deadline passed")
	}
	return nil
}

