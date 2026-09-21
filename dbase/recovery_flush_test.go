package dbase

import (
	"errors"
	"testing"
)

// Close performs an explicit flush: the FPT is Sync-ed before the DBF so that
// referenced memo blocks are durable before the records pointing at them.
// These tests fail each Sync at Close and verify the reopen contract under
// both durability interpretations a storage layer can provide:
//
//   - the unflushed delta is lost (Sync rollback, modelling a hard crash on
//     media that rejected the flush);
//   - the bytes survive even though Sync reported an error (modelling a
//     filesystem that persisted the data but returned a flush error).

func TestRecoveryCloseFlushSyncFailure(t *testing.T) {
	fx := buildRecoveryFixture(t, baselineRows())
	m := sameLengthMutation()

	t.Run("sync-failure-loses-delta", func(t *testing.T) {
		pair := newFaultPair(fx.dbf, fx.fpt)
		file := openRecoveryFile(t, fx, pair, false)
		pair.dbf.SetPhase("mutate")
		pair.fpt.SetPhase("mutate")
		if err := m.run(t, file); err != nil {
			t.Fatalf("mutation failed: %v", err)
		}
		// Model a hard crash at the first (FPT) durability barrier: nothing
		// since the last barrier was acknowledged, including the DBF bytes
		// that would have been flushed afterwards.
		pair.dbf.syncFailsRollback = true
		pair.fpt.syncFailsRollback = true
		pair.dbf.SetPhase("close")
		pair.fpt.SetPhase("close")
		pair.fpt.Arm(faultRule{
			Handle: "FPT", Op: faultSync, Phase: "close", Nth: 1,
			Cause: errInjectedRollbackBoth(pair),
		})
		closeErr := file.Close()
		if closeErr == nil || !errors.Is(closeErr, ErrInjected) {
			t.Fatalf("expected injected flush error, got: %v", closeErr)
		}
		results := reopenAndInspect(t, pair, fx)
		r0 := findResult(results, 0)
		if r0 == nil {
			t.Fatal("record 0 missing after a rollback flush failure")
		}
		// Nothing reached stable storage: the complete old generation survives.
		expectRowGeneration(t, *r0, m.oldRow0)
	})

	t.Run("sync-failure-bytes-survive", func(t *testing.T) {
		pair := newFaultPair(fx.dbf, fx.fpt)
		file := openRecoveryFile(t, fx, pair, false)
		pair.dbf.SetPhase("mutate")
		pair.fpt.SetPhase("mutate")
		if err := m.run(t, file); err != nil {
			t.Fatalf("mutation failed: %v", err)
		}
		// Sync fails but the written bytes are kept in the file: the data is
		// effectively durable, the error only reaches the caller.
		pair.dbf.SetPhase("close")
		pair.fpt.SetPhase("close")
		pair.fpt.Arm(faultRule{Handle: "FPT", Op: faultSync, Phase: "close", Nth: 1})
		closeErr := file.Close()
		if closeErr == nil || !errors.Is(closeErr, ErrInjected) {
			t.Fatalf("expected injected flush error, got: %v", closeErr)
		}
		results := reopenAndInspect(t, pair, fx)
		r0 := findResult(results, 0)
		if r0 == nil {
			t.Fatal("record 0 missing after surviving flush failure")
		}
		// Either the old generation survives or the complete new generation is
		// readable; mixing is forbidden by the shared contract.
		expectRowGeneration(t, *r0, m.oldRow0, m.newRow0)
	})
}

// A Sync failure before any mutation (for example on an idle Close) must not
// corrupt readable data when the flush is retried successfully.
func TestRecoveryCloseFlushRetry(t *testing.T) {
	fx := buildRecoveryFixture(t, baselineRows())
	pair := newFaultPair(fx.dbf, fx.fpt)
	file := openRecoveryFile(t, fx, pair, false)
	pair.dbf.SetPhase("close")
	pair.fpt.SetPhase("close")
	pair.fpt.Arm(faultRule{Handle: "FPT", Op: faultSync, Phase: "close", Nth: 1})
	if err := file.Close(); err == nil || !errors.Is(err, ErrInjected) {
		t.Fatalf("expected injected flush error, got: %v", err)
	}
	// Second close attempt: rules stay installed for the one-shot fault only,
	// disarm everything and sync explicitly.
	pair.dbf.Reopen()
	pair.fpt.Reopen()
	pair.dbf.Disarm()
	pair.fpt.Disarm()
	if err := pair.fpt.Sync(); err != nil {
		t.Fatalf("retry FPT sync: %v", err)
	}
	if err := pair.dbf.Sync(); err != nil {
		t.Fatalf("retry DBF sync: %v", err)
	}
	results := reopenAndInspect(t, pair, fx)
	if len(results) != 2 {
		t.Fatalf("expected 2 rows after retry, got %d", len(results))
	}
	if results[0].values == nil || *results[0].values != fx.row0 {
		t.Fatalf("row0 changed after an idle flush failure: %+v", results[0].values)
	}
}

// errInjectedRollbackBoth returns the injected sentinel while rolling back the
// paired DBF as well, modelling a process that dies before either file's flush
// barrier was acknowledged.
func errInjectedRollbackBoth(pair *faultPair) error {
	pair.dbf.mu.Lock()
	pair.dbf.data = append([]byte(nil), pair.dbf.lastSync...)
	pair.dbf.mu.Unlock()
	return ErrInjected
}
