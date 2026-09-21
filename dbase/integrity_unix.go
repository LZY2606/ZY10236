//go:build !windows
// +build !windows

package dbase

import (
	"os"
	"reflect"
)

// fileSize returns the physical size of the DBF (related=false) or FPT
// (related=true) *os.File handle through Stat. A non-*os.File handle falls
// back to the seek based implementation so that tests with custom handles keep
// working on Unix as well.
func fileSize(file *File, related bool) (int64, error) {
	target := file.handle
	label := "DBF"
	if related {
		target = file.relatedHandle
		label = "FPT"
	}
	if handle, ok := target.(*os.File); ok && handle != nil && !reflect.ValueOf(handle).IsNil() {
		info, err := handle.Stat()
		if err != nil {
			return 0, NewError("failed to determine the " + label + " file size").Details(err)
		}
		return info.Size(), nil
	}
	return fileSizeSeeker(target, label)
}
