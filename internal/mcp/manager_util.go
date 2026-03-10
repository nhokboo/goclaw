package mcp

import "strings"

func mapToEnvSlice(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	s := make([]string, 0, len(env))
	for k, v := range env {
		s = append(s, k+"="+v)
	}
	return s
}

func toSet(items []string) map[string]struct{} {
	if len(items) == 0 {
		return nil
	}
	s := make(map[string]struct{}, len(items))
	for _, item := range items {
		s[item] = struct{}{}
	}
	return s
}

func joinErrors(errs []string) string {
	var result strings.Builder
	for i, e := range errs {
		if i > 0 {
			result.WriteString("; ")
		}
		result.WriteString(e)
	}
	return result.String()
}

// sanitizeConnError strips internal details (hostnames, ports, paths) from
// connection errors before exposing them via the API. Returns a generic
// category so operators know *what* failed without leaking infrastructure.
func sanitizeConnError(raw string) string {
	if raw == "" {
		return ""
	}
	low := strings.ToLower(raw)
	switch {
	case strings.Contains(low, "timeout") || strings.Contains(low, "deadline"):
		return "connection timeout"
	case strings.Contains(low, "refused"):
		return "connection refused"
	case strings.Contains(low, "session") && strings.Contains(low, "expired"):
		return "session expired"
	case strings.Contains(low, "max reconnect"):
		return raw // already a safe, controlled message
	case strings.Contains(low, "eof") || strings.Contains(low, "broken pipe"):
		return "connection lost"
	default:
		return "connection error"
	}
}

// jsonBytesToStringSlice converts JSONB []byte to []string. Returns nil on error.
func jsonBytesToStringSlice(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	var result []string
	if err := jsonUnmarshal(data, &result); err != nil {
		return nil
	}
	return result
}

// jsonBytesToStringMap converts JSONB []byte to map[string]string. Returns nil on error.
func jsonBytesToStringMap(data []byte) map[string]string {
	if len(data) == 0 {
		return nil
	}
	var result map[string]string
	if err := jsonUnmarshal(data, &result); err != nil {
		return nil
	}
	return result
}
