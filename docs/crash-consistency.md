# Crash consistency and the DBF/FPT write contract

A dBase table is two independent files: the table file (`.DBF`) and the memo
file (`.FPT`). There is no shared journal, no write-ahead log and no
checksum on a record. This document defines exactly what the library promises
when a process dies (or the storage layer returns an error) **between** two
file updates, how those states are diagnosed on reopen, and which states can
never be detected without changing the on-disk format.

The behaviour is pinned by `dbase/recovery_*_test.go`, which injects
deterministic I/O failures (full failure, offset-range failure, short writes
with `n > 0` plus an error, and failed flushes) through in-memory wrappers
implementing `io.ReaderAt`, `io.WriterAt`, `io.Seeker`, truncation and
`sync`. The same tests run on Windows and Unix because they drive the
platform-neutral `GenericIO`; the native `UnixIO` and `WindowsIO` share the
identical ordering and validation code.

## Guaranteed boundaries

Writes are ordered so that every observable state is one whole generation,
never a mix:

1. **Memo blocks before record pointers.** Writing a row serialises its memo
   fields first. For each memo the FPT header reservation is advanced, then
   the block payload is written. Only afterwards does the DBF record carry the
   new block address.
2. **Every memo revision is appended, never overwritten.** A same-length or
   growing update allocates a fresh block at `NextFree` instead of reusing the
   previously referenced block. Until the DBF pointer is published, the old
   block remains the only reachable version. The trade-off is that old blocks
   become unreachable space (no free-list compaction is performed).
3. **Record count before record bytes for appends.** An appended record is
   published by incrementing `RowsCount` in the DBF header and then writing
   the record bytes. A failure before or within the record write therefore
   leaves every previously committed record readable; the
   header-ahead-of-data state is diagnosed on reopen instead of returning a
   zero-filled record.
4. **Flush order on `Close`.** Closing flushes the FPT first and the DBF
   second (`fsync`/`FlushFileBuffers`, or `Sync` on a custom handle). Memo
   blocks are the dependency of the records that reference them, so they
   reach stable storage first. Handles without a `Sync` method (pure
   in-memory readers) are simply closed.

## Outcomes after an interrupted write

After reopening, every record is in exactly one of these states:

- **Old generation** — the last fully committed value;
- **New generation** — the complete value of the last mutation;
- **Structured corruption** — a `dbase.CorruptionError` (matched by
  `errors.Is(err, dbase.ErrCorruption)`) with a machine-readable `Kind`
  (`CorruptDBFHeader`, `CorruptFPTHeader`, `CorruptTruncatedRecord`,
  `CorruptInvalidRecordMarker`, `CorruptMemoReference`, `CorruptMemoBlock`),
  the affected file and the byte offset/record index.

Reopen never succeeds with a record whose plain fields come from one
generation while its memo contents come from another.

### Specific failure points

| Failure point | Reopen result |
| --- | --- |
| FPT reservation header write fails (full/short) | Old generation. `NextFree` is clamped to the physical FPT size. |
| Reserved header advanced, block payload fails | Old generation; the reserved range is unreferenced space. |
| Block payload written, DBF record pointer not yet published | Old generation; one unreachable block (space leak). |
| DBF header count published, record bytes missing/short | `CorruptTruncatedRecord` on open. |
| In-place record write fails before the memo pointer bytes | Old generation. |
| Memo pointer references a block outside the FPT | `CorruptMemoReference` when the memo is read. |
| Memo block signature/length invalid or payload truncated | `CorruptMemoBlock` when the memo is read. |
| FPT flush fails at `Close` | Error returned, handles stay open for retry; durable state is the old generation if the unflushed delta is lost. |
| DBF flush fails at `Close` | Error returned; both files contain a complete generation or the open-time validator diagnoses the mismatch. |

An unreferenced FPT reservation that survived but was never pointed at is
**not** corruption: `NextFree` is silently clamped to the physical file size
on open. This mirrors how FoxPro treats orphaned memo space.

## Limits that the format cannot remove

- **No cross-file transaction.** DBF and FPT are separate files; the library
  cannot atomically commit both. Atomicity exists only at the boundaries
  above. Applications needing global atomicity must use an external
  coordinator or journal of their own.
- **Invisible torn writes inside one record's memo-pointer run.** A record is
  one contiguous byte range with no checksum or generation tag. If a partial
  write begins at the first memo pointer byte and stops before the record is
  complete (for the test layout, prefixes of 15–22 bytes), one pointer can
  point at the old block and the other at a new block and the library has no
  structural way to detect it. The recovery tests therefore cover only
  contract-coverable prefixes (before the pointers, and the complete record)
  rather than asserting a guarantee the format does not provide.
- **Plain field tearing.** Partial writes that split an integer or character
  field are similarly undetectable; the bytes are simply decoded as the new
  partial value.
- **Durability is the filesystem's word.** `Sync` reduces the window but does
  not create a transaction; a disk that reports a successful flush and later
  loses bytes is outside any software contract.

## Complexity

- Open-time validation is `O(1)`: it compares `FirstRow + RowsCount*RowLength`
  against the physical DBF size and clamps `NextFree` against the physical
  FPT size. Memo reference/block validation is `O(1)` per memo read.
- Each memo update costs one FPT allocation (`ceil(length/blockSize)`
  blocks); old blocks are not reclaimed. Compaction, if needed, is the
  caller's responsibility.
- `Close` performs one flush per open file; there is no extra per-write
  flush cost.

## Compatibility

- No on-disk format change: files remain standard FoxPro DBF/FPT and stay
  readable by other tools.
- Newly created tables now initialise the FPT with the FoxPro convention
  (`NextFree` starts after the 512-byte header area; a block size of `0`
  defaults to 64). Files created by earlier library versions and by FoxPro
  open unchanged.
- `Close` now flushes before closing; read-only files on platforms/systems
  where flushing a read-only descriptor is rejected surface that error from
  `Close`.
