package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/tools"
)

// BridgeToolNames is the subset of GoClaw tools exposed via the MCP bridge.
// Excluded: spawn (agent loop), create_forum_topic (channels),
// handoff/delegate_search/evaluate_loop/team_* (managed mode stores).
var BridgeToolNames = map[string]bool{
	// Filesystem
	"read_file":  true,
	"write_file": true,
	"list_files": true,
	"edit":       true,
	"exec":       true,
	// Web
	"web_search": true,
	"web_fetch":  true,
	// Memory & knowledge
	"memory_search": true,
	"memory_get":    true,
	"skill_search":  true,
	// Media
	"read_image":   true,
	"create_image": true,
	"tts":          true,
	// Browser automation
	"browser": true,
	// Scheduler
	"cron": true,
	// Messaging (send text/files to channels)
	"message": true,
	// Sessions (read + send)
	"sessions_list":    true,
	"session_status":   true,
	"sessions_history": true,
	"sessions_send":    true,
}

// NewBridgeServer creates a StreamableHTTPServer that exposes GoClaw tools as MCP tools.
// It reads tools from the registry, filters to BridgeToolNames, and serves them
// over streamable-http transport (stateless mode).
// msgBus is optional; when provided, tools that produce media (deliver:true) will
// publish file attachments directly to the outbound bus.
func NewBridgeServer(reg *tools.Registry, version string, msgBus ...*bus.MessageBus) *mcpserver.StreamableHTTPServer {
	srv := mcpserver.NewMCPServer("goclaw-bridge", version,
		mcpserver.WithToolCapabilities(false),
	)

	var mb *bus.MessageBus
	if len(msgBus) > 0 {
		mb = msgBus[0]
	}

	// Register each safe tool from the GoClaw registry
	var registered int
	for name := range BridgeToolNames {
		t, ok := reg.Get(name)
		if !ok {
			continue
		}

		mcpTool := convertToMCPTool(t)
		handler := makeToolHandler(reg, name, mb)
		srv.AddTool(mcpTool, handler)
		registered++
	}

	slog.Info("mcp.bridge: tools registered", "count", registered)

	return mcpserver.NewStreamableHTTPServer(srv,
		mcpserver.WithStateLess(true),
	)
}

// convertToMCPTool converts a GoClaw tools.Tool into an mcp-go Tool.
func convertToMCPTool(t tools.Tool) mcpgo.Tool {
	schema, err := json.Marshal(t.Parameters())
	if err != nil {
		// Fallback: empty object schema
		schema = []byte(`{"type":"object"}`)
	}
	return mcpgo.NewToolWithRawSchema(t.Name(), t.Description(), schema)
}

// makeToolHandler creates a ToolHandlerFunc that delegates to the GoClaw tool registry.
// When msgBus is non-nil and a tool result contains Media paths, the handler publishes
// them as outbound media attachments so files reach the user (e.g. Telegram document).
func makeToolHandler(reg *tools.Registry, toolName string, msgBus *bus.MessageBus) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		args := req.GetArguments()

		result := reg.Execute(ctx, toolName, args)

		if result.IsError {
			return mcpgo.NewToolResultError(result.ForLLM), nil
		}

		// Forward media files to the outbound bus so they reach the user as attachments.
		// This is necessary because Claude CLI processes tool results internally —
		// GoClaw's agent loop never sees result.Media from bridge tool calls.
		if msgBus != nil && len(result.Media) > 0 {
			channel := tools.ToolChannelFromCtx(ctx)
			chatID := tools.ToolChatIDFromCtx(ctx)
			if channel != "" && chatID != "" {
				var attachments []bus.MediaAttachment
				for _, mf := range result.Media {
					ct := mf.MimeType
					if ct == "" {
						ct = mimeFromExt(filepath.Ext(mf.Path))
					}
					attachments = append(attachments, bus.MediaAttachment{
						URL:         mf.Path,
						ContentType: ct,
					})
				}
				peerKind := tools.ToolPeerKindFromCtx(ctx)
				var meta map[string]string
				if peerKind == "group" {
					meta = map[string]string{"group_id": chatID}
				}
				msgBus.PublishOutbound(bus.OutboundMessage{
					Channel:  channel,
					ChatID:   chatID,
					Media:    attachments,
					Metadata: meta,
				})
				slog.Debug("mcp.bridge: forwarded media to outbound bus",
					"tool", toolName, "channel", channel, "files", len(attachments))
			}
		}

		return mcpgo.NewToolResultText(result.ForLLM), nil
	}
}

// mimeFromExt returns a MIME type for common file extensions.
func mimeFromExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".mp4":
		return "video/mp4"
	case ".ogg", ".opus":
		return "audio/ogg"
	case ".mp3":
		return "audio/mpeg"
	case ".wav":
		return "audio/wav"
	case ".pdf":
		return "application/pdf"
	case ".md":
		return "text/markdown"
	case ".txt":
		return "text/plain"
	case ".csv":
		return "text/csv"
	case ".json":
		return "application/json"
	case ".html", ".htm":
		return "text/html"
	case ".xml":
		return "application/xml"
	case ".zip":
		return "application/zip"
	default:
		return "application/octet-stream"
	}
}
