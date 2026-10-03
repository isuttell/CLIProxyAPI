package traceflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/rand"
	"net/http"
	"regexp"
	"strings"
	"time"

	collector "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

const contractMarker = "cliproxyapi.execution/2"

var rulePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
var spanRules = map[string]bool{"span_shape": true, "status": true, "timing": true, "account_coverage": true, "account_ref": true, "required_attribute": true, "otel_identity": true, "span_name": true, "ttft_range": true, "http_status_range": true, "duplicate_execution": true}

type uploadResult struct {
	kind, reason string
	retryAfter   time.Duration
}

func (e *Exporter) upload(ctx context.Context, batch []sourceRecord) uploadResult {
	spans := make([]*tracepb.ResourceSpans, 0, len(batch))
	for _, r := range batch {
		var item tracepb.ResourceSpans
		if err := proto.Unmarshal(r.Payload, &item); err != nil {
			return uploadResult{kind: "pause", reason: "outbox_corrupt"}
		}
		spans = append(spans, &item)
	}
	body, err := proto.Marshal(&collector.ExportTraceServiceRequest{ResourceSpans: spans})
	if err != nil {
		return uploadResult{kind: "pause", reason: "encoding_error"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, bytes.NewReader(body))
	if err != nil {
		return uploadResult{kind: "pause", reason: "request_invalid"}
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("X-Trace-Flow-Api-Key", e.apiKey)
	response, err := e.client.Do(req)
	if err != nil {
		return uploadResult{kind: "retry", reason: "transport_error"}
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil {
		return uploadResult{kind: "retry", reason: "response_read_error"}
	}
	if response.StatusCode == http.StatusOK {
		rejected, hasRejected, message, valid := parseAcknowledgement(data)
		if !valid {
			return uploadResult{kind: "pause", reason: "ack_malformed"}
		}
		if response.Header.Get("X-Trace-Flow-Recording") == "false" && hasRejected && rejected == int64(len(batch)) {
			return uploadResult{kind: "pause", reason: "recording_disabled"}
		}
		if rejected != 0 || message != "" {
			return uploadResult{kind: "quarantine_pause", reason: "partial_success_ambiguous"}
		}
		if response.Header.Get("X-Trace-Flow-Contract") != contractMarker {
			return uploadResult{kind: "pause", reason: "ack_marker_missing"}
		}
		if response.Header.Get("X-Trace-Flow-Recording") != "true" {
			return uploadResult{kind: "pause", reason: "ack_recording_missing"}
		}
		return uploadResult{kind: "ack"}
	}
	if response.StatusCode == http.StatusRequestEntityTooLarge {
		return uploadResult{kind: "split", reason: "oversize"}
	}
	if response.StatusCode == http.StatusBadRequest {
		var failure struct {
			Error struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &failure) != nil || failure.Error.Code != 400 || !rulePattern.MatchString(failure.Error.Message) {
			return uploadResult{kind: "quarantine_pause", reason: "bad_request"}
		}
		code := failure.Error.Message
		if spanRules[code] || strings.HasPrefix(code, "attribute_") || strings.HasPrefix(code, "usage_") {
			return uploadResult{kind: "split", reason: code}
		}
		return uploadResult{kind: "quarantine_pause", reason: code}
	}
	if response.StatusCode == http.StatusRequestTimeout || response.StatusCode == http.StatusTooEarly || response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500 {
		return uploadResult{kind: "retry", reason: "upstream_unavailable", retryAfter: retryAfter(response.Header.Get("Retry-After"))}
	}
	reason := "upstream_rejected"
	if response.StatusCode == 401 || response.StatusCode == 403 {
		reason = "authentication_rejected"
	}
	if response.StatusCode == 415 {
		reason = "encoding_rejected"
	}
	return uploadResult{kind: "pause", reason: reason}
}
func retryAfter(value string) time.Duration {
	if value == "" {
		return 0
	}
	if parsed, err := http.ParseTime(value); err == nil {
		return time.Until(parsed)
	}
	var seconds int64
	for _, r := range value {
		if r < '0' || r > '9' {
			return 0
		}
		seconds = seconds*10 + int64(r-'0')
		if seconds > 3600 {
			return time.Hour
		}
	}
	return time.Duration(seconds) * time.Second
}
func (e *Exporter) deliver(ctx context.Context, batch []sourceRecord) (retry time.Duration, err error) {
	requests := 0
	offenders := 0
	stopped := false
	var visit func([]sourceRecord) (time.Duration, error)
	visit = func(part []sourceRecord) (time.Duration, error) {
		if len(part) == 0 || stopped {
			return 0, nil
		}
		if requests >= 17 {
			stopped = true
			e.recordError("split_limit")
			if err := e.box.quarantine(part, "split_limit"); err != nil {
				return 0, err
			}
			return 0, e.box.pause(e.binding, "split_limit")
		}
		requests++
		result := e.upload(ctx, part)
		switch result.kind {
		case "ack":
			if err := e.box.acknowledge(part); err != nil {
				return 0, err
			}
			if previous := e.lastError.Load(); previous != nil {
				switch previous.(string) {
				case "transport_error", "response_read_error", "upstream_unavailable":
					e.lastError.CompareAndSwap(previous, "")
				}
			}
			return 0, nil
		case "retry":
			e.recordError(result.reason)
			if result.retryAfter <= 0 {
				return time.Second, nil
			}
			return result.retryAfter, nil
		case "pause":
			stopped = true
			e.recordError(result.reason)
			return 0, e.box.pause(e.binding, result.reason)
		case "quarantine_pause":
			stopped = true
			e.recordError(result.reason)
			if err := e.box.quarantine(part, result.reason); err != nil {
				return 0, err
			}
			return 0, e.box.pause(e.binding, result.reason)
		case "split":
			if len(part) == 1 {
				if err := e.box.quarantine(part, result.reason); err != nil {
					return 0, err
				}
				offenders++
				if offenders > 1 {
					stopped = true
					e.recordError("multiple_invalid_spans")
					return 0, e.box.pause(e.binding, "multiple_invalid_spans")
				}
				return 0, nil
			}
			middle := len(part) / 2
			first, errFirst := visit(part[:middle])
			if errFirst != nil || first > 0 || stopped {
				return first, errFirst
			}
			return visit(part[middle:])
		default:
			return 0, errors.New("unknown trace flow upload result")
		}
	}
	return visit(batch)
}
func (e *Exporter) uploader(ctx context.Context) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		if !e.enabled.Load() {
			if !sleepContext(ctx, 500*time.Millisecond) {
				return
			}
			continue
		}
		batch, err := e.box.batch(e.binding)
		if err != nil {
			e.recordError("outbox_read_error")
			if !sleepContext(ctx, time.Second) {
				return
			}
			continue
		}
		if len(batch) == 0 {
			if !sleepContext(ctx, 500*time.Millisecond) {
				return
			}
			continue
		}
		retry, err := e.deliver(ctx, batch)
		if err != nil {
			e.recordError("outbox_write_error")
		}
		if retry > 0 || err != nil {
			wait := backoff + time.Duration(rand.Int63n(int64(backoff/2+1)))
			if retry > wait {
				wait = retry
			}

			e.retryAt.Store(time.Now().Add(wait).UnixNano())
			if !sleepContext(ctx, wait) {
				return
			}
			e.retryAt.Store(0)
			if backoff < time.Minute/2 {
				backoff *= 2
			}
		} else {
			backoff = time.Second
		}
	}
}
func sleepContext(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func parseAcknowledgement(data []byte) (rejected int64, hasRejected bool, message string, valid bool) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil || top == nil || len(top) > 1 {
		return 0, false, "", false
	}
	raw, exists := top["partialSuccess"]
	if !exists {
		return 0, false, "", len(top) == 0
	}
	var partial map[string]json.RawMessage
	if err := json.Unmarshal(raw, &partial); err != nil || partial == nil || len(partial) > 2 {
		return 0, false, "", false
	}
	for key := range partial {
		if key != "rejectedSpans" && key != "errorMessage" {
			return 0, false, "", false
		}
	}
	if value, ok := partial["rejectedSpans"]; ok {
		if err := json.Unmarshal(value, &rejected); err != nil {
			return 0, false, "", false
		}
		hasRejected = true
	}
	if value, ok := partial["errorMessage"]; ok {
		if err := json.Unmarshal(value, &message); err != nil {
			return 0, false, "", false
		}
	}
	return rejected, hasRejected, message, true
}
