//go:build windows

package traceflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsPrivateStateDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace-flow-private")
	if err := ensurePrivateStateDir(path); err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateStateDir(path); err != nil {
		t.Fatalf("existing restricted directory: %v", err)
	}
}

func TestWindowsRejectsPublicStateFile(t *testing.T) {
	for _, suffix := range []string{"", ".lock", ".compact.tmp"} {
		t.Run(suffix, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "private-state", "outbox.db")
			box, err := openOutbox(path, 1<<20, 1)
			if err != nil {
				t.Fatal(err)
			}
			if err := box.close(); err != nil {
				t.Fatal(err)
			}
			statePath := path + suffix
			if suffix == ".lock" || suffix == ".compact.tmp" {
				if err := os.WriteFile(statePath, []byte("orphan"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			sd, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;WD)")
			if err != nil {
				t.Fatal(err)
			}
			dacl, _, err := sd.DACL()
			if err != nil {
				t.Fatal(err)
			}
			if err := windows.SetNamedSecurityInfo(statePath, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
				t.Fatal(err)
			}
			if _, err := openOutbox(path, 1<<20, 1); err == nil || !strings.Contains(err.Error(), "grants access to another account") {
				t.Fatalf("expected public state ACL rejection, got %v", err)
			}
			if err := checkExistingPrivateStateFile(statePath); err == nil {
				t.Fatal("opening the outbox changed the public ACL")
			}
		})
	}
}

func TestWindowsRejectsPublicStateDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "public-state")
	if err := ensurePrivateStateDir(path); err != nil {
		t.Fatal(err)
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := openOutbox(filepath.Join(path, "outbox.db"), 1<<20, 1); err == nil {
		t.Fatal("accepted public state directory")
	}
	if err := validatePrivateStateACL(path, "directory"); err == nil {
		t.Fatal("changed public state directory ACL")
	}
}
