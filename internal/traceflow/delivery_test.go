package traceflow

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

func testOutbox(t *testing.T) *outbox {
	t.Helper()
	box, err := openOutbox(filepath.Join(t.TempDir(), "private-state", "outbox.db"), 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := box.close(); err != nil {
			t.Error(err)
		}
	})
	return box
}
func testSource(t *testing.T, box *outbox) sourceRecord {
	t.Helper()
	item, id, reason, err := mapRecord(sampleRecord(), box.installation, box.secret)
	if err != nil {
		t.Fatalf("map: %v %s", err, reason)
	}
	payload, err := proto.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	return sourceRecord{ID: id, Schema: 2, Payload: payload, Digest: sourceDigest(payload), Binding: "binding"}
}
func commitSource(t *testing.T, box *outbox, source sourceRecord) []sourceRecord {
	t.Helper()
	committed, _, _, _, err := box.commit([]sourceRecord{source})
	if err != nil || committed != 1 {
		t.Fatalf("commit: %d %v", committed, err)
	}
	batch, err := box.batch(source.Binding)
	if err != nil || len(batch) != 1 {
		t.Fatalf("batch: %d %v", len(batch), err)
	}
	return batch
}
func testExporter(box *outbox, server *httptest.Server) *Exporter {
	return &Exporter{box: box, binding: "binding", endpoint: server.URL + "/v1/traces", apiKey: "synthetic-key", client: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}
func TestStrictAcknowledgement(t *testing.T) {
	cases := []struct {
		name                    string
		status                  int
		marker, recording, body string
		wantState, wantPause    string
	}{
		{"accepted", 200, contractMarker, "true", `{"partialSuccess":{}}`, "deleted", ""},
		{"marker missing", 200, "", "true", `{"partialSuccess":{}}`, "pending", "ack_marker_missing"},
		{"recording disabled", 200, "", "false", `{"partialSuccess":{"rejectedSpans":1}}`, "pending", "recording_disabled"},
		{"partial", 200, "", "true", `{"partialSuccess":{"rejectedSpans":1}}`, "quarantined", "partial_success_ambiguous"},
		{"auth", 401, "", "", `{"error":{"code":401}}`, "pending", "authentication_rejected"},
		{"encoding", 415, "", "", `{"error":{"code":415}}`, "pending", "encoding_rejected"},
		{"malformed", 200, contractMarker, "true", `{"partialSuccess":null}`, "pending", "ack_malformed"},
		{"body error", 200, contractMarker, "true", `{"error":{"message":"bad"}}`, "pending", "ack_malformed"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			box := testOutbox(t)
			batch := commitSource(t, box, testSource(t, box))
			var seen atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen.Add(1)
				if r.URL.Path != "/v1/traces" || r.Header.Get("X-Trace-Flow-Api-Key") != "synthetic-key" || r.Header.Get("Content-Type") != "application/x-protobuf" {
					t.Error("wrong request")
				}
				var envelope v1.ExportTraceServiceRequest
				raw, errRead := io.ReadAll(r.Body)
				if errRead != nil {
					t.Errorf("read request: %v", errRead)
					return
				}
				if err := proto.Unmarshal(raw, &envelope); err != nil || len(envelope.ResourceSpans) != 1 {
					t.Errorf("invalid protobuf: %v", err)
				}
				if test.marker != "" {
					w.Header().Set("X-Trace-Flow-Contract", test.marker)
				}
				if test.recording != "" {
					w.Header().Set("X-Trace-Flow-Recording", test.recording)
				}
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			exporter := testExporter(box, server)
			_, err := exporter.deliver(context.Background(), batch)
			if err != nil {
				t.Fatal(err)
			}
			status, err := box.status("binding")
			if err != nil {
				t.Fatal(err)
			}
			if seen.Load() != 1 {
				t.Fatalf("requests: %d", seen.Load())
			}
			if test.wantState == "deleted" {
				if status.States["pending"].Count != 0 || status.Tombstones != 1 {
					t.Fatalf("not acknowledged: %+v", status)
				}
			} else if status.States[test.wantState].Count != 1 {
				t.Fatalf("state mismatch: %+v", status.States)
			}
			if status.Pauses["binding"] != test.wantPause {
				t.Fatalf("pause mismatch: %+v", status.Pauses)
			}
		})
	}
}
func TestRetryRetainsIdenticalSourceAndRecoversHealth(t *testing.T) {
	box := testOutbox(t)
	batch := commitSource(t, box, testSource(t, box))
	var seen atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if seen.Add(1) == 1 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(503)
			return
		}
		w.Header().Set("X-Trace-Flow-Contract", contractMarker)
		w.Header().Set("X-Trace-Flow-Recording", "true")
		_, _ = w.Write([]byte(`{"partialSuccess":{}}`))
	}))
	defer server.Close()
	e := testExporter(box, server)
	e.enabled.Store(true)
	retry, err := e.deliver(context.Background(), batch)
	if err != nil || retry.Seconds() != 2 {
		t.Fatalf("retry: %v %v", retry, err)
	}
	unhealthy, err := e.Status()
	if err != nil || unhealthy.Healthy || unhealthy.LastError == "" {
		t.Fatalf("retry health: %+v %v", unhealthy, err)
	}
	again, err := box.batch("binding")
	if err != nil || len(again) != 1 || again[0].Digest != batch[0].Digest {
		t.Fatal("lost source after retry")
	}
	_, err = e.deliver(context.Background(), again)
	if err != nil {
		t.Fatal(err)
	}
	status, err := e.Status()
	if err != nil || !status.Healthy || status.LastError != "" {
		t.Fatalf("recovered health: %+v %v", status, err)
	}
	if status.Tombstones != 1 || status.States["pending"].Count != 0 {
		t.Fatalf("not acknowledged: %+v", status)
	}
}
func TestRequestWideBadRequestPauses(t *testing.T) {
	box := testOutbox(t)
	batch := commitSource(t, box, testSource(t, box))
	var seen atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Add(1)
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":{"code":400,"message":"request_shape"}}`))
	}))
	defer server.Close()
	e := testExporter(box, server)
	_, err := e.deliver(context.Background(), batch)
	if err != nil {
		t.Fatal(err)
	}
	status, _ := box.status("binding")
	if seen.Load() != 1 || status.States["quarantined"].Count != 1 || status.Pauses["binding"] != "request_shape" {
		t.Fatalf("systematic 400: %+v", status)
	}
}
func TestOversizeSingleQuarantines(t *testing.T) {
	box := testOutbox(t)
	batch := commitSource(t, box, testSource(t, box))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(413) }))
	defer server.Close()
	e := testExporter(box, server)
	_, err := e.deliver(context.Background(), batch)
	if err != nil {
		t.Fatal(err)
	}
	status, _ := box.status("binding")
	if status.States["quarantined"].Count != 1 || status.Reasons["oversize"].Count != 1 {
		t.Fatalf("oversize: %+v", status)
	}
}
func TestOutboxRestartDedupeAndConflict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private-state", "outbox.db")
	box, err := openOutbox(path, 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	source := testSource(t, box)
	batch := commitSource(t, box, source)
	if err := box.close(); err != nil {
		t.Fatal(err)
	}
	box, err = openOutbox(path, 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer box.close()
	replay, err := box.batch("binding")
	if err != nil || len(replay) != 1 || replay[0].Digest != batch[0].Digest {
		t.Fatal("restart changed source")
	}
	committed, dup, conflict, _, err := box.commit([]sourceRecord{source})
	if err != nil || committed != 0 || dup != 1 || conflict != 0 {
		t.Fatalf("dedupe: %d %d %d %v", committed, dup, conflict, err)
	}
	changed := source
	changed.Payload = append([]byte(nil), source.Payload...)
	changed.Payload = append(changed.Payload, 1)
	changed.Digest = sourceDigest(changed.Payload)
	committed, dup, conflict, _, err = box.commit([]sourceRecord{changed})
	if err != nil || committed != 0 || dup != 0 || conflict != 1 {
		t.Fatalf("conflict: %d %d %d %v", committed, dup, conflict, err)
	}
	if err := box.acknowledge(replay); err != nil {
		t.Fatal(err)
	}
	committed, dup, conflict, _, err = box.commit([]sourceRecord{source})
	if err != nil || committed != 0 || dup != 1 || conflict != 0 {
		t.Fatal("tombstone failed")
	}
}
func TestAcknowledgementParser(t *testing.T) {
	for _, value := range []string{`null`, `[]`, `{"error":{}}`, `{"partialSuccess":null}`, `{"partialSuccess":{"rejectedSpans":"0"}}`, `{"partialSuccess":{"unknown":0}}`} {
		_, _, _, valid := parseAcknowledgement([]byte(value))
		if valid {
			t.Errorf("accepted %s", value)
		}
	}
	for _, value := range []string{`{}`, `{"partialSuccess":{}}`, `{"partialSuccess":{"rejectedSpans":0,"errorMessage":""}}`} {
		_, _, _, valid := parseAcknowledgement([]byte(value))
		if !valid {
			t.Errorf("rejected %s", value)
		}
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(`{"partialSuccess":{}}`), &parsed); err != nil {
		t.Fatal(err)
	}
}

func TestBoundedSplitIsolatesOneBadSpan(t *testing.T) {
	box := testOutbox(t)
	sources := make([]sourceRecord, 0, 256)
	for i := 0; i < 256; i++ {
		record := sampleRecord()
		record.RequestID = fmt.Sprintf("aaaaaaaa-aaaa-4aaa-8aaa-%012x", i+1)
		item, id, reason, err := mapRecord(record, box.installation, box.secret)
		if err != nil {
			t.Fatalf("map %d: %v %s", i, err, reason)
		}
		payload, err := proto.Marshal(item)
		if err != nil {
			t.Fatal(err)
		}
		sources = append(sources, sourceRecord{ID: id, Schema: 2, Payload: payload, Digest: sourceDigest(payload), Binding: "binding"})
	}
	committed, _, _, _, err := box.commit(sources)
	if err != nil || committed != 256 {
		t.Fatalf("commit %d: %v", committed, err)
	}
	batch, err := box.batch("binding")
	if err != nil || len(batch) != 256 {
		t.Fatalf("batch %d: %v", len(batch), err)
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		var payload v1.ExportTraceServiceRequest
		if err := proto.Unmarshal(raw, &payload); err != nil {
			t.Error(err)
			return
		}
		for _, resource := range payload.ResourceSpans {
			for _, scope := range resource.ScopeSpans {
				for _, span := range scope.Spans {
					for _, attr := range span.Attributes {
						if attr.Key == "cliproxyapi.execution.id" && attr.Value.GetStringValue() == sources[117].ID {
							w.WriteHeader(400)
							_, _ = w.Write([]byte(`{"error":{"code":400,"message":"usage_total_invariant"}}`))
							return
						}
					}
				}
			}
		}
		w.Header().Set("X-Trace-Flow-Contract", contractMarker)
		w.Header().Set("X-Trace-Flow-Recording", "true")
		_, _ = w.Write([]byte(`{"partialSuccess":{}}`))
	}))
	defer server.Close()
	e := testExporter(box, server)
	_, err = e.deliver(context.Background(), batch)
	if err != nil {
		t.Fatal(err)
	}
	status, err := box.status("binding")
	if err != nil {
		t.Fatal(err)
	}
	if requests.Load() > 17 || status.States["quarantined"].Count != 1 || status.States["pending"].Count != 0 || status.Tombstones != 255 || len(status.Pauses) != 0 {
		t.Fatalf("split result requests=%d pending=%d quarantined=%d acked=%d pauses=%v", requests.Load(), status.States["pending"].Count, status.States["quarantined"].Count, status.Tombstones, status.Pauses)
	}
}

