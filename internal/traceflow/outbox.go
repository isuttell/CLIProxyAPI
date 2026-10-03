package traceflow

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gofrs/flock"
	"github.com/google/uuid"
	"go.etcd.io/bbolt"
)

var (
	metaBucket       = []byte("meta")
	recordsBucket    = []byte("records")
	indexBucket      = []byte("index")
	tombstonesBucket = []byte("tombstones")
	pausesBucket     = []byte("pauses")
	auditBucket      = []byte("audit")
)

const tombstoneHorizon = 7 * 24 * time.Hour
const maxTombstones = 250000

type sourceRecord struct {
	ID        string `json:"id"`
	Schema    int    `json:"schema"`
	Payload   []byte `json:"payload"`
	Digest    string `json:"digest"`
	Binding   string `json:"binding"`
	State     string `json:"state"`
	Reason    string `json:"reason,omitempty"`
	Sequence  uint64 `json:"sequence"`
	CreatedAt int64  `json:"created_at"`
}
type tombstone struct {
	Digest  string `json:"digest"`
	AckedAt int64  `json:"acked_at"`
}
type outbox struct {
	db                     *bbolt.DB
	lock                   *flock.Flock
	installation           uuid.UUID
	secret                 []byte
	path                   string
	compaction             string
	repaired               bool
	maxBytes, minFreeBytes int64
	closeOnce              sync.Once
	closeErr               error
}

func openOutbox(path string, maxBytes, minFreeBytes int64) (*outbox, error) {
	if path == "" {
		return nil, errors.New("trace flow outbox path is required")
	}
	if maxBytes == 0 {
		maxBytes = 256 << 20
	}
	if minFreeBytes == 0 {
		minFreeBytes = 1 << 30
	}
	if maxBytes < 0 || minFreeBytes < 0 {
		return nil, errors.New("negative trace flow storage limit")
	}
	dir := filepath.Dir(path)
	if err := ensurePrivateStateDir(dir); err != nil {
		return nil, err
	}
	for _, statePath := range []string{path, path + ".lock", path + ".compact.tmp"} {
		if err := checkExistingPrivateStateFile(statePath); err != nil {
			return nil, fmt.Errorf("check trace flow state file %s: %w", filepath.Base(statePath), err)
		}
	}
	// flock uses a private cross-platform lock file across compaction rename/reopen.
	lock := flock.New(path + ".lock")
	locked, err := lock.TryLock()
	if err != nil {
		return nil, fmt.Errorf("lock trace flow outbox: %w", err)
	}
	if !locked {
		return nil, errors.New("trace flow outbox already in use")
	}
	_, statErr := os.Stat(path)
	existed := statErr == nil
	if statErr != nil && !os.IsNotExist(statErr) {
		lock.Close()
		return nil, fmt.Errorf("inspect trace flow outbox: %w", statErr)
	}
	db, err := bbolt.Open(path, 0600, &bbolt.Options{Timeout: 100 * time.Millisecond, NoSync: false})
	if err != nil {
		lock.Close()
		return nil, fmt.Errorf("open trace flow outbox: %w", err)
	}
	if _, err := os.Stat(path + ".compact.tmp"); err == nil {
		if errRemove := os.Remove(path + ".compact.tmp"); errRemove != nil {
			db.Close()
			lock.Close()
			return nil, fmt.Errorf("remove incomplete compacted outbox: %w", errRemove)
		}
	}
	if err := os.Chmod(path, 0600); err != nil {
		db.Close()
		lock.Close()
		return nil, fmt.Errorf("secure trace flow outbox: %w", err)
	}
	ob := &outbox{db: db, lock: lock, path: path, maxBytes: maxBytes, minFreeBytes: minFreeBytes}
	err = db.Update(func(tx *bbolt.Tx) error {
		for _, name := range [][]byte{metaBucket, recordsBucket, indexBucket, tombstonesBucket, pausesBucket, auditBucket} {
			if _, errCreate := tx.CreateBucketIfNotExists(name); errCreate != nil {
				return errCreate
			}
		}
		meta := tx.Bucket(metaBucket)
		storedID := meta.Get([]byte("installation"))
		storedSecret := meta.Get([]byte("secret"))
		if (storedID == nil) != (storedSecret == nil) {
			return errors.New("incomplete trace flow installation state")
		}
		if storedID == nil {
			if existed {
				return errors.New("existing trace flow outbox lacks installation state (possible interrupted initialization); remove it only after verifying it contains no records, tombstones, or other state")
			}
			if tx.Bucket(recordsBucket).Stats().KeyN != 0 || tx.Bucket(tombstonesBucket).Stats().KeyN != 0 {
				return errors.New("trace flow backlog without installation state")
			}
			id := uuid.New()
			secret := make([]byte, 32)
			if _, errRead := rand.Read(secret); errRead != nil {
				return errRead
			}
			if errPut := meta.Put([]byte("installation"), []byte(id.String())); errPut != nil {
				return errPut
			}
			if errPut := meta.Put([]byte("secret"), secret); errPut != nil {
				return errPut
			}
			ob.installation = id
			ob.secret = secret
		} else {
			id, errParse := uuid.Parse(string(storedID))
			if errParse != nil || id.Version() != 4 || len(storedSecret) != 32 {
				return errors.New("invalid trace flow installation state")
			}
			ob.installation = id
			ob.secret = append([]byte(nil), storedSecret...)
		}
		var computed uint64
		cursor := tx.Bucket(recordsBucket).Cursor()
		for _, raw := cursor.First(); raw != nil; _, raw = cursor.Next() {
			_, payload, err := recordMetadata(raw)
			if err != nil {
				return err
			}
			computed += uint64(len(payload))
		}
		stored := meta.Get([]byte("pending_bytes"))
		if len(stored) != 8 || binary.BigEndian.Uint64(stored) != computed {
			ob.repaired = len(stored) != 0 || computed != 0
			if err := meta.Put([]byte("pending_bytes"), sequenceKey(computed)); err != nil {
				return err
			}
			if ob.repaired {
				count := binary.BigEndian.Uint64(padded8(meta.Get([]byte("repair_count"))))
				if err := meta.Put([]byte("repair_count"), sequenceKey(count+1)); err != nil {
					return err
				}
			}
		}
		return pruneTombstones(tx, time.Now())
	})
	if err != nil {
		_ = ob.close()
		return nil, fmt.Errorf("initialize trace flow outbox: %w", err)
	}
	result, errCompact := ob.compactIfNeeded()
	if errCompact != nil {
		_ = ob.close()
		return nil, errCompact
	}
	ob.compaction = result
	return ob, nil
}
func (o *outbox) close() error {
	if o == nil {
		return nil
	}
	o.closeOnce.Do(func() {
		if o.db != nil {
			o.closeErr = o.db.Close()
		}
		if o.lock != nil {
			if err := o.lock.Close(); err != nil && o.closeErr == nil {
				o.closeErr = err
			}
		}
	})
	return o.closeErr
}

