package dbase

import (
	"encoding/binary"
	"errors"
	"fmt"
	"testing"
)

// Explicit recovery scenarios called out by the fault model. They pin named
// failure windows that are easy to lose in the generated matrix.

// Scenario "memo block reserved but pointer never published": the FPT header
// reservation advances and the block payload lands, but the DBF record write
// fails completely before its bytes are updated. Reopen must return the old
// generation; the new blocks are unreachable space.
func TestRecoveryExplicitMemoReservedPointerUnpublished(t *testing.T) {
	fx := buildRecoveryFixture(t, baselineRows())
	m := sameLengthMutation()
	dbfEvents, _, _ := collectDryRunEvents(t, fx, m)

	// Locate the last mutate-phase write to the DBF record area.
	var recordEvent *faultEvent
	for i := range dbfEvents {
		ev := dbfEvents[i]
		if ev.Phase == "mutate" && ev.Op == faultWrite && ev.Offset == int64(recLayoutFirstRow) {
			recordEvent = &dbfEvents[i]
		}
	}
	if recordEvent == nil {
		t.Fatal("could not locate the DBF record write in the dry-run log")
	}

	pair := newFaultPair(fx.dbf, fx.fpt)
	pair.dbf.Arm(faultRule{
		Handle: "DBF",
		Op:     faultWrite,
		Nth:    opOrdinal(dbfEvents, *recordEvent),
		Phase:  "mutate",
		Cause:  wrapInjected("record pointer never published"),
	})
	file := openRecoveryFile(t, fx, pair, false)
	pair.dbf.SetPhase("mutate")
	pair.fpt.SetPhase("mutate")
	if err := m.run(t, file); err == nil {
		t.Fatal("expected the mutation to fail")
	}
	results := reopenAndInspect(t, pair, fx)
	r0 := findResult(results, 0)
	if r0 == nil || r0.values == nil || *r0.values != m.oldRow0 {
		t.Fatalf("row 0 must be the old generation, got %+v (corrupt=%v err=%v)",
			rowValue(r0), r0 != nil && r0.corrupt, r0)
	}
}

// Scenario "header already written but record not written" for an append: the
// count reaches 3, the record write fails entirely, so the physical DBF is one
// record short of what the header advertises. Reopen must surface
// CorruptTruncatedRecord, not a zero-filled third record.
func TestRecoveryExplicitHeaderWrittenRecordMissing(t *testing.T) {
	fx := buildRecoveryFixture(t, baselineRows())
	m := newAppendMutation()
	dbfEvents, _, _ := collectDryRunEvents(t, fx, m)

	// The mutate-phase write at the first row area is the new record write.
	var recordEvent *faultEvent
	for i := range dbfEvents {
		ev := dbfEvents[i]
		if ev.Phase == "mutate" && ev.Op == faultWrite && ev.Offset == int64(recLayoutFirstRow+2*recLayoutRowLength) {
			recordEvent = &dbfEvents[i]
		}
	}
	if recordEvent == nil {
		t.Fatal("could not locate the appended record write")
	}

	pair := newFaultPair(fx.dbf, fx.fpt)
	pair.dbf.Arm(faultRule{
		Handle: "DBF",
		Op:     faultWrite,
		Nth:    opOrdinal(dbfEvents, *recordEvent),
		Phase:  "mutate",
		Cause:  wrapInjected("record never written"),
	})
	file := openRecoveryFile(t, fx, pair, false)
	pair.dbf.SetPhase("mutate")
	pair.fpt.SetPhase("mutate")
	if err := m.run(t, file); err == nil {
		t.Fatal("expected the append to fail")
	}
	// Header must now advertise 3 records while the file holds 2.
	if binary.LittleEndian.Uint32(pair.dbf.data[4:8]) != 3 {
		t.Fatalf("header count = %d, want 3", binary.LittleEndian.Uint32(pair.dbf.data[4:8]))
	}
	results := reopenAndInspect(t, pair, fx)
	found := false
	var ce *CorruptionError
	for _, r := range results {
		if r.corrupt && errorsAsCorruption(r.err, &ce) && ce.Kind == CorruptTruncatedRecord {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a %s diagnosis, results=%+v", CorruptTruncatedRecord, results)
	}
}

// Short write with n>0 plus an error, mid FPT reservation header: previously
// committed rows stay fully readable and no pointer references the damaged
// reservation.
func TestRecoveryExplicitShortFPTReservation(t *testing.T) {
	fx := buildRecoveryFixture(t, baselineRows())
	m := growMemoMutation()
	_, fptEvents, _ := collectDryRunEvents(t, fx, m)

	// First mutate write at FPT offset 0 is the reservation header update.
	var reservationEvent *faultEvent
	for i := range fptEvents {
		ev := fptEvents[i]
		if ev.Phase == "mutate" && ev.Op == faultWrite && ev.Offset == 0 {
			reservationEvent = &fptEvents[i]
			break
		}
	}
	if reservationEvent == nil {
		t.Fatal("could not locate the FPT reservation write")
	}

	pair := newFaultPair(fx.dbf, fx.fpt)
	pair.fpt.Arm(faultRule{
		Handle:     "FPT",
		Op:         faultWrite,
		Nth:        opOrdinal(fptEvents, *reservationEvent),
		Phase:      "mutate",
		ShortWrite: 4, // only NextFree partially published
		ShortCause: wrapInjected("torn FPT reservation"),
	})
	file := openRecoveryFile(t, fx, pair, false)
	pair.dbf.SetPhase("mutate")
	pair.fpt.SetPhase("mutate")
	if err := m.run(t, file); err == nil {
		t.Fatal("expected the growing update to fail")
	}
	results := reopenAndInspect(t, pair, fx)
	r0 := findResult(results, 0)
	if r0 == nil || r0.corrupt {
		t.Fatalf("torn reservation must not corrupt old rows: %+v", r0)
	}
	if *r0.values != m.oldRow0 {
		t.Fatalf("old generation expected, got %+v", r0.values)
	}
}

func errSimulated(msg string) error { return &simulatedErr{msg: msg} }

type simulatedErr struct{ msg string }

func (e *simulatedErr) Error() string { return e.msg }

func errorsAsCorruption(err error, target **CorruptionError) bool {
	return errors.As(err, target)
}

func wrapInjected(msg string) error {
	return fmt.Errorf("%w: %s", ErrInjected, msg)
}
