package main

import (
	"context"
	"github.com/GlassOnTin/openwrt-mcp/control"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPlatformRejectsUnboundedOperations(t *testing.T) {
	ex := &RouterExecutor{ConfigDir: t.TempDir(), StateDir: t.TempDir()}
	for _, op := range []control.Operation{
		{Kind: "exec", Params: map[string]string{"command": "sh"}},
		{Kind: "config_set", Params: map[string]string{"key": "system.main.password", "value": "secret"}},
		{Kind: "config_set", Params: map[string]string{"key": "network.lan.ipaddr", "value": "127.0.0.1"}},
		{Kind: "config_set", Params: map[string]string{"key": "../network.lan.ipaddr", "value": "192.168.2.20"}},
		{Kind: "restart", Params: map[string]string{"command": "reboot"}},
	} {
		if _, err := ex.Describe(context.Background(), op); err == nil {
			t.Errorf("accepted unsafe operation: %s", op.Kind)
		}
	}
}

func TestRecoveryRestoresActualFilesAndRetainsFailedRecords(t *testing.T) {
	root := t.TempDir()
	config := filepath.Join(root, "config")
	state := filepath.Join(root, "state")
	os.MkdirAll(config, 0700)
	os.MkdirAll(state, 0700)
	file := filepath.Join(config, "network")
	os.WriteFile(file, []byte("old configuration"), 0600)
	ex := &RouterExecutor{ConfigDir: config, StateDir: state, BootID: "boot-a", Reload: func(context.Context, []string) error { return nil }}
	if err := ex.armRecovery("request_1", []string{"network"}, time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(file, []byte("new configuration"), 0600)
	if err := ex.Recover(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(file)
	if string(got) != "old configuration" {
		t.Fatalf("not restored: %s", got)
	}
	status, err := ex.Result(context.Background(), "request_1")
	if err != nil || status != "rolledback" {
		t.Fatalf("status %q: %v", status, err)
	}
}

func TestRecoveryBootChangeAndLateConfirmation(t *testing.T) {
	root := t.TempDir()
	config := filepath.Join(root, "config")
	state := filepath.Join(root, "state")
	os.MkdirAll(config, 0700)
	os.MkdirAll(state, 0700)
	os.WriteFile(filepath.Join(config, "network"), []byte("before"), 0600)
	ex := &RouterExecutor{ConfigDir: config, StateDir: state, BootID: "old", Reload: func(context.Context, []string) error { return nil }}
	if err := ex.armRecovery("request_2", []string{"network"}, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(config, "network"), []byte("after"), 0600)
	ex.BootID = "new"
	if err := ex.Confirm(context.Background(), "request_2"); err == nil {
		t.Fatal("confirmed across boot identity")
	}
	if err := ex.Recover(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(config, "network"))
	if string(data) != "before" {
		t.Fatal("boot recovery not performed")
	}
}

