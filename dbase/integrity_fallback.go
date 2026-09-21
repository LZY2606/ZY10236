package dbase

import "io"

// fileSizeSeeker contains the seek based size lookup shared by all platform
// implementations for handles that are not the platform native file type
// (GenericIO, fault-injection test handles, custom user handles).
func fileSizeSeeker(target interface{}, label string) (int64, error) {
	handle, ok := target.(io.Seeker)
	if !ok {
		return 0, NewErrorf("%s handle of type %T cannot determine its file size", label, target)
	}
	current, err := handle.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, NewErrorf("failed to record the current offset of the %s handle", label).Details(err)
	}
	size, err := handle.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, NewErrorf("failed to seek to the end of the %s handle", label).Details(err)
	}
	if _, err := handle.Seek(current, io.SeekStart); err != nil {
		return 0, NewErrorf("failed to restore the offset of the %s handle", label).Details(err)
	}
	return size, nil
}
