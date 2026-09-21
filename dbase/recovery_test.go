package dbase

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"testing"

	"golang.org/x/text/encoding/charmap"
)

// Crash-recovery contract tests.
//
// These tests pin the guarantees the repository's own implementation can make
// about a table (one .DBF and one .FPT file) after a process dies between two
// file updates. They deliberately do not fake cross-file transactions: the
// contract below is derived solely from the write ordering of the code and
// structural invariants of the dBase/FoxPro format.
//
// Promised boundaries (documented in docs/crash-consistency.md):
//
//  1. Memo blocks are reserved (FPT header) and fully written (FPT data)
//     before any address is published in a DBF record; every memo revision is
//     appended at a fresh block, never overwritten in place.
//  2. An appended record is published by "update DBF header count, then write
//     record". A death before/within the record write leaves the old record
//     set readable; the header-ahead state is diagnosed on reopen.
//  3. Reopen never returns a record whose plain fields come from one
//     generation and whose memo contents come from another. Either the whole
//     old generation, the whole new generation, or a structured
//     *CorruptionError (errors.Is(err, ErrCorruption)) is returned.
//
// Everything here runs against the pure-Go GenericIO with the deterministic
// faultFile wrappers, so the same contract executes on Windows and Unix without
// touching the filesystem, the clock or the OS page cache.

const (
	recTestBlockSize = 64 // FPT block size used by the recovery fixtures
	recTestMemoCap   = recTestBlockSize - 8
	// Fixed geometry of the recovery fixture rows:
	// [delete marker 1][ID 4][NAME 10][M1 pointer 4][M2 pointer 4].
	recLayoutColumns = 4
	// The library writes columns starting at 32 and a terminator at
	// 296 + n*32; the reserved header area extends to FirstRow.
	recLayoutFirstRow  = 296 + recLayoutColumns*32
	recLayoutRowLength = 1 + 4 + 10 + 4 + 4
)

// recFixture is one prepared small table with two live rows and two memos.
type recFixture struct {
	columns []*Column
	header  Header
	dbf     []byte
	fpt     []byte
	// baseline values of record 0 (index 0) and record 1
	row0 recValues
	row1 recValues
}

type recValues struct {
	id   int32
	name string
	m1   string
	m2   string
}

func recoveryConverter() EncodingConverter {
	return NewDefaultConverter(charmap.Windows1250)
}

func recoveryColumns() []*Column {
	cols := make([]*Column, 0, 4)
	for _, spec := range []struct {
		name string
		dt   DataType
		len  uint8
	}{
		{"ID", Integer, 0},
		{"NAME", Character, 10},
		{"M1", Memo, 0},
		{"M2", Memo, 0},
	} {
		col, err := NewColumn(spec.name, spec.dt, spec.len, 0, false)
		if err != nil {
			panic(err)
		}
		cols = append(cols, col)
	}
	return cols
}

