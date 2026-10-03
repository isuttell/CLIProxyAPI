package traceflow

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"go.etcd.io/bbolt"
	"google.golang.org/protobuf/proto"
)

func TestCompactionPreservesIdentitySourceAndTombstones(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private-state", "outbox.db")
	box, err := openOutbox(path, 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	installation := box.installation
	secret := append([]byte(nil), box.secret...)
	first := testSource(t, box)
	acked := commitSource(t, box, first)
	if err := box.acknowledge(acked); err != nil {
		t.Fatal(err)
	}
	record := sampleRecord()
	record.RequestID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	item, id, reason, err := mapRecord(record, box.installation, box.secret)
	if err != nil {
		t.Fatalf("map: %v %s", err, reason)
	}
	payload, err := proto.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	second := sourceRecord{ID: id, Schema: 2, Payload: payload, Digest: sourceDigest(payload), Binding: "binding"}
	pending := commitSource(t, box, second)
	const temporaryBytes = 80 << 20
	if err := box.db.Update(func(tx *bbolt.Tx) error {
		bucket, err := tx.CreateBucketIfNotExists([]byte("temporary-padding"))
		if err != nil {
			return err
		}
		return bucket.Put([]byte("padding"), bytes.Repeat([]byte{'x'}, temporaryBytes))
	}); err != nil {
		t.Fatal(err)
	}
	if err := box.db.Update(func(tx *bbolt.Tx) error { return tx.DeleteBucket([]byte("temporary-padding")) }); err != nil {
		t.Fatal(err)
	}
	// Advance the free-page list through a committed transaction before reopening.
	if err := box.db.Update(func(tx *bbolt.Tx) error { return tx.Bucket(metaBucket).Put([]byte("compaction-test"), []byte("done")) }); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := box.close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".compact.tmp", []byte("incomplete compacted copy"), 0600); err != nil {
		t.Fatal(err)
	}
	box, err = openOutbox(path, 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer box.close()
	if box.compaction != "completed" {
		t.Fatalf("expected startup compaction, got %q", box.compaction)
	}
	if box.installation != installation || !bytes.Equal(box.secret, secret) {
		t.Fatal("compaction changed installation identity")
	}
	replay, err := box.batch("binding")
	if err != nil || len(replay) != 1 || replay[0].ID != pending[0].ID || replay[0].Digest != pending[0].Digest || replay[0].Sequence != pending[0].Sequence || !bytes.Equal(replay[0].Payload, pending[0].Payload) {
		t.Fatal("compaction changed immutable source")
	}
	status, err := box.status("binding")
	if err != nil {
		t.Fatal(err)
	}
	if status.Tombstones != 1 || status.States["pending"].Count != 1 {
		t.Fatalf("compaction changed state: %+v", status)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() >= before.Size()/2 {
		t.Fatalf("compaction did not reclaim space: before=%d after=%d", before.Size(), after.Size())
	}
	if _, err := openOutbox(path, 1<<20, 1); err == nil {
		t.Fatal("second opener bypassed sidecar lock")
	}
	if _, err := os.Stat(path + ".compact.tmp"); !os.IsNotExist(err) {
		t.Fatalf("orphan compact file remains: %v", err)
	}
}
