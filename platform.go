package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/GlassOnTin/openwrt-mcp/control"
	"golang.org/x/sys/unix"
)

// RouterExecutor owns only bounded router actions. Management APIs never accept shell commands.
type RouterExecutor struct {
	ConfigDir string
	StateDir  string
	BootID    string
	Reload    func(context.Context, []string) error
}

var safeIdentifier = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,80}$`)
var safeUCIName = regexp.MustCompile(`^[a-zA-Z0-9_]{1,48}$`)

func newRouterExecutor(dir string) *RouterExecutor {
	boot, _ := os.ReadFile("/proc/sys/kernel/random/boot_id")
	return &RouterExecutor{ConfigDir: "/etc/config", StateDir: dir, BootID: strings.TrimSpace(string(boot))}
}
func (e *RouterExecutor) uci(ctx context.Context, args ...string) (string, error) {
	return e.uciAt(ctx, e.ConfigDir, "", args...)
}
func (e *RouterExecutor) uciAt(ctx context.Context, configDir, deltaDir string, args ...string) (string, error) {
	argv := []string{"uci", "-c", configDir}
	if deltaDir != "" {
		argv = append(argv, "-t", deltaDir)
	}
	out, err := run(ctx, 10*time.Second, append(argv, args...)...)
	if err != nil {
		return "", fmt.Errorf("UCI operation failed: %s", args[0])
	}
	return strings.TrimSpace(out), nil
}
func (e *RouterExecutor) controller(ctx context.Context, method, endpoint string, body any) (map[string]any, error) {
	port, err := e.uci(ctx, "-q", "get", "openclash.config.cn_port")
	if err != nil {
		return nil, errors.New("OpenClash controller port is unavailable")
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 {
		return nil, errors.New("invalid OpenClash controller port")
	}
	secret, _ := e.uci(ctx, "-q", "get", "openclash.config.dashboard_password")
	var data io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		data = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://127.0.0.1:"+port+endpoint, data)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	cl := &http.Client{Timeout: 8 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := cl.Do(req)
	if err != nil {
		return nil, errors.New("cannot reach local OpenClash controller")
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("OpenClash controller returned HTTP %d", res.StatusCode)
	}
	out := map[string]any{}
	if res.StatusCode == 204 {
		return out, nil
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return out, nil
	}
	if err = json.Unmarshal(b, &out); err != nil {
		return nil, errors.New("invalid controller response")
	}
	return out, nil
}
func (e *RouterExecutor) OpenClash(ctx context.Context) (map[string]any, error) {
	cfg, err := e.controller(ctx, "GET", "/configs", nil)
	if err != nil {
		return nil, err
	}
	proxies, err := e.controller(ctx, "GET", "/proxies", nil)
	if err != nil {
		return nil, err
	}
	groups := map[string]any{}
	if all, ok := proxies["proxies"].(map[string]any); ok {
		for name, value := range all {
			p, ok := value.(map[string]any)
			if !ok {
				continue
			}
			if _, ok = p["all"].([]any); ok {
				groups[name] = map[string]any{"type": p["type"], "now": p["now"], "all": p["all"]}
			}
		}
	}
	return map[string]any{"mode": cfg["mode"], "groups": groups}, nil
}
func exactParams(op control.Operation, required ...string) error {
	if len(op.Params) != len(required) {
		return errors.New("unexpected or missing parameters")
	}
	for _, key := range required {
		if op.Params[key] == "" || len(op.Params[key]) > 2048 || strings.ContainsAny(op.Params[key], "\x00\r\n") {
			return fmt.Errorf("invalid parameter %s", key)
		}
	}
	return nil
}
func (e *RouterExecutor) validateConfig(ctx context.Context, key, value string) error {
	parts := strings.Split(key, ".")
	if len(parts) != 3 {
		return errors.New("expected config.section.option")
	}
	for _, p := range parts {
		if !safeUCIName.MatchString(p) {
			return errors.New("invalid UCI identifier")
		}
	}
	switch {
	case key == "network.lan.ipaddr":
		ip := net.ParseIP(value)
		if ip == nil || ip.To4() == nil || ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() {
			return errors.New("LAN address must be a unicast IPv4 address")
		}
	case key == "network.lan.netmask":
		ip := net.ParseIP(value)
		if ip == nil || ip.To4() == nil {
			return errors.New("invalid netmask")
		}
		ones, bits := net.IPMask(ip.To4()).Size()
		if bits != 32 || ones < 1 || ones > 30 {
			return errors.New("netmask must be /1 to /30")
		}
	case key == "network.wan.dns":
		if len(strings.Fields(value)) < 1 || len(strings.Fields(value)) > 4 {
			return errors.New("provide 1 to 4 DNS addresses")
		}
		for _, v := range strings.Fields(value) {
			if net.ParseIP(v) == nil {
				return errors.New("DNS entries must be IP addresses")
			}
		}
	case key == "network.wan.peerdns":
		if value != "0" && value != "1" {
			return errors.New("peerdns must be 0 or 1")
		}
	case key == "uhttpd.main.listen_http" || key == "uhttpd.main.listen_https":
		addresses := strings.Fields(value)
		if len(addresses) == 0 || len(addresses) > 4 {
			return errors.New("provide 1 to 4 listen addresses")
		}
		for _, v := range addresses {
			host, port, err := net.SplitHostPort(v)
			if err != nil || net.ParseIP(host) == nil {
				return errors.New("listen address must be an IP and port")
			}
			p, err := strconv.Atoi(port)
			if err != nil || p < 1 || p > 65535 {
				return errors.New("invalid listen port")
			}
		}
	case parts[0] == "firewall" && (parts[2] == "src_ip" || parts[2] == "dest_port" || parts[2] == "target" || parts[2] == "enabled"):
		typ, err := e.uci(ctx, "-q", "get", "firewall."+parts[1])
		if err != nil || typ != "rule" {
			return errors.New("only existing named firewall rules can be edited")
		}
		src, _ := e.uci(ctx, "-q", "get", "firewall."+parts[1]+".src")
		if src != "lan" {
			return errors.New("only LAN management rules can be edited")
		}
		dest, _ := e.uci(ctx, "-q", "get", "firewall."+parts[1]+".dest")
		proto, _ := e.uci(ctx, "-q", "get", "firewall."+parts[1]+".proto")
		if dest != "" || (proto != "tcp" && proto != "tcp udp" && proto != "udp tcp") {
			return errors.New("only router-input LAN management rules are supported")
		}
		if parts[2] == "target" && !contains([]string{"ACCEPT", "REJECT", "DROP"}, value) {
			return errors.New("invalid firewall target")
		}
		if parts[2] == "enabled" && value != "0" && value != "1" {
			return errors.New("enabled must be 0 or 1")
		}
		if parts[2] == "src_ip" {
			entries := strings.Fields(value)
			if len(entries) == 0 {
				return errors.New("source IP list cannot be empty")
			}
			for _, ip := range entries {
				if net.ParseIP(ip) == nil {
					if _, _, err := net.ParseCIDR(ip); err != nil {
						return errors.New("invalid source IP or CIDR")
					}
				}
			}
		}
		if parts[2] == "dest_port" {
			ports := strings.Fields(value)
			if len(ports) == 0 {
				return errors.New("management port list cannot be empty")
			}
			for _, port := range ports {
				n, err := strconv.Atoi(port)
				if err != nil || (n != 22 && n != 80 && n != 443 && n != 1443 && n != 8730) {
					return errors.New("only known router-management TCP ports can be used")
				}
			}
		}
	default:
		return errors.New("this configuration field is not supported; arbitrary UCI writes are prohibited")
	}
	return nil
}
func (e *RouterExecutor) Describe(ctx context.Context, op control.Operation) (control.Preview, error) {
	p := control.Preview{Before: map[string]string{}, After: map[string]string{}}
	switch op.Kind {
	case "node":
		if err := exactParams(op, "group", "node"); err != nil {
			return p, err
		}
		out, err := e.controller(ctx, "GET", "/proxies", nil)
		if err != nil {
			return p, err
		}
		all, ok := out["proxies"].(map[string]any)
		if !ok {
			return p, errors.New("proxy catalog unavailable")
		}
		group, ok := all[op.Params["group"]].(map[string]any)
		if !ok || group["type"] != "Selector" {
			return p, errors.New("group must be an existing selectable group")
		}
		targets, _ := group["all"].([]any)
		found := false
		for _, v := range targets {
			if v == op.Params["node"] {
				found = true
			}
		}
		if !found {
			return p, errors.New("node is not in the selected group")
		}
		p.Before["node"], _ = group["now"].(string)
		p.After["node"] = op.Params["node"]
		p.Summary = "切换 OpenClash 节点"
	case "mode":
		if err := exactParams(op, "mode"); err != nil {
			return p, err
		}
		if !contains([]string{"rule", "global", "direct"}, op.Params["mode"]) {
			return p, errors.New("unsupported proxy mode")
		}
		out, err := e.controller(ctx, "GET", "/configs", nil)
		if err != nil {
			return p, err
		}
		p.Before["mode"], _ = out["mode"].(string)
		p.After["mode"] = op.Params["mode"]
		p.Summary = "切换 OpenClash 代理模式"
	case "restart":
		if len(op.Params) != 0 {
			return p, errors.New("restart takes no parameters")
		}
		if _, err := os.Stat("/etc/init.d/openclash"); err != nil {
			return p, errors.New("OpenClash service is unavailable")
		}
		p.Before["service"] = "当前 OpenClash 服务"
		p.After["service"] = "重启服务，现有连接可能中断"
		p.Sensitive = true
		p.Summary = "重启 OpenClash 服务（不可回滚的服务动作）"
	case "config_set":
		if err := exactParams(op, "key", "value"); err != nil {
			return p, err
		}
		key, value := op.Params["key"], op.Params["value"]
		if err := e.validateConfig(ctx, key, value); err != nil {
			return p, err
		}
		before, err := e.uci(ctx, "-q", "get", key)
		if err != nil {
			return p, errors.New("configuration field does not exist")
		}
		p.Before[key] = before
		p.After[key] = value
		p.Sensitive = true
		p.Reversible = true
		p.Summary = "修改路由器配置：" + key
	case "dhcp_lease":
		if err := exactParams(op, "section", "ip", "mac", "hostname"); err != nil {
			return p, err
		}
		if !safeUCIName.MatchString(op.Params["section"]) || !strings.HasPrefix(op.Params["section"], "mcp_") {
			return p, errors.New("DHCP lease section must start with mcp_")
		}
		ip := net.ParseIP(op.Params["ip"])
		if ip == nil || ip.To4() == nil || ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() {
			return p, errors.New("invalid lease IPv4")
		}
		mac, err := net.ParseMAC(op.Params["mac"])
		if err != nil || len(mac) != 6 {
			return p, errors.New("invalid MAC address")
		}
		if !regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9-]{0,62}$`).MatchString(op.Params["hostname"]) {
			return p, errors.New("invalid hostname")
		}
		prefix := "dhcp." + op.Params["section"]
		typ, err := e.uci(ctx, "-q", "get", prefix)
		if err == nil && typ != "host" {
			return p, errors.New("target section is not a DHCP host")
		}
		p.Before["section"] = typ
		for _, k := range []string{"ip", "mac", "hostname"} {
			opt := k
			if k == "hostname" {
				opt = "name"
			}
			p.Before[k], _ = e.uci(ctx, "-q", "get", prefix+"."+opt)
			p.After[k] = op.Params[k]
		}
		p.Sensitive = true
		p.Reversible = true
		p.Summary = "设置 DHCP 静态租约"
	default:
		return p, errors.New("unknown operation kind")
	}
	return p, nil
}

