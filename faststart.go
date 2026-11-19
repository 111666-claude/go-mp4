package mp4

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
)

// Errors returned by FastStart and FastStartFile.
// They can be tested with errors.Is.
var (
	// ErrFastStartNotApplicable means that the input is not a single-moov,
	// non-fragmented MP4 file that can be reorganized by FastStart.
	ErrFastStartNotApplicable = errors.New("fast start is not applicable to this MP4 file")

	// ErrChunkOffsetOverflow means that a chunk offset does not fit into
	// the offset table (stco uses 32-bit unsigned integers).
	ErrChunkOffsetOverflow = errors.New("chunk offset overflow")

	// ErrInvalidBox means that the input contains a corrupted box structure.
	ErrInvalidBox = errors.New("invalid MP4 box")
)

// fastStartCopyBufferSize is the size of the temporary buffer used while
// box payloads (including mdat media data) are streamed. Media payloads
// are never fully loaded into memory.
const fastStartCopyBufferSize = 1024 * 1024

// FastStart reorganizes a non-fragmented MP4 stream so that the single moov
// box is placed before the mdat box (a.k.a. "fast start" or "web optimized").
// All chunk offsets in stco/co64 boxes are updated to the new positions.
//
// FastStart targets single-moov, non-fragmented MP4 files. It returns
// ErrFastStartNotApplicable (possibly wrapped) when the input contains
// multiple moov/mdat boxes, fragmentation related boxes (moof/styp/mvex),
// or no moov/mdat box at all. It returns ErrInvalidBox for a corrupted box
// structure, ErrChunkOffsetOverflow when an updated offset does not fit,
// and propagates I/O errors from r and w.
//
// Unknown boxes and mdat payloads are copied byte-by-byte without being
// parsed, and data is streamed with a bounded buffer, so large media
// payloads are not loaded into memory. When the input is already fast start,
// the output is byte-identical to the input.
//
// r must support Seek and w must support Seek.
func FastStart(r io.ReadSeeker, w io.WriteSeeker) error {
	plan, err := scanFastStartPlan(r)
	if err != nil {
		return err
	}

	if plan.alreadyFastStart {
		// The moov box already precedes the mdat box: emit the input as-is
		// so that already-fast-start files are copied byte-identically.
		if _, err := r.Seek(0, io.SeekStart); err != nil {
			return err
		}
		if _, err := w.Seek(0, io.SeekStart); err != nil {
			return err
		}
		buf := make([]byte, fastStartCopyBufferSize)
		if _, err := io.CopyBuffer(w, r, buf); err != nil {
			return err
		}
		return nil
	}

	return writeFastStart(r, w, plan)
}

// FastStartFile is a file-oriented wrapper of FastStart.
// It writes the reorganized MP4 to a temporary file in the destination
// directory and atomically renames it to outputPath only after the whole
// output was written successfully. A write failure therefore never leaves
// a truncated file at outputPath. The input file is not modified.
func FastStartFile(inputPath, outputPath string) error {
	in, err := os.Open(inputPath)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.CreateTemp(filepath.Dir(outputPath), "mp4-faststart-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := out.Name()
	cleanup := func() {
		out.Close()
		os.Remove(tmpPath)
	}

	if err := FastStart(in, out); err != nil {
		cleanup()
		return err
	}
	if err := out.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, outputPath); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}

// fastStartPlan holds the information collected by the pre-write scan.
type fastStartPlan struct {
	fileSize uint64

	moov *rawBox
	mdat *rawBox

	// topLevel is every top-level box in the original order.
	topLevel []*rawBox

	// delta is added to every stco/co64 chunk offset.
	// delta = newMdatOffset - oldMdatOffset
	delta int64

	alreadyFastStart bool
}

// rawBox is a box located without parsing its payload.
type rawBox struct {
	info BoxInfo

	children    []*rawBox
	hasChildren bool

	// legacyMetaPrefix is set for a legacy (QuickTime style) meta box whose
	// payload starts with a 4-byte prefix instead of a FullBox header.
	legacyMetaPrefix bool
}

