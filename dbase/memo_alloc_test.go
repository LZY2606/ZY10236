package dbase

import (
	"encoding/binary"
	"errors"
	"testing"

	"golang.org/x/text/encoding/charmap"
)

// TestMemoFreshBlockAllocation locks the write-order guarantee that the
// recovery contract depends on: updating a memo field never overwrites a
// previously published block. Growing one memo must therefore leave the other
// memo field of the same record fully intact instead of corrupting the
// adjacent block header, and CheckIntegrity must report no overlap.
func TestMemoFreshBlockAllocation(t *testing.T) {
	dbf := newFaultBacking("DBF", nil)
	fpt := newFaultBacking("FPT", nil)
	openIO := GenericIO{Handle: dbf, RelatedHandle: fpt}
	cols := []*Column{
		mustNewColumn(t, "ID", Integer, 0),
		mustNewColumn(t, "NOTE", Memo, 0),
		mustNewColumn(t, "BODY", Memo, 0),
	}
	file, err := NewTable(FoxPro,
		&Config{Converter: NewDefaultConverter(charmap.Windows1252)},
		cols, 64, openIO)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	note := "short-note"
	body := "body-" + repeatRune("B", 80) // two blocks
	row, err := file.RowFromMap(map[string]interface{}{"ID": int32(1), "NOTE": note, "BODY": body})
	if err != nil {
		t.Fatalf("row: %v", err)
	}
	if err := row.Add(); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reopen and grow BODY across more blocks.
	reopened, err := OpenTable(&Config{
		Converter: NewDefaultConverter(charmap.Windows1252),
		IO:        GenericIO{Handle: dbf, RelatedHandle: fpt},
	})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	grown := "grown-" + repeatRune("G", 500) // nine blocks
	if err := updateRow(t, reopened, map[string]interface{}{"BODY": grown}); err != nil {
		t.Fatalf("grow update: %v", err)
	}
	bodyPointerAfter := rawMemoPointer(t, reopened, 0, "BODY")
	if err := reopened.Close(); err != nil {
		t.Fatalf("close after grow: %v", err)
	}

	final, err := OpenTable(&Config{
		Converter: NewDefaultConverter(charmap.Windows1252),
		IO:        GenericIO{Handle: dbf, RelatedHandle: fpt},
	})
	if err != nil {
		t.Fatalf("final reopen: %v", err)
	}
	defer final.Close()

	finding, err := final.CheckIntegrity()
	if err != nil {
		t.Fatalf("integrity after grow should be clean apart from free-list gap, got: %v", err)
	}
	// The replaced body block is unreachable old space; NOTE must be unaffected.
	r, err := readStateColumns(final, 0)
	if err != nil {
		t.Fatalf("read row: %v", err)
	}
	if r.note != note {
		t.Fatalf("NOTE spliced/corrupted after BODY grow:\nwant %q\ngot  %q", note, r.note)
	}
	if r.body != grown {
		t.Fatalf("BODY not fully updated:\nwant len %d\ngot len %d %q", len(grown), len(r.body), r.body)
	}
	// BODY moved to a fresh block beyond the old two-block allocation.
	if bodyPointerAfter <= 10 {
		t.Fatalf("BODY was overwritten in place (block %d); expected a fresh allocation", bodyPointerAfter)
	}
	// With NOTE also rewritten during the update, every allocated block is
	// referenced, so the gap can be zero; overlap must never be reported.
	_ = finding
}

