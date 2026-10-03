package traceflow

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"go.etcd.io/bbolt"
	"path/filepath"
	"sort"
	"time"
)

func (o *outbox) commit(batch []sourceRecord) (committed, duplicate, conflict, capacity int, err error) {
	err = o.db.Update(func(tx *bbolt.Tx) error {
		recs, index, tombs, meta := tx.Bucket(recordsBucket), tx.Bucket(indexBucket), tx.Bucket(tombstonesBucket), tx.Bucket(metaBucket)
		pendingBytes := int64(binary.BigEndian.Uint64(padded8(meta.Get([]byte("pending_bytes")))))
		free, errFree := freeDiskBytes(filepath.Dir(o.path))
		if errFree != nil {
			return errFree
		}
		for _, r := range batch {
			if existing := index.Get([]byte(r.ID)); existing != nil {
				old, _, errRead := recordMetadata(recs.Get(existing))
				if errRead != nil {
					return errRead
				}
				if old.Digest == r.Digest {
					duplicate++
				} else {
					conflict++
				}
				continue
			}
			if existing := tombs.Get([]byte(r.ID)); existing != nil {
				var old tombstone
				if errUnmarshal := json.Unmarshal(existing, &old); errUnmarshal != nil {
					return errUnmarshal
				}
				if old.Digest == r.Digest {
					duplicate++
				} else {
					conflict++
				}
				continue
			}
			length := int64(len(r.Payload))
			// bbolt pages and record metadata can require more physical space than the payload.
			if length > (int64(^uint64(0)>>1)-(64<<10))/2 {
				capacity++
				continue
			}
			physicalReserve := length*2 + (64 << 10)
			if pendingBytes > o.maxBytes-length || free-physicalReserve < o.minFreeBytes {
				capacity++
				continue
			}
			sequence, errSeq := recs.NextSequence()
			if errSeq != nil {
				return errSeq
			}
			r.Sequence = sequence
			r.State = "pending"
			r.CreatedAt = time.Now().UnixNano()
			if errPut := putRecord(recs, r); errPut != nil {
				return errPut
			}
			if errPut := index.Put([]byte(r.ID), sequenceKey(sequence)); errPut != nil {
				return errPut
			}
			pendingBytes += length
			free -= physicalReserve
			committed++
		}
		if errPut := meta.Put([]byte("pending_bytes"), sequenceKey(uint64(pendingBytes))); errPut != nil {
			return errPut
		}
		if committed > 0 {
			if errPut := meta.Put([]byte("last_commit"), sequenceKey(uint64(time.Now().UnixNano()))); errPut != nil {
				return errPut
			}
		}
		return nil
	})
	if err != nil {
		committed, duplicate, conflict, capacity = 0, 0, 0, 0
	}
	return
}
func padded8(value []byte) []byte {
	if len(value) == 8 {
		return value
	}
	return make([]byte, 8)
}
func pruneTombstones(tx *bbolt.Tx, now time.Time) error {
	return pruneTombstonesWithLimit(tx, now, maxTombstones)
}
func pruneTombstonesWithLimit(tx *bbolt.Tx, now time.Time, limit int) error {
	bucket := tx.Bucket(tombstonesBucket)
	cursor := bucket.Cursor()
	type entry struct {
		key     []byte
		ackedAt int64
	}
	var kept []entry
	for key, value := cursor.First(); key != nil; key, value = cursor.Next() {
		var old tombstone
		if err := json.Unmarshal(value, &old); err != nil {
			return err
		}
		if old.AckedAt <= 0 || now.Sub(time.Unix(0, old.AckedAt)) > tombstoneHorizon {
			if err := cursor.Delete(); err != nil {
				return err
			}
			continue
		}
		kept = append(kept, entry{key: append([]byte(nil), key...), ackedAt: old.AckedAt})
	}
	if len(kept) <= limit {
		return nil
	}
	sort.Slice(kept, func(i, j int) bool {
		if kept[i].ackedAt == kept[j].ackedAt {
			return bytes.Compare(kept[i].key, kept[j].key) < 0
		}
		return kept[i].ackedAt < kept[j].ackedAt
	})
	for _, item := range kept[:len(kept)-limit] {
		if err := bucket.Delete(item.key); err != nil {
			return err
		}
	}
	return nil
}

