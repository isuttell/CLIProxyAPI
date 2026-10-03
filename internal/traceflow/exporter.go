// Package traceflow exports completed CLIProxyAPI executions using the Trace Flow v2 imported-execution contract.
// The wire types come from https://pkg.go.dev/go.opentelemetry.io/proto/otlp/collector/trace/v1
// and durable transactions use https://pkg.go.dev/go.etcd.io/bbolt.
package traceflow

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
	"go.etcd.io/bbolt"
	"google.golang.org/protobuf/proto"
)

type Config struct {
	Enabled         bool
	Endpoint        string
	OutboxPath      string
	APIKeyEnv       string
	MaxPendingBytes int64
	MinFreeBytes    int64
}
type Exporter struct {
	box                                                 *outbox
	endpoint, apiKey, binding                           string
	client                                              *http.Client
	ingress                                             chan sourceRecord
	enabled                                             atomic.Bool
	closed                                              atomic.Bool
	mu                                                  sync.RWMutex
	writerDone                                          chan struct{}
	cancel                                              context.CancelFunc
	uploaderDone                                        chan struct{}
	rejected, overflow, conflicts, capacity, diskErrors atomic.Uint64
	lastError                                           atomic.Value
	retryAt                                             atomic.Int64
	reasonMu                                            sync.Mutex
	captureReasons                                      map[string]uint64
	omissionCounts                                      map[string]uint64
	shutdownOnce                                        sync.Once
	shutdownDone                                        chan struct{}
	shutdownErr                                         error
}

func normalizeEndpoint(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("trace flow endpoint is required")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || strings.Contains(raw, "#") || parsed.EscapedPath() != "/v1/traces" {
		return "", errors.New("invalid trace flow endpoint")
	}
	scheme := strings.ToLower(parsed.Scheme)
	host := strings.ToLower(parsed.Hostname())
	port := parsed.Port()
	if scheme != "https" && !(scheme == "http" && (host == "localhost" || net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback())) {
		return "", errors.New("trace flow endpoint requires HTTPS outside localhost")
	}
	if port == "443" && scheme == "https" || port == "80" && scheme == "http" {
		port = ""
	}
	if port != "" {
		parsed.Host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		parsed.Host = "[" + host + "]"
	} else {
		parsed.Host = host
	}
	parsed.Scheme = scheme
	return parsed.String(), nil
}

