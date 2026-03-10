package mcp

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// poolEntry holds a shared connection and its discovered tools.
type poolEntry struct {
	state    *serverState // connection + health state
	tools    []mcpgo.Tool // discovered MCP tool definitions
	refCount int          // number of active Manager references
}

// Pool manages shared MCP server connections across agents.
// One physical connection per server name, shared by all agents
// that have grants to that server. Each agent creates its own
// BridgeTools pointing to the shared client/connected pointers.
type Pool struct {
	mu      sync.Mutex
	servers map[string]*poolEntry
}

// NewPool creates a shared MCP connection pool.
func NewPool() *Pool {
	return &Pool{
		servers: make(map[string]*poolEntry),
	}
}

// Acquire returns a shared connection for the named server.
// If no connection exists, it connects using the provided config.
// Increments the reference count.
func (p *Pool) Acquire(ctx context.Context, name string, cp ConnParams, timeoutSec int) (*poolEntry, error) {
	p.mu.Lock()

	if entry, ok := p.servers[name]; ok && entry.state.connected.Load() {
		entry.refCount++
		p.mu.Unlock()
		slog.Debug("mcp.pool.reuse", "server", name, "refCount", entry.refCount)
		return entry, nil
	}

	// If entry exists but disconnected, close old connection first
	if old, ok := p.servers[name]; ok {
		if old.state.cancel != nil {
			old.state.cancel()
		}
		if c := old.state.client(); c != nil {
			_ = c.Close()
		}
		delete(p.servers, name)
	}

	p.mu.Unlock()

	// Connect outside the lock (may be slow)
	ss, mcpTools, err := connectAndDiscover(ctx, name, cp, timeoutSec)
	if err != nil {
		return nil, err
	}

	// Start health loop with reconnection support
	hctx, hcancel := context.WithCancel(context.Background())
	ss.cancel = hcancel
	go poolHealthLoop(hctx, ss)

	entry := &poolEntry{
		state:    ss,
		tools:    mcpTools,
		refCount: 1,
	}

	p.mu.Lock()
	// Check if another goroutine connected while we were connecting
	if existing, ok := p.servers[name]; ok && existing.state.connected.Load() {
		existing.refCount++
		p.mu.Unlock()
		// Clean up our unused connection outside the lock
		hcancel()
		_ = ss.client().Close()
		return existing, nil
	}
	p.servers[name] = entry
	p.mu.Unlock()

	slog.Info("mcp.pool.connected", "server", name, "tools", len(mcpTools))
	return entry, nil
}

// Release decrements the reference count for a server.
// The connection is NOT closed when refCount reaches 0 — it stays
// alive for future agents. Use Stop() to close all connections.
func (p *Pool) Release(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if entry, ok := p.servers[name]; ok {
		entry.refCount--
		if entry.refCount < 0 {
			entry.refCount = 0
		}
		slog.Debug("mcp.pool.release", "server", name, "refCount", entry.refCount)
	}
}

// Stop closes all pooled connections. Called on gateway shutdown.
func (p *Pool) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()

	for name, entry := range p.servers {
		if entry.state.cancel != nil {
			entry.state.cancel()
		}
		if c := entry.state.client(); c != nil {
			_ = c.Close()
		}
		slog.Debug("mcp.pool.stopped", "server", name)
	}
	p.servers = make(map[string]*poolEntry)
}

// poolHealthLoop monitors a pool-managed connection and attempts reconnection
// on failure, mirroring the Manager's healthLoop/tryReconnect/deadPoll pattern.
func poolHealthLoop(ctx context.Context, ss *serverState) {
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
					slog.Info("mcp.pool.dead_poll_start", "server", ss.name, "interval", deadPollInterval)
					ticker.Stop()
					poolDeadPoll(ctx, ss)
					if !ss.connected.Load() {
						return
					}
					ticker.Reset(healthCheckInterval)
					slog.Info("mcp.pool.revived", "server", ss.name)
					continue
				}

				slog.Warn("mcp.pool.health_failed", "server", ss.name, "error", err)
				poolTryReconnect(ctx, ss)
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

// poolTryReconnect attempts to reconnect a pool-managed server with exponential backoff.
func poolTryReconnect(ctx context.Context, ss *serverState) {
	ss.mu.Lock()
	if ss.reconnAttempts >= maxReconnectAttempts {
		ss.lastErr = fmt.Sprintf("max reconnect attempts (%d) reached", maxReconnectAttempts)
		ss.mu.Unlock()
		ss.dead.Store(true)
		slog.Error("mcp.pool.reconnect_exhausted", "server", ss.name)
		return
	}
	ss.reconnAttempts++
	attempt := ss.reconnAttempts
	ss.mu.Unlock()

	backoff := min(initialBackoff*time.Duration(1<<(attempt-1)), maxBackoff)

	slog.Info("mcp.pool.reconnecting", "server", ss.name, "attempt", attempt, "backoff", backoff)

	select {
	case <-ctx.Done():
		return
	case <-time.After(backoff):
	}

	// Try ping first
	if err := ss.client().Ping(ctx); err == nil {
		ss.connected.Store(true)
		ss.mu.Lock()
		ss.reconnAttempts = 0
		ss.lastErr = ""
		ss.mu.Unlock()
		slog.Info("mcp.pool.reconnected", "server", ss.name)
		return
	}

	newClient, err := dialAndInit(ctx, ss.params)
	if err != nil {
		slog.Warn("mcp.pool.reconnect_failed", "server", ss.name, "error", err)
		return
	}

	ss.swapAndRestore(newClient)
	slog.Info("mcp.pool.reconnected", "server", ss.name, "method", "new_client")
}

// poolDeadPoll periodically attempts a full reconnect for a pool-managed server
// after all fast retries are exhausted.
func poolDeadPoll(ctx context.Context, ss *serverState) {
	ticker := time.NewTicker(deadPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			slog.Debug("mcp.pool.dead_poll_attempt", "server", ss.name)

			newClient, err := dialAndInit(ctx, ss.params)
			if err != nil {
				slog.Debug("mcp.pool.dead_poll_failed", "server", ss.name, "error", err)
				continue
			}

			ss.swapAndRestore(newClient)
			slog.Info("mcp.pool.dead_poll_recovered", "server", ss.name)
			return
		}
	}
}
