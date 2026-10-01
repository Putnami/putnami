package output

import (
	"fmt"
	"strings"

	"go.putnami.dev/cli/model/jobs"
)

// serveLogLabel builds the "cmd(project)" prefix for serve-mode log lines,
// bolding the command and coloring the project name unless color is disabled.
func serveLogLabel(cmd, project string, noColor bool, projColor string) string {
	if noColor {
		return cmd + "(" + project + ")"
	}
	return colorize(cmd, Bold) + "(" + colorize(project, projColor) + ")"
}

// FormatLogEvent formats a log event as a human-readable line for serve mode output.
// The label (e.g. "serve(putnami.dev)") is prepended as a prefix when non-empty.
// Returns an empty string for debug-level events or if the event lacks a message.
func FormatLogEvent(event jobs.RawJobEvent, noColor bool, label string) string {
	level, _ := event.Data["level"].(string)
	message, _ := event.Data["message"].(string)

	if message == "" || level == "debug" {
		return ""
	}

	ctx, _ := event.Data["context"].(map[string]any)
	errData, _ := event.Data["error"].(map[string]any)

	prefix := buildLogPrefix(label)

	// HTTP request log: context has method + status
	if ctx != nil {
		method, _ := ctx["method"].(string)
		status, _ := ctx["status"].(float64)
		if method != "" && status > 0 {
			// Use routePath from context if available; fall back to message
			path, _ := ctx["routePath"].(string)
			if path == "" {
				path = message
			}
			return formatHTTPLog(level, method, path, int(status), ctx, errData, noColor, prefix)
		}
	}

	// Generic log
	logger := ""
	if ctx != nil {
		logger, _ = ctx["logger"].(string)
	}
	return formatGenericLog(level, logger, message, ctx, errData, noColor, prefix)
}

// buildLogPrefix returns a formatted label prefix like "  serve(project) ".
// The label should already be colorized by the caller if desired.
func buildLogPrefix(label string) string {
	if label == "" {
		return "  "
	}
	return fmt.Sprintf("  %s ", label)
}

// formatHTTPLog formats an HTTP request log line.
// Format: "  {prefix}{icon} [{METHOD}] {path}  → {status}  {duration}"
// For error-level logs with error data, the error message and stack trace are appended.
func formatHTTPLog(level, method, path string, status int, ctx map[string]any, errData map[string]any, noColor bool, prefix string) string {
	dur := ""
	if d, ok := ctx["duration"].(float64); ok && d > 0 {
		dur = formatLogDuration(d)
	}

	statusStr := fmt.Sprintf("%d", status)

	if noColor {
		tag := levelTag(level)
		parts := []string{prefix, tag, " [", method, "] ", path}
		parts = append(parts, "  → ", statusStr)
		if dur != "" {
			parts = append(parts, "  ", dur)
		}
		line := strings.Join(parts, "")
		line += formatLogErrorDetails(errData, noColor, prefix)
		return line
	}

	icon := levelIcon(level)
	lc := levelColor(level)
	sc := statusColor(status)

	parts := []string{
		prefix,
		colorize(icon, lc), " ",
		colorize("["+method+"]", Bold), " ",
		path,
		"  ", colorize("→", Dim), " ", colorize(statusStr, sc),
	}
	if dur != "" {
		parts = append(parts, "  ", colorize(dur, Dim))
	}
	line := strings.Join(parts, "")
	line += formatLogErrorDetails(errData, noColor, prefix)
	return line
}

// formatLogErrorDetails formats the error message and stack trace for log output.
// Returns an empty string when errData is nil.
func formatLogErrorDetails(errData map[string]any, noColor bool, prefix string) string {
	if errData == nil {
		return ""
	}

	var sb strings.Builder

	errMsg, _ := errData["message"].(string)
	if errMsg != "" {
		sb.WriteString("\n")
		sb.WriteString(prefix)
		if noColor {
			sb.WriteString(errMsg)
		} else {
			sb.WriteString(colorize(errMsg, Red))
		}
	}

	stack, _ := errData["stack"].(string)
	if stack != "" {
		sb.WriteString("\n")
		if noColor {
			sb.WriteString(stack)
		} else {
			sb.WriteString(colorize(stack, Dim))
		}
	}

	return sb.String()
}

// formatGenericLog formats a generic (non-HTTP) log line.
func formatGenericLog(level, logger, message string, ctx map[string]any, errData map[string]any, noColor bool, prefix string) string {
	dur := ""
	if ctx != nil {
		if d, ok := ctx["duration"].(float64); ok && d > 0 {
			dur = formatLogDuration(d)
		}
	}

	if noColor {
		tag := levelTag(level)
		base := prefix + tag
		if logger != "" {
			base += " [" + logger + "] " + message
		} else {
			base += " " + message
		}
		if dur != "" {
			base += "  " + dur
		}
		return base + formatLogErrorDetails(errData, noColor, prefix)
	}

	icon := levelIcon(level)
	lc := levelColor(level)

	parts := []string{prefix, colorize(icon, lc), " "}
	if logger != "" {
		parts = append(parts, colorize("["+logger+"]", Dim), " ")
	}
	parts = append(parts, message)
	if dur != "" {
		parts = append(parts, "  ", colorize(dur, Dim))
	}
	return strings.Join(parts, "") + formatLogErrorDetails(errData, noColor, prefix)
}

// levelIcon returns a Unicode icon for the log level.
func levelIcon(level string) string {
	switch level {
	case "error":
		return "✖"
	case "warn":
		return "▲"
	case "info":
		return "●"
	default:
		return "○"
	}
}

// levelTag returns a plain-text tag for no-color mode.
func levelTag(level string) string {
	switch level {
	case "error":
		return "ERR"
	case "warn":
		return "WRN"
	case "info":
		return "INF"
	default:
		return "DBG"
	}
}

// levelColor returns the ANSI color for a log level.
func levelColor(level string) string {
	switch level {
	case "error":
		return Red
	case "warn":
		return Yellow
	case "info":
		return Green
	default:
		return Dim
	}
}

// statusColor returns the ANSI color for an HTTP status code.
func statusColor(status int) string {
	switch {
	case status >= 500:
		return Red
	case status >= 400:
		return Yellow
	case status >= 300:
		return Cyan
	default:
		return Green
	}
}

// formatLogDuration formats a duration in milliseconds for display.
func formatLogDuration(ms float64) string {
	if ms < 1000 {
		return fmt.Sprintf("%.0fms", ms)
	}
	return fmt.Sprintf("%.1fs", ms/1000)
}
