package dbase

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// Fixture sanity: the hand-built baseline reads back exactly as constructed.

func TestRecoveryFixtureSanity(t *testing.T) {
	fx := buildRecoveryFixture(t, baselineRows())
	pair := newFaultPair(fx.dbf, fx.fpt)
	results := reopenAndInspect(t, pair, fx)
	if len(results) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(results))
	}
	for _, r := range results {
		if r.corrupt || r.err != nil {
			t.Fatalf("baseline row %d not clean: %v", r.index, r.err)
		}
	}
	if got := results[0].values; *got != fx.row0 {
		t.Fatalf("row0 = %+v want %+v", got, fx.row0)
	}
	if got := results[1].values; *got != fx.row1 {
		t.Fatalf("row1 = %+v want %+v", got, fx.row1)
	}
}

// Every scenario must commit its full new generation when no fault is present.

func TestRecoveryScenarioSuccess(t *testing.T) {
	fx := buildRecoveryFixture(t, baselineRows())
	for _, m := range []mutation{
		newAppendMutation(),
		sameLengthMutation(),
		growMemoMutation(),
		deleteMutation(),
	} {
		t.Run(m.name, func(t *testing.T) {
			pair := newFaultPair(fx.dbf, fx.fpt)
			file := openRecoveryFile(t, fx, pair, false)
			if err := m.run(t, file); err != nil {
				t.Fatalf("%s failed without faults: %v", m.name, err)
			}
			if err := file.Close(); err != nil {
				t.Fatalf("%s close failed: %v", m.name, err)
			}
			results := reopenAndInspect(t, pair, fx)
			if uint32(len(results)) != uint32(m.rows) {
				t.Fatalf("%s: expected %d rows, got %d", m.name, m.rows, len(results))
			}
			if err := assertSuccessfulGeneration(m, results); err != nil {
				t.Fatalf("%s: %v", m.name, err)
			}
		})
	}
}

func assertSuccessfulGeneration(m mutation, results []rowResult) error {
	r0 := findResult(results, 0)
	if r0 == nil || r0.values == nil {
		return errors.New("row 0 missing after a successful mutation")
	}
	want0 := m.newRow0
	if m.name == "delete-record" {
		if !r0.deleted {
			return errors.New("row 0 was not marked deleted after a successful delete")
		}
	} else if *r0.values != want0 {
		return fmt.Errorf("row0 = %+v want %+v", *r0.values, want0)
	}
	r1 := findResult(results, 1)
	if r1 == nil || r1.values == nil || *r1.values != m.newRow1 {
		return fmt.Errorf("row1 = %+v want %+v", rowValue(r1), m.newRow1)
	}
	if m.name == "append-record" {
		r2 := findResult(results, 2)
		if r2 == nil || r2.values == nil {
			return errors.New("appended row 2 missing after a successful append")
		}
		want := recValues{id: 3, name: "gamma", m1: strings.Repeat("g", 20), m2: strings.Repeat("h", 20)}
		if *r2.values != want {
			return fmt.Errorf("row2 = %+v want %+v", *r2.values, want)
		}
	}
	return nil
}

func rowValue(r *rowResult) *recValues {
	if r == nil {
		return nil
	}
	return r.values
}

// The matrix: inject a controllable failure at every Seek/Write/Sync and at
// short-write boundaries of each scenario; after each point, reopen and assert
// the old-generation/new-generation/structured-corruption contract.

func TestRecoveryMatrixAppend(t *testing.T) {
	fx := buildRecoveryFixture(t, baselineRows())
	runMatrix(t, fx, newAppendMutation())
}

func TestRecoveryMatrixSameLengthMemo(t *testing.T) {
	fx := buildRecoveryFixture(t, baselineRows())
	runMatrix(t, fx, sameLengthMutation())
}

func TestRecoveryMatrixGrowingMemo(t *testing.T) {
	fx := buildRecoveryFixture(t, baselineRows())
	runMatrix(t, fx, growMemoMutation())
}

func TestRecoveryMatrixDelete(t *testing.T) {
	fx := buildRecoveryFixture(t, baselineRows())
	runMatrix(t, fx, deleteMutation())
}
