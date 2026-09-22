package dbase

import (
	"errors"
	"fmt"
)

// ErrCorrupt is the sentinel error for on-disk state that cannot be
// interpreted consistently. It is always wrapped by *CorruptionError,
// use errors.Is(err, ErrCorrupt) to detect any form of corruption and
// errors.As(err, &*CorruptionError) to inspect the structured details.
var ErrCorrupt = errors.New("DBF/FPT corruption detected")

// CorruptionKind classifies a concrete, unrecoverable on-disk inconsistency.
// All kinds describe state the library promises to diagnose instead of
// silently returning mixed-generation data.
type CorruptionKind string

const (
	// CorruptHeaderShape means the DBF header points outside the file
	// (FirstRow or RowLength inconsistent with the actual DBF size).
	CorruptHeaderShape CorruptionKind = "DBF_HEADER_SHAPE"
	// CorruptRecordTruncated means RowsCount advertises a record whose bytes
	// are not fully present in the DBF file (e.g. header was committed
	// before the record bytes reached storage).
	CorruptRecordTruncated CorruptionKind = "DBF_RECORD_TRUNCATED"
	// CorruptRecordMarker means the delete flag byte of a record is neither
	// Active (0x20) nor Deleted (0x2A) - the record region was never written.
	CorruptRecordMarker CorruptionKind = "DBF_RECORD_MARKER"
	// CorruptMemoPointer means a record points at a memo block that starts
	// outside the FPT file or overlaps the 512 byte FPT header area.
	CorruptMemoPointer CorruptionKind = "FPT_MEMO_POINTER"
	// CorruptMemoBlock means a referenced memo block header advertises a
	// payload length that runs past the end of the FPT file, or the payload
	// bytes are only partially present.
	CorruptMemoBlock CorruptionKind = "FPT_MEMO_BLOCK_TRUNCATED"
	// CorruptMemoOverlap means two referenced memo blocks claim overlapping
	// byte ranges - a published block pointer aliases another live block.
	CorruptMemoOverlap CorruptionKind = "FPT_MEMO_BLOCK_OVERLAP"
)

// CorruptionError is the structured diagnosis for an unrecoverable state.
// It implements Unwrap so errors.Is(err, ErrCorrupt) works, and it can be
// inspected with errors.As to obtain Kind, File and Offset.
type CorruptionError struct {
	Kind   CorruptionKind // Machine readable classification.
	File   string         // "DBF" or "FPT".
	Offset int64          // Byte offset involved, -1 when not applicable.
	Detail string         // Human readable context (record number, block, ...).
	err    error          // Optional wrapped underlying error.
}

func newCorruption(kind CorruptionKind, target string, offset int64, detail string) *CorruptionError {
	return &CorruptionError{Kind: kind, File: target, Offset: offset, Detail: detail}
}

func (c *CorruptionError) wrap(err error) *CorruptionError {
	c.err = err
	return c
}

// Error implements the error interface with readable, failure-context rich text.
func (c *CorruptionError) Error() string {
	msg := fmt.Sprintf("%s: %s", c.File, c.Kind)
	if c.Offset >= 0 {
		msg += fmt.Sprintf(" at offset %d", c.Offset)
	}
	if c.Detail != "" {
		msg += " (" + c.Detail + ")"
	}
	if c.err != nil {
		msg += ": " + c.err.Error()
	}
	return msg
}

// Unwrap exposes ErrCorrupt (and any underlying read error) so errors.Is
// and errors.As keep working through the dbase.Error wrapping layers.
func (c *CorruptionError) Unwrap() []error {
	if c.err != nil {
		return []error{ErrCorrupt, c.err}
	}
	return []error{ErrCorrupt}
}

// IntegrityFinding describes a repairable anomaly: published records stay
// fully readable and consistent, but auxiliary bookkeeping drifted.
// Currently the only finding is a memo free-list gap: the FPT header's
// next-free-block counter points beyond the highest referenced (and beyond
// the physically present) block, which happens when a block was allocated
// and the counter advanced before the record pointer was committed.
type IntegrityFinding struct {
	// NextFreeBlocks is the next-free-block counter stored in the FPT header.
	NextFreeBlocks uint32
	// HighWaterBlocks is one past the highest block required by the data:
	// it covers the physical FPT size and every block referenced by a record.
	HighWaterBlocks uint32
	// LeakedBlocks is NextFreeBlocks - HighWaterBlocks. The bytes are simply
	// unreachable, never reused, until the memo file is packed externally.
	LeakedBlocks uint32
}

// String renders the finding for logs and test failure context.
func (f IntegrityFinding) String() string {
	return fmt.Sprintf("FPT free-list gap: header next-free block %d, data high-water block %d, %d leaked block(s)",
		f.NextFreeBlocks, f.HighWaterBlocks, f.LeakedBlocks)
}