// buildRecoveryFixture creates a valid FoxPro DBF/FPT pair directly, with
// memo blocks at controlled addresses (block 8/9 for row 0, 10/11 for row 1).
// Building the bytes (instead of writing them through the library) keeps the
// fixture independent of the very allocation behaviour under test and gives
// every unused block a distinct 0xFF fill so a torn 4-byte memo pointer that
// lands on a stale block is structurally diagnosed rather than silently read.
func buildRecoveryFixture(t *testing.T, rows [2]recValues) *recFixture {
	t.Helper()
	cols := recoveryColumns()
	firstRow := uint16(recLayoutFirstRow) // library convention; records begin here
	rowLength := uint16(1)
	for _, c := range cols {
		c.Position = uint32(rowLength)
		rowLength += uint16(c.Length)
	}
	h := Header{
		FileType:   byte(FoxPro),
		Year:       26,
		Month:      9,
		Day:        22,
		RowsCount:  2,
		FirstRow:   firstRow,
		RowLength:  rowLength,
		TableFlags: byte(MemoFlag),
		CodePage:   recoveryConverter().CodePage(),
	}
	var dbf bytes.Buffer
	if err := binary.Write(&dbf, binary.LittleEndian, &h); err != nil {
		t.Fatal(err)
	}
	// Column descriptors begin at offset 32 (the 30-byte header is followed
	// by a two-byte gap), each 32 bytes long; ReadColumns finds the terminator
	// at 32 + n*32 = 160 for four columns.
	if gap := 32 - dbf.Len(); gap > 0 {
		dbf.Write(make([]byte, gap))
	}
	for _, c := range cols {
		if err := binary.Write(&dbf, binary.LittleEndian, c); err != nil {
			t.Fatal(err)
		}
	}
	dbf.WriteByte(byte(ColumnEnd))
	if pad := int(firstRow) - dbf.Len(); pad > 0 {
		dbf.Write(make([]byte, pad))
	}

	conv := recoveryConverter()
	blocks := []struct {
		block uint32
		text  string
	}{
		{8, rows[0].m1}, {9, rows[0].m2}, {10, rows[1].m1}, {11, rows[1].m2},
	}
	blockData := map[uint32][]byte{}
	for _, b := range blocks {
		raw, err := fromUtf8String([]byte(b.text), conv)
		if err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 8)
		binary.BigEndian.PutUint32(buf[:4], 1) // text memo
		binary.BigEndian.PutUint32(buf[4:8], uint32(len(raw)))
		buf = append(buf, raw...)
		if len(buf) < recTestBlockSize {
			pad := make([]byte, recTestBlockSize-len(buf))
			buf = append(buf, pad...)
		}
		blockData[b.block] = buf
	}

	for ri, row := range rows {
		rec := make([]byte, rowLength)
		rec[0] = byte(Active)
		putLE32(rec[1:5], uint32(row.id))
		nameRaw, _ := fromUtf8String([]byte(row.name), conv)
		copy(rec[5:15], appendSpaces(nameRaw, 10))
		putLE32(rec[15:19], blocks[ri*2+0].block)
		putLE32(rec[19:23], blocks[ri*2+1].block)
		dbf.Write(rec)
	}

	// FPT: 512 byte header area, NextFree=12, block size 64, then 12 blocks.
	// Unused blocks are 0xFF filled so pointer tearing cannot find a valid
	// block where no live memo exists.
	fpt := make([]byte, 12*recTestBlockSize)
	binary.BigEndian.PutUint32(fpt[0:4], 12)
	binary.BigEndian.PutUint16(fpt[6:8], recTestBlockSize)
	for blk := uint32(0); blk < 12; blk++ {
		off := blk * recTestBlockSize
		if data, ok := blockData[blk]; ok {
			copy(fpt[off:off+recTestBlockSize], data)
		} else if off >= 512 {
			for i := off; i < off+recTestBlockSize; i++ {
				fpt[i] = 0xFF
			}
		}
	}

	return &recFixture{
		columns: cols,
		header:  h,
		dbf:     dbf.Bytes(),
		fpt:     fpt,
		row0:    rows[0],
		row1:    rows[1],
	}
}

func putLE32(b []byte, v uint32) { binary.LittleEndian.PutUint32(b, v) }

func baselineRows() [2]recValues {
	return [2]recValues{
		{id: 1, name: "alpha", m1: strings.Repeat("a", 20), m2: strings.Repeat("b", 20)},
		{id: 2, name: "beta", m1: strings.Repeat("c", 20), m2: strings.Repeat("d", 20)},
	}
}

// openRecoveryFile opens a fixture through GenericIO backed by fault files.
func openRecoveryFile(t *testing.T, fx *recFixture, pair *faultPair, readonly bool) *File {
	t.Helper()
	cfg := &Config{
		Converter: recoveryConverter(),
		IO: GenericIO{
			Handle:        pair.dbf,
			RelatedHandle: pair.fpt,
		},
	}
	file, err := OpenTable(cfg)
	if err != nil {
		t.Fatalf("opening recovery fixture failed: %v", err)
	}
	return file
}

// rowResult is the generation-classified outcome of reading one row after a
// crash and reopen.
type rowResult struct {
	index   int
	values  *recValues
	deleted bool
	corrupt bool
	err     error
}

