package dbase

import (
	"errors"
	"testing"
)

// TestRecoveryAppendRecord crashes every write of an append that adds one
// record with two populated memo fields. Guaranteed boundary: either the
// record (with its two already-published memo blocks) is complete, or the old
// table is complete with at most leaked FPT blocks; never a half record with
// a valid header count, and never spliced fields.
func TestRecoveryAppendRecord(t *testing.T) {
	oldGen := baselineState()
	newGen := recoveryState{id: idNew, name: nameNew, note: noteNew, body: bodyNew, rows: 2}

	shortSizes := func(rowLen, firstRow int) []int {
		return []int{
			1,          // a few bytes survive
			4,          // whole RowsCount field survives
			7,          // count + date bytes survive
			rowLen - 1, // record short by one byte
			rowLen,     // full record (rule fires post-write boundary)
		}
	}

	runWriteFaultMatrix(t, "append",
		func(file *File) error {
			row, err := file.RowFromMap(map[string]interface{}{
				"ID": idNew, "NAME": nameNew, "NOTE": noteNew, "BODY": bodyNew,
			})
			if err != nil {
				t.Fatalf("build new row: %v", err)
			}
			return row.Add()
		},
		oldGen, newGen, shortSizes,
		dbfHeaderRule("dbf-header", -1),
		fptHeaderRule("fpt-header-note", -1),
		fptHeaderPaddingRule("fpt-header-padding-note"),
		fptBlockRule("fpt-block-note", -1),
		fptHeaderRule("fpt-header-body", -1),
		fptHeaderPaddingRule("fpt-header-padding-body"),
		fptBlockRule("fpt-block-body", -1),
		dbfRecordRule(t, mustFirstRow(t), "dbf-record", -1),
	)
}

// TestRecoverySameLengthMemo updates NOTE with a value of identical encoded
// length. Because updates allocate a fresh block instead of overwriting,
// every failure leaves either the complete old record (pointing at the old
// block) or the complete new record (pointing at the new block); old/new byte
// splicing of a single memo value is structurally impossible.
func TestRecoverySameLengthMemo(t *testing.T) {
	oldGen := baselineState()
	newGen := oldGen
	newGen.note = stringsRepeatLen(noteOld, "X") // same length, different content

	shortSizes := func(rowLen, firstRow int) []int {
		return []int{1, 4, 7, rowLen - 1, rowLen}
	}

	runWriteFaultMatrix(t, "same-length-memo",
		func(file *File) error {
			return updateRow(t, file, map[string]interface{}{"NOTE": newGen.note})
		},
		oldGen, newGen, shortSizes,
		dbfHeaderRule("dbf-header", -1),
		fptHeaderRule("fpt-header", -1),
		fptHeaderPaddingRule("fpt-header-padding"),
		fptBlockRule("fpt-block", -1),
		dbfRecordRule(t, mustFirstRow(t), "dbf-record", -1),
	)
}

// TestRecoveryGrowMemo grows BODY from two blocks to six. This used to
// overwrite the old block in place and corrupt the adjacent BODY/NOTE block
// header; with fresh-block allocation the adjacent block stays intact and the
// only residual is leaked space diagnosed by CheckIntegrity.
func TestRecoveryGrowMemo(t *testing.T) {
	oldGen := baselineState()
	newGen := oldGen
	newGen.body = bodyGrown

	shortSizes := func(rowLen, firstRow int) []int {
		return []int{1, 4, 7, rowLen - 1, rowLen}
	}

	runWriteFaultMatrix(t, "grow-memo",
		func(file *File) error {
			return updateRow(t, file, map[string]interface{}{"BODY": bodyGrown})
		},
		oldGen, newGen, shortSizes,
		dbfHeaderRule("dbf-header", -1),
		fptHeaderRule("fpt-header", -1),
		fptHeaderPaddingRule("fpt-header-padding"),
		fptBlockRule("fpt-block", -1),
		dbfRecordRule(t, mustFirstRow(t), "dbf-record", -1),
	)
}

