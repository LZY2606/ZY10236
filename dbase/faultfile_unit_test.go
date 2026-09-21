package dbase

import (
	"errors"
	"io"
	"testing"
)

// Direct tests of the fault-injection wrapper itself: the wrapper is the
// foundation of the recovery matrix, so its primitive semantics are pinned
// independently.

func TestFaultFileBasicReadWriteSeek(t *testing.T) {
	f := newFaultFile("DBF", []byte("hello world"))
	buf := make([]byte, 5)
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if n, err := f.Read(buf); err != nil || string(buf) != "hello" {
		t.Fatalf("read n=%d buf=%q err=%v", n, buf, err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if n, err := f.Write([]byte("HELLO")); err != nil || n != 5 {
		t.Fatalf("write n=%d err=%v", n, err)
	}
	if got := string(f.Snapshot()); got != "HELLO world" {
		t.Fatalf("snapshot = %q", got)
	}
}

func TestFaultFileWriteExtendsWithZeroGap(t *testing.T) {
	f := newFaultFile("DBF", []byte("abc"))
	if _, err := f.Seek(6, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if n, err := f.Write([]byte("XYZ")); err != nil || n != 3 {
		t.Fatalf("write n=%d err=%v", n, err)
	}
	got := f.Snapshot()
	if string(got) != "abc\x00\x00\x00XYZ" {
		t.Fatalf("gap write = % x", got)
	}
}

func TestFaultFileWriteAt(t *testing.T) {
	f := newFaultFile("DBF", make([]byte, 8))
	if n, err := f.WriteAt([]byte{0xde, 0xad}, 2); err != nil || n != 2 {
		t.Fatalf("writeat n=%d err=%v", n, err)
	}
	got := f.Snapshot()
	if got[2] != 0xde || got[3] != 0xad {
		t.Fatalf("writeat bytes wrong: % x", got)
	}
	if _, err := f.ReadAt(make([]byte, 2), 2); err != nil {
		t.Fatalf("readat: %v", err)
	}
}

func TestFaultFileTruncate(t *testing.T) {
	f := newFaultFile("DBF", []byte("abcdef"))
	if err := f.Truncate(3); err != nil {
		t.Fatal(err)
	}
	if string(f.Snapshot()) != "abc" {
		t.Fatalf("shrink = %q", f.Snapshot())
	}
	if err := f.Truncate(5); err != nil {
		t.Fatal(err)
	}
	if got := f.Snapshot(); len(got) != 5 || string(got[:3]) != "abc" || got[3] != 0 || got[4] != 0 {
		t.Fatalf("grow = % x", got)
	}
	f.Arm(faultRule{Handle: "DBF", Op: faultTruncate, Nth: 1})
	if err := f.Truncate(5); !errors.Is(err, ErrInjected) {
		t.Fatalf("expected injected truncate error, got %v", err)
	}
}

func TestFaultFileShortWriteNAndError(t *testing.T) {
	f := newFaultFile("DBF", make([]byte, 0, 16))
	f.Arm(faultRule{Handle: "DBF", Op: faultWrite, Nth: 1, ShortWrite: 3})
	n, err := f.Write([]byte("abcdef"))
	if err == nil || !errors.Is(err, ErrInjected) {
		t.Fatalf("expected injected short-write error, got n=%d err=%v", n, err)
	}
	if n != 3 {
		t.Fatalf("short write must persist n>0 bytes, got n=%d", n)
	}
	if string(f.Snapshot()) != "abc" {
		t.Fatalf("partial payload = %q", f.Snapshot())
	}
}

func TestFaultFileOffsetRangeRule(t *testing.T) {
	f := newFaultFile("DBF", make([]byte, 32))
	f.Arm(faultRule{Handle: "DBF", Op: faultWrite, Nth: 1, OffsetMin: 8, OffsetMax: 15, HasOffset: true})
	// Out of range write succeeds.
	if _, err := f.WriteAt([]byte("ok"), 0); err != nil {
		t.Fatalf("out-of-range write should succeed: %v", err)
	}
	// In range write fails (counts as the first matching write; earlier writes
	// at offset 0 are not counted for the range rule because the rule counter
	// counts all writes of the handle regardless of range - disambiguate with
	// a fresh file instead).
	g := newFaultFile("DBF", make([]byte, 32))
	g.Arm(faultRule{Handle: "DBF", Op: faultWrite, Nth: 1, OffsetMin: 8, OffsetMax: 15, HasOffset: true})
	if _, err := g.WriteAt([]byte("xx"), 10); !errors.Is(err, ErrInjected) {
		t.Fatalf("in-range write should fail, got %v", err)
	}
}

func TestFaultFileNthOperation(t *testing.T) {
	f := newFaultFile("DBF", []byte("0123456789"))
	f.Arm(faultRule{Handle: "DBF", Op: faultRead, Nth: 2})
	buf := make([]byte, 2)
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Read(buf); err != nil {
		t.Fatalf("first read should succeed: %v", err)
	}
	if _, err := f.Read(buf); !errors.Is(err, ErrInjected) {
		t.Fatalf("second read should be injected, got %v", err)
	}
}

func TestFaultFileSyncBarrier(t *testing.T) {
	f := newFaultFile("DBF", []byte("base"))
	if _, err := f.WriteAt([]byte("DATA"), 4); err != nil {
		t.Fatal(err)
	}
	// Failed sync with rollback restores the last barrier.
	f.syncFailsRollback = true
	f.Arm(faultRule{Handle: "DBF", Op: faultSync, Nth: 1})
	if err := f.Sync(); !errors.Is(err, ErrInjected) {
		t.Fatalf("expected injected sync error, got %v", err)
	}
	if string(f.Snapshot()) != "base" {
		t.Fatalf("rollback = %q", f.Snapshot())
	}
	// After a successful barrier, later delta is rolled back to the new one.
	f.Disarm()
	f.syncFailsRollback = false
	if _, err := f.WriteAt([]byte("new!"), 4); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("??"), 4); err != nil {
		t.Fatal(err)
	}
	f.syncFailsRollback = true
	f.Arm(faultRule{Handle: "DBF", Op: faultSync, Nth: 1})
	_ = f.Sync()
	if string(f.Snapshot()) != "basenew!" {
		t.Fatalf("rollback to latest barrier = %q", f.Snapshot())
	}
}

func TestFaultFileCloseAndReopen(t *testing.T) {
	f := newFaultFile("DBF", []byte("abc"))
	if _, err := f.Write([]byte("def")); err != nil {
		t.Fatal(err)
	}
	f.Arm(faultRule{Handle: "DBF", Op: faultClose, Nth: 1})
	if err := f.Close(); !errors.Is(err, ErrInjected) {
		t.Fatalf("expected injected close error, got %v", err)
	}
	// A failed close leaves the handle usable: the caller may retry.
	if _, err := f.Write([]byte("x")); err != nil {
		t.Fatalf("write after failed close should still work: %v", err)
	}
	f.Disarm()
	if err := f.Close(); err != nil {
		t.Fatalf("retry close: %v", err)
	}
	// Once really closed, writes fail.
	if _, err := f.Write([]byte("x")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("closed write err = %v", err)
	}
	f.Reopen()
	if _, err := f.Write([]byte("XYZ")); err != nil {
		t.Fatalf("write after reopen: %v", err)
	}
}
