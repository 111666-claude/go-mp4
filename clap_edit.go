package mp4

import (
	"errors"
	"fmt"
	"io"
	"math"
)

// CleanApertureBoxSize is the on-wire size of a clap box including its
// 8-byte header: 8 header bytes plus eight 32-bit rational fields.
const CleanApertureBoxSize = 40

var (
	// ErrCleanApertureNotFound is returned when a clap box is expected but
	// does not exist.
	ErrCleanApertureNotFound = errors.New("clap box not found")
	// ErrDuplicateCleanAperture is returned when a sample entry contains
	// more than one clap box, which is forbidden by ISO/IEC 14496-12.
	ErrDuplicateCleanAperture = errors.New("duplicate clap box")
	// ErrSampleEntryNotFound is returned when the specified video sample
	// entry does not exist in the file.
	ErrSampleEntryNotFound = errors.New("video sample entry not found")
)

// sizeShift describes a size change at an input-file position. Byte offsets
// pointing at or after "at" move by "delta"; earlier offsets are unaffected.
type sizeShift struct {
	at    int64
	delta int64
}

// SetCleanAperture adds or replaces the clap box of every video sample entry
// whose box type equals sampleEntryType (e.g. BoxTypeAvc1()). The rest of the
// file, including other child boxes of the sample entry and the mdat payload,
// is preserved byte-for-byte except for the chunk offset tables (stco/co64),
// which are rewritten so that they keep pointing at the same media data.
//
// The rational numbers carried by clap are validated before the file is
// touched: zero denominators and an aperture that is not fully inside the
// encoded image produce an error.
func SetCleanAperture(r io.ReadSeeker, w io.WriteSeeker, sampleEntryType BoxType, clap *CleanApertureBox) error {
	return editCleanAperture(r, w, sampleEntryType, clap, true)
}

// RemoveCleanAperture removes the clap box from every video sample entry
// whose box type equals sampleEntryType (e.g. BoxTypeAvc1()). It returns
// ErrCleanApertureNotFound when the sample entry exists but has no clap box
// and ErrSampleEntryNotFound when the sample entry does not exist. The mdat
// payload is preserved and chunk offset tables are rewritten accordingly.
func RemoveCleanAperture(r io.ReadSeeker, w io.WriteSeeker, sampleEntryType BoxType) error {
	return editCleanAperture(r, w, sampleEntryType, nil, false)
}

func editCleanAperture(r io.ReadSeeker, w io.WriteSeeker, sampleEntryType BoxType, clap *CleanApertureBox, setMode bool) error {
	if !isVisualSampleEntryType(sampleEntryType) {
		return fmt.Errorf("box type %s is not a video sample entry", sampleEntryType.String())
	}
	if setMode {
		if clap == nil {
			return errors.New("clap box must not be nil")
		}
		clap.SetType(BoxTypeClap())
		// Intrinsic checks (zero denominators, positive aperture) before any
		// I/O happens. Bounds against the actual image are re-checked once the
		// sample entry dimensions are known.
		if err := clap.Validate(math.MaxUint32, math.MaxUint32); err != nil {
			return err
		}
	}

	writer := NewWriter(w)

	// Pre-scan the targeted sample entries so every value and structural
	// error is reported before any output byte is produced. This makes the
	// edit atomic: a failure leaves the destination untouched.
	if err := preflightCleanAperture(r, sampleEntryType, clap, setMode); err != nil {
		return err
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return err
	}

	// shifts records size changes in input-file order. Chunk offsets are
	// adjusted only for edits located before the referenced media data, which
	// keeps the mdat payload reachable even when it precedes the moov box.
	var shifts []sizeShift
	// typeStack records the box types from the root to the current box and is
	// used to enforce the clap box hierarchy.
	typeStack := make([]BoxType, 0, 16)
	targetEntryCount := 0

	_, err := ReadBoxStructure(r, func(h *ReadHandle) (interface{}, error) {
		typeStack = append(typeStack, h.BoxInfo.Type)
		defer func() { typeStack = typeStack[:len(typeStack)-1] }()

		switch {
		// The targeted video sample entry: rewrite its fixed payload and walk
		// its children manually so that clap boxes can be inserted/removed
		// without disturbing sibling boxes (avcC, pasp, ...).
		case h.BoxInfo.Type == sampleEntryType:
			targetEntryCount++
			diff, e := rewriteVisualSampleEntry(r, writer, h, clap, setMode)
			if e != nil {
				return nil, e
			}
			if diff != 0 {
				shifts = append(shifts, sizeShift{
					at:    int64(h.BoxInfo.Offset + h.BoxInfo.Size),
					delta: diff,
				})
			}
			return nil, nil

		// clap anywhere outside a visual sample entry is a hierarchy
		// violation. clap boxes inside the targeted sample entry are consumed
		// by rewriteVisualSampleEntry and never reach this handler.
		case h.BoxInfo.Type == BoxTypeClap():
			if len(typeStack) < 2 || !isVisualSampleEntryType(typeStack[len(typeStack)-2]) {
				return nil, fmt.Errorf("clap box must be a child of a video sample entry, found under %s",
					parentTypeName(typeStack))
			}
			return copyGenericBox(r, writer, h)

		// Rewrite chunk offset tables, adjusting only offsets affected by an
		// earlier size change.
		case h.BoxInfo.Type == BoxTypeStco():
			return rewriteStco(writer, h, shifts)

		case h.BoxInfo.Type == BoxTypeCo64():
			return rewriteCo64(writer, h, shifts)

		case !h.BoxInfo.IsSupportedType() || h.BoxInfo.Type == BoxTypeMdat():
			return copyGenericBox(r, writer, h)

		default:
			if _, e := writer.StartBox(&h.BoxInfo); e != nil {
				return nil, e
			}
			box, _, e := h.ReadPayload()
			if e != nil {
				return nil, e
			}
			if _, e := Marshal(writer, box, h.BoxInfo.Context); e != nil {
				return nil, e
			}
			if _, e := h.Expand(); e != nil {
				return nil, e
			}
			if _, e := writer.EndBox(); e != nil {
				return nil, e
			}
			return nil, nil
		}
	})
	if err != nil {
		return err
	}
	if targetEntryCount == 0 {
		return ErrSampleEntryNotFound
	}
	return nil
}

