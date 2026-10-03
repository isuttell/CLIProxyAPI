package traceflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go.etcd.io/bbolt"
	"google.golang.org/protobuf/proto"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMaintenanceRebindRequeueDiscardAndLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private-state", "outbox.db")
	oldKey := "TF_TEST_OLD_KEY"
	newKey := "TF_TEST_NEW_KEY"
	t.Setenv(oldKey, "synthetic-old-key")
	t.Setenv(newKey, "synthetic-new-key")
	cfg := Config{Endpoint: "http://localhost:5555/v1/traces", OutboxPath: path, APIKeyEnv: newKey, MaxPendingBytes: 1 << 20, MinFreeBytes: 1}
	if _, err := OpenMaintenance(cfg); err == nil {
		t.Fatal("maintenance created missing outbox")
	}
	box, err := openOutbox(path, 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	old := bindingID(box.secret, "http://localhost:5555/v1/traces", os.Getenv(oldKey))
	newBinding := bindingID(box.secret, "http://localhost:5555/v1/traces", os.Getenv(newKey))
	source := testSource(t, box)
	source.Binding = old
	commitSource(t, box, source)
	if _, err := OpenMaintenance(cfg); err == nil {
		t.Fatal("maintenance opened locked outbox")
	}
	if err := box.close(); err != nil {
		t.Fatal(err)
	}
	m, err := OpenMaintenance(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	status, err := m.Status()
	if err != nil {
		t.Fatal(err)
	}
	if status.Bindings[old].Count != 1 || status.CurrentBinding != newBinding {
		t.Fatalf("rotation not visible: %+v", status)
	}
	if err := m.Rebind(old, strings.Repeat("0", 64)); err == nil {
		t.Fatal("rebind accepted wrong destination")
	}
	if err := m.Rebind(old, newBinding); err != nil {
		t.Fatal(err)
	}
	status, _ = m.Status()
	if status.Bindings[old].Count != 0 || status.Bindings[newBinding].Count != 1 {
		t.Fatal("rebind failed")
	}
	batch, err := m.box.batch(newBinding)
	if err != nil || len(batch) != 1 {
		t.Fatalf("batch after rebind: %d %v", len(batch), err)
	}
	originalDigest := batch[0].Digest
	if err := m.box.quarantine(batch, "usage_total_invariant"); err != nil {
		t.Fatal(err)
	}
	if err := m.Requeue("reason:usage_total_invariant"); err != nil {
		t.Fatal(err)
	}
	batch, err = m.box.batch(newBinding)
	if err != nil || len(batch) != 1 || batch[0].Digest != originalDigest {
		t.Fatal("requeue changed source")
	}
	if err := m.Discard("execution:"+source.ID, false); err == nil {
		t.Fatal("discard lacked confirmation")
	}
	if err := m.Discard("execution:"+source.ID, true); err != nil {
		t.Fatal(err)
	}
	status, _ = m.Status()
	if status.States["pending"].Count != 0 || status.Tombstones != 1 || status.Discards != 1 {
		t.Fatalf("discard failed: %+v", status)
	}
}
func TestEndpointNormalizationPreservesPath(t *testing.T) {
	normalized, err := normalizeEndpoint("HTTPS://EXAMPLE.COM:443/v1/traces")
	if err != nil {
		t.Fatal(err)
	}
	if normalized != "https://example.com/v1/traces" {
		t.Fatalf("normalized path: %s", normalized)
	}
	for _, endpoint := range []string{"http://example.com/v1/traces", "https://name:secret@example.com/v1/traces", "https://example.com/v1/traces?key=secret", "https://example.com/v1/traces#fragment", "https://example.com/base"} {
		if _, err := normalizeEndpoint(endpoint); err == nil {
			t.Errorf("accepted bad endpoint %q", endpoint)
		}
	}
}
func TestBudgetPreservesExistingRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private-state", "outbox.db")
	box, err := openOutbox(path, 1024, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer box.close()
	first := testSource(t, box)
	first.Payload = []byte(strings.Repeat("x", 600))
	first.Digest = sourceDigest(first.Payload)
	committed, _, _, capacity, err := box.commit([]sourceRecord{first})
	if err != nil || committed != 1 || capacity != 0 {
		t.Fatalf("first commit: %d %d %v", committed, capacity, err)
	}
	second := first
	second.ID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	second.Payload = []byte(strings.Repeat("y", 600))
	second.Digest = sourceDigest(second.Payload)
	committed, _, _, capacity, err = box.commit([]sourceRecord{second})
	if err != nil || committed != 0 || capacity != 1 {
		t.Fatalf("capacity: %d %d %v", committed, capacity, err)
	}
	status, _ := box.status("binding")
	if status.States["pending"].Count != 1 || status.States["pending"].Bytes != 600 {
		t.Fatalf("existing record lost: %+v", status)
	}
}

func TestPendingBytesAcrossCommitsAcknowledgeAndDiscard(t *testing.T) {
	box := testOutbox(t)
	makeSource := func(id string) sourceRecord {
		r := sampleRecord()
		r.RequestID = id
		item, actual, reason, err := mapRecord(r, box.installation, box.secret)
		if err != nil {
			t.Fatalf("map: %v %s", err, reason)
		}
		payload, err := proto.Marshal(item)
		if err != nil {
			t.Fatal(err)
		}
		return sourceRecord{ID: actual, Schema: 2, Payload: payload, Digest: sourceDigest(payload), Binding: "binding"}
	}
	first := makeSource("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa1")
	second := makeSource("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa2")
	third := makeSource("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa3")
	for _, r := range []sourceRecord{first, second} {
		committed, _, _, capacity, err := box.commit([]sourceRecord{r})
		if err != nil || committed != 1 || capacity != 0 {
			t.Fatalf("separate commit: %d %d %v", committed, capacity, err)
		}
	}
	status, err := box.status("binding")
	if err != nil {
		t.Fatal(err)
	}
	expected := int64(len(first.Payload) + len(second.Payload))
	if status.States["pending"].Count != 2 || status.States["pending"].Bytes != expected {
		t.Fatalf("after separate commits: %+v", status.States)
	}
	batch, err := box.batch("binding")
	if err != nil || len(batch) != 2 {
		t.Fatalf("batch: %d %v", len(batch), err)
	}
	if err := box.acknowledge(batch[:1]); err != nil {
		t.Fatal(err)
	}
	committed, _, _, capacity, err := box.commit([]sourceRecord{third})
	if err != nil || committed != 1 || capacity != 0 {
		t.Fatalf("commit after ack: %d %d %v", committed, capacity, err)
	}
	status, err = box.status("binding")
	if err != nil {
		t.Fatal(err)
	}
	if status.States["pending"].Count != 2 || status.States["pending"].Bytes != int64(len(second.Payload)+len(third.Payload)) || status.Tombstones != 1 {
		t.Fatalf("after ack/new commit: %+v", status)
	}
	maintenance := &Maintenance{box: box}
	if err := maintenance.Discard("execution:"+third.ID, true); err != nil {
		t.Fatal(err)
	}
	fourth := makeSource("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa4")
	committed, _, _, capacity, err = box.commit([]sourceRecord{fourth})
	if err != nil || committed != 1 || capacity != 0 {
		t.Fatalf("commit after discard: %d %d %v", committed, capacity, err)
	}
	status, err = box.status("binding")
	if err != nil {
		t.Fatal(err)
	}
	if status.States["pending"].Count != 2 || status.States["pending"].Bytes != int64(len(second.Payload)+len(fourth.Payload)) || status.Discards != 1 {
		t.Fatalf("after discard/new commit: %+v", status)
	}
}

func TestStartupRepairsPendingByteIndexWithoutChangingSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private-state", "outbox.db")
	box, err := openOutbox(path, 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	source := testSource(t, box)
	commitSource(t, box, source)
	if err := box.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(metaBucket).Put([]byte("pending_bytes"), sequenceKey(uint64(time.Now().UnixNano())))
	}); err != nil {
		t.Fatal(err)
	}
	if err := box.close(); err != nil {
		t.Fatal(err)
	}
	box, err = openOutbox(path, 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer box.close()
	status, err := box.status("binding")
	if err != nil {
		t.Fatal(err)
	}
	if status.StorageRepairs != 1 || status.States["pending"].Bytes != int64(len(source.Payload)) {
		t.Fatalf("repair status: %+v", status)
	}
	second := source
	second.ID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	second.Payload = append([]byte(nil), source.Payload...)
	second.Payload = append(second.Payload, 1)
	second.Digest = sourceDigest(second.Payload)
	committed, _, _, capacity, err := box.commit([]sourceRecord{second})
	if err != nil || committed != 1 || capacity != 0 {
		t.Fatalf("commit after repair: %d %d %v", committed, capacity, err)
	}
	batch, err := box.batch("binding")
	if err != nil || len(batch) != 2 || batch[0].Digest != source.Digest {
		t.Fatal("repair changed source")
	}
}

