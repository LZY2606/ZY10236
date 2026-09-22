//go:build !windows

package dbase

import "os"

// platformHandleSize uses fstat on regular files and falls back to seeking
// for custom GenericIO handles.
func platformHandleSize(h interface{}) (int64, error) {
	if f, ok := h.(*os.File); ok {
		fi, err := f.Stat()
		if err != nil {
			return 0, err
		}
		return fi.Size(), nil
	}
	return seekableSize(h)
}