func Open(cfg Config) (*Exporter, error) {
	if !cfg.Enabled {
		e := &Exporter{}
		e.enabled.Store(false)
		return e, nil
	}
	endpoint, err := normalizeEndpoint(cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	if cfg.APIKeyEnv == "" {
		return nil, errors.New("trace flow API key variable name is required")
	}
	key, ok := os.LookupEnv(cfg.APIKeyEnv)
	if !ok || key == "" {
		return nil, errors.New("trace flow API key environment variable is missing or empty")
	}
	box, err := openOutbox(cfg.OutboxPath, cfg.MaxPendingBytes, cfg.MinFreeBytes)
	if err != nil {
		return nil, err
	}
	e := &Exporter{box: box, endpoint: endpoint, apiKey: key, binding: bindingID(box.secret, endpoint, key), ingress: make(chan sourceRecord, 1024), writerDone: make(chan struct{}), uploaderDone: make(chan struct{}), captureReasons: map[string]uint64{}, omissionCounts: map[string]uint64{}, shutdownDone: make(chan struct{}), client: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	e.enabled.Store(true)
	saved, err := box.status(e.binding)
	if err != nil {
		box.close()
		return nil, err
	}
	e.rejected.Store(saved.CaptureRejections)
	e.overflow.Store(saved.BufferOverflows)
	e.conflicts.Store(saved.LocalConflicts)
	e.capacity.Store(saved.CapacityLoss)
	e.diskErrors.Store(saved.DiskErrors)
	if saved.CaptureReasons != nil {
		e.captureReasons = saved.CaptureReasons
	}
	if saved.Omissions != nil {
		e.omissionCounts = saved.Omissions
	}
	if box.repaired {
		e.recordError("pending_bytes_repaired")
	}
	if err := box.pauseMismatched(e.binding); err != nil {
		box.close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	go e.writer()
	go func() { defer close(e.uploaderDone); e.uploader(ctx) }()
	return e, nil
}
func (e *Exporter) SetEnabled(enabled bool) {
	if e != nil && e.box != nil {
		e.enabled.Store(enabled)
	}
}
func (e *Exporter) HandleUsage(_ context.Context, record usage.Record) {
	if e == nil || e.box == nil || !e.enabled.Load() {
		return
	}
	item, id, reason, err := mapRecord(record, e.box.installation, e.box.secret)
	if err != nil {
		e.recordCaptureRejection(reason)
		return
	}
	payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(item)
	if err != nil {
		e.recordCaptureRejection("encoding_error")
		return
	}
	if reasons := omissionReasons(record); len(reasons) > 0 {
		e.reasonMu.Lock()
		for _, reason := range reasons {
			e.omissionCounts[reason]++
		}
		e.reasonMu.Unlock()
	}
	// Source payload is immutable after this point. Delivery fields can change independently.
	r := sourceRecord{ID: id, Schema: 2, Payload: payload, Digest: sourceDigest(payload), Binding: e.binding}
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.closed.Load() {
		e.overflow.Add(1)
		e.recordError("capture_closed")
		return
	}
	select {
	case e.ingress <- r:
	default:
		e.overflow.Add(1)
		e.recordError("buffer_full")
	}
}
func (e *Exporter) writer() {
	defer close(e.writerDone)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	pruneTicker := time.NewTicker(time.Hour)
	defer pruneTicker.Stop()
	statusTicker := time.NewTicker(time.Minute)
	defer statusTicker.Stop()
	batch := make([]sourceRecord, 0, 256)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		_, _, conflicts, capacity, err := e.box.commit(batch)
		e.conflicts.Add(uint64(conflicts))
		if conflicts > 0 {
			e.recordError("local_conflict")
		}
		e.capacity.Add(uint64(capacity))
		if capacity > 0 {
			e.recordError("capacity_loss")
		}
		if err != nil {
			e.diskErrors.Add(uint64(len(batch)))
			e.recordError("outbox_commit_error")
		}
		batch = batch[:0]
	}
	for {
		select {
		case r, ok := <-e.ingress:
			if !ok {
				flush()
				if err := e.persistMetrics(); err != nil {
					e.recordError("metrics_write_error")
				}
				return
			}
			batch = append(batch, r)
			if len(batch) >= 256 {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-statusTicker.C:
			if err := e.persistMetrics(); err != nil {
				e.recordError("metrics_write_error")
			}
			e.logStatus()
		case <-pruneTicker.C:
			if err := e.box.db.Update(func(tx *bbolt.Tx) error { return pruneTombstones(tx, time.Now()) }); err != nil {
				e.recordError("tombstone_prune_error")
			}
		}
	}
}
func (e *Exporter) recordError(reason string) {
	if e != nil {
		previous := e.lastError.Load()
		if previous == nil || previous.(string) != reason {
			e.lastError.Store(reason)
			log.WithFields(log.Fields{"component": "traceflow", "reason": reason}).Warn("trace flow exporter unhealthy")
		}
	}
}
func (e *Exporter) Close(ctx context.Context) error {
	if e == nil || e.box == nil {
		return nil
	}
	e.shutdownOnce.Do(func() {
		e.mu.Lock()
		e.closed.Store(true)
		close(e.ingress)
		e.mu.Unlock()
		go func() {
			defer close(e.shutdownDone)
			<-e.writerDone
			e.cancel()
			<-e.uploaderDone
			e.shutdownErr = e.box.close()
		}()
	})
	select {
	case <-e.shutdownDone:
		return e.shutdownErr
	case <-ctx.Done():
		return fmt.Errorf("stop trace flow exporter: %w", ctx.Err())
	}
}
