package dbase

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"golang.org/x/text/encoding/charmap"
)

// This file holds the shared harness of the crash-recovery contract tests.
//
// The contract intentionally runs on GenericIO backed by faultBacking: the
// read/write/seek/truncate/sync semantics are the common layer used on both
// Unix and Windows (the platform IO types only swap the concrete syscall
// behind those primitives), so the same fault enumeration validates the
// guaranteed atomic boundary on every supported platform without relying on
// the real filesystem, OS caches, sleeps or directory iteration order.

const (
	recoveryTable = "RECOVERY.DBF"
	memoBlockSize = 64
	// NOTE values are kept at one 64 byte block: payload+8 <= 64.
	noteOld       = "note-old-0001"
	noteNew       = "note-new-0002"
	idOld   int32 = 100
	idNew   int32 = 101
	nameOld       = "alpha"
	nameNew       = "beta!"
)

// BODY spans two blocks even in its old generation, so the table always
// contains two live, adjacent memo blocks; the grown value spans six blocks.
var (
	bodyOld   = "body-old-" + strings.Repeat("O", 80)
	bodyGrown = "body-grow-" + strings.Repeat("G", 300)
	bodyNew   = "body-new-" + strings.Repeat("N", 80)
)

// recoveryState is the logical content the contract reasons about.
type recoveryState struct {
	id      int32
	name    string
	note    string
	body    string
	deleted bool
	rows    uint32
}

func baselineState() recoveryState {
	return recoveryState{id: idOld, name: nameOld, note: noteOld, body: bodyOld, rows: 1}
}

// recoveryFixture owns one DBF/FPT pair. Every scenario rebuilds it
// independently from the seeded baseline so fault rules never bleed across
// cases.
type recoveryFixture struct {
	t   *testing.T
	dbf *faultBacking
	fpt *faultBacking
}

