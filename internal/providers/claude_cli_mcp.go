package providers

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/nextlevelbuilder/goclaw/internal/config"
)

// MCPConfigData holds the base MCP server entries built at startup.
// Per-session configs are written via WriteMCPConfig with agent context injected.
type MCPConfigData struct {
	Servers      map[string]interface{} // external MCP server entries (stdio/sse/http)
	GatewayAddr  string
	GatewayToken string
}

// BuildCLIMCPConfigData builds the base MCP server map from config.
// Does NOT include the goclaw-bridge entry — that's added per-session
// with agent context headers in WriteMCPConfig.
func BuildCLIMCPConfigData(servers map[string]*config.MCPServerConfig, gatewayAddr string, gatewayToken ...string) *MCPConfigData {
	mcpServers := make(map[string]interface{}, len(servers))

	for name, srv := range servers {
		if !srv.IsEnabled() {
			continue
		}

		entry := make(map[string]interface{})

		switch srv.Transport {
		case "stdio":
			if srv.Command != "" {
				entry["command"] = srv.Command
			}
			if len(srv.Args) > 0 {
				entry["args"] = srv.Args
			}
			if len(srv.Env) > 0 {
				entry["env"] = srv.Env
			}

		case "sse":
			if srv.URL != "" {
				entry["url"] = srv.URL
				entry["type"] = "sse"
			}
			if len(srv.Headers) > 0 {
				entry["headers"] = srv.Headers
			}

		case "streamable-http":
			if srv.URL != "" {
				entry["url"] = srv.URL
				entry["type"] = "http"
			}
			if len(srv.Headers) > 0 {
				entry["headers"] = srv.Headers
			}

		default:
			continue
		}

		if len(entry) > 0 {
			mcpServers[name] = entry
		}
	}

	token := ""
	if len(gatewayToken) > 0 {
		token = gatewayToken[0]
	}

	return &MCPConfigData{
		Servers:      mcpServers,
		GatewayAddr:  gatewayAddr,
		GatewayToken: token,
	}
}

// mcpConfigBaseDir returns ~/.goclaw/mcp-configs, separate from workDir
// so agent cannot read tokens from the MCP config files.
func mcpConfigBaseDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "goclaw-mcp-configs")
	}
	return filepath.Join(home, ".goclaw", "mcp-configs")
}

// BridgeContext holds per-call context for MCP bridge headers.
type BridgeContext struct {
	AgentID  string
	UserID   string
	Channel  string
	ChatID   string
	PeerKind string
}

// WriteMCPConfig writes a per-session MCP config file with agent context headers.
// Files are stored at ~/.goclaw/mcp-configs/<safe-session-key>/mcp-config.json,
// outside the agent's workDir so tokens are not exposed.
// Skips write if content is unchanged. Returns the file path.
func (d *MCPConfigData) WriteMCPConfig(sessionKey string, bc BridgeContext) string {
	return d.writeMCPConfigInternal(sessionKey, bc.AgentID, bc.UserID, bc.Channel, bc.ChatID, bc.PeerKind)
}

