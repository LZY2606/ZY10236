package dbase

import "testing"

// Enumeration of injected fault points for the write operations that move the
// table between two consistent generations. The fault point labels describe
// the semantic location in the write order, which is identical in GenericIO,
// UnixIO and WindowsIO:
//
//	append record:   W dbf header (count + date) -> W dbf record (includes the
//	                 newly published memo pointers)
//	with memos:      per memo field: W fpt header (free-list) -> W fpt block
//	                 (header + payload), the pointer only enters the DBF record
//	delete/update:   same order, record bytes rewritten in place
//
// The rules below target these locations by (file, offset window) instead of
// hardcoded operation counters, so the cases stay readable and independent of
// incidental Seek/Read counts.

// dbfHeaderRule targets the 30 byte DBF header write.
func dbfHeaderRule(label string, shortN int) *faultRule {
	return &faultRule{label: label, file: "DBF", kind: "write", n: 1,
		hasRange: true, fromOff: 0, toOff: 29, shortN: shortN,
		shortSizes: []int{1, 4, 7, 29}}
}

// dbfRecordRule targets the record region starting at FirstRow.
func dbfRecordRule(t *testing.T, firstRow int, label string, shortN int) *faultRule {
	t.Helper()
	return &faultRule{label: label, file: "DBF", kind: "write", n: 1,
		hasRange: true, fromOff: int64(firstRow), toOff: 1 << 30, shortN: shortN}
	// short sizes are supplied per scenario via the matrix callback, which
	// knows the concrete row length.
}

// fptHeaderRule targets the 8 meaningful FPT header bytes (the 504 zero bytes
// padding are a separate rule so the padding-only failure point is covered).
func fptHeaderRule(label string, shortN int) *faultRule {
	return &faultRule{label: label, file: "FPT", kind: "write", n: 1,
		hasRange: true, fromOff: 0, toOff: 7, shortN: shortN,
		shortSizes: []int{1, 3, 4, 7}}
}

func fptHeaderPaddingRule(label string) *faultRule {
	return &faultRule{label: label, file: "FPT", kind: "write", n: 1,
		hasRange: true, fromOff: 8, toOff: 511,
		shortSizes: []int{1, 256, 503}}
}

// fptBlockRule targets a write at or beyond the first data block (offset >=512).
func fptBlockRule(label string, shortN int) *faultRule {
	return &faultRule{label: label, file: "FPT", kind: "write", n: 1,
		hasRange: true, fromOff: 512, toOff: 1 << 30, shortN: shortN,
		shortSizes: []int{1, 4, 8, memoBlockSize - 1, memoBlockSize,
			memoBlockSize + 1, 2 * memoBlockSize}}
}

// runWriteFaultMatrix executes op against fresh fixtures for every supplied
// rule. Each sub-case snapshots the minimal byte difference, performs the
// mutation, reopens and validates the outcome through assertAcceptable.
func runWriteFaultMatrix(t *testing.T, caseName string,
	op func(file *File) error,
	oldGen, newGen recoveryState,
	shortNSizes func(rowLen, firstRow int) []int,
	rules ...*faultRule) {
	t.Helper()
	firstRow, rowLen := seededGeometry(t)
	for _, rule := range rules {
		rule := rule
		shorts := []int{-1} // -1 = pure failure
		if rule.kind == "write" {
			if len(rule.shortSizes) > 0 {
				shorts = append(shorts, rule.shortSizes...)
			} else {
				shorts = append(shorts, shortNSizes(rowLen, firstRow)...)
			}
		}
		for _, shortN := range shorts {
			shortN := shortN
			mode := "fail"
			r := *rule
			if shortN >= 0 {
				mode = "short"
				r.shortN = shortN
			}
			sub := caseName + "/" + r.label + "/" + mode
			t.Run(sub, func(t *testing.T) {
				fx := newRecoveryFixture(t)
				dbf0, fpt0 := fx.snapshot()
				file := fx.openForMutation(&r)
				opErr := op(file)
				if opErr == nil {
					// A rule that never fires is a test bug, not a library bug.
					if !fx.dbf.fired() && !fx.fpt.fired() {
						t.Fatalf("fault rule %q did not fire; operation reported success", r.label)
					}
				} else if !fx.dbf.fired() && !fx.fpt.fired() {
					t.Fatalf("operation failed %v but no fault rule fired", opErr)
				}
				_ = file.Close() // close after crash must never panic/fail fatally
				dbf1, fpt1 := fx.snapshot()
				diffs := []byteDiff{
					diffBytes("DBF", dbf0, dbf1),
					diffBytes("FPT", fpt0, fpt1),
				}
				res := fx.reopen()
				// FPT free-list gaps are the documented, recoverable anomaly
				// whenever a block was reserved before the pointer committed.
				fx.assertAcceptable(sub, opErr, res, oldGen, newGen, diffs, true)
				fx.closeReopened(res)
			})
		}
	}
}

// seededGeometry reports FirstRow/RowLength of a seeded recovery table.
func seededGeometry(t *testing.T) (firstRow, rowLen int) {
	t.Helper()
	fx := newRecoveryFixture(t)
	res := fx.reopen()
	defer fx.closeReopened(res)
	if res.openErr != nil {
		t.Fatal(res.openErr)
	}
	return int(res.file.header.FirstRow), int(res.file.header.RowLength)
}