func (o *outbox) batch(binding string) ([]sourceRecord, error) {
	var batch []sourceRecord
	var size int
	err := o.db.View(func(tx *bbolt.Tx) error {
		if tx.Bucket(pausesBucket).Get([]byte(binding)) != nil {
			return nil
		}
		cursor := tx.Bucket(recordsBucket).Cursor()
		for _, value := cursor.First(); value != nil; _, value = cursor.Next() {
			metadata, payload, err := recordMetadata(value)
			if err != nil {
				return err
			}
			if metadata.Binding != binding || metadata.State != "pending" {
				continue
			}
			if len(batch) > 0 && (len(batch) >= 256 || size+len(payload) > 1<<20) {
				break
			}
			r, err := readRecord(value)
			if err != nil {
				return err
			}
			batch = append(batch, r)
			size += len(payload)
		}
		return nil
	})
	return batch, err
}
func (o *outbox) acknowledge(batch []sourceRecord) error {
	return o.db.Update(func(tx *bbolt.Tx) error {
		recs, index, tombs, meta := tx.Bucket(recordsBucket), tx.Bucket(indexBucket), tx.Bucket(tombstonesBucket), tx.Bucket(metaBucket)
		pendingBytes := binary.BigEndian.Uint64(padded8(meta.Get([]byte("pending_bytes"))))
		for _, r := range batch {
			raw := recs.Get(sequenceKey(r.Sequence))
			if raw == nil {
				continue
			}
			current, err := readRecord(raw)
			if err != nil {
				return err
			}
			if current.ID != r.ID || current.Digest != r.Digest || current.Binding != r.Binding || current.State != "pending" {
				continue
			}
			if err := recs.Delete(sequenceKey(r.Sequence)); err != nil {
				return err
			}
			if err := index.Delete([]byte(r.ID)); err != nil {
				return err
			}
			t, _ := json.Marshal(tombstone{Digest: r.Digest, AckedAt: time.Now().UnixNano()})
			if err := tombs.Put([]byte(r.ID), t); err != nil {
				return err
			}
			pendingBytes -= uint64(len(r.Payload))
		}
		if err := meta.Put([]byte("pending_bytes"), sequenceKey(pendingBytes)); err != nil {
			return err
		}
		return meta.Put([]byte("last_ack"), sequenceKey(uint64(time.Now().UnixNano())))
	})
}
func (o *outbox) pause(binding, reason string) error {
	return o.db.Update(func(tx *bbolt.Tx) error { return tx.Bucket(pausesBucket).Put([]byte(binding), []byte(reason)) })
}
func (o *outbox) quarantine(batch []sourceRecord, reason string) error {
	return o.db.Update(func(tx *bbolt.Tx) error {
		recs := tx.Bucket(recordsBucket)
		for _, r := range batch {
			raw := recs.Get(sequenceKey(r.Sequence))
			if raw == nil {
				continue
			}
			current, err := readRecord(raw)
			if err != nil {
				return err
			}
			if current.Digest != r.Digest {
				continue
			}
			current.State = "quarantined"
			current.Reason = reason
			if err := putRecord(recs, current); err != nil {
				return err
			}
		}
		return nil
	})
}

func (o *outbox) pauseMismatched(current string) error {
	return o.db.Update(func(tx *bbolt.Tx) error {
		cursor := tx.Bucket(recordsBucket).Cursor()
		paused := tx.Bucket(pausesBucket)
		for _, raw := cursor.First(); raw != nil; _, raw = cursor.Next() {
			r, err := readRecord(raw)
			if err != nil {
				return err
			}
			if r.Binding != current && paused.Get([]byte(r.Binding)) == nil {
				if err := paused.Put([]byte(r.Binding), []byte("binding_mismatch")); err != nil {
					return err
				}
			}
		}
		return nil
	})
}
