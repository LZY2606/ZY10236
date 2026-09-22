# Crash Recovery, Atomic Boundaries and Memo Allocation

A dBase table is stored in **two independent files**: the table file (`.DBF`)
and the memo file (`.FPT`). The operating system provides no joint
transaction across them. This document states exactly what this library
guarantees when a process dies or a write/sync fails mid-operation, how the
state is diagnosed afterwards, and which trade-offs were chosen.

## Write Order

Record and memo writes happen in a fixed order in all three I/O backends
(`GenericIO`, `UnixIO`, `WindowsIO`):

1. For every populated memo field, in column order:
   1. The FPT header is rewritten with an advanced `next-free-block` counter.
   2. The new memo block (8 byte block header + payload, zero-padded to the
      block size) is written at the freshly allocated block position.
2. The DBF header is rewritten (record count + modification date).
3. The DBF record bytes are written, carrying the new memo block pointers.

A memo **update always allocates a fresh block**. The previously referenced
block is never overwritten. This is what makes the boundary below possible.

## Guaranteed Atomic Boundary

After a crash or an injected I/O failure at **any** point, reopening the pair
produces exactly one of the following:

- **Complete old generation.** Nothing of the new logical record is visible.
  Memo blocks that were reserved before the crash are unreachable space.
- **Complete new generation.** The record and every memo value it references
  are fully present.
- **Structured corruption error.** When the DBF header advertised a record
  whose bytes never reached storage (count committed, record missing), the
  reopen or the first read returns a `*CorruptionError`, detectable with
  `errors.Is(err, dbase.ErrCorrupt)`.

The library never returns a record whose plain fields come from one
generation and whose memo values come from another. In particular, a memo
pointer only exists in a record **after** the block it names is fully on
storage, so a block is never observed half-published.

### Free-list gaps are space leaks, not data loss

If the process fails after the FPT header advanced the free-list counter but
before the DBF record committed a pointer into the reserved range, those
blocks are allocated yet unreachable. `File.CheckIntegrity` reports this as an
`IntegrityFinding` (`LeakedBlocks > 0`) while every published record stays
readable and consistent. Reclaiming the space requires packing the memo file
with an external dBase/FoxPro tool; this library does not implement a free
list reuse scheme and therefore cannot reclaim or reuse such blocks itself.

## What Cannot Be Guaranteed

- **Cross-file transactional atomicity.** A power loss between flushing the
  FPT and flushing the DBF is exactly the free-list-gap case above; the
  library cannot make the two files commit as one.
- **Torn single-file writes at byte granularity.** If the storage layer
  tears a single `write` of an in-place record rewrite without returning a
  short-write error, no checksum exists in the dBase format to distinguish
  those bytes. Short writes that *are* reported (`n > 0` together with an
  error) are handled: the writer fails loudly and `CheckIntegrity` diagnoses
  the resulting truncated record or invalid record marker on reopen.
- **Durability without an explicit flush.** `Close` releases handles; call
  `File.Flush()` first when durability matters. `Flush` syncs the DBF and the
  FPT separately; a failure from either sync is returned to the caller. The
  two syncs are still not one transaction.

## Diagnostics

```go
finding, err := table.CheckIntegrity()
if errors.Is(err, dbase.ErrCorrupt) {
    var c *dbase.CorruptionError
    errors.As(err, &c)
    // c.Kind, c.File ("DBF"/"FPT"), c.Offset, c.Detail
}
// finding.LeakedBlocks > 0: unreachable but harmless reserved memo space
```

`CorruptionKind` values:

| Kind | Meaning |
| --- | --- |
| `DBF_HEADER_SHAPE` | `FirstRow`/`RowLength` are zero or inconsistent with the file. |
| `DBF_RECORD_TRUNCATED` | Header count advertises bytes absent from the DBF file. |
| `DBF_RECORD_MARKER` | A record starts with neither `0x20` (active) nor `0x2A` (deleted). |
| `FPT_MEMO_POINTER` | A record points at a block outside the FPT or inside its header area. |
| `FPT_MEMO_BLOCK_TRUNCATED` | A referenced block declares a payload past the end of the FPT. |
| `FPT_MEMO_BLOCK_OVERLAP` | Two referenced blocks claim overlapping byte ranges. |

`CheckIntegrity` does one sequential pass over the records and the 8 byte
header of every distinct referenced memo block: time complexity is
`O(records * rowLength + referencedMemoBlocks)`, memory is `O(blocks)` for the
overlap intervals; memo payloads are never buffered.

## Compatibility Notes

- Newly created tables now initialize the FPT `next-free-block` counter to the
  first block after the 512 byte memo header (block `8` at the standard 64
  byte block size), matching files produced by Visual FoxPro. Previously it
  started at `0`, which let the first memo allocation overwrite the memo file
  header.
- Memo updates allocate fresh blocks instead of overwriting in place. On-disk
  layout stays valid FoxPro FPT; the only consequence is that replaced blocks
  remain as unreachable space until an external pack, identical to deleted
  memo behavior in FoxPro. The public Go API is unchanged.
- `File.Flush()` and `File.CheckIntegrity()` are additive; custom `IO`
  implementations need no changes. Handles that cannot `Sync` are skipped by
  `Flush`.
