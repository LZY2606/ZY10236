package dbase

import (
	"errors"
	"fmt"
	"io"
	"sync"
)

// errInjectedIO is the deterministic failure all injected rules return.
// Recovery tests assert errors.Is on it (or on ErrCorrupt when the resulting
// on-disk state is diagnosed as unrecoverable) rather than matching strings.
var errInjectedIO = errors.New("injected I/O failure")

// faultOp records one raw operation against a faultBacking. The log is only
// used for readable failure context (the harness reports the exact mutation
// point); assertions never depend on operation timing or caches.
type faultOp struct {
	file   string
	kind   string
	offset int64
	length int
}

// faultRule fires deterministically: the nth matching operation (1-based;
// n == 0 means "every match") on the named file/kind, optionally constrained to
// an offset window. For writes, shortN makes the rule a short write (write
// shortN bytes, then return an error) instead of a pure failure.
type faultRule struct {
	label      string
	file       string // "", "DBF" or "FPT"
	kind       string // "", "read", "write", "seek", "truncate", "sync", "close"
	n          int    // 1-based match count; 0 = every match
	fromOff    int64  // inclusive start of the offset window
	hasRange   bool   // whether fromOff/toOff constrain the match
	toOff      int64  // inclusive end of the offset window
	shortN     int    // >=0: number of bytes that survive before the error
	shortSizes []int  // optional explicit short-write lengths for this point
	seen       int
	fired      bool
}

func (r *faultRule) matches(file, kind string, off, length int64) bool {
	if r.file != "" && r.file != file {
		return false
	}
	if r.kind != "" && r.kind != kind {
		return false
	}
	if r.hasRange {
		end := off + length - 1
		if length == 0 {
			end = off
		}
		if r.fromOff > end || r.toOff < off {
			return false
		}
	}
	return true
}

// faultBacking is an in-memory file implementing Read, Write, ReadAt,
// WriteAt, Seek, Truncate, Sync and Close. It is the filesystem-free,
// cache-timing-free substrate the recovery contract runs on, so the same
// test exercises GenericIO - the shared layer underneath UnixIO and
// WindowsIO - on every platform.
type faultBacking struct {
	mu     sync.Mutex
	label  string
	data   []byte
	pos    int64
	closed bool
	rules  []*faultRule
	Log    []faultOp
}

func newFaultBacking(label string, initial []byte) *faultBacking {
	d := make([]byte, len(initial))
	copy(d, initial)
	return &faultBacking{label: label, data: d}
}

// rule installs a fault rule. Writes use kind "write" whether they arrive via
// Write or WriteAt; reads use kind "read" likewise.
func (f *faultBacking) rule(r *faultRule) *faultRule {
	f.rules = append(f.rules, r)
	return r
}

func (f *faultBacking) grow(end int64) {
	if end > int64(len(f.data)) {
		f.data = append(f.data, make([]byte, end-int64(len(f.data)))...)
	}
}

func (f *faultBacking) record(kind string, off int64, n int) {
	f.Log = append(f.Log, faultOp{file: f.label, kind: kind, offset: off, length: n})
}

func (f *faultBacking) check(kind string, off, length int64) *faultRule {
	for _, r := range f.rules {
		if r.fired && r.n > 0 {
			continue
		}
		if !r.matches(f.label, kind, off, length) {
			continue
		}
		r.seen++
		if r.n == 0 || r.seen == r.n {
			r.fired = true
			return r
		}
	}
	return nil
}

func (f *faultBacking) Len() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.data)
}

// Snapshot returns an independent copy of the bytes, used to persist the
// minimal byte difference of every fault point.
func (f *faultBacking) Snapshot() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]byte, len(f.data))
	copy(out, f.data)
	return out
}

func (f *faultBacking) Seek(offset int64, whence int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var np int64
	switch whence {
	case io.SeekStart:
		np = offset
	case io.SeekCurrent:
		np = f.pos + offset
	case io.SeekEnd:
		np = int64(len(f.data)) + offset
	default:
		return 0, fmt.Errorf("invalid whence %d", whence)
	}
	if np < 0 {
		return 0, errors.New("negative seek position")
	}
	f.pos = np
	f.record("seek", np, 0)
	if r := f.check("seek", np, 0); r != nil {
		return np, fmt.Errorf("%w: %s", errInjectedIO, r.label)
	}
	return np, nil
}

// applyWrite returns n>0 together with the injected error for short writes,
// exactly as an os.File write is allowed to do, and persists exactly n bytes.
func (f *faultBacking) applyWrite(kind string, p []byte, off int64, advance bool) (int, error) {
	if r := f.check("write", off, int64(len(p))); r != nil {
		n := r.shortN
		if n < 0 {
			n = 0
		}
		if n > len(p) {
			n = len(p)
		}
		if n > 0 {
			f.grow(off + int64(n))
			copy(f.data[off:off+int64(n)], p[:n])
			if advance {
				f.pos = off + int64(n)
			}
		}
		f.record(kind, off, n)
		return n, fmt.Errorf("%w: %s", errInjectedIO, r.label)
	}
	f.grow(off + int64(len(p)))
	n := copy(f.data[off:], p)
	if advance {
		f.pos = off + int64(n)
	}
	f.record(kind, off, n)
	return n, nil
}

func (f *faultBacking) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.applyWrite("write", p, f.pos, true)
}

func (f *faultBacking) WriteAt(p []byte, off int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.applyWrite("writeat", p, off, false)
}

func (f *faultBacking) doRead(kind string, p []byte, off int64, advance bool) (int, error) {
	if r := f.check("read", off, int64(len(p))); r != nil {
		f.record(kind, off, 0)
		return 0, fmt.Errorf("%w: %s", errInjectedIO, r.label)
	}
	if off >= int64(len(f.data)) {
		f.record(kind, off, 0)
		return 0, io.EOF
	}
	n := copy(p, f.data[off:])
	if advance {
		f.pos = off + int64(n)
	}
	f.record(kind, off, n)
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (f *faultBacking) Read(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.doRead("read", p, f.pos, true)
}

func (f *faultBacking) ReadAt(p []byte, off int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.doRead("readat", p, off, false)
}

func (f *faultBacking) Truncate(size int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r := f.check("truncate", size, 0); r != nil {
		f.record("truncate", size, 0)
		return fmt.Errorf("%w: %s", errInjectedIO, r.label)
	}
	f.grow(size)
	f.data = f.data[:size]
	if f.pos > size {
		f.pos = size
	}
	f.record("truncate", size, 0)
	return nil
}

func (f *faultBacking) Sync() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("sync", 0, 0)
	if r := f.check("sync", 0, 0); r != nil {
		return fmt.Errorf("%w: %s", errInjectedIO, r.label)
	}
	return nil
}

func (f *faultBacking) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r := f.check("close", 0, 0); r != nil {
		f.record("close", 0, 0)
		return fmt.Errorf("%w: %s", errInjectedIO, r.label)
	}
	f.closed = true
	f.record("close", 0, 0)
	return nil
}

// fired reports whether any rule fired.
func (f *faultBacking) fired() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.rules {
		if r.fired {
			return true
		}
	}
	return false
}

var _ interface {
	io.Reader
	io.Writer
	io.ReaderAt
	io.WriterAt
	io.Seeker
	io.Closer
} = (*faultBacking)(nil)
