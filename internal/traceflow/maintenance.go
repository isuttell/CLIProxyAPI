package traceflow

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go.etcd.io/bbolt"
	"os"
	"strings"
	"time"
)

type Maintenance struct {
	box            *outbox
	cfg            Config
	endpoint       string
	currentBinding string
}

func OpenMaintenance(cfg Config) (*Maintenance, error) {
	if cfg.OutboxPath == "" {
		return nil, errors.New("trace flow outbox path is required")
	}
	if _, err := os.Stat(cfg.OutboxPath); err != nil {
		return nil, fmt.Errorf("trace flow outbox does not exist: %w", err)
	}
	existing, err := bbolt.Open(cfg.OutboxPath, 0600, &bbolt.Options{ReadOnly: true, Timeout: 100 * time.Millisecond})
	if err != nil {
		return nil, fmt.Errorf("open existing trace flow outbox: %w", err)
	}
	err = existing.View(func(tx *bbolt.Tx) error {
		meta := tx.Bucket(metaBucket)
		if meta == nil || len(meta.Get([]byte("installation"))) == 0 || len(meta.Get([]byte("secret"))) != 32 {
			return errors.New("trace flow installation state missing")
		}
		return nil
	})
	if closeErr := existing.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, err
	}
	box, err := openOutbox(cfg.OutboxPath, cfg.MaxPendingBytes, cfg.MinFreeBytes)
	if err != nil {
		return nil, err
	}
	m := &Maintenance{box: box, cfg: cfg}
	if cfg.Endpoint != "" {
		endpoint, errEndpoint := normalizeEndpoint(cfg.Endpoint)
		if errEndpoint != nil {
			box.close()
			return nil, errEndpoint
		}
		m.endpoint = endpoint
		if key, ok := os.LookupEnv(cfg.APIKeyEnv); ok && key != "" {
			m.currentBinding = bindingID(box.secret, endpoint, key)
		}
	}
	return m, nil
}
func (m *Maintenance) Close() error {
	if m == nil || m.box == nil {
		return nil
	}
	return m.box.close()
}
func (m *Maintenance) Status() (Status, error) { return m.box.status(m.currentBinding) }
func (m *Maintenance) Resume() error {
	if m.currentBinding == "" {
		return errors.New("trace flow binding requires configured endpoint and API key")
	}
	return m.box.db.Update(func(tx *bbolt.Tx) error { return tx.Bucket(pausesBucket).Delete([]byte(m.currentBinding)) })
}
func parseSelector(selector string) (kind, value string, err error) {
	kind, value, ok := strings.Cut(selector, ":")
	if !ok || value == "" {
		return "", "", errors.New("trace flow selector must be execution, reason, or binding")
	}
	switch kind {
	case "execution":
		if !validExecutionID(value) {
			return "", "", errors.New("invalid execution selector")
		}
	case "reason":
		if !rulePattern.MatchString(value) {
			return "", "", errors.New("invalid reason selector")
		}
	case "binding":
		if len(value) != 64 {
			return "", "", errors.New("invalid binding selector")
		}
		if _, decodeErr := hex.DecodeString(value); decodeErr != nil {
			return "", "", errors.New("invalid binding selector")
		}
	default:
		return "", "", errors.New("unsupported trace flow selector")
	}
	return kind, value, nil
}
func selectorMatches(kind, value string, r sourceRecord) bool {
	switch kind {
	case "execution":
		return r.ID == value
	case "reason":
		return r.Reason == value
	case "binding":
		return r.Binding == value
	}
	return false
}
func (m *Maintenance) Requeue(selector string) error {
	kind, value, err := parseSelector(selector)
	if err != nil {
		return err
	}
	return m.box.db.Update(func(tx *bbolt.Tx) error {
		count := 0
		cursor := tx.Bucket(recordsBucket).Cursor()
		for key, raw := cursor.First(); key != nil; key, raw = cursor.Next() {
			r, errRead := readRecord(raw)
			if errRead != nil {
				return errRead
			}
			if r.State != "quarantined" || !selectorMatches(kind, value, r) {
				continue
			}
			r.State = "pending"
			r.Reason = ""
			if errPut := putRecord(tx.Bucket(recordsBucket), r); errPut != nil {
				return errPut
			}
			count++
		}
		if count == 0 {
			return errors.New("trace flow selector matched no quarantined records")
		}
		return nil
	})
}
func (m *Maintenance) Rebind(oldBinding, newBinding string) error {
	if m.currentBinding == "" {
		return errors.New("trace flow rebind requires configured endpoint and API key")
	}
	if oldBinding == "" || newBinding != m.currentBinding || oldBinding == newBinding {
		return errors.New("trace flow rebind IDs do not match current destination")
	}
	if _, _, err := parseSelector("binding:" + oldBinding); err != nil {
		return err
	}
	return m.box.db.Update(func(tx *bbolt.Tx) error {
		count := 0
		cursor := tx.Bucket(recordsBucket).Cursor()
		for key, raw := cursor.First(); key != nil; key, raw = cursor.Next() {
			r, err := readRecord(raw)
			if err != nil {
				return err
			}
			if r.Binding != oldBinding {
				continue
			}
			r.Binding = newBinding
			if err := putRecord(tx.Bucket(recordsBucket), r); err != nil {
				return err
			}
			count++
		}
		if count == 0 {
			return errors.New("trace flow old binding has no records")
		}
		if err := tx.Bucket(pausesBucket).Delete([]byte(oldBinding)); err != nil {
			return err
		}
		entry, _ := json.Marshal(struct {
			Old   string `json:"old"`
			New   string `json:"new"`
			Count int    `json:"count"`
			At    int64  `json:"at"`
		}{oldBinding, newBinding, count, time.Now().UnixNano()})
		sequence, err := tx.Bucket(auditBucket).NextSequence()
		if err != nil {
			return err
		}
		return tx.Bucket(auditBucket).Put(sequenceKey(sequence), entry)
	})
}
func (m *Maintenance) Discard(selector string, confirm bool) error {
	if !confirm {
		return errors.New("trace flow discard requires explicit confirmation")
	}
	kind, value, err := parseSelector(selector)
	if err != nil {
		return err
	}
	return m.box.db.Update(func(tx *bbolt.Tx) error {
		recs, index, tombs, meta := tx.Bucket(recordsBucket), tx.Bucket(indexBucket), tx.Bucket(tombstonesBucket), tx.Bucket(metaBucket)
		bytes := binary.BigEndian.Uint64(padded8(meta.Get([]byte("pending_bytes"))))
		count := binary.BigEndian.Uint64(padded8(meta.Get([]byte("discard_count"))))
		cursor := recs.Cursor()
		found := false
		for key, raw := cursor.First(); key != nil; key, raw = cursor.Next() {
			r, errRead := readRecord(raw)
			if errRead != nil {
				return errRead
			}
			if !selectorMatches(kind, value, r) {
				continue
			}
			if errDelete := cursor.Delete(); errDelete != nil {
				return errDelete
			}
			if errDelete := index.Delete([]byte(r.ID)); errDelete != nil {
				return errDelete
			}
			tomb, _ := json.Marshal(tombstone{Digest: r.Digest, AckedAt: time.Now().UnixNano()})
			if errPut := tombs.Put([]byte(r.ID), tomb); errPut != nil {
				return errPut
			}
			bytes -= uint64(len(r.Payload))
			count++
			found = true
		}
		if !found {
			return errors.New("trace flow selector matched no records")
		}
		if err := meta.Put([]byte("pending_bytes"), sequenceKey(bytes)); err != nil {
			return err
		}
		return meta.Put([]byte("discard_count"), sequenceKey(count))
	})
}
