package logging

import (
	"net/http"
	"strings"
	"testing"
)

func TestParseTraceparent(t *testing.T) {
	valid := "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"
	tests := []struct {
		name    string
		headers http.Header
		ok      bool
	}{
		{"valid", http.Header{"Traceparent": {valid}}, true},
		{"surrounding whitespace", http.Header{"Traceparent": {"  " + valid + "  "}}, true},
		{"tracestate ignored", http.Header{"Traceparent": {valid}, "Tracestate": {"bad=value"}}, true},
		{"future version", http.Header{"Traceparent": {"01" + valid[2:] + "-extra"}}, true},
		{"version zero extras", http.Header{"Traceparent": {valid + "-extra"}}, false},
		{"upper case", http.Header{"Traceparent": {strings.ToUpper(valid)}}, false},
		{"zero trace", http.Header{"Traceparent": {"00-" + strings.Repeat("0", 32) + "-0123456789abcdef-01"}}, false},
		{"zero span", http.Header{"Traceparent": {"00-0123456789abcdef0123456789abcdef-" + strings.Repeat("0", 16) + "-01"}}, false},
		{"reserved version", http.Header{"Traceparent": {"ff" + valid[2:]}}, false},
		{"duplicate", http.Header{"Traceparent": {valid, valid}}, false},
		{"case duplicate", http.Header{"Traceparent": {valid}, "traceparent": {valid}}, false},
		{"comma joined", http.Header{"Traceparent": {valid + "," + valid}}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			traceID, spanID, ok := ParseTraceparent(test.headers)
			if ok != test.ok {
				t.Fatalf("ok = %v, want %v", ok, test.ok)
			}
			if ok && (traceID != "0123456789abcdef0123456789abcdef" || spanID != "0123456789abcdef") {
				t.Fatalf("unexpected IDs %q %q", traceID, spanID)
			}
		})
	}
}
