package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/GlassOnTin/openwrt-mcp/control"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func managedReadGroup(tool string, scopes []string) string {
	if tool == "ubus_list" {
		return "discovery"
	}
	if tool == "logread" {
		return "logs"
	}
	if tool != "ubus_call" || len(scopes) != 1 {
		return ""
	}
	scope := scopes[0]
	switch scope {
	case "system.board", "system.info":
		return "system"
	case "network.device.status", "network.interface.dump", "network.interface.status", "luci-rpc.getNetworkDevices":
		return "network"
	case "luci-rpc.getHostHints", "luci-rpc.getDUIDHints", "luci-rpc.getDHCPLeases", "luci-rpc.getWirelessDevices", "luci-rpc.getBoardJSON", "iwinfo.devices", "iwinfo.info", "iwinfo.assoclist", "iwinfo.freqlist", "iwinfo.txpowerlist", "iwinfo.countrylist", "iwinfo.survey", "iwinfo.phyname":
		return "devices"
	}
	if regexp.MustCompile(`^network\.interface\.[a-zA-Z0-9_-]+\.status$`).MatchString(scope) {
		return "network"
	}
	return ""
}

var managedURL = regexp.MustCompile(`https?://[^\s"'<>]+`)
var managedSecretLine = regexp.MustCompile(`(?im)[^\n]*(?:password|passwd|secret|authorization|bearer|token|subscription|订阅)[^\n]*`)

func sanitizeManagedText(s string) string {
	return managedURL.ReplaceAllString(managedSecretLine.ReplaceAllString(s, "[敏感内容已隐藏]"), "[链接已隐藏]")
}

func (s *Server) initControl(ctx context.Context) error {
	e := newRouterExecutor(s.statePath)
	if err := os.MkdirAll(s.statePath, 0700); err != nil {
		return err
	}
	if err := os.Chmod(s.statePath, 0700); err != nil {
		return err
	}
	if err := e.Recover(ctx, true); err != nil {
		return fmt.Errorf("protected recovery failed; refusing to start: %w", err)
	}
	manager, err := control.New(filepath.Join(s.statePath, "control"), e)
	if err != nil {
		return err
	}
	// Managed query events use seven-day retention and an 8 MB two-file cap, leaving
	// bounded space for the independent permission/recovery journal.
	s.audit.maxMB = 4
	s.audit.retention = 7 * 24 * time.Hour
	qfile := filepath.Join(s.statePath, "query-overrides.json")
	if b, readErr := os.ReadFile(qfile); readErr == nil {
		if err := json.Unmarshal(b, &s.queryOverrides); err != nil {
			return errors.New("query override state is invalid; refusing to start")
		}
	} else if !os.IsNotExist(readErr) {
		return readErr
	}
	if s.queryOverrides == nil {
		s.queryOverrides = map[string]map[string]bool{}
	}
	for _, id := range s.tokens.Clients() {
		queries := map[string]bool{}
		for _, group := range []string{"system", "network", "devices", "logs"} {
			queries[group] = legacyGroupConfigured(s.cfg(), id, group, time.Now())
		}
		if err := manager.ImportClient(control.Client{ID: id, Name: id, Queries: queries, Limit: 60}); err != nil {
			return fmt.Errorf("import client: %w", err)
		}
	}
	s.control, s.platform = manager, e
	return nil
}

func legacyGroupConfigured(cfg *Config, client, group string, now time.Time) bool {
	for _, p := range cfg.Policies {
		if !p.Enabled || p.Client != client || (!p.Expires.IsZero() && now.After(p.Expires)) {
			continue
		}
		for _, tool := range p.Tools {
			if group == "logs" && tool == "logread" {
				return true
			}
			if tool != "ubus_call" {
				continue
			}
			for _, scope := range p.Scopes {
				switch group {
				case "system":
					if strings.HasPrefix(scope, "system.") {
						return true
					}
				case "network":
					if strings.HasPrefix(scope, "network.") {
						return true
					}
				case "devices":
					if strings.HasPrefix(scope, "luci-rpc.") || strings.HasPrefix(scope, "iwinfo.") {
						return true
					}
				}
			}
		}
	}
	return false
}

