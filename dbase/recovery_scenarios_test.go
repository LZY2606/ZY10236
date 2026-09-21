package dbase

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// mutation is one user-level operation exercised against a fresh fixture.
type mutation struct {
	name    string
	run     func(t *testing.T, file *File) error
	oldRow0 recValues
	newRow0 recValues
	oldRow1 recValues
	newRow1 recValues
	rows    int // number of rows that must exist after a fully successful run
}

func newAppendMutation() mutation {
	base := baselineRows()
	old0, old1 := base[0], base[1]
	newVals := recValues{id: 3, name: "gamma", m1: strings.Repeat("g", 20), m2: strings.Repeat("h", 20)}
	return mutation{
		name: "append-record",
		run: func(t *testing.T, file *File) error {
			row, err := file.RowFromMap(map[string]interface{}{
				"ID":   newVals.id,
				"NAME": newVals.name,
				"M1":   newVals.m1,
				"M2":   newVals.m2,
			})
			if err != nil {
				return err
			}
			return row.Add()
		},
		oldRow0: old0, newRow0: old0, oldRow1: old1, newRow1: old1, rows: 3,
	}
}

func sameLengthMutation() mutation {
	base := baselineRows()
	old0, old1 := base[0], base[1]
	new0 := old0
	new0.m1 = strings.Repeat("A", 20)
	new0.m2 = strings.Repeat("B", 20)
	return mutation{
		name: "update-same-length-memo",
		run: func(t *testing.T, file *File) error {
			if err := file.GoTo(0); err != nil {
				return err
			}
			row, err := file.Row()
			if err != nil {
				return err
			}
			if err := row.FieldByName("M1").SetValue(new0.m1); err != nil {
				return err
			}
			if err := row.FieldByName("M2").SetValue(new0.m2); err != nil {
				return err
			}
			return row.Write()
		},
		oldRow0: old0, newRow0: new0, oldRow1: old1, newRow1: old1, rows: 2,
	}
}

func growMemoMutation() mutation {
	base := baselineRows()
	old0, old1 := base[0], base[1]
	new0 := old0
	// Grow M1 from one block (20 payload bytes) to two blocks (80 payload bytes).
	new0.m1 = strings.Repeat("G", 80)
	new0.m2 = strings.Repeat("B", 20)
	return mutation{
		name: "update-growing-memo",
		run: func(t *testing.T, file *File) error {
			if err := file.GoTo(0); err != nil {
				return err
			}
			row, err := file.Row()
			if err != nil {
				return err
			}
			if err := row.FieldByName("M1").SetValue(new0.m1); err != nil {
				return err
			}
			if err := row.FieldByName("M2").SetValue(new0.m2); err != nil {
				return err
			}
			return row.Write()
		},
		oldRow0: old0, newRow0: new0, oldRow1: old1, newRow1: old1, rows: 2,
	}
}

