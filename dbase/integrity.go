package dbase

import (
	"encoding/binary"
	"io"
)

// CheckIntegrity validates the cross-file invariants the library can prove
// without external tooling:
//
//   - The DBF header shape (FirstRow/RowLength) matches the physical file and
//     every record advertised by RowsCount is present with a valid marker.
//   - Every memo pointer of every present record references a block inside the
//     FPT file whose declared payload is fully present, and referenced blocks
//     do not overlap each other.
//   - The FPT next-free-block counter is consistent with the referenced data.
//
// A *CorruptionError (errors.Is(err, ErrCorrupt)) is returned for hard,
// unrecoverable state. A memo free-list gap is not data loss: it is reported
// through the returned IntegrityFinding while all records remain readable.
//
// CheckIntegrity performs one sequential pass over the DBF records and reads
// only the 8 byte header of every distinct referenced memo block: time
// complexity is O(records*rowLength + memoBlocks), memory is O(memoBlocks)
// for the overlap intervals; memo payloads are never buffered.
func (file *File) CheckIntegrity() (IntegrityFinding, error) {
	finding := IntegrityFinding{}
	if file == nil || file.header == nil {
		return finding, newCorruption(CorruptHeaderShape, "DBF", -1, "table is not initialized")
	}
	h := file.header
	if h.FirstRow == 0 || h.RowLength == 0 {
		return finding, newCorruption(CorruptHeaderShape, "DBF", -1,
			"first-row offset or row length is zero")
	}
	dbfSize, err := handleSize(file.handle)
	if err != nil {
		return finding, err
	}
	dataEnd := int64(h.FirstRow) + int64(h.RowsCount)*int64(h.RowLength)
	if dataEnd > dbfSize {
		missingRows := (dataEnd - dbfSize + int64(h.RowLength) - 1) / int64(h.RowLength)
		return finding, newCorruption(CorruptRecordTruncated, "DBF", dbfSize,
			plural("record", int(missingRows))+" declared in header but missing from file")
	}

	hasMemo := file.memoHeader != nil && file.relatedHandle != nil
	blockSize, fptSize, highBlock := int64(0), int64(0), uint32(0)
	if hasMemo {
		blockSize = int64(file.memoHeader.BlockSize)
		if blockSize <= 0 {
			return finding, newCorruption(CorruptHeaderShape, "FPT", -1,
				"memo block size is zero")
		}
		fptSize, err = handleSize(file.relatedHandle)
		if err != nil {
			return finding, err
		}
		// The 512 byte memo header occupies the first blocks; referenced data
		// must start past it.
		headerBlocks := int64(512) / blockSize
		if int64(512)%blockSize > 0 {
			headerBlocks++
		}
		highBlock = uint32(headerBlocks)
		if fptSize > 0 {
			if need := (fptSize + blockSize - 1) / blockSize; uint32(need) > highBlock {
				highBlock = uint32(need)
			}
		}
	}

	// Overlap bookkeeping over referenced blocks, keyed by [start,end) bytes.
	type span struct{ start, end int64 }
	spans := make([]span, 0)
	containsOverlap := func(s span) (int64, bool) {
		for _, k := range spans {
			if s.start < k.end && k.start < s.end {
				if s.start > k.start {
					return s.start, true
				}
				return k.start, true
			}
		}
		return 0, false
	}

	memoCols := make([]int, 0)
	for i, c := range file.table.columns {
		if DataType(c.DataType) == Memo {
			memoCols = append(memoCols, i)
		}
	}

	// Single raw pass over every record: validate the marker and every memo
	// pointer from the same buffered row.
	rowBuf := make([]byte, h.RowLength)
	for rec := uint32(0); rec < h.RowsCount; rec++ {
		off := int64(h.FirstRow) + int64(rec)*int64(h.RowLength)
		if _, err := readAtHandle(file.handle, rowBuf, off); err != nil {
			return finding, newCorruption(CorruptRecordTruncated, "DBF", off,
				"record "+itoa(int(rec)+1)+" cannot be read").wrap(err)
		}
		switch Marker(rowBuf[0]) {
		case Active, Deleted:
		default:
			return finding, newCorruption(CorruptRecordMarker, "DBF", off,
				"record "+itoa(int(rec)+1)+" has marker 0x"+hexByte(rowBuf[0]))
		}
		if !hasMemo {
			continue
		}
		for _, ci := range memoCols {
			c := file.table.columns[ci]
			fieldStart := 1
			for j := 0; j < ci; j++ {
				fieldStart += int(file.table.columns[j].Length)
			}
			raw := rowBuf[fieldStart : fieldStart+int(c.Length)]
			if isEmptyBytes(raw) {
				continue
			}
			block := binary.LittleEndian.Uint32(raw)
			blockStart := int64(block) * blockSize
			if block == 0 || blockStart < blockSize || blockStart >= fptSize {
				return finding, newCorruption(CorruptMemoPointer, "FPT", blockStart,
					"record "+itoa(int(rec)+1)+" column "+c.Name()+" points at block "+itoa(int(block)))
			}
			var hb [8]byte
			if _, err := readAtHandle(file.relatedHandle, hb[:], blockStart); err != nil {
				return finding, newCorruption(CorruptMemoBlock, "FPT", blockStart,
					"record "+itoa(int(rec)+1)+" column "+c.Name()+" block "+itoa(int(block))+" header unreadable").wrap(err)
			}
			leng := int64(binary.BigEndian.Uint32(hb[4:]))
			payloadEnd := blockStart + 8 + leng
			if payloadEnd > fptSize {
				return finding, newCorruption(CorruptMemoBlock, "FPT", blockStart,
					"record "+itoa(int(rec)+1)+" column "+c.Name()+" block "+itoa(int(block))+
						" declares "+itoa(int(leng))+" payload bytes but the FPT ends at "+itoa(int(fptSize)))
			}
			s := span{blockStart, payloadEnd}
			if at, overlap := containsOverlap(s); overlap {
				return finding, newCorruption(CorruptMemoOverlap, "FPT", at,
					"record "+itoa(int(rec)+1)+" column "+c.Name()+" block "+itoa(int(block))+" overlaps another referenced block")
			}
			spans = append(spans, s)
			if endBlock := uint32((payloadEnd + blockSize - 1) / blockSize); endBlock > highBlock {
				highBlock = endBlock
			}
		}
	}

	finding.HighWaterBlocks = highBlock
	if hasMemo {
		finding.NextFreeBlocks = file.memoHeader.NextFree
		if file.memoHeader.NextFree > highBlock {
			finding.LeakedBlocks = file.memoHeader.NextFree - highBlock
		}
	}
	return finding, nil
}

