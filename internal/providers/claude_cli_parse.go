package providers

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

// errNoResult is returned when CLI output contains JSON events but no extractable text.
// Callers can check this to trigger session reset + retry.
var errNoResult = errors.New("claude-cli: no result in JSON output")

// parseJSONResponse parses the CLI JSON output into a ChatResponse.
func parseJSONResponse(data []byte) (*ChatResponse, error) {
	// Try parsing as JSON array first (CLI may output all events as a single array).
	if resp := parseJSONArray(data); resp != nil {
		return resp, nil
	}

	// Fallback: CLI may output one JSON object per line.
	for line := range bytes.SplitSeq(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		if resp := parseSingleJSONResult(line); resp != nil {
			return resp, nil
		}
	}

	// Last resort: treat entire output as text response.
	// Guard against raw JSON leaking to users — if the output looks like
	// a JSON array or object (CLI events without a result), return an error
	// instead of forwarding internal protocol data as chat content.
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return nil, fmt.Errorf("claude-cli: empty response")
	}
	if len(trimmed) > 0 && (trimmed[0] == '[' || trimmed[0] == '{') {
		// Log a snippet of the output to help debug session/format issues.
		snippet := trimmed
		if len(snippet) > 512 {
			snippet = snippet[:512] + "..."
		}
		slog.Warn("claude-cli: unparseable JSON output", "bytes", len(trimmed), "snippet", snippet)
		return nil, fmt.Errorf("%w (got %d bytes of internal events)", errNoResult, len(trimmed))
	}
	return &ChatResponse{
		Content:      trimmed,
		FinishReason: "stop",
	}, nil
}

// parseJSONArray tries to parse data as a JSON array of CLI events, extracting
// the "result" event's text content and "assistant" event's text blocks.
func parseJSONArray(data []byte) *ChatResponse {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil
	}

	var events []json.RawMessage
	if err := json.Unmarshal(trimmed, &events); err != nil {
		return nil
	}

	var resultText string
	var assistantText strings.Builder
	var usage *Usage
	finishReason := "stop"

	for _, raw := range events {
		var ev struct {
			Type    string          `json:"type"`
			Subtype string          `json:"subtype,omitempty"`
			Result  string          `json:"result,omitempty"`
			Message json.RawMessage `json:"message,omitempty"`
			Usage   *cliUsage       `json:"usage,omitempty"`
		}
		if err := json.Unmarshal(raw, &ev); err != nil {
			continue
		}

		switch ev.Type {
		case "result":
			resultText = ev.Result
			// Newer CLI versions may embed content in Message blocks on result events.
			if resultText == "" && ev.Message != nil {
				var msg cliStreamMsg
				if err := json.Unmarshal(ev.Message, &msg); err == nil {
					text, _ := extractStreamContent(&msg)
					resultText = text
				}
			}
			if ev.Subtype == "error" {
				finishReason = "error"
			}
			if ev.Usage != nil {
				usage = &Usage{
					PromptTokens:     ev.Usage.InputTokens,
					CompletionTokens: ev.Usage.OutputTokens,
					TotalTokens:      ev.Usage.InputTokens + ev.Usage.OutputTokens,
				}
			}

		case "assistant":
			// Extract text from content blocks
			if ev.Message != nil {
				var msg cliStreamMsg
				if err := json.Unmarshal(ev.Message, &msg); err == nil {
					for _, block := range msg.Content {
						if block.Type == "text" {
							assistantText.WriteString(block.Text)
						}
					}
				}
			}
		}
	}

	// Prefer "result" text, fall back to concatenated assistant text blocks
	content := resultText
	if content == "" {
		content = assistantText.String()
	}
	if content == "" {
		return nil
	}

	return &ChatResponse{
		Content:      content,
		FinishReason: finishReason,
		Usage:        usage,
	}
}

// parseSingleJSONResult tries to parse a single JSON line as a "result" event.
func parseSingleJSONResult(line []byte) *ChatResponse {
	var resp cliJSONResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil
	}
	if resp.Type != "result" {
		return nil
	}

	content := resp.Result
	// Newer CLI versions may put content in Message blocks instead of Result string.
	if content == "" && resp.Message != nil {
		text, _ := extractStreamContent(resp.Message)
		content = text
	}
	if content == "" {
		return nil
	}

	cr := &ChatResponse{
		Content:      content,
		FinishReason: "stop",
	}
	if resp.Subtype == "error" {
		cr.FinishReason = "error"
	}
	if resp.Usage != nil {
		cr.Usage = &Usage{
			PromptTokens:     resp.Usage.InputTokens,
			CompletionTokens: resp.Usage.OutputTokens,
			TotalTokens:      resp.Usage.InputTokens + resp.Usage.OutputTokens,
		}
	}
	return cr
}

// extractStreamContent extracts text and thinking from a stream message.
func extractStreamContent(msg *cliStreamMsg) (text, thinking string) {
	var textBuf, thinkBuf strings.Builder
	for _, block := range msg.Content {
		switch block.Type {
		case "text":
			textBuf.WriteString(block.Text)
		case "thinking":
			thinkBuf.WriteString(block.Thinking)
		}
	}
	return textBuf.String(), thinkBuf.String()
}