// scanFastStartPlan walks the top-level boxes and the moov box tree without
// reading any media payload, validates the structure and computes the offset
// delta that must be applied to the chunk offset tables.
func scanFastStartPlan(r io.ReadSeeker) (*fastStartPlan, error) {
	fileSize, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	if fileSize < 0 || uint64(fileSize) > math.MaxInt64 {
		return nil, fmt.Errorf("%w: file is too large: size=%d", ErrFastStartNotApplicable, fileSize)
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}

	plan := &fastStartPlan{
		fileSize: uint64(fileSize),
	}

	var mdatAppeared bool
	var position uint64
	for position < plan.fileSize {
		bi, err := ReadBoxInfo(r)
		if err != nil {
			return nil, fmt.Errorf("%w: failed to read top-level box header at offset %d: %v", ErrInvalidBox, position, err)
		}
		if bi.Offset != position {
			return nil, fmt.Errorf("%w: unexpected box offset: got=%d want=%d", ErrInvalidBox, bi.Offset, position)
		}
		if bi.Size < bi.HeaderSize {
			return nil, fmt.Errorf("%w: box %s size %d is smaller than its header", ErrInvalidBox, bi.Type, bi.Size)
		}
		if bi.Offset+bi.Size > plan.fileSize {
			return nil, fmt.Errorf("%w: box %s exceeds the file: offset=%d size=%d fileSize=%d",
				ErrInvalidBox, bi.Type, bi.Offset, bi.Size, plan.fileSize)
		}

		rb := &rawBox{info: *bi}
		plan.topLevel = append(plan.topLevel, rb)

		switch bi.Type {
		case BoxTypeMoov():
			if plan.moov != nil {
				return nil, fmt.Errorf("%w: multiple moov boxes", ErrFastStartNotApplicable)
			}
			plan.moov = rb
			plan.alreadyFastStart = !mdatAppeared

			// Parse the moov box tree to locate stco/co64 boxes and to
			// reject fragmented content (mvex) and corrupted structures.
			if err := scanMoovTree(r, rb); err != nil {
				return nil, err
			}

		case BoxTypeMdat():
			if plan.mdat != nil {
				return nil, fmt.Errorf("%w: multiple mdat boxes", ErrFastStartNotApplicable)
			}
			if bi.ExtendToEOF {
				return nil, fmt.Errorf("%w: mdat box extending to EOF cannot be reorganized", ErrFastStartNotApplicable)
			}
			plan.mdat = rb
			mdatAppeared = true

		case BoxTypeMoof(), BoxTypeStyp():
			return nil, fmt.Errorf("%w: fragmented MP4 (%s box) is not supported", ErrFastStartNotApplicable, bi.Type)
		}

		// A box extending to EOF is only valid when it is the last box.
		if bi.ExtendToEOF && bi.Offset+bi.Size != plan.fileSize {
			return nil, fmt.Errorf("%w: box %s extending to EOF is not the last box", ErrInvalidBox, bi.Type)
		}

		position = bi.Offset + bi.Size
		if _, err := r.Seek(int64(position), io.SeekStart); err != nil {
			return nil, err
		}
	}

	if plan.moov == nil {
		return nil, fmt.Errorf("%w: moov box not found", ErrFastStartNotApplicable)
	}
	if plan.mdat == nil {
		return nil, fmt.Errorf("%w: mdat box not found", ErrFastStartNotApplicable)
	}

	// Compute the relocation delta. The output layout keeps every non-moov
	// box in its original order and inserts moov directly before mdat. The
	// mdat box therefore moves forward by exactly the moov box size, and
	// every chunk offset it contains increases by that same amount.
	// Each new offset is validated against width overflow while the output
	// is written.
	if plan.moov.info.Size > math.MaxInt64 {
		return nil, fmt.Errorf("%w: moov box is too large", ErrChunkOffsetOverflow)
	}
	plan.delta = int64(plan.moov.info.Size)

	return plan, nil
}

