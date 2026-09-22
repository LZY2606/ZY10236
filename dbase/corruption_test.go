package dbase

import (
	"errors"
	"strings"
	"testing"
)

func TestCorruptionErrorWrapping(t *testing.T) {
	base := errors.New("boom")
	ce := newCorruption(CorruptMemoBlock, "FPT", 576, "record 1 column BODY block 9 truncated").wrap(base)

	if !errors.Is(ce, ErrCorrupt) {
		t.Fatalf("errors.Is must detect ErrCorrupt, got %v", ce)
	}
	if !errors.Is(ce, base) {
		t.Fatalf("errors.Is must reach the wrapped read error")
	}
	var got *CorruptionError
	if !errors.As(ce, &got) {
		t.Fatalf("errors.As must recover *CorruptionError")
	}
	if got.Kind != CorruptMemoBlock || got.File != "FPT" || got.Offset != 576 {
		t.Fatalf("unexpected structured fields: %+v", got)
	}
	// Readable failure context required by the recovery contract.
	for _, want := range []string{"FPT", "FPT_MEMO_BLOCK_TRUNCATED", "576", "record 1 column BODY", "boom"} {
		if !strings.Contains(got.Error(), want) {
			t.Fatalf("error %q must mention %q", got.Error(), want)
		}
	}

	// WrapError (the package-wide wrapper used in read paths) must preserve
	// the structured diagnosis through errors.As.
	wrapped := WrapError(ce)
	var fromWrap *CorruptionError
	if !errors.As(wrapped, &fromWrap) {
		t.Fatalf("WrapError must preserve *CorruptionError, chain: %v", wrapped)
	}
	if fromWrap.Kind != CorruptMemoBlock {
		t.Fatalf("wrapped kind changed: %s", fromWrap.Kind)
	}
	if !errors.Is(wrapped, ErrCorrupt) {
		t.Fatalf("wrapped error must still satisfy ErrCorrupt")
	}
}

// TestCheckIntegrityClassifiesDirectCorruption builds byte-level damaged
// tables and asserts each CorruptionKind is diagnosed with a readable offset.
func TestCheckIntegrityClassifiesDirectCorruption(t *testing.T) {
	t.Run("memo pointer beyond file", func(t *testing.T) {
		fx := newRecoveryFixture(t)
		firstRow, _ := seededGeometry(t)
		// NOTE is the first memo column; its 4 byte pointer starts after
		// delete flag (1) + ID (4) + NAME (10).
		off := firstRow + 1 + 4 + 10
		dbf := fx.dbf.Snapshot()
		putLE32(dbf[off:off+4], 999) // block 999 far beyond the FPT
		fx.dbf = newFaultBacking("DBF", dbf)
		res := fx.reopen()
		if res.checkErr == nil && res.firstReadErr() == nil {
			t.Fatalf("expected FPT_MEMO_POINTER corruption")
		}
		assertKind(t, res, CorruptMemoPointer)
		fx.closeReopened(res)
	})

	t.Run("record marker invalid", func(t *testing.T) {
		fx := newRecoveryFixture(t)
		firstRow, _ := seededGeometry(t)
		dbf := fx.dbf.Snapshot()
		dbf[firstRow] = 0x7F // neither 0x20 nor 0x2A
		fx.dbf = newFaultBacking("DBF", dbf)
		res := fx.reopen()
		assertKind(t, res, CorruptRecordMarker)
		fx.closeReopened(res)
	})

	t.Run("record truncated by header count", func(t *testing.T) {
		fx := newRecoveryFixture(t)
		dbf := fx.dbf.Snapshot()
		putLE32(dbf[4:8], 5) // advertise five records, only one physically present
		fx.dbf = newFaultBacking("DBF", dbf)
		res := fx.reopen()
		assertKind(t, res, CorruptRecordTruncated)
		fx.closeReopened(res)
	})

	t.Run("memo block payload truncated", func(t *testing.T) {
		fx := newRecoveryFixture(t)
		// NOTE block begins at 512; widen its declared length past EOF.
		fpt := fx.fpt.Snapshot()
		putBE32(fpt[512+4:512+8], uint32(len(fpt)*2))
		fx.fpt = newFaultBacking("FPT", fpt)
		res := fx.reopen()
		assertKind(t, res, CorruptMemoBlock)
		fx.closeReopened(res)
	})

	t.Run("memo blocks overlap", func(t *testing.T) {
		fx := newRecoveryFixture(t)
		firstRow, _ := seededGeometry(t)
		dbf := fx.dbf.Snapshot()
		// NOTE pointer at +15, BODY pointer at +19. Point BODY at NOTE's block.
		noteOff := firstRow + 1 + 4 + 10
		putLE32(dbf[noteOff+4:noteOff+8], leUint32(dbf[noteOff:noteOff+4]))
		fx.dbf = newFaultBacking("DBF", dbf)
		res := fx.reopen()
		assertKind(t, res, CorruptMemoOverlap)
		fx.closeReopened(res)
	})
}

// TestCheckIntegrityCleanBaseline verifies a freshly seeded table opens with
// no findings and no hard error - the control for the corruption cases.
func TestCheckIntegrityCleanBaseline(t *testing.T) {
	fx := newRecoveryFixture(t)
	res := fx.reopen()
	if res.openErr != nil {
		t.Fatalf("open: %v", res.openErr)
	}
	if res.checkErr != nil {
		t.Fatalf("baseline integrity error: %v", res.checkErr)
	}
	if res.finding.LeakedBlocks != 0 {
		t.Fatalf("baseline free-list gap: %s", res.finding.String())
	}
	if len(res.states) != 1 {
		t.Fatalf("expected 1 readable record, got %d", len(res.states))
	}
	fx.closeReopened(res)
}

func assertKind(t *testing.T, res *reopenResult, want CorruptionKind) {
	t.Helper()
	err := res.checkErr
	if err == nil {
		err = res.firstReadErr()
	}
	if err == nil {
		t.Fatalf("expected corruption %s, got consistent state %+v", want, res.states)
	}
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("error %v does not satisfy ErrCorrupt", err)
	}
	kind, ok := corruptionKind(err)
	if !ok {
		t.Fatalf("expected structured corruption, got %T: %v", err, err)
	}
	if kind != want {
		t.Fatalf("corruption kind = %s, want %s (%v)", kind, want, err)
	}
}

func putLE32(b []byte, v uint32) {
	b[0], b[1], b[2], b[3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24)
}

func putBE32(b []byte, v uint32) {
	b[0], b[1], b[2], b[3] = byte(v>>24), byte(v>>16), byte(v>>8), byte(v)
}

func leUint32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}
