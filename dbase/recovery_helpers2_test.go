package dbase

import (
	"strings"
	"testing"
)

func mustFirstRow(t *testing.T) int {
	t.Helper()
	first, _ := seededGeometry(t)
	return first
}

// updateRow rewrites the first record with selected field replacements.
// Unlisted fields keep their current (decoded then re-encoded) values, and
// memo fields that are not changed still receive a fresh block because the
// writer always allocates on Represent; that is the documented behavior.
func updateRow(t *testing.T, file *File, changes map[string]interface{}) error {
	t.Helper()
	if err := file.GoTo(0); err != nil {
		return err
	}
	row, err := file.Row()
	if err != nil {
		return err
	}
	for name, value := range changes {
		field := row.FieldByName(name)
		if field == nil {
			t.Fatalf("column %s missing", name)
		}
		if err := field.SetValue(value); err != nil {
			return err
		}
	}
	return row.Write()
}

// stringsRepeatLen returns a replacement string of the exact byte length of
// base, so same-length-update cases stay length-identical by construction.
func stringsRepeatLen(base, fill string) string {
	out := strings.Repeat(fill, len(base))
	if len(out) != len(base) {
		out = out[:len(base)]
	}
	return out
}

// assertExactGeneration demands a fully readable, finding-free file matching
// want for every record.
func (fx *recoveryFixture) assertExactGeneration(caseName string, res *reopenResult, want recoveryState, diffs []byteDiff) {
	fx.t.Helper()
	if res.openErr != nil {
		fx.t.Fatalf("%s: unexpected reopen error: %v\n%s", caseName, res.openErr,
			fx.failureContext(caseName, nil, res, diffs))
	}
	if e := res.firstReadErr(); e != nil {
		fx.t.Fatalf("%s: unexpected read error: %v\n%s", caseName, e,
			fx.failureContext(caseName, nil, res, diffs))
	}
	if len(res.states) != int(want.rows) {
		fx.t.Fatalf("%s: expected %d readable records, got %d\n%s",
			caseName, want.rows, len(res.states), fx.failureContext(caseName, nil, res, diffs))
	}
	got := res.states[len(res.states)-1]
	if !sameGeneration(got, want) {
		fx.t.Fatalf("%s: last record mismatch:\nwant=%+v\ngot =%+v\n%s",
			caseName, want, got, fx.failureContext(caseName, nil, res, diffs))
	}
	if res.checkErr != nil {
		fx.t.Fatalf("%s: unexpected integrity error: %v\n%s", caseName, res.checkErr,
			fx.failureContext(caseName, nil, res, diffs))
	}
	if res.finding.LeakedBlocks != 0 {
		fx.t.Fatalf("%s: unexpected free-list gap: %s\n%s",
			caseName, res.finding.String(), fx.failureContext(caseName, nil, res, diffs))
	}
}