func newRecoveryFixture(t *testing.T) *recoveryFixture {
	t.Helper()
	fx := &recoveryFixture{t: t}
	fx.dbf = newFaultBacking("DBF", nil)
	fx.fpt = newFaultBacking("FPT", nil)
	io := GenericIO{Handle: fx.dbf, RelatedHandle: fx.fpt}
	cols := []*Column{
		mustNewColumn(t, "ID", Integer, 0),
		mustNewColumn(t, "NAME", Character, 10),
		mustNewColumn(t, "NOTE", Memo, 0),
		mustNewColumn(t, "BODY", Memo, 0),
	}
	file, err := NewTable(FoxPro,
		&Config{Filename: recoveryTable, Converter: NewDefaultConverter(charmap.Windows1252)},
		cols, memoBlockSize, io)
	if err != nil {
		t.Fatalf("seeding: create table: %v", err)
	}
	row, err := file.RowFromMap(map[string]interface{}{
		"ID": idOld, "NAME": nameOld, "NOTE": noteOld, "BODY": bodyOld,
	})
	if err != nil {
		t.Fatalf("seeding: build row: %v", err)
	}
	if err := row.Add(); err != nil {
		t.Fatalf("seeding: add row: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("seeding: close: %v", err)
	}
	return fx
}

func mustNewColumn(t *testing.T, name string, dt DataType, length uint8) *Column {
	t.Helper()
	c, err := NewColumn(name, dt, length, 0, false)
	if err != nil {
		t.Fatalf("column %s: %v", name, err)
	}
	return c
}

func (fx *recoveryFixture) snapshot() (dbf, fpt []byte) {
	return fx.dbf.Snapshot(), fx.fpt.Snapshot()
}

// openForMutation reopens the pair with the currently installed rules.
func (fx *recoveryFixture) openForMutation(rules ...*faultRule) *File {
	fx.dbf.rules = rulesOf(rules, "DBF")
	fx.fpt.rules = rulesOf(rules, "FPT")
	file, err := OpenTable(&Config{
		Converter: NewDefaultConverter(charmap.Windows1252),
		IO:        GenericIO{Handle: fx.dbf, RelatedHandle: fx.fpt},
	})
	if err != nil {
		fx.t.Fatalf("reopen for mutation: %v", err)
	}
	return file
}

// reopen simulates a process restart on the persisted bytes: brand new
// backing copies, no fault rules, fresh File. It always returns the
// structured integrity diagnosis in addition to any open/read error.
type reopenResult struct {
	file     *File
	openErr  error
	states   []recoveryState
	readErrs []error
	finding  IntegrityFinding
	checkErr error
}

func (fx *recoveryFixture) reopen() *reopenResult {
	fx.dbf.rules = nil
	fx.fpt.rules = nil
	file, err := OpenTable(&Config{
		Converter: NewDefaultConverter(charmap.Windows1252),
		IO:        GenericIO{Handle: fx.dbf, RelatedHandle: fx.fpt},
	})
	res := &reopenResult{file: file, openErr: err}
	if err != nil {
		return res
	}
	res.finding, res.checkErr = file.CheckIntegrity()
	n := file.RowsCount()
	for i := uint32(0); i < n; i++ {
		r, rerr := readStateAt(file, i)
		if rerr != nil {
			res.readErrs = append(res.readErrs, fmt.Errorf("record %d: %w", i+1, rerr))
			continue
		}
		res.states = append(res.states, r)
	}
	return res
}

func (fx *recoveryFixture) closeReopened(r *reopenResult) {
	if r.file != nil {
		_ = r.file.Close()
	}
}

func readStateAt(file *File, pos uint32) (recoveryState, error) {
	var st recoveryState
	if err := file.GoTo(pos); err != nil {
		return st, err
	}
	row, err := file.Row()
	if err != nil {
		return st, err
	}
	st.deleted = row.Deleted
	if v, err := row.IntValueByName("ID"); err != nil {
		return st, err
	} else {
		st.id = int32(v)
	}
	if v, err := row.StringValueByName("NAME"); err != nil {
		return st, err
	} else {
		st.name = strings.TrimRight(v, " ")
	}
	if v, err := row.StringValueByName("NOTE"); err != nil {
		return st, err
	} else {
		st.note = v
	}
	if v, err := row.StringValueByName("BODY"); err != nil {
		return st, err
	} else {
		st.body = v
	}
	return st, nil
}

func rulesOf(all []*faultRule, file string) []*faultRule {
	out := make([]*faultRule, 0)
	for _, r := range all {
		if r.file == "" || r.file == file {
			out = append(out, r)
		}
	}
	return out
}

// byteDiff is the minimal persisted difference of one fault point.
type byteDiff struct {
	file    string
	changed int
	first   int // -1 when identical
	detail  string
}

func diffBytes(label string, old, now []byte) byteDiff {
	d := byteDiff{file: label, first: -1}
	n := len(old)
	if len(now) > n {
		n = len(now)
	}
	for i := 0; i < n; i++ {
		var a, b byte
		if i < len(old) {
			a = old[i]
		}
		if i < len(now) {
			b = now[i]
		}
		if a != b {
			if d.first < 0 {
				d.first = i
				d.detail = fmt.Sprintf("first differing byte at offset %d: 0x%02x -> 0x%02x", i, a, b)
			}
			d.changed++
		}
	}
	if len(old) != len(now) {
		d.detail += fmt.Sprintf("; size %d -> %d", len(old), len(now))
	}
	return d
}

// corruptionKind extracts the structured corruption kind, if any.
func corruptionKind(err error) (CorruptionKind, bool) {
	var ce *CorruptionError
	if errors.As(err, &ce) {
		return ce.Kind, true
	}
	return "", false
}

// firstReadErr returns the first read/reopen error.
func (r *reopenResult) firstReadErr() error {
	if r.openErr != nil {
		return r.openErr
	}
	if len(r.readErrs) > 0 {
		return r.readErrs[0]
	}
	return nil
}

// assertAcceptable validates the three allowed reopen outcomes:
//  1. full old generation (mutation lost, no splicing),
//  2. full new generation (mutation fully committed),
//  3. a structured *CorruptionError (state is diagnosed, not silently mixed),
//
// plus a recorded free-list finding attached to either consistent outcome.
// Any "no error but old/new fields spliced together" result fails hard.
func (fx *recoveryFixture) assertAcceptable(caseName string, opErr error, res *reopenResult,
	oldGen, newGen recoveryState, diffs []byteDiff, softFindingOK bool) {
	fx.t.Helper()
	ctx := fx.failureContext(caseName, opErr, res, diffs)

	// Case 3: structured corruption, either at open or on record read.
	if kind, ok := corruptionKind(res.openErr); ok {
		fx.t.Logf("%s: reopen diagnosed hard corruption %s\n%s", caseName, kind, ctx)
		return
	}
	if res.file == nil {
		fx.t.Fatalf("%s: reopen failed without structured corruption:\n%s", caseName, ctx)
	}
	if cErr := res.firstReadErr(); cErr != nil {
		if kind, ok := corruptionKind(cErr); ok {
			fx.t.Logf("%s: reads diagnosed hard corruption %s\n%s", caseName, kind, ctx)
			return
		}
		fx.t.Fatalf("%s: read failed with unclassified error %v:\n%s", caseName, cErr, ctx)
	}
	// At this point every advertised record must be fully readable.
	if len(res.states) == 0 {
		fx.t.Fatalf("%s: no readable records after reopen:\n%s", caseName, ctx)
	}
	for i, st := range res.states {
		switch {
		case sameGeneration(st, oldGen):
			// old generation: rows must not carry new generation pieces
		case sameGeneration(st, newGen):
			// new generation: complete, not spliced. For an in-place update
			// the new generation is only legal when it is the sole record; an
			// append may legitimately show old records followed by new ones.
			if oldGen.rows == newGen.rows && len(res.states) != 1 {
				fx.t.Fatalf("%s: in-place update produced %d records, expected 1:\n%s",
					caseName, len(res.states), ctx)
			}
		default:
			fx.t.Fatalf("%s: record %d is neither old nor new generation (possible old/new splice):\n"+
				"got  = %+v\nold  = %+v\nnew  = %+v\n%s",
				caseName, i+1, st, oldGen, newGen, ctx)
		}
	}
	// For appends, readable generations must form a non-splicing prefix of the
	// old table followed by the new record: never a new record before old ones.
	if newGen.rows > oldGen.rows {
		for i := 0; i < int(oldGen.rows) && i < len(res.states); i++ {
			if !sameGeneration(res.states[i], oldGen) {
				fx.t.Fatalf("%s: record %d must still be the old generation prefix:\n%s",
					caseName, i+1, ctx)
			}
		}
	}
	// Header count must agree with the readable generations.
	wantRows := oldGen.rows
	if len(res.states) == int(newGen.rows) &&
		sameGeneration(res.states[len(res.states)-1], newGen) &&
		(newGen.rows > oldGen.rows || sameGeneration(res.states[0], newGen)) {
		wantRows = newGen.rows
	}
	if res.file.RowsCount() != wantRows {
		fx.t.Fatalf("%s: header rows %d disagrees with readable generations (%d):\n%s",
			caseName, res.file.RowsCount(), wantRows, ctx)
	}
	if res.checkErr != nil {
		fx.t.Fatalf("%s: CheckIntegrity reported an error on a state presented as consistent:\n%v\n%s",
			caseName, res.checkErr, ctx)
	}
	if res.finding.LeakedBlocks > 0 && !softFindingOK {
		fx.t.Fatalf("%s: undeclared memo free-list gap (%s):\n%s",
			caseName, res.finding.String(), ctx)
	}
	summary := "no integrity findings"
	if res.finding.LeakedBlocks > 0 {
		summary = res.finding.String()
	}
	fx.t.Logf("%s: reopened consistently as %s generation (%s)\n%s",
		caseName, generationLabel(res.states[0], oldGen, newGen), summary, ctx)
}

// sameGeneration compares logical field content, ignoring the rows count
// metadata carried by recoveryState.
func sameGeneration(a, b recoveryState) bool {
	return a.id == b.id && a.name == b.name && a.note == b.note &&
		a.body == b.body && a.deleted == b.deleted
}

func generationLabel(st, oldGen, newGen recoveryState) string {
	switch {
	case sameGeneration(st, oldGen):
		return "old"
	case sameGeneration(st, newGen):
		return "new"
	default:
		return "unknown"
	}
}

func (fx *recoveryFixture) failureContext(caseName string, opErr error, res *reopenResult, diffs []byteDiff) string {
	var b strings.Builder
	fmt.Fprintf(&b, "  case: %s\n", caseName)
	if opErr != nil {
		fmt.Fprintf(&b, "  operation error: %v\n", opErr)
	}
	if res.openErr != nil {
		fmt.Fprintf(&b, "  reopen error: %v\n", res.openErr)
	}
	for _, e := range res.readErrs {
		fmt.Fprintf(&b, "  read error: %v\n", e)
	}
	if res.file != nil {
		fmt.Fprintf(&b, "  header rows: %d, readable: %d\n", res.file.RowsCount(), len(res.states))
	}
	if res.finding.LeakedBlocks > 0 || res.checkErr != nil {
		fmt.Fprintf(&b, "  integrity: %s checkErr=%v\n", res.finding.String(), res.checkErr)
	}
	for _, d := range diffs {
		fmt.Fprintf(&b, "  diff %s: %d changed byte(s), %s\n", d.file, d.changed, d.detail)
	}
	return b.String()
}
