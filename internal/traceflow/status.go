package traceflow

import (
	"encoding/binary"
	"encoding/json"
	"go.etcd.io/bbolt"
	"time"
)

type StateCount struct {
	Count int64 `json:"count"`
	Bytes int64 `json:"bytes"`
}
type Status struct {
	Enabled             bool                  `json:"enabled"`
	Healthy             bool                  `json:"healthy"`
	InstallationID      string                `json:"installation_id,omitempty"`
	CurrentBinding      string                `json:"current_binding,omitempty"`
	States              map[string]StateCount `json:"states,omitempty"`
	Bindings            map[string]StateCount `json:"bindings,omitempty"`
	Reasons             map[string]StateCount `json:"reasons,omitempty"`
	Pauses              map[string]string     `json:"pauses,omitempty"`
	Tombstones          int                   `json:"tombstones"`
	Compaction          string                `json:"compaction,omitempty"`
	StorageRepairs      uint64                `json:"storage_repairs"`
	OldestPendingAt     time.Time             `json:"oldest_pending_at,omitempty"`
	LastCommit          time.Time             `json:"last_commit,omitempty"`
	LastAcknowledgement time.Time             `json:"last_acknowledgement,omitempty"`
	CaptureRejections   uint64                `json:"capture_rejections"`
	CaptureReasons      map[string]uint64     `json:"capture_reasons,omitempty"`
	Omissions           map[string]uint64     `json:"omissions,omitempty"`
	BufferOverflows     uint64                `json:"buffer_overflows"`
	LocalConflicts      uint64                `json:"local_conflicts"`
	CapacityLoss        uint64                `json:"capacity_loss"`
	DiskErrors          uint64                `json:"disk_errors"`
	Discards            uint64                `json:"discards"`
	LastError           string                `json:"last_error,omitempty"`
	RetryAfter          time.Time             `json:"retry_after,omitempty"`
}

func (o *outbox) status(current string) (Status, error) {
	s := Status{Enabled: true, InstallationID: o.installation.String(), CurrentBinding: current, Compaction: o.compaction, States: map[string]StateCount{}, Bindings: map[string]StateCount{}, Reasons: map[string]StateCount{}, Pauses: map[string]string{}}
	err := o.db.View(func(tx *bbolt.Tx) error {
		cursor := tx.Bucket(recordsBucket).Cursor()
		for _, value := cursor.First(); value != nil; _, value = cursor.Next() {
			r, payload, err := recordMetadata(value)
			if err != nil {
				return err
			}
			size := int64(len(payload))
			addCount(s.States, r.State, size)
			addCount(s.Bindings, r.Binding, size)
			if r.Reason != "" {
				addCount(s.Reasons, r.Reason, size)
			}
			if r.State == "pending" && (s.OldestPendingAt.IsZero() || r.CreatedAt < s.OldestPendingAt.UnixNano()) {
				s.OldestPendingAt = time.Unix(0, r.CreatedAt)
			}
		}
		pauseCursor := tx.Bucket(pausesBucket).Cursor()
		for key, value := pauseCursor.First(); key != nil; key, value = pauseCursor.Next() {
			s.Pauses[string(key)] = string(value)
		}
		s.Tombstones = tx.Bucket(tombstonesBucket).Stats().KeyN
		meta := tx.Bucket(metaBucket)
		if b := meta.Get([]byte("last_commit")); len(b) == 8 {
			s.LastCommit = time.Unix(0, int64(binary.BigEndian.Uint64(b)))
		}
		if b := meta.Get([]byte("last_ack")); len(b) == 8 {
			s.LastAcknowledgement = time.Unix(0, int64(binary.BigEndian.Uint64(b)))
		}
		if b := meta.Get([]byte("discard_count")); len(b) == 8 {
			s.Discards = binary.BigEndian.Uint64(b)
		}
		if b := meta.Get([]byte("repair_count")); len(b) == 8 {
			s.StorageRepairs = binary.BigEndian.Uint64(b)
		}
		if raw := meta.Get([]byte("metrics")); raw != nil {
			var saved persistedMetrics
			if err := json.Unmarshal(raw, &saved); err != nil {
				return err
			}
			s.CaptureRejections = saved.Rejected
			s.CaptureReasons = saved.Reasons
			s.Omissions = saved.Omissions
			s.BufferOverflows = saved.Overflow
			s.LocalConflicts = saved.Conflicts
			s.CapacityLoss = saved.Capacity
			s.DiskErrors = saved.DiskErrors
		}
		return nil
	})
	s.Healthy = healthyStatus(s)
	return s, err
}
func addCount(m map[string]StateCount, key string, size int64) {
	count := m[key]
	count.Count++
	count.Bytes += size
	m[key] = count
}
func (e *Exporter) Status() (Status, error) {
	if e == nil || e.box == nil {
		return Status{Enabled: false}, nil
	}
	s, err := e.box.status(e.binding)
	if err != nil {
		return s, err
	}
	s.Enabled = e.enabled.Load()
	s.CaptureRejections = e.rejected.Load()
	s.BufferOverflows = e.overflow.Load()
	s.LocalConflicts = e.conflicts.Load()
	s.CapacityLoss = e.capacity.Load()
	s.DiskErrors = e.diskErrors.Load()
	metrics := e.metrics()
	s.CaptureReasons = metrics.Reasons
	s.Omissions = metrics.Omissions
	if retry := e.retryAt.Load(); retry > 0 {
		s.RetryAfter = time.Unix(0, retry)
	}
	if value := e.lastError.Load(); value != nil {
		s.LastError = value.(string)
	}
	s.Healthy = healthyStatus(s)
	return s, nil
}
func healthyStatus(s Status) bool {
	return s.Enabled && len(s.Pauses) == 0 && s.CaptureRejections == 0 && s.BufferOverflows == 0 && s.LocalConflicts == 0 && s.CapacityLoss == 0 && s.DiskErrors == 0 && s.RetryAfter.IsZero() && s.LastError == ""
}
