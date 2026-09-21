package dbase

import (
	"encoding/binary"
	"fmt"
)

// validateOnOpen performs the cross-platform structural checks that can be made
// in constant time right after the DBF and (when present) FPT headers have been
// read. The checks only enforce invariants derivable from the format itself;
// they never perform a transactional rollback, because a DBF/FPT pair is two
// independent files and the library cannot provide cross-file transactions.
//
// The promised invariants are:
//
//   - the DBF file physically contains at least FirstRow + RowsCount*RowLength
//     bytes, otherwise the last published record(s) were never fully written;
//   - the FPT file physically contains at least NextFree blocks, otherwise the
//     memo header promised blocks that do not exist.
//
// When the FPT header advertises more blocks than the file contains but no row
// references the missing range, NextFree is clamped down instead of rejecting
// the file: that state is the benign residue of a memo block that was reserved
// (header updated) before the block data or its referencing record reached the
// disk. The reserved range stays unusable until the file is compacted.
func validateOnOpen(file *File) error {
	dbfSize, err := fileSize(file, false)
	if err != nil {
		return err
	}
	minSize := int64(file.header.FirstRow) + int64(file.header.RowsCount)*int64(file.header.RowLength)
	if dbfSize < minSize {
		return NewCorruptionError(
			CorruptTruncatedRecord, "DBF",
			fmt.Sprintf("header advertises %d record(s) but the file ends before the last record", file.header.RowsCount),
		).At(int64(file.header.FirstRow)).Size(minSize, dbfSize)
	}
	if file.memoHeader == nil {
		return nil
	}
	fptSize, err := fileSize(file, true)
	if err != nil {
		return err
	}
	if file.memoHeader.BlockSize == 0 {
		return NewCorruptionError(
			CorruptFPTHeader, "FPT",
			"memo header declares a block size of zero, memo addresses cannot be resolved",
		).At(0)
	}
	advertised := int64(file.memoHeader.NextFree) * int64(file.memoHeader.BlockSize)
	if advertised > fptSize {
		// The header reservation is ahead of the physical file. Clamp it to the
		// physically available range. Live references are validated lazily while
		// rows are read, so referenced-but-missing blocks still surface there.
		file.memoHeader.NextFree = uint32(fptSize / int64(file.memoHeader.BlockSize))
	}
	return nil
}

// validateMemoAddress checks a 4-byte little-endian memo address stored in a
// record against the current FPT size. It returns the block number.
// Address zero is valid and represents an empty memo.
func validateMemoAddress(file *File, address []byte, column *Column) (uint32, error) {
	block := binary.LittleEndian.Uint32(address)
	if block == 0 {
		return 0, nil
	}
	fptSize, err := fileSize(file, true)
	if err != nil {
		return 0, err
	}
	blockSize := int64(file.memoHeader.BlockSize)
	position := int64(block) * blockSize
	if position+8 > fptSize {
		return block, NewCorruptionError(
			CorruptMemoReference, "FPT",
			fmt.Sprintf("column %s references memo block %d which is outside the memo file", column.Name(), block),
		).At(position)
	}
	return block, nil
}

// validateMemoBlock validates the 8-byte memo block header (big-endian
// signature and length) at the given file offset.
func validateMemoBlock(file *File, block uint32, position int64, header []byte) (uint32, uint32, error) {
	sign := binary.BigEndian.Uint32(header[:4])
	length := binary.BigEndian.Uint32(header[4:])
	if sign != 0 && sign != 1 {
		return sign, length, NewCorruptionError(
			CorruptMemoBlock, "FPT",
			fmt.Sprintf("memo block %d has an unknown signature 0x%08x", block, sign),
		).At(position)
	}
	fptSize, err := fileSize(file, true)
	if err != nil {
		return sign, length, err
	}
	if position+8+int64(length) > fptSize {
		return sign, length, NewCorruptionError(
			CorruptMemoBlock, "FPT",
			fmt.Sprintf("memo block %d declares %d data bytes but the memo file ends earlier", block, length),
		).At(position).Size(fptSize-position-8, int64(length))
	}
	return sign, length, nil
}