// shiftForOffset sums the size changes located at or before an input offset.
func shiftForOffset(shifts []sizeShift, offset int64) int64 {
	var total int64
	for _, s := range shifts {
		if s.at <= offset {
			total += s.delta
		}
	}
	return total
}

// preflightCleanAperture validates every targeted sample entry without
// writing anything: existence, the at-most-one-clap rule, integrity of an
// existing clap box and bounds of the new values against the image size.
func preflightCleanAperture(r io.ReadSeeker, sampleEntryType BoxType, clap *CleanApertureBox, setMode bool) error {
	entryBIs, err := ExtractBoxes(r, nil, []BoxPath{{
		BoxTypeMoov(), BoxTypeTrak(), BoxTypeMdia(), BoxTypeMinf(),
		BoxTypeStbl(), BoxTypeStsd(), sampleEntryType,
	}})
	if err != nil {
		return err
	}
	if len(entryBIs) == 0 {
		return ErrSampleEntryNotFound
	}
	for _, entry := range entryBIs {
		payloads, err := ExtractBoxesWithPayload(r, entry, []BoxPath{{BoxTypeClap()}})
		if err != nil {
			return err
		}
		if len(payloads) > 1 {
			return ErrDuplicateCleanAperture
		}
		if !setMode && len(payloads) == 0 {
			return ErrCleanApertureNotFound
		}
		if setMode {
			if _, err := entry.SeekToPayload(r); err != nil {
				return err
			}
			box, _, err := UnmarshalAny(r, sampleEntryType,
				entry.Size-entry.HeaderSize, entry.Context)
			if err != nil {
				return fmt.Errorf("failed to read %s sample entry: %w", sampleEntryType.String(), err)
			}
			vse, ok := box.(*VisualSampleEntry)
			if !ok {
				return fmt.Errorf("box type %s is not a visual sample entry", sampleEntryType.String())
			}
			if err := clap.Validate(uint32(vse.Width), uint32(vse.Height)); err != nil {
				return err
			}
		}
	}
	return nil
}

