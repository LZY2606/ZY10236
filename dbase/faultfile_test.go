package dbase

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

// faultOp identifies an I/O operation intercepted by the fault-injection
// wrapper used by the crash recovery tests.
type faultOp string

const (
	faultRead     faultOp = "Read"
	faultWrite    faultOp = "Write"
	faultSeek     faultOp = "Seek"
	faultTruncate faultOp = "Truncate"
	faultSync     faultOp = "Sync"
	faultClose    faultOp = "Close"
)

// ErrInjected is the sentinel error returned by every injected failure.
// Individual rules can attach a more specific cause (for example
// io.ErrShortWrite), but every failure keeps matching errors.Is(err,
// ErrInjected) so tests never mistake an injected failure for a real one.
var ErrInjected = errors.New("injected I/O failure")

// faultRule describes one deterministic failure point.
//
// The rule fires on the Handle ("DBF" or "FPT") for the operation Op at the
// Nth matching call (Nth is 1-based and counts only calls of the same handle
// and operation). When OffsetMin >= 0, the call only matches when its target
// offset lies within [OffsetMin, OffsetMax] (OffsetMax < 0 means open ended).
// ShortWrite > 0 turns the failure into a partial write: that many bytes are
// actually persisted (and may extend the file) before ShortCause is returned,
// modelling the Go contract "n > 0 with a non-nil error".
type faultRule struct {
	Handle     string
	Op         faultOp
	Nth        int
	Phase      string // "", "open" or "mutate": restrict counting to that phase
	OffsetMin  int64
	OffsetMax  int64
	ShortWrite int
	ShortCause error
	Cause      error
	HasOffset  bool // true enables OffsetMin/OffsetMax filtering (a zero offset is valid)
}

func (r faultRule) label() string {
	parts := []string{r.Handle, string(r.Op)}
	if r.Nth > 0 {
		phase := r.Phase
		if phase == "" {
			phase = "mutate"
		}
		parts = append(parts, fmt.Sprintf("%s#%d", phase, r.Nth))
	}
	if r.OffsetMin >= 0 {
		if r.OffsetMax < 0 {
			parts = append(parts, fmt.Sprintf("offset>=%d", r.OffsetMin))
		} else {
			parts = append(parts, fmt.Sprintf("offset=%d..%d", r.OffsetMin, r.OffsetMax))
		}
	}
	if r.ShortWrite > 0 {
		parts = append(parts, fmt.Sprintf("short=%d", r.ShortWrite))
	}
	return strings.Join(parts, " ")
}

// faultEvent is one recorded I/O call. Fault-free dry runs enumerate the
// operations of a scenario so the matrix test can inject a failure at every
// single one of them.
type faultEvent struct {
	Index  int
	Handle string
	Op     faultOp
	Phase  string
	Offset int64
	Length int
}

func (e faultEvent) String() string {
	return fmt.Sprintf("#%d %s %s @%d len=%d", e.Index, e.Handle, e.Op, e.Offset, e.Length)
}

// SetPhase tags subsequent calls ("open" vs "mutate") so rules and ordinals
// only count calls belonging to one phase; reopening resets it to "open".
func (f *faultFile) SetPhase(phase string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.phase = phase
}

// faultFile is an in-memory file implementing the same surface the tests need
// from an OS file: ReaderAt, WriterAt, Seeker, Truncater, Syncer, Closer and
// Writer. Writes beyond the current size extend the file; a write landing past
// the end after a seek creates a zero gap, exactly like a regular file.
//
// Sync models durability explicitly: every successful Sync snapshots the
// current contents, and a failing Sync is allowed to lose the delta since the
// last snapshot (SnapshotOnSyncFailure) or keep it. The tests do not rely on
// sleeps or OS cache timing — durability is a property of this model.
type faultFile struct {
	name              string
	mu                sync.Mutex
	data              []byte
	pos               int64
	closed            bool
	rules             []faultRule
	seen              map[string]int
	events            []faultEvent
	phase             string
	lastSync          []byte
	syncFailsRollback bool
}

func newFaultFile(name string, initial []byte) *faultFile {
	data := make([]byte, len(initial))
	copy(data, initial)
	return &faultFile{
		name:     name,
		data:     data,
		seen:     make(map[string]int),
		lastSync: append([]byte(nil), data...),
	}
}

// Snapshot returns a copy of the current bytes.
func (f *faultFile) Snapshot() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]byte, len(f.data))
	copy(out, f.data)
	return out
}

