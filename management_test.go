package main

import "testing"

func TestManagedRawToolsCannotBypassApproval(t *testing.T) {
	cases := []struct{ tool, scope, want string }{
		{"ubus_call", "system.info", "system"}, {"ubus_call", "network.interface.wan.status", "network"},
		{"ubus_call", "luci-rpc.getDHCPLeases", "devices"}, {"logread", "", "logs"},
		{"ubus_call", "system.reboot", ""}, {"ubus_call", "network.interface.lan.down", ""},
		{"ubus_call", "uci.set", ""}, {"ubus_call", "file.exec", ""}, {"ubus_call", "session.login", ""},
		{"exec", "uci", ""}, {"uci_apply", "network.lan.ipaddr", ""}, {"uci_confirm", "", ""}, {"wg_new_client", "", ""},
		{"uci_get", "network", ""}, {"ubus_call", "unknown.get", ""},
	}
	for _, c := range cases {
		if got := managedReadGroup(c.tool, []string{c.scope}); got != c.want {
			t.Errorf("%s %s group=%q want %q", c.tool, c.scope, got, c.want)
		}
	}
}
func TestManagedLogsScrubURLsAndCredentials(t *testing.T) {
	got := sanitizeManagedText("notice password=private-value\nsubscription https://example.invalid/token/secret\nnetwork up")
	if got == "" || containsText(got, "private-value") || containsText(got, "example.invalid") {
		t.Fatal("managed text exposes secret")
	}
}
func containsText(s, needle string) bool {
	for i := 0; i+len(needle) <= len(s); i++ {
		if s[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

