//go:build windows

package dbase

import (
	"golang.org/x/sys/windows"
)

type syncer interface {
	Sync() error
}

func syncHandle(handle interface{}, label string) error {
	switch h := handle.(type) {
	case nil:
		return nil
	case *windows.Handle:
		if err := windows.FlushFileBuffers(*h); err != nil {
			return NewErrorf("syncing %s failed", label).Details(err)
		}
		return nil
	case syncer:
		if err := h.Sync(); err != nil {
			return NewErrorf("syncing %s failed", label).Details(err)
		}
		return nil
	default:
		return nil
	}
}
