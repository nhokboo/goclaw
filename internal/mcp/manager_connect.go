package mcp

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/nextlevelbuilder/goclaw/internal/tools"
)

// reconnectPollInterval is how often waitForReconnect checks the connected flag.
const reconnectPollInterval = 500 * time.Millisecond

// dialAndInit creates an MCP client, starts the transport (for non-stdio),
// and performs the Initialize handshake. On any failure the client is closed
// and an error is returned. This is the single place that implements the
// create→Start→Initialize sequence used by connect, reconnect, and discovery.
func dialAndInit(ctx context.Context, cp ConnParams) (*mcpclient.Client, error) {
	client, err := createClient(cp)
	if err != nil {
		return nil, fmt.Errorf("create client: %w", err)
	}

	if cp.Transport != "stdio" {
		if err := client.Start(ctx); err != nil {
			_ = client.Close()
			return nil, fmt.Errorf("start transport: %w", err)
		}
	}

	initReq := mcpgo.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcpgo.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcpgo.Implementation{
		Name:    "goclaw",
		Version: "1.0.0",
	}

	if _, err := client.Initialize(ctx, initReq); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("initialize: %w", err)
	}

	return client, nil
}

// swapAndRestore atomically swaps the client in the holder and resets
// the serverState connection flags. The old client is closed.
func (ss *serverState) swapAndRestore(newClient *mcpclient.Client) {
	oldClient := ss.holder.Swap(newClient)
	if oldClient != nil {
		_ = oldClient.Close()
	}

	ss.dead.Store(false)
	ss.connected.Store(true)
	ss.mu.Lock()
	ss.reconnAttempts = 0
	ss.lastErr = ""
	ss.mu.Unlock()
}

// connectAndDiscover creates a client, initializes the MCP handshake, and
// discovers tools. Returns a connected serverState with discovered tool
// definitions. The caller is responsible for registering tools and starting
// the health loop. This function is shared by both Manager and Pool.
func connectAndDiscover(ctx context.Context, name string, cp ConnParams, timeoutSec int) (*serverState, []mcpgo.Tool, error) {
	client, err := dialAndInit(ctx, cp)
	if err != nil {
		return nil, nil, err
	}

	toolsResult, err := client.ListTools(ctx, mcpgo.ListToolsRequest{})
	if err != nil {
		_ = client.Close()
		return nil, nil, fmt.Errorf("list tools: %w", err)
	}

	if timeoutSec <= 0 {
		timeoutSec = 60
	}

	ss := &serverState{
		name:       name,
		params:     cp,
		holder:     &clientHolder{client: client},
		timeoutSec: timeoutSec,
	}
	ss.connected.Store(true)

	return ss, toolsResult.Tools, nil
}

// connectServer creates a client, initializes the connection, discovers tools, and registers them.
func (m *Manager) connectServer(ctx context.Context, name string, cp ConnParams, toolPrefix string, timeoutSec int) error {
	ss, mcpTools, err := connectAndDiscover(ctx, name, cp, timeoutSec)
	if err != nil {
		return err
	}

	// Register tools
	registeredNames := m.registerBridgeTools(ss, mcpTools, name, toolPrefix, timeoutSec)
	ss.toolNames = registeredNames

	// Create health monitoring context
	hctx, hcancel := context.WithCancel(context.Background())
	ss.cancel = hcancel

	// Store server state BEFORE updating MCP group
	m.mu.Lock()
	m.servers[name] = ss
	m.mu.Unlock()

	if len(registeredNames) > 0 {
		tools.RegisterToolGroup("mcp:"+name, registeredNames)
		m.updateMCPGroup()
	}

	go m.healthLoop(hctx, ss)

	slog.Info("mcp.server.connected",
		"server", name,
		"transport", cp.Transport,
		"tools", len(registeredNames),
	)

	return nil
}

// registerBridgeTools creates BridgeTools from MCP tool definitions and
// registers them in the Manager's registry. Returns registered tool names.
// Works for both standalone serverState and pool-backed entries.
func (m *Manager) registerBridgeTools(ss *serverState, mcpTools []mcpgo.Tool, serverName, toolPrefix string, timeoutSec int) []string {
	var registeredNames []string
	for _, mcpTool := range mcpTools {
		bt := NewBridgeTool(serverName, mcpTool, ss.holder, toolPrefix, timeoutSec, &ss.connected, &ss.dead)

		if _, exists := m.registry.Get(bt.Name()); exists {
			slog.Warn("mcp.tool.name_collision",
				"server", serverName,
				"tool", bt.Name(),
				"action", "skipped",
			)
			continue
		}

		m.registry.Register(bt)
		registeredNames = append(registeredNames, bt.Name())
	}
	return registeredNames
}

// connectViaPool acquires a shared connection from the pool and creates
// per-agent BridgeTools pointing to the shared client/connected pointers.
func (m *Manager) connectViaPool(ctx context.Context, name string, cp ConnParams, toolPrefix string, timeoutSec int) error {
	entry, err := m.pool.Acquire(ctx, name, cp, timeoutSec)
	if err != nil {
		return err
	}

	// Create per-agent BridgeTools from the pool's shared connection
	registeredNames := m.registerBridgeTools(entry.state, entry.tools, name, toolPrefix, timeoutSec)

	// Track server state and per-agent tool names
	m.mu.Lock()
	m.servers[name] = entry.state
	if m.poolServers == nil {
		m.poolServers = make(map[string]struct{})
	}
	m.poolServers[name] = struct{}{}
	if m.poolToolNames == nil {
		m.poolToolNames = make(map[string][]string)
	}
	m.poolToolNames[name] = registeredNames
	m.mu.Unlock()

	if len(registeredNames) > 0 {
		tools.RegisterToolGroup("mcp:"+name, registeredNames)
		m.updateMCPGroup()
	}

	slog.Info("mcp.server.connected_via_pool",
		"server", name,
		"transport", cp.Transport,
		"tools", len(registeredNames),
	)

	return nil
}

