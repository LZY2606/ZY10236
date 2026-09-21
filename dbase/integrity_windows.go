//go:build windows
// +build windows

package dbase

import (
	"golang.org/x/sys/windows"
)

// fileSize returns the physical size of the DBF (related=false) or FPT
// (related=true) handle. WindowsIO stores raw windows.Handle values, which are
// queried with GetFileSizeEx. Any other handle type falls back to the seek
// based implementation used by GenericIO.
func fileSize(file *File, related bool) (int64, error) {
	target := file.handle
	label := "DBF"
	if related {
		target = file.relatedHandle
		label = "FPT"
	}
	if handle, ok := target.(*windows.Handle); ok && handle != nil {
		var info windows.ByHandleFileInformation
		if err := windows.GetFileInformationByHandle(*handle, &info); err != nil {
			return 0, NewError("failed to determine the " + label + " file size").Details(err)
		}
		return int64(info.FileSizeHigh)<<32 | int64(info.FileSizeLow), nil
	}
	return fileSizeSeeker(target, label)
}
