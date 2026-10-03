package traceflow

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/proto"
)

func TestOutboxCrashProcessHelper(t *testing.T) {
	if os.Getenv("TRACEFLOW_TEST_CRASH_HELPER") != "1" {
		return
	}
	box, err := openOutbox(os.Getenv("TRACEFLOW_TEST_CRASH_PATH"), 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	source := testSource(t, box)
	committed, _, _, _, err := box.commit([]sourceRecord{source})
	if err != nil || committed != 1 {
		t.Fatalf("commit: %d %v", committed, err)
	}
	os.Exit(0)
}
func TestCommittedOutboxSurvivesAbruptProcessExit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private-state", "outbox.db")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestOutboxCrashProcessHelper$")
	cmd.Env = append(os.Environ(), "TRACEFLOW_TEST_CRASH_HELPER=1", "TRACEFLOW_TEST_CRASH_PATH="+path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child exit: %v %s", err, output)
	}
	box, err := openOutbox(path, 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer box.close()
	batch, err := box.batch("binding")
	if err != nil || len(batch) != 1 {
		t.Fatalf("replay batch: %d %v", len(batch), err)
	}
	expected, _, reason, err := mapRecord(sampleRecord(), box.installation, box.secret)
	if err != nil {
		t.Fatalf("map: %v %s", err, reason)
	}
	expectedPayload, err := proto.Marshal(expected)
	if err != nil {
		t.Fatal(err)
	}
	if batch[0].Digest != sourceDigest(expectedPayload) || string(batch[0].Payload) != string(expectedPayload) {
		t.Fatal("committed source changed after crash")
	}
}
