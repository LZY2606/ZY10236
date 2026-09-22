//go:build !windows

package dbase

type syncer interface {
	Sync() error
}

func syncHandle(handle interface{}, label string) error {
	switch h := handle.(type) {
	case nil:
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