func TestCloseFlushesBeforeCancelAndCanFinishAfterDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	t.Setenv("TF_TEST_CLOSE_KEY", "synthetic-close-key")
	cfg := Config{Enabled: true, Endpoint: server.URL + "/v1/traces", OutboxPath: filepath.Join(t.TempDir(), "private-state", "outbox.db"), APIKeyEnv: "TF_TEST_CLOSE_KEY", MaxPendingBytes: 1 << 20, MinFreeBytes: 1}
	exporter, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	exporter.HandleUsage(context.Background(), sampleRecord())
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	if err := exporter.Close(expired); err == nil {
		t.Fatal("expired deadline did not report interruption")
	}
	ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if err := exporter.Close(ctx); err != nil {
		t.Fatal(err)
	}
	maintenance, err := OpenMaintenance(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer maintenance.Close()
	status, err := maintenance.Status()
	if err != nil {
		t.Fatal(err)
	}
	if status.States["pending"].Count != 1 {
		t.Fatalf("shutdown lost committed record: %+v", status)
	}
}

func TestLostAcknowledgementReplaysSameSource(t *testing.T) {
	box := testOutbox(t)
	batch := commitSource(t, box, testSource(t, box))
	var requests atomic.Int32
	var firstDigest string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		digest := sourceDigest(raw)
		if requests.Add(1) == 1 {
			firstDigest = digest
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Error("hijack unavailable")
				return
			}
			conn, _, err := hijacker.Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			conn.Close()
			return
		}
		if digest != firstDigest {
			t.Error("replay changed protobuf request")
		}
		w.Header().Set("X-Trace-Flow-Contract", contractMarker)
		w.Header().Set("X-Trace-Flow-Recording", "true")
		_, _ = w.Write([]byte(`{"partialSuccess":{}}`))
	}))
	defer server.Close()
	e := testExporter(box, server)
	retry, err := e.deliver(context.Background(), batch)
	if err != nil || retry <= 0 {
		t.Fatalf("lost ack retry: %v %v", retry, err)
	}
	persisted, err := box.batch("binding")
	if err != nil || len(persisted) != 1 {
		t.Fatal("lost source after ambiguous acknowledgement")
	}
	_, err = e.deliver(context.Background(), persisted)
	if err != nil {
		t.Fatal(err)
	}
	status, _ := box.status("binding")
	if requests.Load() != 2 || status.Tombstones != 1 || status.States["pending"].Count != 0 {
		t.Fatalf("lost ack result: requests=%d status=%+v", requests.Load(), status)
	}
}
func TestRetryAfterBeyondLocalBackoffCap(t *testing.T) {
	if retryAfter("120") != 2*time.Minute {
		t.Fatal("retry-after was capped")
	}
}