type recoveryRecord struct {
	ID           string            `json:"id"`
	BootID       string            `json:"boot_id"`
	Deadline     time.Time         `json:"deadline"`
	BootDeadline float64           `json:"boot_deadline,omitempty"`
	Backups      map[string][]byte `json:"backups,omitempty"`
	Expected     map[string]string `json:"expected,omitempty"`
	Original     map[string]string `json:"original,omitempty"`
	Ready        bool              `json:"ready,omitempty"`
	Kind         string            `json:"kind,omitempty"`
	Section      string            `json:"section,omitempty"`
	Key          string            `json:"key,omitempty"`
	Status       string            `json:"status"`
	Error        string            `json:"error,omitempty"`
}

func bootSeconds() float64 {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return 0
	}
	n, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}
func recoveryExpired(r recoveryRecord) bool {
	if !time.Now().Before(r.Deadline) {
		return true
	}
	return r.BootDeadline > 0 && bootSeconds() >= r.BootDeadline
}

func atomicWrite(file string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(file), ".mcp-state-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(name, file); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(file))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func (e *RouterExecutor) recordPath(id string) (string, error) {
	if !safeIdentifier.MatchString(id) {
		return "", errors.New("invalid request identifier")
	}
	return filepath.Join(e.StateDir, "recovery", id+".json"), nil
}
func (e *RouterExecutor) lockRecovery() (func(), error) {
	if err := os.MkdirAll(e.StateDir, 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(e.StateDir, "recovery.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { unix.Flock(int(f.Fd()), unix.LOCK_UN); f.Close() }, nil
}
func (e *RouterExecutor) saveRecord(r recoveryRecord) error {
	p, err := e.recordPath(r.ID)
	if err != nil {
		return err
	}
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return atomicWrite(p, data)
}
func (e *RouterExecutor) readRecord(id string) (recoveryRecord, error) {
	var r recoveryRecord
	p, err := e.recordPath(id)
	if err != nil {
		return r, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return r, err
	}
	if err = json.Unmarshal(b, &r); err != nil {
		return r, err
	}
	if r.ID != id || r.BootID == "" || r.Deadline.IsZero() || (r.Status != "waiting" && r.Status != "kept" && r.Status != "rolledback") {
		return r, errors.New("invalid recovery record")
	}
	if r.Status == "waiting" && len(r.Backups) == 0 {
		return r, errors.New("waiting recovery record has no backups")
	}
	return r, nil
}
func (e *RouterExecutor) armRecovery(id string, configs []string, deadline time.Time, state ...any) error {
	files, err := os.ReadDir(filepath.Join(e.StateDir, "recovery"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, f := range files {
		if strings.HasSuffix(f.Name(), ".json") {
			old, err := e.readRecord(strings.TrimSuffix(f.Name(), ".json"))
			if err != nil {
				return err
			}
			if old.Status == "waiting" {
				return errors.New("another protected change is awaiting confirmation")
			}
		}
	}
	var original, expected map[string]string
	if len(state) > 0 {
		if v, ok := state[0].(map[string]string); ok {
			original = maps.Clone(v)
		}
	}
	if len(state) > 1 {
		if v, ok := state[1].(map[string]string); ok {
			expected = maps.Clone(v)
		}
	}
	bootDeadline := 0.0
	if now := bootSeconds(); now > 0 {
		bootDeadline = now + deadline.Sub(time.Now()).Seconds()
	}
	r := recoveryRecord{ID: id, BootID: e.BootID, Deadline: deadline, BootDeadline: bootDeadline, Backups: map[string][]byte{}, Original: original, Expected: expected, Status: "waiting"}
	if len(state) > 2 {
		if op, ok := state[2].(control.Operation); ok {
			r.Kind, r.Key, r.Section = op.Kind, op.Params["key"], op.Params["section"]
		}
	}
	for _, name := range configs {
		if !contains([]string{"network", "firewall", "uhttpd", "dhcp"}, name) {
			return errors.New("unsupported snapshot config")
		}
		data, err := os.ReadFile(filepath.Join(e.ConfigDir, name))
		if err != nil {
			return err
		}
		r.Backups[name] = data
	}
	return e.saveRecord(r)
}
func (e *RouterExecutor) reload(ctx context.Context, configs []string) error {
	if e.Reload != nil {
		return e.Reload(ctx, configs)
	}
	for _, config := range configs {
		script, action := "", "reload"
		switch config {
		case "network":
			script = "/etc/init.d/network"
		case "firewall":
			script = "/etc/init.d/firewall"
		case "uhttpd":
			script, action = "/etc/init.d/uhttpd", "restart"
		case "dhcp":
			script = "/etc/init.d/dnsmasq"
		default:
			return errors.New("unsupported reload target")
		}
		if out, err := run(ctx, 30*time.Second, script, action); err != nil {
			return fmt.Errorf("%s reload failed: %s", config, strings.TrimSpace(out))
		}
	}
	return nil
}
func (e *RouterExecutor) restoreLocked(ctx context.Context, r *recoveryRecord) error {
	if r.Kind == "config_set" || r.Kind == "dhcp_lease" {
		if err := e.restoreScoped(ctx, r); err != nil {
			r.Error = "scoped restore requires retry"
			_ = e.saveRecord(*r)
			return err
		}
		names := make([]string, 0, len(r.Backups))
		for name := range r.Backups {
			names = append(names, name)
		}
		sort.Strings(names)
		if err := e.reload(ctx, names); err != nil {
			r.Error = "restore requires reload retry"
			_ = e.saveRecord(*r)
			return err
		}
		if err := e.verifyRuntime(ctx, names, r.Original); err != nil {
			r.Error = "restore requires runtime verification retry"
			_ = e.saveRecord(*r)
			return err
		}
		r.Status, r.Error, r.Ready, r.Backups = "rolledback", "", false, nil
		return e.saveRecord(*r)
	}
	names := []string{}
	for name, data := range r.Backups {
		if !contains([]string{"network", "firewall", "uhttpd", "dhcp"}, name) {
			return errors.New("invalid recovery config")
		}
		if err := atomicWrite(filepath.Join(e.ConfigDir, name), data); err != nil {
			return err
		}
		names = append(names, name)
	}
	sort.Strings(names)
	if err := e.reload(ctx, names); err != nil {
		r.Error = "restore requires reload retry"
		_ = e.saveRecord(*r)
		return err
	}
	if err := e.verifyRuntime(ctx, names, r.Original); err != nil {
		r.Error = "restore requires runtime verification retry"
		_ = e.saveRecord(*r)
		return err
	}
	r.Status = "rolledback"
	r.Error = ""
	r.Backups = nil
	return e.saveRecord(*r)
}

func mapsForOperation(op control.Operation, p control.Preview) (map[string]string, map[string]string) {
	if op.Kind == "config_set" {
		return maps.Clone(p.Before), maps.Clone(p.After)
	}
	if op.Kind == "dhcp_lease" {
		old, next := map[string]string{}, map[string]string{}
		section := "dhcp." + op.Params["section"] + "."
		for _, k := range []string{"ip", "mac", "hostname"} {
			field := k
			if k == "hostname" {
				field = "name"
			}
			old[section+field] = p.Before[k]
			value := op.Params[k]
			next[section+field] = value
		}
		old["dhcp."+op.Params["section"]] = p.Before["section"]
		next["dhcp."+op.Params["section"]] = "host"
		return old, next
	}
	return nil, nil
}

func (e *RouterExecutor) restoreScoped(ctx context.Context, r *recoveryRecord) error {
	config := "dhcp"
	if r.Kind == "config_set" {
		config = strings.SplitN(r.Key, ".", 2)[0]
	}
	keys := make([]string, 0, len(r.Expected))
	for key := range r.Expected {
		if key != "" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		got, err := e.uci(ctx, "-q", "get", key)
		if err != nil {
			got = ""
		}
		if strings.Join(strings.Fields(got), " ") != strings.Join(strings.Fields(r.Expected[key]), " ") {
			return fmt.Errorf("configuration field %s changed after MCP applied it; refusing to overwrite the newer value", key)
		}
	}
	base, err := os.ReadFile(filepath.Join(e.ConfigDir, config))
	if err != nil {
		return err
	}
	stage := filepath.Join(e.StateDir, "staging", r.ID+"-rollback")
	stageConfig, stageDelta := filepath.Join(stage, "config"), filepath.Join(stage, "delta")
	if err = os.MkdirAll(stageConfig, 0700); err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if err = os.MkdirAll(stageDelta, 0700); err != nil {
		return err
	}
	if err = atomicWrite(filepath.Join(stageConfig, config), base); err != nil {
		return err
	}
	uciStage := func(args ...string) error { _, e := e.uciAt(ctx, stageConfig, stageDelta, args...); return e }
	if r.Kind == "config_set" {
		if err = uciStage("delete", r.Key); err != nil {
			return err
		}
		if r.Key == "network.wan.dns" || strings.HasPrefix(r.Key, "uhttpd.main.listen_") {
			for _, value := range strings.Fields(r.Original[r.Key]) {
				if err = uciStage("add_list", r.Key+"="+value); err != nil {
					return err
				}
			}
		} else if r.Original[r.Key] != "" {
			if err = uciStage("set", r.Key+"="+r.Original[r.Key]); err != nil {
				return err
			}
		}
	} else {
		prefix := "dhcp." + r.Section
		if r.Original[prefix] == "" {
			if err = uciStage("delete", prefix); err != nil {
				return err
			}
		} else {
			for _, field := range []string{"ip", "mac", "name"} {
				key := prefix + "." + field
				if r.Original[key] == "" {
					_ = uciStage("delete", key)
				} else if err = uciStage("set", key+"="+r.Original[key]); err != nil {
					return err
				}
			}
		}
	}
	if err = uciStage("commit", config); err != nil {
		return err
	}
	updated, err := os.ReadFile(filepath.Join(stageConfig, config))
	if err != nil {
		return err
	}
	current, err := os.ReadFile(filepath.Join(e.ConfigDir, config))
	if err != nil || !bytes.Equal(current, base) {
		return errors.New("router configuration changed during rollback; retry after reviewing the current value")
	}
	return atomicWrite(filepath.Join(e.ConfigDir, config), updated)
}

func (e *RouterExecutor) verifyRuntime(ctx context.Context, configs []string, expected map[string]string) error {
	if len(expected) == 0 {
		return nil
	}
	for key, want := range expected {
		got, err := e.uci(ctx, "-q", "get", key)
		if err != nil {
			got = ""
		}
		if strings.Join(strings.Fields(got), " ") != strings.Join(strings.Fields(want), " ") {
			return fmt.Errorf("effective UCI value for %s does not match the approved value", key)
		}
	}
	if contains(configs, "network") {
		want := expected["network.lan.ipaddr"]
		if want != "" {
			deadline := time.Now().Add(20 * time.Second)
			for time.Now().Before(deadline) {
				out, err := run(ctx, 3*time.Second, "ubus", "call", "network.interface.lan", "status")
				if err == nil {
					var status struct {
						Addresses []struct {
							Address string `json:"address"`
						} `json:"ipv4-address"`
					}
					if json.Unmarshal([]byte(out), &status) == nil {
						for _, a := range status.Addresses {
							if a.Address == want {
								return nil
							}
						}
					}
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(400 * time.Millisecond):
				}
			}
			return errors.New("LAN interface did not adopt the approved IPv4 address")
		}
	}
	if contains(configs, "uhttpd") {
		var wanted []string
		for _, key := range []string{"uhttpd.main.listen_http", "uhttpd.main.listen_https"} {
			wanted = append(wanted, strings.Fields(expected[key])...)
		}
		if len(wanted) > 0 {
			deadline := time.Now().Add(15 * time.Second)
			for time.Now().Before(deadline) {
				out, err := run(ctx, 3*time.Second, "netstat", "-lnt")
				if err == nil {
					all := true
					for _, want := range wanted {
						wh, wp, _ := net.SplitHostPort(want)
						found := false
						for _, line := range strings.Split(out, "\n") {
							f := strings.Fields(line)
							if len(f) < 4 || !strings.Contains(f[0], "tcp") || !strings.HasSuffix(f[len(f)-1], "LISTEN") {
								continue
							}
							lh, lp, err := net.SplitHostPort(strings.Trim(f[3], "[]"))
							if err == nil && lp == wp && (lh == wh || ((wh == "::" || wh == "0.0.0.0") && (lh == "::" || lh == "0.0.0.0"))) {
								found = true
								break
							}
						}
						if !found {
							all = false
							break
						}
					}
					if all {
						return nil
					}
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(400 * time.Millisecond):
				}
			}
			return errors.New("uhttpd did not bind every approved management listen address")
		}
	}
	return nil
}
func (e *RouterExecutor) Execute(ctx context.Context, id string, op control.Operation, deadline time.Time) (bool, error) {
	preview, err := e.Describe(ctx, op)
	if err != nil {
		return false, err
	}
	return e.ExecuteApproved(ctx, id, op, preview, deadline)
}
func (e *RouterExecutor) ExecuteApproved(ctx context.Context, id string, op control.Operation, approved control.Preview, deadline time.Time) (bool, error) {
	current, err := e.Describe(ctx, op)
	if err != nil {
		return false, err
	}
	if !reflect.DeepEqual(current, approved) {
		return false, errors.New("router state changed after approval; submit a new request")
	}
	switch op.Kind {
	case "node":
		_, err := e.controller(ctx, "PUT", "/proxies/"+url.PathEscape(op.Params["group"]), map[string]string{"name": op.Params["node"]})
		return false, err
	case "mode":
		_, err := e.controller(ctx, "PATCH", "/configs", map[string]string{"mode": op.Params["mode"]})
		return false, err
	case "restart":
		_, err := run(ctx, 45*time.Second, "/etc/init.d/openclash", "restart")
		if err != nil {
			return false, errors.New("OpenClash restart failed; inspect service logs")
		}
		return false, nil
	}
	unlock, err := e.lockRecovery()
	if err != nil {
		return false, err
	}
	defer unlock()
	if !time.Now().Before(deadline) {
		return false, errors.New("authorization expired before platform write")
	}
	current, err = e.Describe(ctx, op)
	if err != nil {
		return false, err
	}
	if !reflect.DeepEqual(current, approved) {
		return false, errors.New("router state changed while waiting for recovery lock")
	}
	changes, err := e.uci(ctx, "changes")
	if err != nil {
		return false, err
	}
	if changes != "" {
		return false, errors.New("uncommitted UCI changes exist; save or revert them first")
	}
	config := "dhcp"
	if op.Kind == "config_set" {
		config = strings.Split(op.Params["key"], ".")[0]
	}
	original, expected := mapsForOperation(op, approved)
	if config == "network" {
		oldIP, readErr := e.uci(ctx, "-q", "get", "network.lan.ipaddr")
		if readErr == nil {
			original["network.lan.ipaddr"] = oldIP
			if op.Params["key"] == "network.lan.ipaddr" {
				expected["network.lan.ipaddr"] = op.Params["value"]
			} else {
				expected["network.lan.ipaddr"] = oldIP
			}
		}
	}
	if err = e.armRecovery(id, []string{config}, deadline, original, expected, op); err != nil {
		return false, err
	}
	recovery, err := e.readRecord(id)
	if err != nil {
		return true, err
	}
	fail := func(cause error) (bool, error) {
		r, readErr := e.readRecord(id)
		if readErr != nil {
			return true, errors.Join(cause, readErr)
		}
		live, liveErr := os.ReadFile(filepath.Join(e.ConfigDir, config))
		if liveErr == nil && bytes.Equal(live, r.Backups[config]) {
			r.Status, r.Error, r.Backups = "rolledback", "", nil
			return true, errors.Join(cause, e.saveRecord(r))
		}
		restoreErr := e.restoreLocked(ctx, &r)
		return true, errors.Join(cause, restoreErr)
	}
	stage := filepath.Join(e.StateDir, "staging", id)
	stageConfig, stageDelta := filepath.Join(stage, "config"), filepath.Join(stage, "delta")
	if err = os.MkdirAll(stageConfig, 0700); err != nil {
		return fail(err)
	}
	if err = os.MkdirAll(stageDelta, 0700); err != nil {
		return fail(err)
	}
	baseBytes, err := os.ReadFile(filepath.Join(e.ConfigDir, config))
	if err != nil {
		return fail(err)
	}
	if err = atomicWrite(filepath.Join(stageConfig, config), baseBytes); err != nil {
		return fail(err)
	}
	uciStage := func(args ...string) (string, error) { return e.uciAt(ctx, stageConfig, stageDelta, args...) }
	if op.Kind == "config_set" {
		key, value := op.Params["key"], op.Params["value"]
		if strings.HasPrefix(key, "uhttpd.") || key == "network.wan.dns" {
			if _, err = uciStage("delete", key); err != nil {
				return fail(err)
			}
			for _, v := range strings.Fields(value) {
				if _, err = uciStage("add_list", key+"="+v); err != nil {
					return fail(err)
				}
			}
		} else if _, err = uciStage("set", key+"="+value); err != nil {
			return fail(err)
		}
	} else {
		prefix := "dhcp." + op.Params["section"]
		for _, kv := range []string{prefix + "=host", prefix + ".ip=" + op.Params["ip"], prefix + ".mac=" + op.Params["mac"], prefix + ".name=" + op.Params["hostname"]} {
			if _, err = uciStage("set", kv); err != nil {
				return fail(err)
			}
		}
	}
	if _, err = uciStage("commit", config); err != nil {
		return fail(err)
	}
	staged, err := os.ReadFile(filepath.Join(stageConfig, config))
	if err != nil {
		return fail(err)
	}
	if !bytes.Equal(baseBytes, recovery.Backups[config]) {
		return fail(errors.New("config changed before staging"))
	}
	currentBytes, err := os.ReadFile(filepath.Join(e.ConfigDir, config))
	if err != nil || !bytes.Equal(currentBytes, recovery.Backups[config]) {
		return fail(errors.New("router configuration changed before write; no changes applied"))
	}
	if err = atomicWrite(filepath.Join(e.ConfigDir, config), staged); err != nil {
		return fail(err)
	}
	if err = e.reload(ctx, []string{config}); err != nil {
		return fail(err)
	}
	r, err := e.readRecord(id)
	if err != nil {
		return true, err
	}
	if err = e.verifyRuntime(ctx, []string{config}, r.Expected); err != nil {
		r.Error = "new configuration requires runtime verification retry"
		_ = e.saveRecord(r)
		return true, err
	}
	r.Ready, r.Error = true, ""
	if err = e.saveRecord(r); err != nil {
		return true, err
	}
	_ = os.RemoveAll(stage)
	return true, nil
}
func (e *RouterExecutor) Confirm(ctx context.Context, id string) error {
	unlock, err := e.lockRecovery()
	if err != nil {
		return err
	}
	defer unlock()
	r, err := e.readRecord(id)
	if err != nil {
		return err
	}
	if r.Status != "waiting" || !r.Ready || recoveryExpired(r) || r.BootID != e.BootID {
		return errors.New("confirmation expired, recovered, or boot identity changed")
	}
	names := make([]string, 0, len(r.Backups))
	for name := range r.Backups {
		names = append(names, name)
	}
	if err := e.verifyRuntime(ctx, names, r.Expected); err != nil {
		return err
	}
	r.Status = "kept"
	r.Backups = nil
	return e.saveRecord(r)
}
func (e *RouterExecutor) Rollback(ctx context.Context, id string) error {
	unlock, err := e.lockRecovery()
	if err != nil {
		return err
	}
	defer unlock()
	r, err := e.readRecord(id)
	if err != nil {
		return err
	}
	if r.Status != "waiting" {
		return errors.New("no unconfirmed change for this request")
	}
	return e.restoreLocked(ctx, &r)
}
func (e *RouterExecutor) Result(ctx context.Context, id string) (string, error) {
	r, err := e.readRecord(id)
	return r.Status, err
}
func (e *RouterExecutor) Recover(ctx context.Context, startup bool) error {
	unlock, err := e.lockRecovery()
	if err != nil {
		return err
	}
	defer unlock()
	files, err := os.ReadDir(filepath.Join(e.StateDir, "recovery"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var problems []error
	for _, f := range files {
		if !strings.HasSuffix(f.Name(), ".json") {
			continue
		}
		r, err := e.readRecord(strings.TrimSuffix(f.Name(), ".json"))
		if err != nil {
			problems = append(problems, err)
			continue
		}
		if r.Status == "waiting" && (startup || recoveryExpired(r) || r.BootID != e.BootID) {
			unchanged := true
			for name, before := range r.Backups {
				current, readErr := os.ReadFile(filepath.Join(e.ConfigDir, name))
				if readErr != nil || !bytes.Equal(current, before) {
					unchanged = false
					break
				}
			}
			if unchanged {
				r.Status, r.Backups, r.Error = "rolledback", nil, ""
				err = e.saveRecord(r)
			} else {
				err = e.restoreLocked(ctx, &r)
			}
			if err != nil {
				problems = append(problems, err)
			}
		}
	}
	return errors.Join(problems...)
}