// Size reports the current byte length.
func (f *faultFile) Size() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return int64(len(f.data))
}

// Events returns the recorded dry-run events in call order.
func (f *faultFile) Events() []faultEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]faultEvent, len(f.events))
	copy(out, f.events)
	return out
}

// Arm installs fault rules and re-enables event counting for a fresh run.
func (f *faultFile) Arm(rules ...faultRule) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rules = append(f.rules[:0], rules...)
	f.seen = make(map[string]int)
	f.events = nil
	// Arm starts a fresh fault window without changing the phase; callers tag
	// the phase explicitly (open/mutate/close) before arming.
}

// Disarm removes all rules; event recording continues for diagnostics.
func (f *faultFile) Disarm() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rules = nil
}

// Reopen simulates reopening the same file after a crash: the handle is marked
// usable again, rules are removed and the seek position reset to zero.
func (f *faultFile) Reopen() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = false
	f.pos = 0
	f.rules = nil
	f.phase = ""
	f.seen = make(map[string]int)
}

func (f *faultFile) log(handle string, op faultOp, offset int64, length int) int {
	idx := len(f.events)
	f.events = append(f.events, faultEvent{Index: idx, Handle: handle, Op: op, Phase: f.phase, Offset: offset, Length: length})
	return idx
}

// matchingRule returns the first rule matching the next (handle, op, offset)
// call, if this is its Nth occurrence.
func (f *faultFile) matchingRule(handle string, op faultOp, offset int64, length int) (faultRule, bool) {
	// Two independent ordinals per (handle, op): the total ordinal counts every
	// call regardless of phase, while the phase ordinal only counts calls in
	// the current phase. Phase-scoped rules use the latter, so opening the
	// files never consumes ordinals meant for the mutation window.
	totalKey := handle + "|" + string(op)
	f.seen[totalKey]++
	phaseKey := handle + "|" + string(op) + "#" + f.phase
	f.seen[phaseKey]++
	for _, r := range f.rules {
		if r.Handle != handle || r.Op != op {
			continue
		}
		phaseScoped := r.Phase != ""
		if phaseScoped && r.Phase != f.phase {
			continue
		}
		n := f.seen[totalKey]
		if phaseScoped {
			n = f.seen[handle+"|"+string(op)+"#"+r.Phase]
		}
		if r.Nth > 0 && r.Nth != n {
			continue
		}
		if r.HasOffset {
			end := offset
			if op == faultWrite {
				end = offset + int64(length) - 1
			}
			if offset < r.OffsetMin {
				continue
			}
			if r.OffsetMax >= 0 && end > r.OffsetMax {
				continue
			}
		}
		return r, true
	}
	return faultRule{}, false
}

// grow ensures the data slice can hold n bytes.
func (f *faultFile) grow(n int64) {
	if int64(len(f.data)) < n {
		extended := make([]byte, n)
		copy(extended, f.data)
		f.data = extended
	}
}

// Read implements io.Reader.
func (f *faultFile) Read(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, io.ErrClosedPipe
	}
	idx := f.log(f.name, faultRead, f.pos, len(p))
	if r, ok := f.matchingRule(f.name, faultRead, f.pos, len(p)); ok {
		_ = idx
		return 0, ruleCause(r, ErrInjected)
	}
	if f.pos >= int64(len(f.data)) {
		return 0, io.EOF
	}
	n := copy(p, f.data[f.pos:])
	f.pos += int64(n)
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// ReadAt implements io.ReaderAt.
func (f *faultFile) ReadAt(p []byte, off int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, io.ErrClosedPipe
	}
	f.log(f.name, faultRead, off, len(p))
	if r, ok := f.matchingRule(f.name, faultRead, off, len(p)); ok {
		return 0, ruleCause(r, ErrInjected)
	}
	if off >= int64(len(f.data)) {
		return 0, io.EOF
	}
	n := copy(p, f.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// Write implements io.Writer and supports short-write injection.
func (f *faultFile) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, io.ErrClosedPipe
	}
	idx := f.log(f.name, faultWrite, f.pos, len(p))
	if r, ok := f.matchingRule(f.name, faultWrite, f.pos, len(p)); ok {
		keep := r.ShortWrite
		if keep > len(p) {
			keep = len(p)
		}
		if keep > 0 {
			f.grow(f.pos + int64(keep))
			copy(f.data[f.pos:f.pos+int64(keep)], p[:keep])
			f.pos += int64(keep)
		}
		cause := r.ShortCause
		if cause == nil {
			cause = io.ErrShortWrite
		}
		return keep, fmt.Errorf("%w at %s (%s)", ErrInjected, f.name, faultEvent{Index: idx}.String()+": "+cause.Error())
	}
	f.grow(f.pos + int64(len(p)))
	n := copy(f.data[f.pos:], p)
	f.pos += int64(n)
	return n, nil
}