// scanMoovTree recursively parses every known container box inside moov.
// Unknown boxes are treated as opaque leaf boxes and will be copied as-is.
func scanMoovTree(r io.ReadSeeker, parent *rawBox) error {
	if _, err := parent.info.SeekToPayload(r); err != nil {
		return err
	}

	// A legacy (QuickTime style) meta box starts with a 4-byte prefix rather
	// than a FullBox header. A version-0 FullBox with zero flags is
	// byte-identical to an all-zero legacy prefix, so distinguish them by
	// checking whether the bytes following the 4-byte prefix form a valid
	// child box header.
	if parent.info.Type == BoxTypeMeta() {
		payloadStart, err := r.Seek(0, io.SeekCurrent)
		if err != nil {
			return err
		}
		if _, err := r.Seek(payloadStart+4, io.SeekStart); err != nil {
			return err
		}
		var head [8]byte
		if _, err := io.ReadFull(r, head[:]); err != nil {
			return fmt.Errorf("%w: failed to read meta box header: %v", ErrInvalidBox, err)
		}
		if looksLikeBoxHeader(head[:]) &&
			uint64(binary.BigEndian.Uint32(head[:])) <= parent.info.Size-parent.info.HeaderSize-4 {
			// The bytes at payload+4 describe a child box: the first 4 bytes
			// are the legacy meta prefix.
			parent.legacyMetaPrefix = true
		}

		if parent.legacyMetaPrefix {
			// Skip the 4-byte legacy prefix; the child walk starts after it.
			if _, err := r.Seek(payloadStart+4, io.SeekStart); err != nil {
				return err
			}
		} else {
			// Standard FullBox meta: its first child starts at the payload.
			if _, err := r.Seek(payloadStart, io.SeekStart); err != nil {
				return err
			}
		}
	}

	// Child boxes start after the optional 4-byte legacy prefix and end at
	// the meta box's own end. Only the starting region excludes the prefix.
	childAreaEnd := parent.info.Offset + parent.info.Size
	pos, err := r.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	remaining := childAreaEnd - uint64(pos)
	for remaining >= SmallHeaderSize {
		start, err := r.Seek(0, io.SeekCurrent)
		if err != nil {
			return err
		}
		bi, err := ReadBoxInfo(r)
		if err != nil {
			return fmt.Errorf("%w: failed to read box header in %s at offset %d: %v",
				ErrInvalidBox, parent.info.Type, start, err)
		}
		if bi.Size < bi.HeaderSize {
			return fmt.Errorf("%w: box %s size %d is smaller than its header", ErrInvalidBox, bi.Type, bi.Size)
		}
		if bi.Offset+bi.Size > childAreaEnd {
			return fmt.Errorf("%w: box %s exceeds its parent box %s", ErrInvalidBox, bi.Type, parent.info.Type)
		}

		rb := &rawBox{info: *bi}
		parent.children = append(parent.children, rb)
		parent.hasChildren = true

		switch bi.Type {
		case BoxTypeMvex():
			return fmt.Errorf("%w: fragmented MP4 (mvex box) is not supported", ErrFastStartNotApplicable)

		case BoxTypeStco(), BoxTypeCo64():
			if err := validateOffsetTable(r, bi, parent); err != nil {
				return err
			}

		default:
			if isKnownContainerBox(bi.Type) {
				if err := scanMoovTree(r, rb); err != nil {
					return err
				}
			}
		}

		next := bi.Offset + bi.Size
		if next > childAreaEnd {
			return fmt.Errorf("%w: box %s exceeds its parent box %s", ErrInvalidBox, bi.Type, parent.info.Type)
		}
		if _, err := r.Seek(int64(next), io.SeekStart); err != nil {
			return err
		}
		remaining = childAreaEnd - next
	}

	if remaining != 0 {
		return fmt.Errorf("%w: %d trailing byte(s) in box %s", ErrInvalidBox, remaining, parent.info.Type)
	}
	return nil
}

