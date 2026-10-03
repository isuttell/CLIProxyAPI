package traceflow

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"go.etcd.io/bbolt"
)

func (o *outbox) compactIfNeeded() (string, error) {
	file, err := os.Stat(o.path)
	if err != nil {
		return "", err
	}
	stats := o.db.Stats()
	pageSize := int64(o.db.Info().PageSize)
	freeBytes := int64(stats.FreePageN+stats.PendingPageN) * pageSize
	if freeBytes < 64<<20 || freeBytes*2 < int64(file.Size()) {
		return "not_needed", nil
	}
	liveBytes := int64(file.Size()) - freeBytes
	available, err := freeDiskBytes(filepath.Dir(o.path))
	if err != nil {
		return "", err
	}
	if available < liveBytes+o.minFreeBytes {
		return "insufficient_headroom", nil
	}
	temp := o.path + ".compact.tmp"
	destination, err := bbolt.Open(temp, 0600, &bbolt.Options{Timeout: 100 * time.Millisecond})
	if err != nil {
		return "", fmt.Errorf("open compacted trace flow outbox: %w", err)
	}
	if err := bbolt.Compact(destination, o.db, 0); err != nil {
		destination.Close()
		os.Remove(temp)
		return "", fmt.Errorf("compact trace flow outbox: %w", err)
	}
	if err := destination.Sync(); err != nil {
		destination.Close()
		os.Remove(temp)
		return "", fmt.Errorf("sync compacted trace flow outbox: %w", err)
	}
	if err := destination.Close(); err != nil {
		os.Remove(temp)
		return "", fmt.Errorf("close compacted trace flow outbox: %w", err)
	}
	if err := o.db.Close(); err != nil {
		return "", fmt.Errorf("close original trace flow outbox: %w", err)
	}
	if err := replaceOutbox(temp, o.path); err != nil {
		return "", fmt.Errorf("replace compacted trace flow outbox: %w", err)
	}
	o.db, err = bbolt.Open(o.path, 0600, &bbolt.Options{Timeout: 100 * time.Millisecond, NoSync: false})
	if err != nil {
		return "", fmt.Errorf("reopen compacted trace flow outbox: %w", err)
	}
	return "completed", nil
}
