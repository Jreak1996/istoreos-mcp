package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type platform struct {
	before    string
	sensitive bool
	waits     bool
	result    string
	fail      bool
	calls     int
	onExecute func(string)
}

func (p *platform) Describe(ctx context.Context, op Operation) (Preview, error) {
	return Preview{Before: map[string]string{"value": p.before}, After: op.Params, Sensitive: p.sensitive, Reversible: p.waits, Summary: op.Kind}, nil
}
func (p *platform) Execute(ctx context.Context, id string, op Operation, deadline time.Time) (bool, error) {
	p.calls++
	if p.onExecute != nil {
		p.onExecute(id)
	}
	if p.fail {
		return p.waits, errors.New("private backend details")
	}
	p.result = "waiting"
	return p.waits, nil
}

func TestUnacknowledgedProtectedExecutionRecoversAcrossCrash(t *testing.T) {
	m, p, dir := setup(t)
	p.waits = true
	p.sensitive = true
	var claim []byte
	path := filepath.Join(dir, "state.json")
	p.onExecute = func(string) { var err error; claim, err = os.ReadFile(path); must(t, err) }
	r := submit(t, m)
	_, err := m.Approve(ctx, r.ID, r.Digest)
	must(t, err)
	// The crash erased the daemon's successful return, but the platform journal survived.
	must(t, os.WriteFile(path, claim, 0600))
	restarted, err := New(dir, p)
	must(t, err)
	state := snapshot(t, restarted)
	if state.Requests[0].Status != "rolledback" || p.result != "rolledback" {
		t.Fatal("lost platform recovery after crash", state.Requests[0].Status)
	}
}

func TestPartialExecutionFailureCanOnlyRollBack(t *testing.T) {
	m, p, _ := setup(t)
	p.sensitive = true
	p.waits = true
	p.fail = true
	p.result = "waiting"
	r := submit(t, m)
	_, err := m.Approve(ctx, r.ID, r.Digest)
	denied(t, err)
	denied(t, m.Confirm(ctx, r.ID))
	if snapshot(t, m).Requests[0].Status != "rolledback" {
		t.Fatal("partial change survived")
	}
}

func TestExpiredAtDurableClaimCannotExecute(t *testing.T) {
	for _, direct := range []bool{false, true} {
		m, p, dir := setup(t)
		base := time.Now()
		m.now = func() time.Time { return base }
		must(t, m.Grant("c", "mode", time.Minute))
		r := submit(t, m)
		m.now = func() time.Time {
			data, err := os.ReadFile(filepath.Join(dir, "state.json"))
			must(t, err)
			if strings.Contains(string(data), `"status":"executing"`) {
				return base.Add(6 * time.Minute)
			}
			return base
		}
		var err error
		if direct {
			_, err = m.ExecuteGranted(ctx, "c", mode())
		} else {
			_, err = m.Approve(ctx, r.ID, r.Digest)
		}
		denied(t, err)
		if p.calls != 0 {
			t.Fatal("expired operation reached the platform")
		}
	}
}
func (p *platform) Confirm(context.Context, string) error          { p.result = "kept"; return nil }
func (p *platform) Rollback(context.Context, string) error         { p.result = "rolledback"; return nil }
func (p *platform) Result(context.Context, string) (string, error) { return p.result, nil }

var ctx = context.Background()

func mode() Operation { return Operation{"mode", map[string]string{"mode": "rule"}} }
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func denied(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected denial")
	}
}
func setup(t *testing.T) (*Manager, *platform, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "control")
	p := &platform{before: "old"}
	m, err := New(dir, p)
	must(t, err)
	must(t, m.ImportClient(Client{ID: "c", Queries: map[string]bool{"system": true}, Limit: 600}))
	return m, p, dir
}
func snapshot(t *testing.T, m *Manager) State {
	t.Helper()
	s, e := m.Snapshot(ctx)
	must(t, e)
	return s
}
func submit(t *testing.T, m *Manager) Request {
	t.Helper()
	r, e := m.Submit(ctx, "c", mode())
	must(t, e)
	return r
}