func TestSplitStopsAfterSystematicHalf(t *testing.T) {
	box := testOutbox(t)
	sources := make([]sourceRecord, 0, 4)
	for i := 0; i < 4; i++ {
		record := sampleRecord()
		record.RequestID = fmt.Sprintf("aaaaaaaa-aaaa-4aaa-8aaa-%012x", i+1)
		item, id, reason, err := mapRecord(record, box.installation, box.secret)
		if err != nil {
			t.Fatalf("map: %v %s", err, reason)
		}
		payload, err := proto.Marshal(item)
		if err != nil {
			t.Fatal(err)
		}
		sources = append(sources, sourceRecord{ID: id, Schema: 2, Payload: payload, Digest: sourceDigest(payload), Binding: "binding"})
	}
	committed, _, _, _, err := box.commit(sources)
	if err != nil || committed != 4 {
		t.Fatalf("commit: %d %v", committed, err)
	}
	batch, err := box.batch("binding")
	if err != nil || len(batch) != 4 {
		t.Fatal("batch size")
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		var envelope v1.ExportTraceServiceRequest
		if err := proto.Unmarshal(raw, &envelope); err != nil {
			t.Error(err)
			return
		}
		reason := "span_shape"
		if len(envelope.ResourceSpans) == 2 {
			reason = "request_shape"
		}
		w.WriteHeader(400)
		_, _ = w.Write([]byte(fmt.Sprintf(`{"error":{"code":400,"message":%q}}`, reason)))
	}))
	defer server.Close()
	e := testExporter(box, server)
	_, err = e.deliver(context.Background(), batch)
	if err != nil {
		t.Fatal(err)
	}
	status, _ := box.status("binding")
	if requests.Load() != 2 || status.States["quarantined"].Count != 2 || status.States["pending"].Count != 2 || status.Pauses["binding"] != "request_shape" {
		t.Fatalf("continued after pause: requests=%d status=%+v", requests.Load(), status)
	}
}

