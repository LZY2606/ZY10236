package dbase

import (
	"errors"
	"fmt"
	"runtime"
)

var (
	// ErrEOF is returned when the end of a dBase database file is reached
	ErrEOF = errors.New("EOF")
	// ErrBOF is returned when the row pointer is attempted to be moved before the first row
	ErrBOF = errors.New("BOF")
	// ErrIncomplete is returned when the read of a row or column did not finish
	ErrIncomplete = errors.New("INCOMPLETE")
	// ErrNoFPT is returned when a file operation is attempted on a non-existent FPT file
	ErrNoFPT = errors.New("FPT_FILE_NOT_FOUND")
	// ErrNoDBF is returned when a file operation is attempted on a non-existent DBF file
	ErrNoDBF = errors.New("DBF_FILE_NOT_FOUND")
	// ErrInvalidPosition is returned when an invalid column position is used (x<1 or x>number of columns)
	ErrInvalidPosition = errors.New("INVALID_POSITION")
	// ErrInvalidEncoding is returned when an invalid encoding is encountered
	ErrInvalidEncoding = errors.New("INVALID_ENCODING")
	// ErrUnknownDataType is returned when an invalid data type is used
	ErrUnknownDataType = errors.New("UNKNOWN_DATA_TYPE")
	// ErrCorruption is wrapped by CorruptionError when an opened file can be proven
	// structurally inconsistent. Callers can detect an unrecoverable on-disk
	// state with errors.Is(err, ErrCorruption).
	ErrCorruption = errors.New("CORRUPT_FILE")
)

// CorruptionKind categorizes a proven structural inconsistency of a DBF/FPT pair.
// The kinds describe which on-disk invariant is violated so that callers can
// distinguish a torn write that the library can diagnose from one it cannot.
type CorruptionKind string

const (
	// CorruptDBFHeader means the DBF header is truncated or not decodable.
	CorruptDBFHeader CorruptionKind = "corrupt_dbf_header"
	// CorruptFPTHeader means the memo header is truncated or not decodable.
	CorruptFPTHeader CorruptionKind = "corrupt_fpt_header"
	// CorruptTruncatedRecord means RowsCount advertises records beyond the
	// physical end of the DBF file.
	CorruptTruncatedRecord CorruptionKind = "truncated_record"
	// CorruptInvalidRecordMarker means a record does not start with the active
	// (0x20) or deleted (0x2A) marker.
	CorruptInvalidRecordMarker CorruptionKind = "invalid_record_marker"
	// CorruptMemoReference means a memo address in a row points outside the
	// FPT file or at an invalid block boundary.
	CorruptMemoReference CorruptionKind = "invalid_memo_reference"
	// CorruptMemoBlock means a referenced memo block header (signature/length)
	// is unreadable or declares a length beyond the FPT file.
	CorruptMemoBlock CorruptionKind = "invalid_memo_block"
)

// CorruptionError is returned when the on-disk state can be proven structurally
// inconsistent after an interrupted or partially failed write. It always wraps
// ErrCorruption and carries the affected file ("DBF" or "FPT"), the violated
// invariant and the byte offset/record position where the violation was found.
type CorruptionError struct {
	Kind     CorruptionKind
	File     string // "DBF" or "FPT"
	Offset   int64  // byte offset inside the affected file, -1 when unknown
	Record   int64  // zero-based record index, -1 when not applicable
	Expected int64  // expected size/length/value when meaningful
	Actual   int64  // actual size/length/value when meaningful
	msg      string
	details  []error
}

// NewCorruptionError creates a CorruptionError with a human readable message.
func NewCorruptionError(kind CorruptionKind, file string, msg string) *CorruptionError {
	return &CorruptionError{
		Kind:   kind,
		File:   file,
		Offset: -1,
		Record: -1,
		msg:    msg,
	}
}

// At records the byte offset where the corruption was detected.
func (e *CorruptionError) At(offset int64) *CorruptionError {
	e.Offset = offset
	return e
}