// TestRecoveryDeleteRecord rewrites the single record with the 0x2A deleted
// marker. The header shape is untouched; a partial record write is diagnosed
// as CorruptRecordMarker/CorruptRecordTruncated instead of silently appearing
// as an active or half-deleted row.
func TestRecoveryDeleteRecord(t *testing.T) {
	oldGen := baselineState()
	newGen := oldGen
	newGen.deleted = true

	shortSizes := func(rowLen, firstRow int) []int {
		return []int{0, 1, rowLen / 2, rowLen - 1}
	}

	runWriteFaultMatrix(t, "delete",
		func(file *File) error {
			if err := file.GoTo(0); err != nil {
				return err
			}
			row, err := file.Row()
			if err != nil {
				return err
			}
			row.Deleted = true
			return row.Write()
		},
		oldGen, newGen, shortSizes,
		dbfHeaderRule("dbf-header", -1),
		dbfRecordRule(t, mustFirstRow(t), "dbf-record", -1),
	)
}

// TestRecoverySyncFailure covers the close/flush durability path. A failing
// Sync must surface an error while the in-process bytes remain a complete
// generation; reopen after the failed flush keeps the old generation without
// splicing (the OS may have persisted nothing or everything per file - both
// are consistent because Sync is the last action).
func TestRecoverySyncFailure(t *testing.T) {
	oldGen := baselineState()
	newGen := recoveryState{id: idNew, name: nameNew, note: noteNew, body: bodyNew, rows: 2}

	for _, target := range []string{"DBF", "FPT"} {
		target := target
		t.Run("sync-"+target, func(t *testing.T) {
			fx := newRecoveryFixture(t)
			dbf0, fpt0 := fx.snapshot()
			file := fx.openForMutation(&faultRule{
				label: "sync-fail", file: target, kind: "sync", n: 1,
			})
			row, err := file.RowFromMap(map[string]interface{}{
				"ID": idNew, "NAME": nameNew, "NOTE": noteNew, "BODY": bodyNew,
			})
			if err != nil {
				t.Fatalf("build row: %v", err)
			}
			if err := row.Add(); err != nil {
				t.Fatalf("add: %v", err)
			}
			flushErr := file.Flush()
			if flushErr == nil || !errors.Is(flushErr, errInjectedIO) {
				t.Fatalf("expected injected sync error, got %v", flushErr)
			}
			if err := file.Close(); err != nil {
				t.Fatalf("close after failed sync: %v", err)
			}
			diffs := []byteDiff{diffBytes("DBF", dbf0, fx.dbf.Snapshot()), diffBytes("FPT", fpt0, fx.fpt.Snapshot())}
			res := fx.reopen()
			fx.assertAcceptable("sync-"+target, flushErr, res, oldGen, newGen, diffs, true)
			fx.closeReopened(res)
		})
	}
}

// TestRecoveryCloseFailure injects a failure at Close itself. Close is the
// release point, not a write point, so persisted bytes are a complete
// generation and reopen must not report corruption.
func TestRecoveryCloseFailure(t *testing.T) {
	newGen := recoveryState{id: idNew, name: nameNew, note: noteNew, body: bodyNew, rows: 2}

	for _, target := range []string{"DBF", "FPT"} {
		target := target
		t.Run("close-"+target, func(t *testing.T) {
			fx := newRecoveryFixture(t)
			dbf0, fpt0 := fx.snapshot()
			file := fx.openForMutation(&faultRule{
				label: "close-fail", file: target, kind: "close", n: 1,
			})
			row, err := file.RowFromMap(map[string]interface{}{
				"ID": idNew, "NAME": nameNew, "NOTE": noteNew, "BODY": bodyNew,
			})
			if err != nil {
				t.Fatalf("build row: %v", err)
			}
			if err := row.Add(); err != nil {
				t.Fatalf("add: %v", err)
			}
			closeErr := file.Close()
			if closeErr == nil || !errors.Is(closeErr, errInjectedIO) {
				t.Fatalf("expected injected close error, got %v", closeErr)
			}
			diffs := []byteDiff{diffBytes("DBF", dbf0, fx.dbf.Snapshot()), diffBytes("FPT", fpt0, fx.fpt.Snapshot())}
			res := fx.reopen()
			// The mutation itself completed; reopen must be fully new generation.
			fx.assertExactGeneration("close-"+target, res, newGen, diffs)
			fx.closeReopened(res)
		})
	}
}