func TestOneApprovalBindsStoredOperationAndExecutesOnce(t *testing.T) {
	m, p, dir := setup(t)
	op := mode()
	r, e := m.Submit(ctx, "c", op)
	must(t, e)
	op.Params["mode"] = "changed"
	r.Preview.After["mode"] = "changed"
	view := snapshot(t, m)
	if view.Requests[0].Operation.Params["mode"] != "rule" || view.Requests[0].Preview.After["mode"] != "rule" {
		t.Fatal("caller mutated pending request")
	}
	view.Requests[0].Operation.Params["mode"] = "poisoned"
	_, e = m.Approve(ctx, r.ID, "wrong")
	denied(t, e)
	p.onExecute = func(id string) {
		data, err := os.ReadFile(filepath.Join(dir, "state.json"))
		must(t, err)
		if !strings.Contains(string(data), `"status":"executing"`) {
			t.Fatal("execution began before durable claim")
		}
	}
	var group sync.WaitGroup
	successes := make(chan string, 8)
	for range 8 {
		group.Go(func() {
			if got, e := m.Approve(ctx, r.ID, r.Digest); e == nil {
				successes <- got.Status
			}
		})
	}
	group.Wait()
	close(successes)
	if len(successes) != 1 || <-successes != "done" || p.calls != 1 {
		t.Fatal("approval was replayed or lost")
	}
}

func TestChangedStateExpiryAndFailedExecutorDenyReplay(t *testing.T) {
	m, p, _ := setup(t)
	r := submit(t, m)
	p.before = "changed"
	_, e := m.Approve(ctx, r.ID, r.Digest)
	denied(t, e)
	r = submit(t, m)
	m.now = func() time.Time { return r.ExpiresAt }
	_, e = m.Approve(ctx, r.ID, r.Digest)
	denied(t, e)
	if p.calls != 0 {
		t.Fatal("stale operation executed")
	}
	m.now = time.Now
	r = submit(t, m)
	p.fail = true
	_, e = m.Approve(ctx, r.ID, r.Digest)
	denied(t, e)
	_, e = m.Approve(ctx, r.ID, r.Digest)
	denied(t, e)
	s := snapshot(t, m)
	if p.calls != 1 || s.Requests[len(s.Requests)-1].Status != "failed" {
		t.Fatal("executor failure was replayed or lost")
	}
}

func TestQueryLimitsGrantsPauseAndRevocation(t *testing.T) {
	m, p, _ := setup(t)
	must(t, m.SetLimit("c", 2))
	must(t, m.CheckClient("c"))
	must(t, m.AuthorizeQuery("c", "discovery"))
	must(t, m.AuthorizeQuery("c", "system"))
	denied(t, m.AuthorizeQuery("c", "system"))
	denied(t, m.SetLimit("c", 601))
	must(t, m.SetLimit("c", 600))
	must(t, m.SetQuery("c", "system", false))
	denied(t, m.AuthorizeQuery("c", "system"))
	denied(t, m.SetQuery("c", "unknown", true))
	denied(t, m.Grant("c", "restart", time.Minute))
	denied(t, m.Grant("c", "mode", time.Second))
	must(t, m.Grant("c", "mode", 15*time.Minute))
	expiry := snapshot(t, m).Clients[0].Grants["mode"]
	r, e := m.ExecuteGranted(ctx, "c", mode())
	must(t, e)
	if r.Status != "done" || !snapshot(t, m).Clients[0].Grants["mode"].Equal(expiry) {
		t.Fatal("direct execution renewed grant or failed")
	}
	p.sensitive = true
	_, e = m.ExecuteGranted(ctx, "c", mode())
	denied(t, e)
	if p.calls != 1 {
		t.Fatal("sensitive grant executed")
	}
	pending := submit(t, m)
	must(t, m.Pause())
	must(t, m.AuthorizeQuery("c", "discovery"))
	denied(t, m.Grant("c", "mode", time.Minute))
	must(t, m.Resume())
	_, e = m.Approve(ctx, pending.ID, pending.Digest)
	denied(t, e)
	_, e = m.ExecuteGranted(ctx, "c", mode())
	denied(t, e)
	must(t, m.Revoke("c"))
	must(t, m.ImportClient(Client{ID: "c"}))
	denied(t, m.CheckClient("c"))
	denied(t, m.AuthorizeQuery("c", "discovery"))
}

func TestRecoverySurvivesRestartPauseAndRevoke(t *testing.T) {
	m, p, dir := setup(t)
	p.waits = true
	p.sensitive = true
	r := submit(t, m)
	waiting, e := m.Approve(ctx, r.ID, r.Digest)
	must(t, e)
	if waiting.Status != "waiting" || waiting.ConfirmDeadline.Sub(time.Now()) < 115*time.Second {
		t.Fatal("missing recovery deadline")
	}
	pending := submit(t, m)
	must(t, m.Grant("c", "mode", time.Minute))
	restarted, e := New(dir, p)
	must(t, e)
	s := snapshot(t, restarted)
	if len(s.Clients[0].Grants) > 0 || s.Requests[1].Status != "cancelled" {
		t.Fatal("restart retained transient authority")
	}
	_, e = restarted.Approve(ctx, pending.ID, pending.Digest)
	denied(t, e)
	must(t, restarted.Pause())
	must(t, restarted.Revoke("c"))
	p.result = "unknown"
	if snapshot(t, restarted).Requests[0].Status != "waiting" {
		t.Fatal("unknown recovery outcome marked terminal")
	}
	p.result = "rolledback"
	if snapshot(t, restarted).Requests[0].Status != "rolledback" {
		t.Fatal("rollback lost")
	}
}

