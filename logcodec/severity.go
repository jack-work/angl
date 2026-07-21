package logcodec

import (
	"strings"
	"unicode"
)

// ParseSeverity finds conventional log-level tokens near the beginning of a
// line. It returns the OTEL number and canonical uppercase name.
func ParseSeverity(line string) (int, string, bool) {
	// Levels normally occur in a prefix. Bounding work also prevents a word in
	// a long payload from unexpectedly changing the record's severity.
	if len(line) > 192 {
		line = line[:192]
	}
	for _, token := range strings.FieldsFunc(line, func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_')
	}) {
		switch strings.ToLower(token) {
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