// WriteAt implements io.WriterAt and supports short-write injection.
func (f *faultFile) WriteAt(p []byte, off int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, io.ErrClosedPipe
	}
	idx := f.log(f.name, faultWrite, off, len(p))
	if r, ok := f.matchingRule(f.name, faultWrite, off, len(p)); ok {
		keep := r.ShortWrite
		if keep > len(p) {
			keep = len(p)
		}
		if keep > 0 {
			f.grow(off + int64(keep))
			copy(f.data[off:off+int64(keep)], p[:keep])
		}
		cause := r.ShortCause
		if cause == nil {
			cause = io.ErrShortWrite
		}
		return keep, fmt.Errorf("%w at %s (%s)", ErrInjected, f.name, faultEvent{Index: idx}.String()+": "+cause.Error())
	}
	f.grow(off + int64(len(p)))
	n := copy(f.data[off:], p)
	return n, nil
}

// Seek implements io.Seeker.
func (f *faultFile) Seek(offset int64, whence int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, io.ErrClosedPipe
	}
	target := offset
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		target = f.pos + offset
	case io.SeekEnd:
		target = int64(len(f.data)) + offset
	}
	f.log(f.name, faultSeek, target, 0)
	if r, ok := f.matchingRule(f.name, faultSeek, target, 0); ok {
		return f.pos, ruleCause(r, ErrInjected)
	}
	if target < 0 {
		return f.pos, errors.New("negative seek position")
	}
	f.pos = target
	return target, nil
}

// Truncate implements a file truncation primitive.
func (f *faultFile) Truncate(size int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return io.ErrClosedPipe
	}
	f.log(f.name, faultTruncate, size, 0)
	if r, ok := f.matchingRule(f.name, faultTruncate, size, 0); ok {
		return ruleCause(r, ErrInjected)
	}
	if size < 0 {
		return errors.New("negative truncate size")
	}
	switch {
	case size < int64(len(f.data)):
		f.data = f.data[:size]
		if f.pos > size {
			f.pos = size
		}
	case size > int64(len(f.data)):
		f.grow(size)
	}
	return nil
}

// Sync implements a durability barrier with explicit failure semantics.
func (f *faultFile) Sync() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return io.ErrClosedPipe
	}
	f.log(f.name, faultSync, 0, 0)
	if r, ok := f.matchingRule(f.name, faultSync, 0, 0); ok {
		if f.syncFailsRollback {
			f.data = append([]byte(nil), f.lastSync...)
		}
		return ruleCause(r, ErrInjected)
	}
	f.lastSync = append([]byte(nil), f.data...)
	return nil
}

// Close implements io.Closer.
func (f *faultFile) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log(f.name, faultClose, 0, 0)
	if r, ok := f.matchingRule(f.name, faultClose, 0, 0); ok {
		return ruleCause(r, ErrInjected) // a failed close leaves the handle open
	}
	f.closed = true
	return nil
}

func ruleCause(r faultRule, fallback error) error {
	cause := r.Cause
	if cause == nil {
		cause = fallback
	}
	return fmt.Errorf("%w: %s failed (%s)", ErrInjected, r.label(), cause.Error())
}

// faultPair groups the two files of one table and keeps their committed
// (last-known-good) bytes for byte-level diagnostics.
type faultPair struct {
	dbf *faultFile
	fpt *faultFile
}

func newFaultPair(dbf, fpt []byte) *faultPair {
	return &faultPair{dbf: newFaultFile("DBF", dbf), fpt: newFaultFile("FPT", fpt)}
}

func (p *faultPair) rulesFor(r faultRule) []faultRule { return []faultRule{r} }

// eventLog merges the dry-run logs of both files in call order. Events are
// appended while the scenario runs, and because every call is serialized
// through the single table's mutexes plus the wrapper's own mutexes, the index
// order matches the actual call order within one file; a merged view sorted by
// index per file is enough to address a rule to one concrete operation.
func (p *faultPair) eventLog() []faultEvent {
	out := append(p.dbf.Events(), p.fpt.Events()...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out
}