// reopenAndInspect reopens both fault files and reads every row, classifying
// each as old-generation, new-generation or a structured corruption.
func reopenAndInspect(t *testing.T, pair *faultPair, fx *recFixture) []rowResult {
	t.Helper()
	pair.dbf.Reopen()
	pair.fpt.Reopen()
	cfg := &Config{
		Converter: recoveryConverter(),
		IO: GenericIO{
			Handle:        pair.dbf,
			RelatedHandle: pair.fpt,
		},
	}
	file, err := OpenTable(cfg)
	if err != nil {
		// Only a structured corruption diagnosis is expected here; any other
		// reopen failure is a test harness error.
		if isCorruption(err) {
			return []rowResult{{index: -1, corrupt: true, err: err}}
		}
		t.Fatalf("reopen returned a non-corruption error: %v", err)
	}
	t.Cleanup(func() {
		pair.dbf.Disarm()
		pair.fpt.Disarm()
		_ = file.Close()
	})
	results := make([]rowResult, 0, file.RowsCount()+1)
	for i := uint32(0); i < file.RowsCount(); i++ {
		data, rerr := file.ReadRow(i)
		if rerr != nil {
			results = append(results, rowResult{index: int(i), corrupt: isCorruption(rerr), err: rerr})
			continue
		}
		row, berr := file.BytesToRow(data)
		if berr != nil {
			results = append(results, rowResult{index: int(i), corrupt: isCorruption(berr), err: berr})
			continue
		}
		rv := &recValues{}
		rv.id = mustInt(t, row)
		rv.name = strings.TrimRight(mustString(t, row, "NAME"), " ")
		m1, m1Err := row.StringValueByName("M1")
		m2, m2Err := row.StringValueByName("M2")
		if m1Err != nil || m2Err != nil {
			results = append(results, rowResult{
				index: int(i), corrupt: isCorruption(errors.Join(m1Err, m2Err)),
				err: fmt.Errorf("m1: %v m2: %v", m1Err, m2Err),
			})
			continue
		}
		rv.m1, rv.m2 = m1, m2
		results = append(results, rowResult{index: int(i), values: rv, deleted: row.Deleted})
	}
	return results
}

func isCorruption(err error) bool { return errors.Is(err, ErrCorruption) }

func mustInt(t *testing.T, row *Row) int32 {
	t.Helper()
	v, err := row.IntValueByName("ID")
	if err != nil {
		t.Fatalf("reading ID: %v", err)
	}
	return int32(v)
}

func mustString(t *testing.T, row *Row, name string) string {
	t.Helper()
	v, err := row.StringValueByName(name)
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return v
}

// expectGeneration asserts that an inspected row equals one of the supplied
// expected generations and never a mix of plain fields and memos from two
// generations.
func expectRowGeneration(t *testing.T, got rowResult, generations ...recValues) {
	t.Helper()
	if got.corrupt {
		return // a structured corruption diagnosis is an allowed outcome
	}
	if got.err != nil {
		t.Fatalf("row %d returned a non-corruption error: %v", got.index, got.err)
	}
	if got.values == nil {
		t.Fatalf("row %d has no values and no error", got.index)
	}
	for _, g := range generations {
		if *got.values == g {
			return
		}
	}
	t.Fatalf("row %d = %+v does not match any committed generation %+v (old/new mixing is forbidden)",
		got.index, *got.values, generations)
}

// byteDiff records the minimal byte-level difference between two snapshots.
type byteDiff struct {
	changed, added, removed int
	firstOffset             int
}

func diffBytes(before, after []byte) byteDiff {
	d := byteDiff{firstOffset: -1}
	common := len(before)
	if len(after) < common {
		common = len(after)
	}
	for i := 0; i < common; i++ {
		if before[i] != after[i] {
			if d.firstOffset < 0 {
				d.firstOffset = i
			}
			d.changed++
		}
	}
	switch {
	case len(after) > len(before):
		d.added = len(after) - len(before)
	case len(before) > len(after):
		d.removed = len(before) - len(after)
	}
	return d
}

func (d byteDiff) String() string {
	return fmt.Sprintf("changed=%d added=%d removed=%d firstOffset=%d", d.changed, d.added, d.removed, d.firstOffset)
}

// matrixOutcome summarizes one injected failure point for the test report.
type matrixOutcome struct {
	scenario string
	rule     faultRule
	dbfDiff  byteDiff
	fptDiff  byteDiff
	results  []rowResult
	writeErr error
}