func sequenceKey(n uint64) []byte { var b [8]byte; binary.BigEndian.PutUint64(b[:], n); return b[:] }
func sourceDigest(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}
func recordMetadata(raw []byte) (sourceRecord, []byte, error) {
	var r sourceRecord
	if len(raw) < 4 {
		return r, nil, errors.New("corrupt trace flow source record")
	}
	metadataLength := int(binary.BigEndian.Uint32(raw[:4]))
	if metadataLength < 0 || metadataLength > len(raw)-4 {
		return r, nil, errors.New("corrupt trace flow source record")
	}
	if err := json.Unmarshal(raw[4:4+metadataLength], &r); err != nil {
		return r, nil, err
	}
	if r.ID == "" || r.Schema != 2 || len(r.Digest) != 64 {
		return r, nil, errors.New("corrupt trace flow source metadata")
	}
	return r, raw[4+metadataLength:], nil
}
func readRecord(raw []byte) (sourceRecord, error) {
	r, payload, err := recordMetadata(raw)
	if err != nil {
		return r, err
	}
	r.Payload = append([]byte(nil), payload...)
	if r.Digest != sourceDigest(r.Payload) {
		return r, errors.New("corrupt trace flow source record")
	}
	return r, nil
}

func putRecord(bucket *bbolt.Bucket, r sourceRecord) error {
	payload := r.Payload
	r.Payload = nil
	metadata, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if len(metadata) > int(^uint32(0)) {
		return errors.New("trace flow source metadata too large")
	}
	raw := make([]byte, 4+len(metadata)+len(payload))
	binary.BigEndian.PutUint32(raw[:4], uint32(len(metadata)))
	copy(raw[4:], metadata)
	copy(raw[4+len(metadata):], payload)
	return bucket.Put(sequenceKey(r.Sequence), raw)
}
