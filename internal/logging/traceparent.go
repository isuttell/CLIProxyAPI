package logging

import (
	"context"
	"net/http"
	"strings"

	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// ParseTraceparent extracts an inbound W3C context as data, without propagating it.
func ParseTraceparent(headers http.Header) (traceID, spanID string, ok bool) {
	var value string
	count := 0
	for key, values := range headers {
		if !strings.EqualFold(key, "traceparent") {
			continue
		}
		count += len(values)
		if len(values) > 0 {
			value = strings.TrimSpace(values[0])
		}
	}
	if count != 1 || len(value) < 55 || value[2] != '-' || value[35] != '-' || value[52] != '-' || value[:2] == "ff" {
		return "", "", false
	}
	if value[:2] == "00" && len(value) != 55 || len(value) > 55 && value[55] != '-' {
		return "", "", false
	}
	carrier := propagation.MapCarrier{"traceparent": value}
	span := trace.SpanContextFromContext(propagation.TraceContext{}.Extract(context.Background(), carrier))
	if !span.IsValid() || !span.IsRemote() {
		return "", "", false
	}
	return span.TraceID().String(), span.SpanID().String(), true
}