func TestRetryableHttpStatuses(t *testing.T) {
	for _, status := range []int{408, 425, 429, 500, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			box := testOutbox(t)
			batch := commitSource(t, box, testSource(t, box))
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
			defer server.Close()
			result := testExporter(box, server).upload(context.Background(), batch)
			if result.kind != "retry" {
				t.Fatalf("status %d classified %q", status, result.kind)
			}
		})
	}
}

func TestConcurrentStatusAndClose(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Trace-Flow-Contract", contractMarker)
		w.Header().Set("X-Trace-Flow-Recording", "true")
		_, _ = w.Write([]byte(`{"partialSuccess":{}}`))
	}))
	defer server.Close()
	t.Setenv("TF_TEST_STATUS_KEY", "synthetic-status-key")
	exporter, err := Open(Config{Enabled: true, Endpoint: server.URL + "/v1/traces", OutboxPath: filepath.Join(t.TempDir(), "private-state", "outbox.db"), APIKeyEnv: "TF_TEST_STATUS_KEY", MaxPendingBytes: 1 << 20, MinFreeBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	exporter.HandleUsage(context.Background(), sampleRecord())
	started := make(chan struct{})
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		close(started)
		for {
			select {
			case <-stop:
				return
			default:
				_, _ = exporter.Status()
			}
		}
	}()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := exporter.Close(ctx); err != nil {
		t.Fatal(err)
	}
	close(stop)
	<-done
	if _, err := exporter.Status(); err == nil {
		t.Fatal("status against closed outbox did not return error")
	}
}