func TestConfirmAndDeadlineRollback(t *testing.T) {
	for _, late := range []bool{false, true} {
		m, p, _ := setup(t)
		p.waits = true
		p.sensitive = true
		r := submit(t, m)
		waiting, e := m.Approve(ctx, r.ID, r.Digest)
		must(t, e)
		if late {
			m.now = func() time.Time { return waiting.ConfirmDeadline }
			denied(t, m.Confirm(ctx, r.ID))
			if snapshot(t, m).Requests[0].Status != "rolledback" {
				t.Fatal("overdue change survived")
			}
		} else {
			must(t, m.Confirm(ctx, r.ID))
			if snapshot(t, m).Requests[0].Status != "kept" {
				t.Fatal("confirmation lost")
			}
		}
	}
}

func TestPendingCapacityValidationAndFailClosedPersistence(t *testing.T) {
	m, p, dir := setup(t)
	denied(t, m.ImportClient(Client{ID: "bad/id"}))
	_, e := m.Submit(ctx, "c", Operation{"exec", map[string]string{"password": "secret"}})
	denied(t, e)
	_, e = m.Submit(ctx, "c", Operation{"mode", map[string]string{"mode": "rule", "password": "secret"}})
	denied(t, e)
	_, e = m.Submit(ctx, "c", Operation{"mode", map[string]string{"mode": strings.Repeat("a", 2049)}})
	denied(t, e)
	for range 10 {
		submit(t, m)
	}
	_, e = m.Submit(ctx, "c", mode())
	denied(t, e)
	r := snapshot(t, m).Requests[0]
	must(t, m.Reject(r.ID))
	r = submit(t, m)
	state := filepath.Join(dir, "state.json")
	if fi, e := os.Stat(state); e != nil || fi.Mode().Perm() != 0600 {
		t.Fatal("unprotected state", e)
	}
	if fi, e := os.Stat(dir); e != nil || fi.Mode().Perm() != 0700 {
		t.Fatal("unprotected dir", e)
	}
	must(t, os.Rename(state, state+".old"))
	must(t, os.Mkdir(state, 0700))
	_, e = m.Approve(ctx, r.ID, r.Digest)
	denied(t, e)
	if p.calls != 0 {
		t.Fatal("executed after persistence failed")
	}
	denied(t, m.CheckClient("c"))
	must(t, os.Remove(state))
	must(t, os.WriteFile(state, []byte("{}"), 0600))
	_, e = New(dir, p)
	denied(t, e)
}

func TestLostStateCannotReenableRevokedClient(t *testing.T) {
	m, p, dir := setup(t)
	must(t, m.Revoke("c"))
	must(t, os.Remove(filepath.Join(dir, "state.json")))
	_, err := New(dir, p)
	denied(t, err)
}

func TestMalformedOperationNeverReachesStorage(t *testing.T) {
	m, _, _ := setup(t)
	for _, op := range []Operation{
		{"node", map[string]string{"group": "test"}},
		{"mode", map[string]string{"mode": ""}},
		{"mode", map[string]string{"mode": "global\n"}},
	} {
		_, err := m.Submit(ctx, "c", op)
		denied(t, err)
	}
	if len(snapshot(t, m).Requests) != 0 {
		t.Fatal("invalid operation stored")
	}
}

func TestRevokeGrantIsScopedAndPreservesRecovery(t *testing.T) {
	m, p, _ := setup(t)
	must(t, m.Grant("c", "node", time.Minute))
	must(t, m.Grant("c", "mode", time.Minute))
	p.sensitive = true
	p.waits = true
	r := submit(t, m)
	_, err := m.Approve(ctx, r.ID, r.Digest)
	must(t, err)
	denied(t, m.RevokeGrant("c", "restart"))
	must(t, m.RevokeGrant("c", "mode"))
	p.sensitive = false
	p.waits = false
	_, err = m.ExecuteGranted(ctx, "c", mode())
	denied(t, err)
	_, err = m.ExecuteGranted(ctx, "c", Operation{"node", map[string]string{"group": "test", "node": "B"}})
	must(t, err)
	if snapshot(t, m).Requests[0].Status != "waiting" {
		t.Fatal("recovery lost when grant revoked")
	}
	must(t, m.Rollback(ctx, r.ID))
}

