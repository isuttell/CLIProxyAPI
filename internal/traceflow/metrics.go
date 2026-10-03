package traceflow

import (
	"encoding/json"
	"time"

	log "github.com/sirupsen/logrus"
	"go.etcd.io/bbolt"
)

type persistedMetrics struct {
	Rejected   uint64            `json:"rejected"`
	Reasons    map[string]uint64 `json:"reasons"`
	Omissions  map[string]uint64 `json:"omissions"`
	Overflow   uint64            `json:"overflow"`
	Conflicts  uint64            `json:"conflicts"`
	Capacity   uint64            `json:"capacity"`
	DiskErrors uint64            `json:"disk_errors"`
}

func (e *Exporter) metrics() persistedMetrics {
	e.reasonMu.Lock()
	reasons := make(map[string]uint64, len(e.captureReasons))
	for reason, count := range e.captureReasons {
		reasons[reason] = count
	}
	omissions := make(map[string]uint64, len(e.omissionCounts))
	for reason, count := range e.omissionCounts {
		omissions[reason] = count
	}
	e.reasonMu.Unlock()
	return persistedMetrics{Omissions: omissions, Rejected: e.rejected.Load(), Reasons: reasons, Overflow: e.overflow.Load(), Conflicts: e.conflicts.Load(), Capacity: e.capacity.Load(), DiskErrors: e.diskErrors.Load()}
}
func (e *Exporter) persistMetrics() error {
	raw, err := json.Marshal(e.metrics())
	if err != nil {
		return err
	}
	return e.box.db.Update(func(tx *bbolt.Tx) error { return tx.Bucket(metaBucket).Put([]byte("metrics"), raw) })
}
func (e *Exporter) recordCaptureRejection(reason string) {
	e.rejected.Add(1)
	e.reasonMu.Lock()
	e.captureReasons[reason]++
	e.reasonMu.Unlock()
	e.recordError(reason)
}
func (e *Exporter) logStatus() {
	status, err := e.Status()
	if err != nil {
		e.recordError("status_read_error")
		return
	}
	pending := status.States["pending"]
	quarantined := status.States["quarantined"]
	var oldestSeconds int64
	if !status.OldestPendingAt.IsZero() {
		oldestSeconds = int64(time.Since(status.OldestPendingAt).Seconds())
	}
	log.WithFields(log.Fields{"component": "traceflow", "healthy": status.Healthy, "pending_count": pending.Count, "pending_bytes": pending.Bytes, "quarantined_count": quarantined.Count, "quarantined_bytes": quarantined.Bytes, "oldest_pending_seconds": oldestSeconds, "last_commit": status.LastCommit, "last_ack": status.LastAcknowledgement, "pauses": status.Pauses, "retry_after": status.RetryAfter, "capture_reasons": status.CaptureReasons, "omissions": status.Omissions, "capacity_loss": status.CapacityLoss, "buffer_overflows": status.BufferOverflows, "disk_errors": status.DiskErrors, "health_reason": status.LastError}).Info("trace flow exporter status")
}
