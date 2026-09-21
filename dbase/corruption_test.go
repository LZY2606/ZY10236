package dbase

import (
	"encoding/binary"
	"errors"
	"testing"
)

// Direct tests for the structured corruption diagnosis. These construct the
// specific on-disk states a crash can leave behind and assert that reopen or
// reading the affected record returns a *CorruptionError of the expected kind
// (errors.Is(err, ErrCorruption)), never a silently mixed field set.

func openCorruptFixture(t *testing.T, dbf, fpt []byte) (*File, error) {
	t.Helper()
	pair := newFaultPair(dbf, fpt)
	cfg := &Config{
		Converter: recoveryConverter(),
		IO: GenericIO{
			Handle:        pair.dbf,
			RelatedHandle: pair.fpt,
		},
	}
	file, err := OpenTable(cfg)
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = file.Close() })
	return file, nil
}

func corruptionKind(err error) (CorruptionKind, bool) {
	var ce *CorruptionError
	if errors.As(err, &ce) {
		return ce.Kind, true
	}
	return "", false
}

// A DBF shorter than 30 bytes cannot carry a header.
func TestCorruptionDBFHeaderTruncated(t *testing.T) {
	fx := buildRecoveryFixture(t, baselineRows())
	dbf := fx.dbf[:12]
	_, err := openCorruptFixture(t, dbf, fx.fpt)
	if err == nil {
		t.Fatal("expected an error opening a truncated DBF")
	}
	kind, ok := corruptionKind(err)
	if !ok || kind != CorruptDBFHeader {
		t.Fatalf("expected %s, got %v", CorruptDBFHeader, err)
	}
	if !errors.Is(err, ErrCorruption) {
		t.Fatalf("error must match ErrCorruption: %v", err)
	}
}

// A torn FPT header (<8 bytes) is diagnosed on open.
func TestCorruptionFPTHeaderTruncated(t *testing.T) {
	fx := buildRecoveryFixture(t, baselineRows())
	_, err := openCorruptFixture(t, fx.dbf, fx.fpt[:4])
	if err == nil {
		t.Fatal("expected an error opening a truncated FPT")
	}
	kind, ok := corruptionKind(err)
	if !ok || kind != CorruptFPTHeader {
		t.Fatalf("expected %s, got %v", CorruptFPTHeader, err)
	}
}

// Publishing the header count before the record bytes: RowsCount=3 but the
// third record does not physically exist.
func TestCorruptionHeaderPublishedRecordMissing(t *testing.T) {
	fx := buildRecoveryFixture(t, baselineRows())
	dbf := append([]byte(nil), fx.dbf...)
	binary.LittleEndian.PutUint32(dbf[4:8], 3)
	_, err := openCorruptFixture(t, dbf, fx.fpt)
	kind, ok := corruptionKind(err)
	if !ok || kind != CorruptTruncatedRecord {
		t.Fatalf("expected %s for header-ahead state, got %v", CorruptTruncatedRecord, err)
	}
}

// A record whose delete marker is neither 0x20 nor 0x2A is invalid.
func TestCorruptionInvalidRecordMarker(t *testing.T) {
	fx := buildRecoveryFixture(t, baselineRows())
	pair := newFaultPair(fx.dbf, fx.fpt)
	file := openRecoveryFile(t, fx, pair, false)
	// Corrupt the marker of record 0 on disk, then read it.
	pair.dbf.data[int(recLayoutFirstRow)] = 0x00
	_, rerr := file.ReadRow(0)
	kind, ok := corruptionKind(rerr)
	if !ok || kind != CorruptInvalidRecordMarker {
		t.Fatalf("expected %s, got %v", CorruptInvalidRecordMarker, rerr)
	}
}

// A record that is physically cut short (file truncated inside the last row)
// must fail structurally instead of returning zero-filled fields.
func TestCorruptionTruncatedLastRecord(t *testing.T) {
	fx := buildRecoveryFixture(t, baselineRows())
	dbf := fx.dbf[:int(recLayoutFirstRow)+2*recLayoutRowLength-5]
	_, err := openCorruptFixture(t, dbf, fx.fpt)
	kind, ok := corruptionKind(err)
	if !ok || kind != CorruptTruncatedRecord {
		t.Fatalf("expected %s, got %v", CorruptTruncatedRecord, err)
	}
}

