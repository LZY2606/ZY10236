//go:build windows

package dbase

import "golang.org/x/sys/windows"

// platformHandleSize measures Windows raw handles with SetFilePointerEx and
// falls back to seeking for custom GenericIO handles.
func platformHandleSize(h interface{}) (int64, error) {
	if wh, ok := h.(*windows.Handle); ok {
		pos, err := windows.Seek(*wh, 0, 0) // current
		if err != nil {
			return 0, err
		}
		end, err := windows.Seek(*wh, 0, 2) // end
		if err != nil {
			return 0, err
		}
		if _, err := windows.Seek(*wh, pos, 0); err != nil {
			return 0, err
		}
		return end, nil
	}
	return seekableSize(h)
}