func TestExistingOutboxCannotRegenerateInstallation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private-state", "outbox.db")
	if err := ensurePrivateStateDir(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	empty, err := bbolt.Open(path, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := empty.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := openOutbox(path, 1<<20, 1); err == nil {
		t.Fatal("regenerated installation for existing empty database")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	box, err := openOutbox(path, 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	source := testSource(t, box)
	commitSource(t, box, source)
	if err := box.db.Update(func(tx *bbolt.Tx) error {
		meta := tx.Bucket(metaBucket)
		if err := meta.Delete([]byte("installation")); err != nil {
			return err
		}
		return meta.Delete([]byte("secret"))
	}); err != nil {
		t.Fatal(err)
	}
	if err := box.close(); err != nil {
		t.Fatal(err)
	}
	if _, err := openOutbox(path, 1<<20, 1); err == nil {
		t.Fatal("regenerated installation over committed backlog")
	}
}

func TestTombstoneLimitPrunesOldestAcknowledgement(t *testing.T) {
	box := testOutbox(t)
	now := time.Now()
	if err := box.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(tombstonesBucket)
		for _, item := range []struct {
			id  string
			age time.Duration
		}{{"a", time.Hour}, {"b", 3 * time.Hour}, {"c", 2 * time.Hour}} {
			raw, err := json.Marshal(tombstone{Digest: "synthetic", AckedAt: now.Add(-item.age).UnixNano()})
			if err != nil {
				return err
			}
			if err := bucket.Put([]byte(item.id), raw); err != nil {
				return err
			}
		}
		return pruneTombstonesWithLimit(tx, now, 2)
	}); err != nil {
		t.Fatal(err)
	}
	if err := box.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(tombstonesBucket)
		if bucket.Get([]byte("b")) != nil || bucket.Get([]byte("a")) == nil || bucket.Get([]byte("c")) == nil {
			return errors.New("tombstone pruning did not choose oldest ACK")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRestartKeyRotationHoldsOldBindingUntilAuditedRebind(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(503) }))
	defer server.Close()
	t.Setenv("TF_TEST_ROTATE_KEY", "synthetic-old-key")
	cfg := Config{Enabled: true, Endpoint: server.URL + "/v1/traces", OutboxPath: filepath.Join(t.TempDir(), "private-state", "outbox.db"), APIKeyEnv: "TF_TEST_ROTATE_KEY", MaxPendingBytes: 1 << 20, MinFreeBytes: 1}
	old, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	oldBinding := old.binding
	old.HandleUsage(context.Background(), sampleRecord())
	if err := old.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TF_TEST_ROTATE_KEY", "synthetic-new-key")
	current, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	newBinding := current.binding
	if newBinding == oldBinding {
		t.Fatal("key rotation retained binding")
	}
	status, err := current.Status()
	if err != nil {
		t.Fatal(err)
	}
	if status.Bindings[oldBinding].Count != 1 || status.Pauses[oldBinding] != "binding_mismatch" || status.CurrentBinding != newBinding {
		t.Fatalf("rotation status: %+v", status)
	}
	count := requests.Load()
	timer := time.NewTimer(750 * time.Millisecond)
	<-timer.C
	if requests.Load() != count {
		t.Fatal("old binding uploaded under rotated key")
	}
	if err := current.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	maintenance, err := OpenMaintenance(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer maintenance.Close()
	var original sourceRecord
	if err := maintenance.box.db.View(func(tx *bbolt.Tx) error {
		_, raw := tx.Bucket(recordsBucket).Cursor().First()
		var err error
		original, err = readRecord(raw)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := maintenance.Rebind(oldBinding, newBinding); err != nil {
		t.Fatal(err)
	}
	rebound, err := maintenance.box.batch(newBinding)
	if err != nil || len(rebound) != 1 || rebound[0].Digest != original.Digest || !bytes.Equal(rebound[0].Payload, original.Payload) {
		t.Fatal("rebind changed immutable source")
	}
	if err := maintenance.box.db.View(func(tx *bbolt.Tx) error {
		if tx.Bucket(auditBucket).Stats().KeyN != 1 {
			return errors.New("missing rebind audit")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := maintenance.box.pause(oldBinding, "binding_mismatch"); err != nil {
		t.Fatal(err)
	}
	if err := maintenance.box.pause(newBinding, "authentication_rejected"); err != nil {
		t.Fatal(err)
	}
	if err := maintenance.Resume(); err != nil {
		t.Fatal(err)
	}
	status, err = maintenance.Status()
	if err != nil {
		t.Fatal(err)
	}
	if status.Pauses[newBinding] != "" || status.Pauses[oldBinding] != "binding_mismatch" {
		t.Fatalf("resume cleared wrong binding: %+v", status.Pauses)
	}
}