// readAtHandle reads exactly len(p) bytes at off through any seekable handle.
// It restores the previous cursor so integrity checks have no observable side
// effect on concurrently shared handles.
func readAtHandle(handle interface{}, p []byte, off int64) (int, error) {
	if h, ok := handle.(interface {
		ReadAt([]byte, int64) (int, error)
	}); ok {
		return io.ReadFull(&offsetReaderAt{r: h, off: off}, p)
	}
	h, ok := handle.(io.ReadWriteSeeker)
	if !ok {
		return 0, newCorruption(CorruptHeaderShape, "DBF", -1, "handle is not seekable")
	}
	prev, err := h.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	if _, err := h.Seek(off, io.SeekStart); err != nil {
		return 0, err
	}
	n, err := io.ReadFull(h, p)
	if _, serr := h.Seek(prev, io.SeekStart); serr != nil && err == nil {
		err = serr
	}
	return n, err
}

type offsetReaderAt struct {
	r interface {
		ReadAt([]byte, int64) (int, error)
	}
	off int64
}

func (o *offsetReaderAt) Read(p []byte) (int, error) {
	n, err := o.r.ReadAt(p, o.off)
	o.off += int64(n)
	return n, err
}

// handleSize returns the physical size of an opened handle without changing
// its cursor, using the cheapest primitive available on the platform.
func handleSize(handle interface{}) (int64, error) {
	return platformHandleSize(handle)
}

// seekableSize measures any io.Seeker without disturbing its cursor.
func seekableSize(h interface{}) (int64, error) {
	s, ok := h.(io.Seeker)
	if !ok {
		return 0, newCorruption(CorruptHeaderShape, "DBF", -1, "handle cannot report its size")
	}
	prev, err := s.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	end, err := s.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}
	if _, err := s.Seek(prev, io.SeekStart); err != nil {
		return 0, err
	}
	return end, nil
}

func plural(word string, n int) string {
	if n == 1 {
		return "1 " + word
	}
	return itoa(n) + " " + word + "s"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func hexByte(v byte) string {
	const digits = "0123456789ABCDEF"
	return string([]byte{digits[v>>4], digits[v&0x0f]})
}