func legacyPolicyCovers(cfg *Config, client, tool string, scopes []string, now time.Time) bool {
	for _, p := range cfg.Policies {
		if p.Enabled && p.Client == client && contains(p.Tools, tool) && (p.Expires.IsZero() || !now.After(p.Expires)) {
			if _, ok := firstUncovered(p.Scopes, scopes); ok {
				return true
			}
		}
	}
	return false
}

func (s *Server) legacyQueryAllowed(client, group, tool string, scopes []string) bool {
	s.queryMu.RLock()
	explicit := s.queryOverrides[client] != nil && s.queryOverrides[client][group]
	s.queryMu.RUnlock()
	if explicit {
		return true
	}
	if !legacyPolicyCovers(s.cfg(), client, tool, scopes, time.Now()) {
		return false
	}
	p, _ := s.cfg().AuthorisePolicy(client, tool, scopes, time.Now())
	return p != nil && s.mfaGate(p, client, tool, time.Now()) == ""
}
func (s *Server) queryIsExplicit(client, group string) bool {
	s.queryMu.RLock()
	defer s.queryMu.RUnlock()
	return s.queryOverrides[client] != nil && s.queryOverrides[client][group]
}

type operationInput struct {
	Kind   string            `json:"kind"`
	Params map[string]string `json:"params,omitempty"`
}
type requestIDInput struct {
	ID string `json:"id,omitempty"`
}
type noInput struct{}

func (s *Server) addManagedTools(srv *mcp.Server, clientID string) {
	mcp.AddTool(srv, &mcp.Tool{Name: "request_operation", Description: "Submit a bounded router operation for administrator approval in LuCI. Kinds: node {group,node}, mode {mode:rule|global|direct}, restart {}, config_set {key,value}, dhcp_lease {section:mcp_name,ip,mac,hostname}. This only submits a 5-minute request; it never grants permissions or executes the operation. Sensitive changes require exact one-time web approval."}, func(ctx context.Context, _ *mcp.CallToolRequest, in operationInput) (*mcp.CallToolResult, any, error) {
		r, err := s.control.Submit(ctx, clientID, control.Operation{Kind: in.Kind, Params: in.Params})
		return managedToolResult(r, err)
	})
	mcp.AddTool(srv, &mcp.Tool{Name: "execute_operation", Description: "Execute only an ordinary node/mode action already covered by this client's unexpired temporary grant. Never extends grant expiry. Sensitive actions and OpenClash service restart require request_operation plus explicit LuCI approval."}, func(ctx context.Context, _ *mcp.CallToolRequest, in operationInput) (*mcp.CallToolResult, any, error) {
		r, err := s.control.ExecuteGranted(ctx, clientID, control.Operation{Kind: in.Kind, Params: in.Params})
		return managedToolResult(r, err)
	})
	mcp.AddTool(srv, &mcp.Tool{Name: "operation_status", Description: "Read your own operation requests and their status. Optional id selects one. This cannot read another client's requests, approve, confirm or change grants."}, func(ctx context.Context, _ *mcp.CallToolRequest, in requestIDInput) (*mcp.CallToolResult, any, error) {
		if err := s.control.AuthorizeQuery(clientID, "discovery"); err != nil {
			return managedToolResult(nil, err)
		}
		snapshot, err := s.control.Snapshot(ctx)
		if err != nil {
			return managedToolResult(nil, err)
		}
		out := []control.Request{}
		for _, r := range snapshot.Requests {
			if r.Client == clientID && (in.ID == "" || in.ID == r.ID) {
				out = append(out, r)
			}
		}
		return managedToolResult(out, nil)
	})
	mcp.AddTool(srv, &mcp.Tool{Name: "openclash_status", Description: "Read live OpenClash mode, available groups and their existing nodes through the loopback controller. Requires network query permission. Does not return the controller secret or subscription configuration."}, func(ctx context.Context, _ *mcp.CallToolRequest, in noInput) (*mcp.CallToolResult, any, error) {
		if !s.queryIsExplicit(clientID, "network") {
			return managedToolResult(nil, errors.New("OpenClash status requires explicit network permission in LuCI"))
		}
		if err := s.control.AuthorizeQuery(clientID, "network"); err != nil {
			return managedToolResult(nil, err)
		}
		out, err := s.platform.OpenClash(ctx)
		return managedToolResult(out, err)
	})
}
func managedToolResult(value any, err error) (*mcp.CallToolResult, any, error) {
	if err != nil {
		return errResult(err.Error()), nil, nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return errResult("cannot encode result"), nil, nil
	}
	return textResult(string(data)), nil, nil
}