// validateOffsetTable performs structural validation of a stco/co64 box and
// ensures an stbl box contains exactly one chunk offset table (of either type).
func validateOffsetTable(r io.ReadSeeker, bi *BoxInfo, parent *rawBox) error {
	if parent.info.Type != BoxTypeStbl() {
		return fmt.Errorf("%w: %s box must be inside an stbl box", ErrInvalidBox, bi.Type)
	}
	// At most one chunk offset table (stco or co64) may exist per stbl box.
	for _, sibling := range parent.children {
		if sibling.info.Offset == bi.Offset {
			continue
		}
		if sibling.info.Type == BoxTypeStco() || sibling.info.Type == BoxTypeCo64() {
			return fmt.Errorf("%w: multiple chunk offset tables (%s and %s) in one stbl box",
				ErrInvalidBox, sibling.info.Type, bi.Type)
		}
	}

	payloadSize := bi.Size - bi.HeaderSize
	// FullBox header (4 bytes) + entry_count (4 bytes), then entries.
	const offsetTablePrefix = 8
	if payloadSize < offsetTablePrefix {
		return fmt.Errorf("%w: truncated %s box at offset %d", ErrInvalidBox, bi.Type, bi.Offset)
	}

	var entrySize uint64
	if bi.Type == BoxTypeStco() {
		entrySize = 4
	} else {
		entrySize = 8
	}
	if _, err := bi.SeekToPayload(r); err != nil {
		return err
	}
	var prefix [offsetTablePrefix]byte
	if _, err := io.ReadFull(r, prefix[:]); err != nil {
		return fmt.Errorf("%w: failed to read %s header: %v", ErrInvalidBox, bi.Type, err)
	}
	entryCount := uint64(prefix[4])<<24 | uint64(prefix[5])<<16 | uint64(prefix[6])<<8 | uint64(prefix[7])
	if payloadSize != offsetTablePrefix+entryCount*entrySize {
		return fmt.Errorf("%w: %s box size does not match its entry count: size=%d entryCount=%d",
			ErrInvalidBox, bi.Type, bi.Size, entryCount)
	}
	return nil
}

// isKnownContainerBox reports whether a box type is known to contain child
// boxes and therefore must be descended into while scanning the moov tree.
// Any other (unknown) box is copied opaquely without parsing its payload.
func isKnownContainerBox(t BoxType) bool {
	switch t {
	case BoxTypeMoov(),
		BoxTypeTrak(),
		BoxTypeEdts(),
		BoxTypeMdia(),
		BoxTypeMinf(),
		BoxTypeDinf(),
		BoxTypeStbl(),
		BoxTypeUdta(),
		BoxTypeMeta():
		return true
	}
	return false
}

// looksLikeBoxHeader reports whether the 8 bytes look like an MP4 box header:
// a plausible non-zero size followed by a printable 4-character box type.
func looksLikeBoxHeader(b []byte) bool {
	if len(b) < SmallHeaderSize {
		return false
	}
	size := binary.BigEndian.Uint32(b[:4])
	if size == 0 || size < SmallHeaderSize {
		return false
	}
	for _, c := range b[4:8] {
		if !isPrintable(c) {
			return false
		}
	}
	return true
}

// writeFastStart emits the reorganized MP4 according to the scan plan.
func writeFastStart(r io.ReadSeeker, w io.WriteSeeker, plan *fastStartPlan) error {
	buf := make([]byte, fastStartCopyBufferSize)

	// Output order: all boxes preceding the mdat box except moov (in their
	// original order), then moov, then mdat, then all remaining boxes.
	for _, rb := range plan.topLevel {
		switch rb.info.Type {
		case BoxTypeMoov():
			continue
		case BoxTypeMdat():
			if err := copyBoxWithPatchedMoov(r, w, plan.moov, plan, buf); err != nil {
				return err
			}
			if err := copyRawBox(r, w, rb, buf); err != nil {
				return err
			}
		default:
			if err := copyRawBox(r, w, rb, buf); err != nil {
				return err
			}
		}
	}
	return nil
}