func TestAcknowledgementPreservesUnrelatedHealthFailure(t *testing.T) {
	box := testOutbox(t)
	batch := commitSource(t, box, testSource(t, box))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Trace-Flow-Contract", contractMarker)
		w.Header().Set("X-Trace-Flow-Recording", "true")
		_, _ = w.Write([]byte(`{"partialSuccess":{}}`))
	}))
	defer server.Close()
	exporter := testExporter(box, server)
	exporter.enabled.Store(true)
	exporter.recordError("metrics_write_error")
	if _, err := exporter.deliver(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	status, err := exporter.Status()
	if err != nil || status.Healthy || status.LastError != "metrics_write_error" {
		t.Fatalf("ack hid unrelated failure: %+v %v", status, err)
	}
}

func TestExporterPersistsAndDeliversNativeMetadata(t *testing.T) {
	box := testOutbox(t)
	exporter := &Exporter{
		box: box, binding: "binding", ingress: make(chan sourceRecord, 1), writerDone: make(chan struct{}),
		captureReasons: map[string]uint64{}, omissionCounts: map[string]uint64{},
	}
	exporter.enabled.Store(true)
	go exporter.writer()
	record := sampleRecord()
	record.NativeSource = "codex"
	record.NativeSessionID = "child-thread"
	record.NativeParentSessionID = "root-thread"
	record.NativeOriginSessionID = "origin-session"
	record.InboundTraceID = "0123456789abcdef0123456789abcdef"
	record.InboundSpanID = "0123456789abcdef"
	exporter.HandleUsage(context.Background(), record)
	close(exporter.ingress)
	<-exporter.writerDone
	batch, err := box.batch("binding")
	if err != nil || len(batch) != 1 {
		t.Fatalf("persisted batch: %d %v", len(batch), err)
	}
	var persisted tracepb.ResourceSpans
	if err := proto.Unmarshal(batch[0].Payload, &persisted); err != nil {
		t.Fatal(err)
	}
	if got := persisted.ScopeSpans[0].Spans[0]; got.ParentSpanId != nil || len(got.Links) != 0 {
		t.Fatal("inbound context changed the execution span structure")
	}
	delivered := make(chan *v1.ExportTraceServiceRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Error(errRead)
			return
		}
		var envelope v1.ExportTraceServiceRequest
		if errDecode := proto.Unmarshal(raw, &envelope); errDecode != nil {
			t.Error(errDecode)
			return
		}
		delivered <- &envelope
		w.Header().Set("X-Trace-Flow-Contract", contractMarker)
		w.Header().Set("X-Trace-Flow-Recording", "true")
		_, _ = w.Write([]byte(`{"partialSuccess":{}}`))
	}))
	defer server.Close()
	if _, err := testExporter(box, server).deliver(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	select {
	case envelope := <-delivered:
		if len(envelope.ResourceSpans) != 1 {
			t.Fatalf("delivered %d resource spans", len(envelope.ResourceSpans))
		}
		attrs := map[string]string{}
		for _, attr := range envelope.ResourceSpans[0].ScopeSpans[0].Spans[0].Attributes {
			attrs[attr.Key] = attr.Value.GetStringValue()
		}
		for key, want := range map[string]string{
			"cliproxyapi.client.source": "codex", "cliproxyapi.client.session.id": "child-thread",
			"cliproxyapi.client.session.parent_id": "root-thread", "cliproxyapi.client.session.origin_id": "origin-session",
			"cliproxyapi.inbound.trace_id": "0123456789abcdef0123456789abcdef", "cliproxyapi.inbound.span_id": "0123456789abcdef",
		} {
			if attrs[key] != want {
				t.Fatalf("delivered %s = %q, want %q", key, attrs[key], want)
			}
		}
	default:
		t.Fatal("delivery did not reach the test collector")
	}
}