// createClient creates the appropriate MCP client based on transport type.
func createClient(cp ConnParams) (*mcpclient.Client, error) {
	switch cp.Transport {
	case "stdio":
		envSlice := mapToEnvSlice(cp.Env)
		return mcpclient.NewStdioMCPClient(cp.Command, envSlice, cp.Args...)

	case "sse":
		var opts []transport.ClientOption
		if len(cp.Headers) > 0 {
			opts = append(opts, mcpclient.WithHeaders(cp.Headers))
		}
		return mcpclient.NewSSEMCPClient(cp.URL, opts...)

	case "streamable-http":
		var opts []transport.StreamableHTTPCOption
		if len(cp.Headers) > 0 {
			opts = append(opts, transport.WithHTTPHeaders(cp.Headers))
		}
		return mcpclient.NewStreamableHttpClient(cp.URL, opts...)

	default:
		return nil, fmt.Errorf("unsupported transport: %q", cp.Transport)
	}
}

// newHealthTicker creates a ticker for health check intervals.
func newHealthTicker() *time.Ticker {
	return time.NewTicker(healthCheckInterval)
}

// isMethodNotFound returns true if the error indicates the server
// doesn't implement the "ping" method (still considered healthy).
func isMethodNotFound(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "method not found")
}

// healthLoop periodically pings the MCP server and attempts reconnection on failure.
func (m *Manager) healthLoop(ctx context.Context, ss *serverState) {
	ticker := newHealthTicker()
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := ss.client().Ping(ctx); err != nil {
				if isMethodNotFound(err) {
					ss.connected.Store(true)
					ss.mu.Lock()
					ss.reconnAttempts = 0
					ss.lastErr = ""
					ss.mu.Unlock()
					continue
				}
				ss.connected.Store(false)
				ss.mu.Lock()
				ss.lastErr = err.Error()
				ss.mu.Unlock()

				if ss.dead.Load() {
					// Switch to slow-poll: try full reconnect every deadPollInterval.
					// Server may come back after deploy/restart.
					slog.Info("mcp.server.dead_poll_start", "server", ss.name, "interval", deadPollInterval)
					ticker.Stop()
					m.deadPoll(ctx, ss)
					if !ss.connected.Load() {
						// deadPoll exited because ctx was cancelled
						return
					}
					// Revived — resume normal health checks
					ticker.Reset(healthCheckInterval)
					slog.Info("mcp.server.revived", "server", ss.name)
					continue
				}

				slog.Warn("mcp.server.health_failed", "server", ss.name, "error", err)
				m.tryReconnect(ctx, ss)
			} else {
				ss.connected.Store(true)
				ss.mu.Lock()
				ss.reconnAttempts = 0
				ss.lastErr = ""
				ss.mu.Unlock()
			}
		}
	}
}

// tryReconnect attempts to reconnect with exponential backoff.
func (m *Manager) tryReconnect(ctx context.Context, ss *serverState) {
	ss.mu.Lock()
	if ss.reconnAttempts >= maxReconnectAttempts {
		ss.lastErr = fmt.Sprintf("max reconnect attempts (%d) reached", maxReconnectAttempts)
		ss.mu.Unlock()
		ss.dead.Store(true)
		slog.Error("mcp.server.reconnect_exhausted", "server", ss.name)
		return
	}
	ss.reconnAttempts++
	attempt := ss.reconnAttempts
	ss.mu.Unlock()

	backoff := min(initialBackoff*time.Duration(1<<(attempt-1)), maxBackoff)

	slog.Info("mcp.server.reconnecting",
		"server", ss.name,
		"attempt", attempt,
		"backoff", backoff,
	)

	select {
	case <-ctx.Done():
		return
	case <-time.After(backoff):
	}

	// Try ping first — transport may have auto-reconnected (e.g. stdio restart).
	if err := ss.client().Ping(ctx); err == nil {
		ss.connected.Store(true)
		ss.mu.Lock()
		ss.reconnAttempts = 0
		ss.lastErr = ""
		ss.mu.Unlock()
		slog.Info("mcp.server.reconnected", "server", ss.name)
		return
	}

	// Ping failed — create a fresh client (handles SESSION_EXPIRED, etc.).
	newClient, err := dialAndInit(ctx, ss.params)
	if err != nil {
		slog.Warn("mcp.server.reconnect_failed", "server", ss.name, "error", err)
		return
	}

	ss.swapAndRestore(newClient)
	slog.Info("mcp.server.reconnected", "server", ss.name, "method", "new_client")
}

// deadPoll periodically attempts a full reconnect after all fast retries are
// exhausted. Runs every deadPollInterval until the server comes back or ctx
// is cancelled. On success it clears the dead flag and restores connected.
func (m *Manager) deadPoll(ctx context.Context, ss *serverState) {
	ticker := time.NewTicker(deadPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			slog.Debug("mcp.server.dead_poll_attempt", "server", ss.name)

			newClient, err := dialAndInit(ctx, ss.params)
			if err != nil {
				slog.Debug("mcp.server.dead_poll_failed", "server", ss.name, "error", err)
				continue
			}

			ss.swapAndRestore(newClient)
			slog.Info("mcp.server.dead_poll_recovered", "server", ss.name)
			return
		}
	}
}
