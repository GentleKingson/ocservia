// Package tracecontext validates the Controller's accepted traceparent format.
package tracecontext

import "strings"

// TraceID returns the trace ID of a valid v00 traceparent, or an empty string.
// Future versions and uppercase hexadecimal remain outside the accepted format.
func TraceID(value string) string {
	if len(value) != 55 {
		return ""
	}
	parts := strings.Split(value, "-")
	if len(parts) != 4 || parts[0] != "00" || len(parts[1]) != 32 || len(parts[2]) != 16 || len(parts[3]) != 2 {
		return ""
	}
	for _, part := range parts[1:] {
		for _, c := range part {
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				return ""
			}
		}
	}
	if parts[1] == "00000000000000000000000000000000" || parts[2] == "0000000000000000" {
		return ""
	}
	return parts[1]
}