// copyRawBox copies a whole box (header and payload) byte-by-byte without
// interpreting the payload.
func copyRawBox(r io.ReadSeeker, w io.Writer, rb *rawBox, buf []byte) error {
	if _, err := rb.info.SeekToStart(r); err != nil {
		return err
	}
	return copyN(w, r, rb.info.Size, buf)
}

// copyBoxWithPatchedMoov copies the moov box while rewriting the chunk
// offset entries inside every stco/co64 box. All other moov bytes are
// copied unchanged.
func copyBoxWithPatchedMoov(r io.ReadSeeker, w io.Writer, moov *rawBox, plan *fastStartPlan, buf []byte) error {
	if _, err := moov.info.SeekToStart(r); err != nil {
		return err
	}
	// Copy the box header as-is. Its size does not change because stco and
	// co64 entries keep their original widths.
	if err := copyN(w, r, moov.info.HeaderSize, buf); err != nil {
		return err
	}
	return copyPatchedChildren(r, w, moov, plan, buf, moov.info.Size-moov.info.HeaderSize)
}

// copyPatchedChildren copies the payload of the given container box.
// The scan phase guarantees that children fully and contiguously cover it.
func copyPatchedChildren(r io.ReadSeeker, w io.Writer, container *rawBox, plan *fastStartPlan, buf []byte, payloadSize uint64) error {
	var copied uint64
	for _, child := range container.children {
		// Copy the child header.
		if _, err := child.info.SeekToStart(r); err != nil {
			return err
		}
		if err := copyN(w, r, child.info.HeaderSize, buf); err != nil {
			return err
		}

		switch child.info.Type {
		case BoxTypeStco(), BoxTypeCo64():
			if err := copyPatchedOffsetTable(r, w, &child.info, plan); err != nil {
				return err
			}

		default:
			payload := child.info.Size - child.info.HeaderSize
			if child.hasChildren {
				// A legacy (QuickTime style) meta box stores a 4-byte prefix
				// before its child boxes; copy it verbatim first.
				if child.legacyMetaPrefix {
					if payload < 4 {
						return fmt.Errorf("%w: truncated legacy meta box at offset %d", ErrInvalidBox, child.info.Offset)
					}
					if err := copyN(w, r, 4, buf); err != nil {
						return err
					}
					payload -= 4
				}
				if err := copyPatchedChildren(r, w, child, plan, buf, payload); err != nil {
					return err
				}
			} else {
				if err := copyN(w, r, payload, buf); err != nil {
					return err
				}
			}
		}
		copied += child.info.Size
	}

	if copied != payloadSize {
		return fmt.Errorf("%w: internal mismatch while copying %s box", ErrInvalidBox, container.info.Type)
	}
	return nil
}

