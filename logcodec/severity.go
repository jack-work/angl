package logcodec

import (
	"regexp"
	"strings"
)

var severityPrefix = regexp.MustCompile(`(?i)^(?:\s*\d{4}-\d{2}-\d{2}[T ][^ ]+\s+)?\s*(?:\[|<)?(trace|trc|debug|dbg|info|information|notice|warn|warning|error|err|fatal|panic|critical|crit)(?:\]|>|:|\s|$)`)

// ParseSeverity finds conventional log-level tokens near the beginning of a
// line. It returns the OTEL number and canonical uppercase name.
func ParseSeverity(line string) (int, string, bool) {
	match := severityPrefix.FindStringSubmatch(line)
	if len(match) != 2 {
		return 0, "", false
	}
	switch strings.ToLower(match[1]) {
	case "trace", "trc":
		return SeverityTrace, "TRACE", true
	case "debug", "dbg":
		return SeverityDebug, "DEBUG", true
	case "info", "information", "notice":
		return SeverityInfo, "INFO", true
	case "warn", "warning":
		return SeverityWarn, "WARN", true
	case "error", "err":
		return SeverityError, "ERROR", true
	case "fatal", "panic", "critical", "crit":
		return SeverityFatal, "FATAL", true
	}
	return 0, "", false
}

// SeverityName maps an OTEL severity number to its range's canonical name.
func SeverityName(number int) string {
	switch {
	case number >= SeverityFatal:
		return "FATAL"
	case number >= SeverityError:
		return "ERROR"
	case number >= SeverityWarn:
		return "WARN"
	case number >= SeverityInfo:
		return "INFO"
	case number >= SeverityDebug:
		return "DEBUG"
	case number >= SeverityTrace:
		return "TRACE"
	default:
		return "UNSPECIFIED"
	}
}