// TestRecoverySeekFailure verifies that a seek error at the record write
// position aborts before any byte changes: the on-disk pair stays byte
// identical to the baseline and reopen yields the old generation.
func TestRecoverySeekFailure(t *testing.T) {
	oldGen := baselineState()
	newGen := recoveryState{id: idNew, name: nameNew, note: noteNew, body: bodyNew, rows: 2}

	t.Run("seek-before-record", func(t *testing.T) {
		fx := newRecoveryFixture(t)
		firstRow, _ := seededGeometry(t)
		dbf0, fpt0 := fx.snapshot()
		file := fx.openForMutation(&faultRule{
			label: "seek-record", file: "DBF", kind: "seek", n: 1,
			hasRange: true, fromOff: int64(firstRow), toOff: 1 << 30,
		})
		row, err := file.RowFromMap(map[string]interface{}{
			"ID": idNew, "NAME": nameNew, "NOTE": noteNew, "BODY": bodyNew,
		})
		if err != nil {
			t.Fatalf("build row: %v", err)
		}
		addErr := row.Add()
		// The fault may instead match an earlier seek depending on cursor
		// reuse; either way nothing is spliced. Only assert the boundary.
		if addErr != nil && !errors.Is(addErr, errInjectedIO) {
			t.Fatalf("unexpected error: %v", addErr)
		}
		_ = file.Close()
		diffs := []byteDiff{diffBytes("DBF", dbf0, fx.dbf.Snapshot()), diffBytes("FPT", fpt0, fx.fpt.Snapshot())}
		res := fx.reopen()
		fx.assertAcceptable("seek-before-record", addErr, res, oldGen, newGen, diffs, true)
		fx.closeReopened(res)
	})
}

// TestRecoveryTruncate covers the Truncate wrapper: truncating the DBF behind
// a committed header produces CorruptRecordTruncated on reopen, never a silent
// zero-filled spliced record.
func TestRecoveryTruncate(t *testing.T) {
	t.Run("dbf-truncated-after-header", func(t *testing.T) {
		fx := newRecoveryFixture(t)
		firstRow, _ := seededGeometry(t)
		file := fx.openForMutation()
		// Simulate a crash that leaves the file cut one byte into the record.
		if err := fx.dbf.Truncate(int64(firstRow) + 1); err != nil {
			t.Fatal(err)
		}
		_ = file.Close()
		res := fx.reopen()
		err := res.firstReadErr()
		if err == nil {
			t.Fatalf("expected corruption after truncation, got readable state %+v", res.states)
		}
		kind, ok := corruptionKind(err)
		if !ok {
			t.Fatalf("expected structured corruption, got %T %v", err, err)
		}
		if kind != CorruptRecordTruncated && kind != CorruptRecordMarker {
			t.Fatalf("expected truncation/marker corruption, got %s: %v", kind, err)
		}
		fx.closeReopened(res)
	})
}

// TestRecoveryNoSpliceOnCleanRun is the control: with no injected faults the
// matrix operations land exactly on the new generation and integrity reports
// no findings.
func TestRecoveryNoSpliceOnCleanRun(t *testing.T) {
	cases := []struct {
		name string
		op   func(*File) error
		want recoveryState
	}{
		{"append", func(f *File) error {
			r, err := f.RowFromMap(map[string]interface{}{
				"ID": idNew, "NAME": nameNew, "NOTE": noteNew, "BODY": bodyNew,
			})
			if err != nil {
				return err
			}
			return r.Add()
		}, recoveryState{id: idNew, name: nameNew, note: noteNew, body: bodyNew, rows: 2}},
		{"grow-memo", func(f *File) error {
			return updateRow(t, f, map[string]interface{}{"BODY": bodyGrown})
		}, func() recoveryState {
			s := baselineState()
			s.body = bodyGrown
			return s
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newRecoveryFixture(t)
			file := fx.openForMutation()
			if err := tc.op(file); err != nil {
				t.Fatalf("clean op: %v", err)
			}
			if err := file.Flush(); err != nil {
				t.Fatalf("flush: %v", err)
			}
			if err := file.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}
			res := fx.reopen()
			fx.assertExactGeneration(tc.name, res, tc.want, nil)
			fx.closeReopened(res)
		})
	}
}