// AtRecord records the zero-based record index where the corruption was detected.
func (e *CorruptionError) AtRecord(record int64) *CorruptionError {
	e.Record = record
	return e
}

// Size records an expected/actual size mismatch.
func (e *CorruptionError) Size(expected, actual int64) *CorruptionError {
	e.Expected = expected
	e.Actual = actual
	return e
}

// Details adds an underlying error that caused the corruption diagnosis.
func (e *CorruptionError) Details(err error) *CorruptionError {
	if err != nil {
		e.details = append(e.details, err)
	}
	return e
}

// Error implements the error interface.
func (e *CorruptionError) Error() string {
	msg := string(e.Kind) + ": " + e.msg
	if e.File != "" {
		msg = "[" + e.File + "] " + msg
	}
	if e.Offset >= 0 {
		msg += fmt.Sprintf(" (offset %d)", e.Offset)
	}
	if e.Record >= 0 {
		msg += fmt.Sprintf(" (record %d)", e.Record)
	}
	if e.Expected > 0 || e.Actual > 0 {
		msg += fmt.Sprintf(" (expected %d, actual %d)", e.Expected, e.Actual)
	}
	for _, d := range e.details {
		msg += " => " + d.Error()
	}
	return msg
}

// Unwrap exposes ErrCorruption for errors.Is and the underlying detail errors.
func (e *CorruptionError) Unwrap() []error {
	out := make([]error, 0, len(e.details)+1)
	out = append(out, ErrCorruption)
	out = append(out, e.details...)
	return out
}

// Error is a wrapper for errors that occur in the dbase package
type Error struct {
	trace   []string
	details []error
	msg     string
}

// NewError creates a new Error with the given error message.
func NewError(err string) Error {
	e := Error{
		msg:     err,
		trace:   make([]string, 0),
		details: make([]error, 0),
	}
	e.trace = traceError(e)
	return e
}

// NewErrorf creates a new Error with formatted message using fmt.Sprintf.
func NewErrorf(format string, a ...interface{}) Error {
	e := Error{
		msg:     fmt.Sprintf(format, a...),
		trace:   make([]string, 0),
		details: make([]error, 0),
	}
	e.trace = traceError(e)
	return e
}

// Details adds an additional error detail to this Error.
func (e Error) Details(err error) Error {
	e.details = append(e.details, err)
	return e
}

func (e Error) Error() string {
	details := ""
	for _, d := range e.details {
		details += "=> " + d.Error()
	}

	if debug && len(e.trace) > 0 {
		trace := ""
		for i := len(e.trace) - 1; i >= 0; i-- {
			trace += e.trace[i]
			if i > 0 {
				trace += " -> "
			}
		}

		return fmt.Sprintf("%s: %s %s", trace, e.msg, details)
	}

	return fmt.Sprintf("%s %s", e.msg, details)
}

// Unwrap exposes the detail chain for errors.Is/errors.As. A dbase Error
// returned for a failed operation therefore keeps matching the original
// sentinel (io.ErrShortWrite, injected test failures, ErrCorruption, ...).
func (e Error) Unwrap() []error {
	if len(e.details) == 0 {
		return nil
	}
	return e.details
}

// WrapError wraps an existing error into a dbase Error with trace information.
func WrapError(err error) Error {
	if err == nil {
		return NewError("unknown error occurred - cant wrap nil error")
	}
	if e, ok := err.(Error); ok {
		e.trace = traceError(e)
		return e
	}
	e := Error{
		msg:     err.Error(),
		trace:   make([]string, 0),
		details: []error{err},
	}
	e.trace = traceError(e)
	return e
}

func traceError(e Error) []string {
	_, file, line, ok := runtime.Caller(2)
	if !ok {
		return e.trace
	}

	e.trace = append(e.trace, fmt.Sprintf("%s:%d", file, line))
	return e.trace
}
