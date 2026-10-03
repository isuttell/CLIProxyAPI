//go:build windows

package traceflow

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

func currentUserSID() (*windows.SID, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	return user.User.Sid, nil
}
func allowedStateSID(sid, current, system, admins *windows.SID) bool {
	return sid.Equals(current) || sid.Equals(system) || sid.Equals(admins)
}

func ensurePrivateStateDir(path string) error {
	_, statErr := os.Stat(path)
	newlyCreated := os.IsNotExist(statErr)
	if statErr != nil && !newlyCreated {
		return fmt.Errorf("inspect trace flow state directory: %w", statErr)
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return fmt.Errorf("create trace flow state directory: %w", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("trace flow state path is not a directory")
	}
	current, err := currentUserSID()
	if err != nil {
		return fmt.Errorf("read trace flow state owner: %w", err)
	}
	if newlyCreated {
		sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;" + current.String() + ")(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)")
		if err != nil {
			return fmt.Errorf("prepare trace flow state ACL: %w", err)
		}
		dacl, _, err := sd.DACL()
		if err != nil {
			return err
		}
		if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
			return fmt.Errorf("secure trace flow state directory: %w", err)
		}
	}
	return validatePrivateStateACL(path, "directory")
}

func checkExistingPrivateStateFile(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect trace flow state file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("trace flow state file is not a regular file")
	}
	return validatePrivateStateACL(path, "file")
}

func validatePrivateStateACL(path, kind string) error {
	current, err := currentUserSID()
	if err != nil {
		return fmt.Errorf("read trace flow state owner: %w", err)
	}
	system, err := windows.StringToSid("S-1-5-18")
	if err != nil {
		return err
	}
	admins, err := windows.StringToSid("S-1-5-32-544")
	if err != nil {
		return err
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("inspect trace flow state %s ACL: %w", kind, err)
	}
	if sd == nil {
		return fmt.Errorf("trace flow state %s has no security descriptor", kind)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return err
	}
	if owner == nil || !allowedStateSID(owner, current, system, admins) {
		return fmt.Errorf("trace flow state %s has an untrusted owner", kind)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	if dacl == nil {
		return fmt.Errorf("trace flow state %s has a public ACL", kind)
	}
	userAllowed := false
	for index := uint16(0); index < dacl.AceCount; index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, uint32(index), &ace); err != nil {
			return err
		}
		if ace == nil {
			return fmt.Errorf("trace flow state %s has an invalid ACL entry", kind)
		}
		if ace.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE {
			continue
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return fmt.Errorf("trace flow state %s has an unsupported ACL entry", kind)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !allowedStateSID(sid, current, system, admins) {
			return fmt.Errorf("trace flow state %s grants access to another account", kind)
		}
		if sid.Equals(current) {
			userAllowed = true
		}
	}
	if !userAllowed {
		return fmt.Errorf("trace flow state %s does not grant current user access", kind)
	}
	return nil
}