// rewriteVisualSampleEntry returns the signed difference between the output
// and the input size of the rewritten sample entry.
func rewriteVisualSampleEntry(r io.ReadSeeker, w *Writer, h *ReadHandle, clap *CleanApertureBox, setMode bool) (int64, error) {
	inSize := int64(h.BoxInfo.Size)

	if _, err := w.StartBox(&h.BoxInfo); err != nil {
		return 0, err
	}
	box, _, err := h.ReadPayload()
	if err != nil {
		return 0, err
	}
	vse, ok := box.(*VisualSampleEntry)
	if !ok {
		return 0, fmt.Errorf("box type %s is not a visual sample entry", h.BoxInfo.Type.String())
	}
	if setMode {
		if err := clap.Validate(uint32(vse.Width), uint32(vse.Height)); err != nil {
			return 0, err
		}
	}
	if _, err := Marshal(w, vse, h.BoxInfo.Context); err != nil {
		return 0, err
	}

	childStart, err := r.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	childEnd := int64(h.BoxInfo.Offset + h.BoxInfo.Size)

	clapCount := 0
	for childStart < childEnd {
		childBI, err := ReadBoxInfo(r)
		if err != nil {
			return 0, err
		}
		if int64(childBI.Offset+childBI.Size) > childEnd {
			return 0, fmt.Errorf("child box %s exceeds %s sample entry bounds",
				childBI.Type.String(), h.BoxInfo.Type.String())
		}
		if childBI.Type == BoxTypeClap() {
			clapCount++
			if clapCount > 1 {
				return 0, ErrDuplicateCleanAperture
			}
			// Parse the existing box so that a corrupt clap (e.g. a zero
			// denominator) is rejected rather than silently carried over.
			if _, _, err := UnmarshalAny(r, childBI.Type,
				childBI.Size-childBI.HeaderSize, h.BoxInfo.Context); err != nil {
				return 0, err
			}
			if setMode {
				if _, err := writeCleanApertureBox(w, clap, h.BoxInfo.Context); err != nil {
					return 0, err
				}
			}
			// in remove mode the box is simply skipped
			if _, err := childBI.SeekToEnd(r); err != nil {
				return 0, err
			}
		} else {
			if err := w.CopyBox(r, childBI); err != nil {
				return 0, err
			}
		}
		childStart, err = r.Seek(0, io.SeekCurrent)
		if err != nil {
			return 0, err
		}
	}

	if setMode && clapCount == 0 {
		if _, err := writeCleanApertureBox(w, clap, h.BoxInfo.Context); err != nil {
			return 0, err
		}
	}
	if !setMode && clapCount == 0 {
		return 0, ErrCleanApertureNotFound
	}

	outBI, err := w.EndBox()
	if err != nil {
		return 0, err
	}
	return int64(outBI.Size) - inSize, nil
}

func writeCleanApertureBox(w *Writer, clap *CleanApertureBox, ctx Context) (*BoxInfo, error) {
	if _, err := w.StartBox(&BoxInfo{Type: BoxTypeClap(), Size: CleanApertureBoxSize}); err != nil {
		return nil, err
	}
	if _, err := Marshal(w, clap, ctx); err != nil {
		return nil, err
	}
	return w.EndBox()
}

func copyGenericBox(r io.ReadSeeker, w *Writer, h *ReadHandle) (interface{}, error) {
	return nil, w.CopyBox(r, &h.BoxInfo)
}

func rewriteStco(w *Writer, h *ReadHandle, shifts []sizeShift) (interface{}, error) {
	box, _, err := h.ReadPayload()
	if err != nil {
		return nil, err
	}
	stco := box.(*Stco)
	for i, offset := range stco.ChunkOffset {
		next := int64(offset) + shiftForOffset(shifts, int64(offset))
		if next < 0 || next > math.MaxUint32 {
			return nil, fmt.Errorf("updated chunk offset %d does not fit in stco (32-bit); use co64 instead", next)
		}
		stco.ChunkOffset[i] = uint32(next)
	}
	if _, err := w.StartBox(&h.BoxInfo); err != nil {
		return nil, err
	}
	if _, err := Marshal(w, stco, h.BoxInfo.Context); err != nil {
		return nil, err
	}
	if _, err := h.Expand(); err != nil {
		return nil, err
	}
	if _, err := w.EndBox(); err != nil {
		return nil, err
	}
	return nil, nil
}

func rewriteCo64(w *Writer, h *ReadHandle, shifts []sizeShift) (interface{}, error) {
	box, _, err := h.ReadPayload()
	if err != nil {
		return nil, err
	}
	co64 := box.(*Co64)
	for i, offset := range co64.ChunkOffset {
		d := shiftForOffset(shifts, int64(offset))
		var next uint64
		if d >= 0 {
			next = offset + uint64(d)
			if next < offset {
				return nil, fmt.Errorf("updated chunk offset overflows co64: old=%d delta=%d", offset, d)
			}
		} else {
			shift := uint64(-d)
			if shift > offset {
				return nil, fmt.Errorf("updated chunk offset is negative: old=%d delta=%d", offset, d)
			}
			next = offset - shift
		}
		co64.ChunkOffset[i] = next
	}
	if _, err := w.StartBox(&h.BoxInfo); err != nil {
		return nil, err
	}
	if _, err := Marshal(w, co64, h.BoxInfo.Context); err != nil {
		return nil, err
	}
	if _, err := h.Expand(); err != nil {
		return nil, err
	}
	if _, err := w.EndBox(); err != nil {
		return nil, err
	}
	return nil, nil
}

func parentTypeName(typeStack []BoxType) string {
	if len(typeStack) < 2 {
		return "<root>"
	}
	return typeStack[len(typeStack)-2].String()
}

// isVisualSampleEntryType reports whether t is one of the box types parsed as
// VisualSampleEntry, i.e. containers allowed to hold a clap box.
func isVisualSampleEntryType(t BoxType) bool {
	switch t {
	case BoxTypeMp4v(),
		BoxTypeAvc1(),
		BoxTypeEncv(),
		BoxTypeHev1(),
		BoxTypeHvc1(),
		BoxTypeVp08(),
		BoxTypeVp09(),
		BoxTypeAv01():
		return true
	}
	return false
}