func deleteMutation() mutation {
	base := baselineRows()
	old0, old1 := base[0], base[1]
	// Deleting rewrites the whole record (its memo fields are re-serialised), so
	// it exercises the same publish boundary as an in-place update.
	return mutation{
		name: "delete-record",
		run: func(t *testing.T, file *File) error {
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
		oldRow0: old0, newRow0: old0, oldRow1: old1, newRow1: old1, rows: 2,
	}
}

// collectDryRunEvents executes the mutation once while recording every I/O
// event of both files. The resulting events are the deterministic coordinates
// ("Nth Write of the FPT at offset X") used to inject a failure at every
// single operation of the write path.
func collectDryRunEvents(t *testing.T, fx *recFixture, m mutation) (dbfEvents, fptEvents []faultEvent, pair *faultPair) {
	t.Helper()
	pair = newFaultPair(fx.dbf, fx.fpt)
	file := openRecoveryFile(t, fx, pair, false)
	pair.dbf.SetPhase("mutate")
	pair.fpt.SetPhase("mutate")
	if err := m.run(t, file); err != nil {
		t.Fatalf("%s: dry run failed: %v", m.name, err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("%s: dry run close failed: %v", m.name, err)
	}
	return pair.dbf.Events(), pair.fpt.Events(), pair
}

// opOrdinal converts an event index into the 1-based ordinal of its operation
// within the per-handle event list.
func opOrdinal(events []faultEvent, ev faultEvent) int {
	n := 0
	for _, e := range events {
		if e.Phase != "mutate" || e.Op != ev.Op {
			continue
		}
		n++
		if e.Index == ev.Index {
			return n
		}
	}
	return -1
}

// buildFaultRules enumerates every failure point of one scenario:
//
//   - full failure at each Seek, Write and Sync of both files;
//   - short write (n>0 + non-nil error) at every write site with several n
//     values, including a single-byte partial write.
//
// Read failures are not part of the crash model (they happen while opening,
// not while publishing data) and are covered by separate direct tests.
func buildFaultRules(t *testing.T, dbfEvents, fptEvents []faultEvent) []faultRule {
	t.Helper()
	var rules []faultRule
	addEvent := func(handle string, events []faultEvent, ev faultEvent) {
		ordinal := opOrdinal(events, ev)
		if ordinal < 0 {
			t.Fatalf("internal: event %v missing in %s log", ev, handle)
		}
		rules = append(rules, faultRule{
			Handle: handle,
			Op:     ev.Op,
			Nth:    ordinal,
			Phase:  "mutate",
			Cause:  fmt.Errorf("simulated crash during %s %s @%d", ev.Op, handle, ev.Offset),
		})
	}
	for _, ev := range dbfEvents {
		if ev.Phase == "mutate" && (ev.Op == faultSeek || ev.Op == faultWrite || ev.Op == faultSync) {
			addEvent("DBF", dbfEvents, ev)
		}
	}
	for _, ev := range fptEvents {
		if ev.Phase == "mutate" && (ev.Op == faultSeek || ev.Op == faultWrite || ev.Op == faultSync) {
			addEvent("FPT", fptEvents, ev)
		}
	}
	// Short-write probes, addressed precisely at each observed write call.
	// The probes deliberately stop at byte boundaries at which a torn record
	// is either still the full old generation or already the full new
	// generation: a partial write strictly inside the run of the two 4-byte
	// memo pointers (record prefix lengths 15..22 for this layout) could leave
	// one pointer at the old block and the other at a newly allocated block.
	// Because the dBase record carries no checksum or generation tag, that
	// eight-byte window cannot be diagnosed structurally (see
	// docs/crash-consistency.md). It is therefore excluded from the promised
	// boundary instead of being asserted away.
	shortLengths := func(handle string, ev faultEvent, length int) []int {
		vals := map[int]bool{}
		candidates := []int{1, 2, length / 2, length - 1}
		if length >= 8 {
			candidates = append(candidates, 3, 4, 7)
		}
		isRecordWrite := handle == "DBF" && length == int(recLayoutRowLength) && ev.Offset >= int64(recLayoutFirstRow)
		if isRecordWrite {
			// [marker 1][ID 4][NAME 10][M1 4][M2 4]: only prefixes that touch
			// no memo pointer (<=14) are contract-coverable; from 15 bytes on
			// a torn record mixes two memo generations invisibly.
			candidates = append(candidates, 14)
		}
		out := make([]int, 0, len(candidates))
		for _, v := range candidates {
			if v > 0 && v >= length {
				continue
			}
			if isRecordWrite && v >= 15 {
				continue
			}
			if !vals[v] {
				vals[v] = true
				out = append(out, v)
			}
		}
		return out
	}
	addShorts := func(handle string, events []faultEvent) {
		for _, ev := range events {
			if ev.Phase != "mutate" || ev.Op != faultWrite || ev.Length <= 1 {
				continue
			}
			for _, n := range shortLengths(handle, ev, ev.Length) {
				rules = append(rules, faultRule{
					Handle:     handle,
					Op:         faultWrite,
					Nth:        opOrdinal(events, ev),
					Phase:      "mutate",
					ShortWrite: n,
					ShortCause: fmt.Errorf("%w: simulated partial write before crash", ErrInjected),
				})
			}
		}
	}
	addShorts("DBF", dbfEvents)
	addShorts("FPT", fptEvents)
	return rules
}

// runMatrix injects a failure at every operation of the scenario and checks
// the recovery contract after each one, reopening both files for every point.
func runMatrix(t *testing.T, fx *recFixture, m mutation, extraRules ...faultRule) {
	t.Helper()
	dbfEvents, fptEvents, _ := collectDryRunEvents(t, fx, m)
	rules := append(buildFaultRules(t, dbfEvents, fptEvents), extraRules...)
	t.Logf("%s: %d dry-run DBF ops, %d FPT ops, %d injection points",
		m.name, len(dbfEvents), len(fptEvents), len(rules))

	for ri, rule := range rules {
		rule := rule
		t.Run(rule.label(), func(t *testing.T) {
			pair := newFaultPair(fx.dbf, fx.fpt)
			beforeDBF, beforeFPT := pair.dbf.Snapshot(), pair.fpt.Snapshot()
			target := pair.dbf
			if rule.Handle == "FPT" {
				target = pair.fpt
			}
			target.Arm(rule)
			file := openRecoveryFile(t, fx, pair, false)
			pair.dbf.SetPhase("mutate")
			pair.fpt.SetPhase("mutate")
			writeErr := m.run(t, file)
			afterDBF, afterFPT := pair.dbf.Snapshot(), pair.fpt.Snapshot()
			if writeErr != nil && !errorsIsInjected(writeErr) {
				t.Fatalf("%s: mutation failed with a non-injected error: %v", m.name, writeErr)
			}
			outcome := matrixOutcome{
				scenario: m.name,
				rule:     rule,
				dbfDiff:  diffBytes(beforeDBF, afterDBF),
				fptDiff:  diffBytes(beforeFPT, afterFPT),
				writeErr: writeErr,
				results:  reopenAndInspect(t, pair, fx),
			}
			t.Logf("injection %d/%d %q: writeErr=%v DBF[%s] FPT[%s] rows=%d",
				ri+1, len(rules), rule.label(), writeErr,
				outcome.dbfDiff, outcome.fptDiff, len(outcome.results))
			assertOutcomeContract(t, m, outcome)
		})
	}
}

func errorsIsInjected(err error) bool { return errors.Is(err, ErrInjected) }

// assertOutcomeContract verifies the cross-generation invariant for one
// injected failure: every readable row must match a fully committed
// generation; every other row must surface a structured corruption error.
func assertOutcomeContract(t *testing.T, m mutation, outcome matrixOutcome) {
	t.Helper()
	// An open-time structural diagnosis (for example "header advertises a
	// record that the file does not contain") is one of the allowed outcomes.
	if len(outcome.results) == 1 && outcome.results[0].index == -1 && outcome.results[0].corrupt {
		t.Logf("%s: reopen diagnosed structural corruption: %v", m.name, outcome.results[0].err)
		return
	}
	if len(outcome.results) == 0 {
		t.Fatalf("%s: reopen produced no rows at all (the header must stay decodable)", m.name)
	}
	// The untouched second fixture row must always be fully intact.
	r1 := findResult(outcome.results, 1)
	if r1 == nil {
		t.Fatalf("%s: untouched record 1 vanished after reopen", m.name)
	}
	expectRowGeneration(t, *r1, m.oldRow1, m.newRow1)

	// The mutated row must be old generation, new generation, or corruption.
	r0 := findResult(outcome.results, 0)
	if r0 == nil {
		// A vanished row 0 is only legal together with a proven truncated
		// record (header count ahead of the physical file).
		for _, rr := range outcome.results {
			if rr.corrupt {
				return
			}
		}
		t.Fatalf("%s: record 0 disappeared without a corruption diagnosis", m.name)
	}
	expectRowGeneration(t, *r0, m.oldRow0, m.newRow0)
}

func findResult(results []rowResult, index int) *rowResult {
	for i := range results {
		if results[i].index == index {
			return &results[i]
		}
	}
	return nil
}