func (d *MCPConfigData) writeMCPConfigInternal(sessionKey, agentID, userID, channel, chatID, peerKind string) string {
	if d == nil || (len(d.Servers) == 0 && d.GatewayAddr == "") {
		return ""
	}

	// Shallow-copy the outer map so we can add the bridge entry without mutating the shared base.
	// Inner server entries are not modified, so shallow copy is sufficient.
	servers := make(map[string]interface{}, len(d.Servers)+1)
	for k, v := range d.Servers {
		servers[k] = v
	}

	// Build bridge entry with per-session agent context headers
	if d.GatewayAddr != "" {
		headers := make(map[string]string)
		if d.GatewayToken != "" {
			headers["Authorization"] = "Bearer " + d.GatewayToken
		}
		if agentID != "" {
			headers["X-Agent-ID"] = agentID
		}
		if userID != "" && !strings.ContainsAny(userID, "\r\n\x00") {
			headers["X-User-ID"] = userID
		}
		if channel != "" {
			headers["X-Channel"] = channel
		}
		if chatID != "" {
			headers["X-Chat-ID"] = chatID
		}
		if peerKind != "" {
			headers["X-Peer-Kind"] = peerKind
		}
		// HMAC signature over agent context to prevent header forgery
		if d.GatewayToken != "" && (agentID != "" || userID != "") {
			headers["X-Bridge-Sig"] = SignBridgeContext(d.GatewayToken, agentID, userID)
		}

		bridgeEntry := map[string]interface{}{
			"url":  fmt.Sprintf("http://%s/mcp/bridge", d.GatewayAddr),
			"type": "http",
		}
		if len(headers) > 0 {
			bridgeEntry["headers"] = headers
		}
		servers["goclaw-bridge"] = bridgeEntry
	}

	if len(servers) == 0 {
		return ""
	}

	data, err := json.MarshalIndent(map[string]interface{}{"mcpServers": servers}, "", "  ")
	if err != nil {
		slog.Warn("claude-cli: failed to marshal mcp config", "error", err)
		return ""
	}

	// Write to per-session dir outside workDir
	safe := sanitizePathSegment(sessionKey)
	dir := filepath.Join(mcpConfigBaseDir(), safe)
	if err := os.MkdirAll(dir, 0700); err != nil {
		slog.Warn("claude-cli: failed to create mcp config dir", "error", err)
		return ""
	}

	path := filepath.Join(dir, "mcp-config.json")

	// Skip write if unchanged
	if existing, err := os.ReadFile(path); err == nil && string(existing) == string(data) {
		return path
	}
	// Atomic write: temp file + rename to prevent partial reads
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0600); err != nil {
		slog.Warn("claude-cli: failed to write mcp config tmp", "error", err)
		return ""
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		slog.Warn("claude-cli: failed to rename mcp config", "error", err)
		return ""
	}

	return path
}

// BuildCLIMCPConfig is the legacy helper that builds and writes a global MCP config file.
// Used at startup for standalone mode. For per-session configs, use BuildCLIMCPConfigData + WriteMCPConfig.
func BuildCLIMCPConfig(servers map[string]*config.MCPServerConfig, gatewayAddr string, gatewayToken ...string) (string, func(), error) {
	d := BuildCLIMCPConfigData(servers, gatewayAddr, gatewayToken...)

	// For legacy path, build bridge entry without agent context
	if d.GatewayAddr != "" {
		headers := make(map[string]string)
		if d.GatewayToken != "" {
			headers["Authorization"] = "Bearer " + d.GatewayToken
		}
		bridgeEntry := map[string]interface{}{
			"url":  fmt.Sprintf("http://%s/mcp/bridge", d.GatewayAddr),
			"type": "http",
		}
		if len(headers) > 0 {
			bridgeEntry["headers"] = headers
		}
		d.Servers["goclaw-bridge"] = bridgeEntry
	}

	if len(d.Servers) == 0 {
		return "", func() {}, nil
	}

	data, err := json.MarshalIndent(map[string]interface{}{"mcpServers": d.Servers}, "", "  ")
	if err != nil {
		return "", nil, fmt.Errorf("marshal mcp config: %w", err)
	}

	tmpDir := filepath.Join(os.TempDir(), "goclaw-mcp-configs")
	if err := os.MkdirAll(tmpDir, 0700); err != nil {
		return "", nil, fmt.Errorf("create mcp config dir: %w", err)
	}

	tmpFile := filepath.Join(tmpDir, fmt.Sprintf("mcp-%s.json", uuid.New().String()[:8]))
	if err := os.WriteFile(tmpFile, data, 0600); err != nil {
		return "", nil, fmt.Errorf("write mcp config: %w", err)
	}

	cleanup := func() {
		os.Remove(tmpFile)
	}

	return tmpFile, cleanup, nil
}

// sanitizePathSegment makes a string safe for use as a single filesystem directory name.
// Replaces path separators and special chars, strips null bytes, handles ".." traversal,
// and truncates to 255 chars.
func sanitizePathSegment(s string) string {
	safe := strings.NewReplacer(":", "-", "/", "-", "\\", "-", "\x00", "").Replace(s)
	// Collapse any ".." sequences to prevent traversal
	safe = strings.ReplaceAll(safe, "..", "_")
	if len(safe) > 255 {
		safe = safe[:255]
	}
	if safe == "" || safe == "." {
		safe = "default"
	}
	return safe
}

// SignBridgeContext computes HMAC-SHA256(key, agentID+"|"+userID) for bridge header integrity.
func SignBridgeContext(key, agentID, userID string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(agentID + "|" + userID))
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyBridgeContext checks the HMAC signature against the expected agent/user context.
func VerifyBridgeContext(key, agentID, userID, sig string) bool {
	expected := SignBridgeContext(key, agentID, userID)
	return hmac.Equal([]byte(expected), []byte(sig))
}