var adminMethods = map[string]bool{"status": false, "openclash": false, "create": true, "revoke": true, "query": true, "limit": true, "grant": true, "ungrant": true, "pause": true, "resume": true, "approve": true, "reject": true, "confirm": true, "rollback": true}

type adminInput struct {
	ID      string `json:"id,omitempty"`
	Name    string `json:"name,omitempty"`
	Group   string `json:"group,omitempty"`
	Enabled bool   `json:"enabled,omitempty"`
	Limit   int    `json:"limit,omitempty"`
	Kind    string `json:"kind,omitempty"`
	Minutes int    `json:"minutes,omitempty"`
	Digest  string `json:"digest,omitempty"`
}

func (s *Server) adminDispatch(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	if _, ok := adminMethods[method]; !ok {
		return nil, errors.New("unknown admin method")
	}
	var in adminInput
	if len(raw) > 0 {
		d := json.NewDecoder(strings.NewReader(string(raw)))
		d.DisallowUnknownFields()
		if err := d.Decode(&in); err != nil {
			return nil, errors.New("invalid management payload")
		}
	}
	switch method {
	case "status":
		snapshot, err := s.control.Snapshot(ctx)
		if err != nil {
			return nil, err
		}
		queryModes := map[string]map[string]string{}
		for _, client := range snapshot.Clients {
			queryModes[client.ID] = map[string]string{}
			for _, group := range []string{"system", "network", "devices", "logs"} {
				switch {
				case !client.Queries[group]:
					queryModes[client.ID][group] = "off"
				case s.queryIsExplicit(client.ID, group):
					queryModes[client.ID][group] = "explicit"
				case legacyGroupConfigured(s.cfg(), client.ID, group, snapshot.Now):
					queryModes[client.ID][group] = "legacy"
				default:
					queryModes[client.ID][group] = "expired"
				}
			}
		}
		return map[string]any{"state": snapshot, "query_modes": queryModes, "version": version, "listen": s.cfg().Listen, "boundary": "MCP only; root SSH is outside this control"}, nil
	case "openclash":
		return s.platform.OpenClash(ctx)
	case "create":
		if in.Name == "" || len(in.Name) > 96 || strings.ContainsAny(in.Name, "\x00\r\n") {
			return nil, errors.New("client name is required and must be at most 96 bytes")
		}
		if in.ID == "" {
			in.ID = "client-" + randToken()
		}
		if !regexp.MustCompile(`^[a-zA-Z0-9_-]{1,40}$`).MatchString(in.ID) {
			return nil, errors.New("client ID must be 1-40 ASCII letters, numbers, hyphen or underscore")
		}
		snap, err := s.control.Snapshot(ctx)
		if err != nil {
			return nil, err
		}
		for _, c := range snap.Clients {
			if c.ID == in.ID {
				return nil, errors.New("client ID already exists, including revoked clients")
			}
		}
		if len(snap.Clients) >= 64 {
			return nil, errors.New("client limit reached (64)")
		}
		if err = s.control.ImportClient(control.Client{ID: in.ID, Name: in.Name, Queries: map[string]bool{}, Limit: 60}); err != nil {
			return nil, err
		}
		token, err := s.tokens.Mint(in.ID)
		if err != nil {
			_ = s.control.Revoke(in.ID)
			return nil, errors.New("token creation failed; client disabled")
		}
		return map[string]any{"id": in.ID, "token": token}, nil
	case "revoke":
		if err := s.control.Revoke(in.ID); err != nil {
			return nil, err
		}
		s.tokens.Revoke(in.ID)
		return map[string]any{"revoked": true}, nil
	case "query":
		if err := s.control.SetQuery(in.ID, in.Group, in.Enabled); err != nil {
			return nil, err
		}
		s.queryMu.Lock()
		if s.queryOverrides[in.ID] == nil {
			s.queryOverrides[in.ID] = map[string]bool{}
		}
		s.queryOverrides[in.ID][in.Group] = true
		b, err := json.Marshal(s.queryOverrides)
		if err == nil {
			err = atomicWrite(filepath.Join(s.statePath, "query-overrides.json"), b)
		}
		s.queryMu.Unlock()
		return nil, err
	case "limit":
		return nil, s.control.SetLimit(in.ID, in.Limit)
	case "grant":
		return nil, s.control.Grant(in.ID, in.Kind, time.Duration(in.Minutes)*time.Minute)
	case "ungrant":
		return nil, s.control.RevokeGrant(in.ID, in.Kind)
	case "pause":
		return nil, s.control.Pause()
	case "resume":
		return nil, s.control.Resume()
	case "approve":
		return s.control.Approve(ctx, in.ID, in.Digest)
	case "reject":
		return nil, s.control.Reject(in.ID)
	case "confirm":
		return nil, s.control.Confirm(ctx, in.ID)
	case "rollback":
		return nil, s.control.Rollback(ctx, in.ID)
	}
	return nil, errors.New("unsupported method")
}
func (s *Server) startAdmin() (func(), error) {
	socket := filepath.Join(s.statePath, "admin.sock")
	if st, err := os.Lstat(socket); err == nil {
		if st.Mode()&os.ModeSocket == 0 {
			return nil, errors.New("admin socket path exists and is not a socket")
		}
		_ = os.Remove(socket)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return nil, err
	}
	if err = os.Chmod(socket, 0600); err != nil {
		listener.Close()
		return nil, err
	}
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 70 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != "POST" {
			http.Error(w, "POST required", 405)
			return
		}
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 32768))
		var data any
		if err == nil {
			ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
			defer cancel()
			data, err = s.adminDispatch(ctx, strings.TrimPrefix(r.URL.Path, "/"), raw)
		}
		if err != nil {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": data})
	})}
	go func() { _ = srv.Serve(listener) }()
	return func() { _ = srv.Close(); _ = os.Remove(socket) }, nil
}
func runRPCD(statePath string, args []string) error {
	if len(args) == 0 {
		return errors.New("rpcd list or call method required")
	}
	if args[0] == "list" {
		methods := map[string]any{}
		for name := range adminMethods {
			if name == "status" || name == "openclash" || name == "pause" || name == "resume" {
				methods[name] = map[string]any{}
			} else {
				methods[name] = map[string]any{"payload": ""}
			}
		}
		return json.NewEncoder(os.Stdout).Encode(methods)
	}
	if args[0] != "call" || len(args) != 2 {
		return errors.New("invalid rpcd invocation")
	}
	if _, ok := adminMethods[args[1]]; !ok {
		return errors.New("unknown rpcd method")
	}
	var input struct {
		Payload string `json:"payload"`
	}
	if err := json.NewDecoder(io.LimitReader(os.Stdin, 40000)).Decode(&input); err != nil && err != io.EOF {
		return err
	}
	if input.Payload == "" {
		input.Payload = "{}"
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(statePath, "admin.sock"))
	}}
	client := &http.Client{Transport: transport, Timeout: 65 * time.Second}
	req, err := http.NewRequest("POST", "http://unix/"+args[1], strings.NewReader(input.Payload))
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"ok": false, "error": "MCP management service unavailable; check service status and recovery logs"})
	}
	defer resp.Body.Close()
	_, err = io.Copy(os.Stdout, io.LimitReader(resp.Body, 4<<20))
	return err
}