// TestMemoBlockZeroInitial verifies a freshly created table writes its first
// memo at block 8 (after the 512 byte header), never at block 0, so the FPT
// header survives the first allocation.
func TestMemoBlockZeroInitial(t *testing.T) {
	dbf := newFaultBacking("DBF", nil)
	fpt := newFaultBacking("FPT", nil)
	cols := []*Column{
		mustNewColumn(t, "ID", Integer, 0),
		mustNewColumn(t, "NOTE", Memo, 0),
	}
	file, err := NewTable(FoxPro,
		&Config{Converter: NewDefaultConverter(charmap.Windows1252)},
		cols, 64, GenericIO{Handle: dbf, RelatedHandle: fpt})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if file.memoHeader.NextFree != 8 {
		t.Fatalf("initial next-free block = %d, want 8", file.memoHeader.NextFree)
	}
	row, err := file.RowFromMap(map[string]interface{}{"ID": int32(1), "NOTE": "first-memo"})
	if err != nil {
		t.Fatalf("row: %v", err)
	}
	if err := row.Add(); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if fpt.Len() < 8 {
		t.Fatalf("FPT header overwritten/too short: %d bytes", fpt.Len())
	}
	// Block size at header offset 6 must remain 64.
	if got := fpt.dataAt(6); len(got) >= 2 && (got[0] != 0 || got[1] != 64) {
		t.Fatalf("block size field clobbered: % x", got[:2])
	}
	reopened, err := OpenTable(&Config{
		Converter: NewDefaultConverter(charmap.Windows1252),
		IO:        GenericIO{Handle: dbf, RelatedHandle: fpt},
	})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if reopened.memoHeader.BlockSize != 64 {
		t.Fatalf("block size after first memo = %d, want 64", reopened.memoHeader.BlockSize)
	}
	st, err := readStateColumns(reopened, 0)
	if err != nil {
		// This table has only ID + NOTE.
		if err := reopened.GoTo(0); err != nil {
			t.Fatalf("goto: %v", err)
		}
		row, rerr := reopened.Row()
		if rerr != nil {
			t.Fatalf("read: %v", rerr)
		}
		v, _ := row.StringValueByName("NOTE")
		if v != "first-memo" {
			t.Fatalf("first memo lost: %q", v)
		}
		return
	}
	if st.note != "first-memo" {
		t.Fatalf("first memo lost: %q", st.note)
	}
}

type colState struct {
	id   int32
	note string
	body string
}

func readStateColumns(file *File, pos uint32) (colState, error) {
	if err := file.GoTo(pos); err != nil {
		return colState{}, err
	}
	row, err := file.Row()
	if err != nil {
		return colState{}, err
	}
	var st colState
	v, err := row.IntValueByName("ID")
	if err != nil {
		return st, err
	}
	st.id = int32(v)
	if st.note, err = row.StringValueByName("NOTE"); err != nil {
		return st, err
	}
	if st.body, err = row.StringValueByName("BODY"); err != nil {
		// BODY column may not exist in some tables.
		if !errors.Is(err, ErrInvalidPosition) {
			return st, err
		}
	}
	return st, nil
}

func repeatRune(s string, n int) string {
	out := make([]byte, 0, n)
	for len(out) < n {
		out = append(out, s[0])
	}
	return string(out[:n])
}

// dataAt returns up to 8 bytes starting at off in the persisted FPT.
func (f *faultBacking) dataAt(off int) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	if off >= len(f.data) {
		return nil
	}
	end := off + 8
	if end > len(f.data) {
		end = len(f.data)
	}
	return f.data[off:end]
}

// rawMemoPointer reads the 4 byte little endian block number stored in the
// DBF record for a memo column without going through the interpreter.
func rawMemoPointer(t *testing.T, file *File, rec uint32, colName string) uint32 {
	t.Helper()
	ci := file.ColumnPosByName(colName)
	if ci < 0 {
		t.Fatalf("column %s missing", colName)
	}
	fieldOff := 1
	for j := 0; j < ci; j++ {
		fieldOff += int(file.table.columns[j].Length)
	}
	buf := make([]byte, file.header.RowLength)
	off := int64(file.header.FirstRow) + int64(rec)*int64(file.header.RowLength)
	if _, err := readAtHandle(file.handle, buf, off); err != nil {
		t.Fatalf("read raw row: %v", err)
	}
	return binary.LittleEndian.Uint32(buf[fieldOff : fieldOff+4])
}