// A memo pointer addressing a block beyond the physical FPT is diagnosed when
// the memo is read.
func TestCorruptionMemoReferenceBeyondFile(t *testing.T) {
	fx := buildRecoveryFixture(t, baselineRows())
	pair := newFaultPair(fx.dbf, fx.fpt)
	file := openRecoveryFile(t, fx, pair, false)
	// M1 of record 0 lives at offset FirstRow+15; point it at block 200.
	off := int(recLayoutFirstRow) + 15
	binary.LittleEndian.PutUint32(pair.dbf.data[off:off+4], 200)
	_, _, rerr := file.ReadMemo(pair.dbf.data[off:off+4], file.Column(file.ColumnPosByName("M1")))
	kind, ok := corruptionKind(rerr)
	if !ok || kind != CorruptMemoReference {
		t.Fatalf("expected %s, got %v", CorruptMemoReference, rerr)
	}
}

// A referenced block whose declared length exceeds the FPT is diagnosed.
func TestCorruptionMemoBlockLengthBeyondFile(t *testing.T) {
	fx := buildRecoveryFixture(t, baselineRows())
	fpt := append([]byte(nil), fx.fpt...)
	// Block 8 starts at 512; claim a huge payload.
	binary.BigEndian.PutUint32(fpt[512+4:512+8], 0x00FFFFFF)
	pair := newFaultPair(fx.dbf, fpt)
	file := openRecoveryFile(t, fx, pair, false)
	data, rerr := file.ReadRow(0)
	if rerr != nil {
		t.Fatalf("ReadRow: %v", rerr)
	}
	_, berr := file.BytesToRow(data)
	kind, ok := corruptionKind(berr)
	if !ok || kind != CorruptMemoBlock {
		t.Fatalf("expected %s while decoding memos, got %v", CorruptMemoBlock, berr)
	}
}

// An unknown block signature (neither 0 nor 1) is diagnosed.
func TestCorruptionMemoBlockBadSignature(t *testing.T) {
	fx := buildRecoveryFixture(t, baselineRows())
	fpt := append([]byte(nil), fx.fpt...)
	binary.BigEndian.PutUint32(fpt[512:516], 0x5A5A5A5A)
	pair := newFaultPair(fx.dbf, fpt)
	file := openRecoveryFile(t, fx, pair, false)
	data, _ := file.ReadRow(0)
	_, berr := file.BytesToRow(data)
	kind, ok := corruptionKind(berr)
	if !ok || kind != CorruptMemoBlock {
		t.Fatalf("expected %s, got %v", CorruptMemoBlock, berr)
	}
}

// A reserved-but-unwritten memo block is harmless when unreferenced: the header
// reservation is clamped to the physical size on reopen.
func TestCorruptionReservedBlockClampedOnOpen(t *testing.T) {
	fx := buildRecoveryFixture(t, baselineRows())
	fpt := append([]byte(nil), fx.fpt...)
	// Header says NextFree=20 but the file only contains 12 blocks; nothing
	// references blocks 12..19.
	binary.BigEndian.PutUint32(fpt[0:4], 20)
	file, err := openCorruptFixture(t, fx.dbf, fpt)
	if err != nil {
		t.Fatalf("unreferenced overhang should be clamped, got %v", err)
	}
	if file.memoHeader.NextFree != 12 {
		t.Fatalf("NextFree clamped to %d, want 12", file.memoHeader.NextFree)
	}
}

// Error details carry file/offset/record context for readable failures.
func TestCorruptionErrorContext(t *testing.T) {
	err := NewCorruptionError(CorruptMemoReference, "FPT", "bad block").At(900).AtRecord(3)
	if err.File != "FPT" || err.Offset != 900 || err.Record != 3 {
		t.Fatalf("context lost: %+v", err)
	}
	if !errors.Is(err, ErrCorruption) {
		t.Fatalf("Unwrap must expose ErrCorruption")
	}
	msg := err.Error()
	for _, want := range []string{"invalid_memo_reference", "[FPT]", "offset 900", "record 3"} {
		if !contains(msg, want) {
			t.Fatalf("error message %q missing %q", msg, want)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
