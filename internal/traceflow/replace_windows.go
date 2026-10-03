//go:build windows

package traceflow

import "golang.org/x/sys/windows"

func replaceOutbox(temp, path string) error {
	source, err := windows.UTF16PtrFromString(temp)
	if err != nil {
		return err
	}
	destination, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(source, destination, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}