// copyPatchedOffsetTable copies a stco/co64 payload (everything after the
// box header) while adding the relocation delta to every chunk offset.
func copyPatchedOffsetTable(r io.ReadSeeker, w io.Writer, bi *BoxInfo, plan *fastStartPlan) error {
	payloadSize := bi.Size - bi.HeaderSize
	// FullBox version/flags (4 bytes) + entry_count (4 bytes) are copied raw.
	const prefixSize = 8
	if payloadSize < prefixSize {
		return fmt.Errorf("%w: truncated %s box at offset %d", ErrInvalidBox, bi.Type, bi.Offset)
	}
	if _, err := bi.SeekToPayload(r); err != nil {
		return err
	}
	header := make([]byte, prefixSize)
	if _, err := io.ReadFull(r, header); err != nil {
		return fmt.Errorf("%w: failed to read %s header: %v", ErrInvalidBox, bi.Type, err)
	}
	if _, err := w.Write(header); err != nil {
		return err
	}

	entryCount := uint64(header[4])<<24 | uint64(header[5])<<16 | uint64(header[6])<<8 | uint64(header[7])

	mdatPayloadStart := plan.mdat.info.Offset + plan.mdat.info.HeaderSize
	mdatPayloadEnd := plan.mdat.info.Offset + plan.mdat.info.Size

	if bi.Type == BoxTypeStco() {
		entry := make([]byte, 4)
		for i := uint64(0); i < entryCount; i++ {
			if _, err := io.ReadFull(r, entry); err != nil {
				return fmt.Errorf("%w: truncated stco entry %d: %v", ErrInvalidBox, i, err)
			}
			old := uint64(entry[0])<<24 | uint64(entry[1])<<16 | uint64(entry[2])<<8 | uint64(entry[3])
			if old < mdatPayloadStart || old > mdatPayloadEnd {
				return fmt.Errorf("%w: stco chunk offset %d is outside of the mdat payload", ErrInvalidBox, old)
			}
			newOffset, err := addOffsetDelta(old, plan.delta)
			if err != nil {
				return err
			}
			if newOffset > math.MaxUint32 {
				return fmt.Errorf("%w: stco chunk offset %d does not fit in 32 bits after relocation",
					ErrChunkOffsetOverflow, old)
			}
			entry[0] = byte(newOffset >> 24)
			entry[1] = byte(newOffset >> 16)
			entry[2] = byte(newOffset >> 8)
			entry[3] = byte(newOffset)
			if _, err := w.Write(entry); err != nil {
				return err
			}
		}
		if payloadSize != prefixSize+entryCount*4 {
			return fmt.Errorf("%w: stco box size does not match its entry count", ErrInvalidBox)
		}
		return nil
	}

	// co64
	entry := make([]byte, 8)
	for i := uint64(0); i < entryCount; i++ {
		if _, err := io.ReadFull(r, entry); err != nil {
			return fmt.Errorf("%w: truncated co64 entry %d: %v", ErrInvalidBox, i, err)
		}
		var old uint64
		for _, b := range entry {
			old = old<<8 | uint64(b)
		}
		if old < mdatPayloadStart || old > mdatPayloadEnd {
			return fmt.Errorf("%w: co64 chunk offset %d is outside of the mdat payload", ErrInvalidBox, old)
		}
		newOffset, err := addOffsetDelta(old, plan.delta)
		if err != nil {
			return err
		}
		for j := 0; j < 8; j++ {
			entry[j] = byte(newOffset >> (56 - uint(j)*8))
		}
		if _, err := w.Write(entry); err != nil {
			return err
		}
	}
	if payloadSize != prefixSize+entryCount*8 {
		return fmt.Errorf("%w: co64 box size does not match its entry count", ErrInvalidBox)
	}
	return nil
}

// addOffsetDelta applies a signed relocation delta to an unsigned offset
// and rejects unsigned underflow.
func addOffsetDelta(offset uint64, delta int64) (uint64, error) {
	if delta >= 0 {
		d := uint64(delta)
		if offset > math.MaxUint64-d {
			return 0, fmt.Errorf("%w: chunk offset %d overflows after relocation", ErrChunkOffsetOverflow, offset)
		}
		return offset + d, nil
	}
	d := uint64(-delta)
	if offset < d {
		return 0, fmt.Errorf("%w: chunk offset %d underflows after relocation", ErrChunkOffsetOverflow, offset)
	}
	return offset - d, nil
}

// copyN copies exactly size bytes from r to w using the provided buffer.
func copyN(w io.Writer, r io.Reader, size uint64, buf []byte) error {
	for size > 0 {
		n := len(buf)
		if uint64(n) > size {
			n = int(size)
		}
		m, err := io.ReadFull(r, buf[:n])
		if err != nil {
			return err
		}
		if _, err := w.Write(buf[:m]); err != nil {
			return err
		}
		size -= uint64(m)
	}
	return nil
}
