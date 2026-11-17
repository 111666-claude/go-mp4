package mp4

import (
	"errors"
	"fmt"
	"io"
	"time"
	"unicode/utf8"
)

// Errors returned by the chapter editing functions.
var (
	// ErrNoMoov means that the file does not contain a moov box.
	ErrNoMoov = errors.New("moov box not found")
	// ErrMultipleMoov means that the file contains more than one moov box.
	ErrMultipleMoov = errors.New("multiple moov boxes are not supported")
	// ErrUnsupportedLayout means that the file uses a layout which cannot be
	// edited safely, such as fragmented MP4 files or boxes extending to EOF.
	ErrUnsupportedLayout = errors.New("unsupported MP4 layout")
	// ErrInvalidChpl means that an existing chpl box is corrupted.
	ErrInvalidChpl = errors.New("invalid chpl box")
	// ErrEmptyChapterTitle means that a chapter title is empty.
	ErrEmptyChapterTitle = errors.New("chapter title must not be empty")
	// ErrChapterTitleTooLong means that a chapter title exceeds MaxChapterTitleLen bytes.
	ErrChapterTitleTooLong = errors.New("chapter title is too long")
	// ErrChapterTimeOrder means that chapter start times are not strictly increasing.
	ErrChapterTimeOrder = errors.New("chapter start times must be strictly increasing")
)

// Chapter is a single chapter with a start time and a UTF-8 title.
// The start time is an offset from the beginning of the movie.
type Chapter struct {
	StartTime time.Duration
	Title     string
}

// chplUnit is the time.Duration represented by one chpl timestamp tick
// (100 nanoseconds).
const chplUnit = time.Second / time.Duration(ChplTimeScale)

// durationToChplTime converts a time.Duration to the 100-nanosecond units
// stored in the chpl box.
func durationToChplTime(d time.Duration) uint64 {
	return uint64(d / chplUnit)
}

// chplTimeToDuration converts a 100-nanosecond chpl timestamp.
func chplTimeToDuration(t uint64) time.Duration {
	return time.Duration(t) * chplUnit
}

// findBoxes walks the complete box tree and returns every box of the given
// type, regardless of its depth. Unlike ExtractBox with a fixed path, this
// reaches boxes nested under exact-match containers such as udta.
func findBoxes(r io.ReadSeeker, want BoxType) ([]*BoxInfo, error) {
	var found []*BoxInfo
	_, err := ReadBoxStructure(r, func(h *ReadHandle) (interface{}, error) {
		if h.BoxInfo.Type == want {
			found = append(found, &h.BoxInfo)
		}
		// descend into every supported container so the whole tree is visited
		if h.BoxInfo.IsSupportedType() && hasChildBoxes(h.BoxInfo.Type) {
			if _, err := h.Expand(); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
	if err != nil {
		return nil, err
	}
	return found, nil
}

// ReadChapters reads QuickTime chpl chapters (moov/udta/chpl).
// It returns nil chapters and a nil error when the file has no chpl box.
func ReadChapters(r io.ReadSeeker) ([]Chapter, error) {
	bis, err := findBoxes(r, BoxTypeChpl())
	if err != nil {
		return nil, err
	}
	if len(bis) == 0 {
		return nil, nil
	}
	if len(bis) > 1 {
		return nil, fmt.Errorf("%w: multiple chpl boxes", ErrInvalidChpl)
	}

	box, err := readChplPayload(r, bis[0])
	if err != nil {
		return nil, err
	}

	chapters := make([]Chapter, 0, len(box.Entries))
	for _, entry := range box.Entries {
		chapters = append(chapters, Chapter{
			StartTime: chplTimeToDuration(entry.StartTime),
			Title:     entry.Title,
		})
	}
	return chapters, nil
}

// AddChapter appends a chapter to the chpl box. The new start time must be
// greater than the last existing chapter start time. A chpl box is created
// under moov/udta when the file has none.
func AddChapter(r io.ReadSeeker, w io.WriteSeeker, startTime time.Duration, title string) error {
	chapters, err := ReadChapters(r)
	if err != nil {
		return err
	}
	chapters = append(chapters, Chapter{StartTime: startTime, Title: title})
	return writeChapters(r, w, chapters)
}

// ReplaceChapters replaces all chapters with the given ordered chapters.
// Passing an empty slice removes the chpl box, equivalent to RemoveChapters.
// A chpl box is created under moov/udta when the file has none.
func ReplaceChapters(r io.ReadSeeker, w io.WriteSeeker, chapters []Chapter) error {
	copied := make([]Chapter, len(chapters))
	copy(copied, chapters)
	return writeChapters(r, w, copied)
}

// RemoveChapters removes the chpl box. Files without a chpl box are rewritten
// unchanged except for layout validation.
func RemoveChapters(r io.ReadSeeker, w io.WriteSeeker) error {
	return writeChapters(r, w, nil)
}

func readChplPayload(r io.ReadSeeker, bi *BoxInfo) (*Chpl, error) {
	if _, err := bi.SeekToPayload(r); err != nil {
		return nil, err
	}
	if err := checkPayloadSize(r, bi); err != nil {
		return nil, err
	}
	box, _, err := UnmarshalAny(r, bi.Type, bi.Size-bi.HeaderSize, bi.Context)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidChpl, err)
	}
	chpl, ok := box.(*Chpl)
	if !ok {
		return nil, fmt.Errorf("%w: unexpected box type %s", ErrInvalidChpl, bi.Type.String())
	}
	return chpl, nil
}

func validateChapters(chapters []Chapter) error {
	var prev time.Duration
	for i, chapter := range chapters {
		if chapter.Title == "" {
			return fmt.Errorf("%w: index %d", ErrEmptyChapterTitle, i)
		}
		if !utf8.ValidString(chapter.Title) {
			return fmt.Errorf("%w: index %d is not valid UTF-8", ErrInvalidChpl, i)
		}
		if len(chapter.Title) > MaxChapterTitleLen {
			return fmt.Errorf("%w: index %d has %d bytes", ErrChapterTitleTooLong, i, len(chapter.Title))
		}
		if chapter.StartTime < 0 {
			return fmt.Errorf("%w: index %d has negative start time", ErrChapterTimeOrder, i)
		}
		if i > 0 && chapter.StartTime <= prev {
			return fmt.Errorf("%w: index %d starts at %s after %s", ErrChapterTimeOrder, i, chapter.StartTime, prev)
		}
		prev = chapter.StartTime
	}
	return nil
}

// chapterLayout describes the locations needed to rewrite the chpl box and
// to keep file offsets consistent.
type chapterLayout struct {
	moov *BoxInfo
	udta *BoxInfo
	chpl *BoxInfo
	// moovBeforeMdat is true when the moov box precedes every mdat box.
	moovBeforeMdat bool
	delta          int64
}

// scanChapterLayout validates the file layout and computes the size delta
// applied to the moov box by the upcoming rewrite.
func scanChapterLayout(r io.ReadSeeker, chapters []Chapter) (*chapterLayout, error) {
	if err := validateChapters(chapters); err != nil {
		return nil, err
	}

	var (
		moovInfos       []*BoxInfo
		moovCount       int
		mdatCount       int
		firstMdatOffset uint64
		haveMdat        bool
	)

	_, err := ReadBoxStructure(r, func(h *ReadHandle) (interface{}, error) {
		switch h.BoxInfo.Type {
		case BoxTypeMoov():
			moovCount++
			moovInfos = append(moovInfos, &h.BoxInfo)
		case BoxTypeMdat():
			mdatCount++
			if !haveMdat {
				firstMdatOffset = h.BoxInfo.Offset
				haveMdat = true
			}
		}
		if h.BoxInfo.ExtendToEOF {
			return nil, fmt.Errorf("%w: box %s extends to EOF", ErrUnsupportedLayout, h.BoxInfo.Type.String())
		}
		// descend into every supported container so the whole tree is visited
		if h.BoxInfo.IsSupportedType() && hasChildBoxes(h.BoxInfo.Type) {
			if _, err := h.Expand(); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
	if err != nil {
		return nil, err
	}

	if moovCount == 0 {
		return nil, ErrNoMoov
	}
	if moovCount > 1 {
		return nil, ErrMultipleMoov
	}
	if mdatCount > 1 {
		return nil, fmt.Errorf("%w: multiple mdat boxes", ErrUnsupportedLayout)
	}

	moov := moovInfos[0]

	// mfra (inside moov) only exists in fragmented files.
	mfras, err := findBoxes(r, BoxTypeMfra())
	if err != nil {
		return nil, err
	}
	if len(mfras) > 0 {
		return nil, fmt.Errorf("%w: fragmented MP4 is not supported", ErrUnsupportedLayout)
	}

	udtas, err := findBoxes(r, BoxTypeUdta())
	if err != nil {
		return nil, err
	}
	if len(udtas) > 1 {
		return nil, fmt.Errorf("%w: multiple udta boxes", ErrUnsupportedLayout)
	}

	var udta *BoxInfo
	if len(udtas) == 1 {
		udta = udtas[0]
	}

	chpls, err := findBoxes(r, BoxTypeChpl())
	if err != nil {
		return nil, err
	}
	if len(chpls) > 1 {
		return nil, fmt.Errorf("%w: multiple chpl boxes", ErrInvalidChpl)
	}

	var chplInfo *BoxInfo
	if len(chpls) == 1 {
		chplInfo = chpls[0]
		// A chpl box must live directly under a udta box.
		if udta == nil || chplInfo.Offset < udta.Offset || chplInfo.Offset >= udta.Offset+udta.Size {
			return nil, fmt.Errorf("%w: chpl box is not located under moov/udta", ErrUnsupportedLayout)
		}
		// Validate the existing box eagerly so corrupted input fails
		// before any output byte is written.
		if _, err := readChplPayload(r, chplInfo); err != nil {
			return nil, err
		}
	}

	// Compute the new chpl box size (0 means the box is removed).
	var newChplSize uint64
	if len(chapters) > 0 {
		// header + version/flags(4) + unknown(4) + count(1)
		newChplSize = SmallHeaderSize + 4 + 4 + 1
		for _, chapter := range chapters {
			newChplSize += 8 + 1 + uint64(len(chapter.Title))
		}
	}

	var oldUdtaSize uint64
	if udta != nil {
		oldUdtaSize = udta.Size
	}
	newUdtaSize := oldUdtaSize
	if chplInfo != nil {
		newUdtaSize -= chplInfo.Size
	}
	if newChplSize > 0 {
		if udta == nil {
			newUdtaSize += SmallHeaderSize // new udta header
		}
		newUdtaSize += newChplSize
	}
	if udta != nil && newUdtaSize == SmallHeaderSize {
		// An empty udta box is pointless; drop it entirely.
		newUdtaSize = 0
	}

	delta := int64(newUdtaSize) - int64(oldUdtaSize)

	return &chapterLayout{
		moov:           moov,
		udta:           udta,
		chpl:           chplInfo,
		moovBeforeMdat: !haveMdat || moov.Offset < firstMdatOffset,
		delta:          delta,
	}, nil
}

func writeChapters(r io.ReadSeeker, w io.WriteSeeker, chapters []Chapter) error {
	layout, err := scanChapterLayout(r, chapters)
	if err != nil {
		return err
	}

	writer := NewWriter(w)

	_, err = ReadBoxStructure(r, func(h *ReadHandle) (interface{}, error) {
		bi := &h.BoxInfo

		// The old chpl box is replaced or removed. It must be checked before
		// the generic leaf handling because chpl is not a container.
		if bi.Type == BoxTypeChpl() && layout.chpl != nil && bi.Offset == layout.chpl.Offset {
			if len(chapters) == 0 {
				return uint64(0), nil
			}
			return nil, writeNewChpl(writer, chapters)
		}

		// mdat and other leaf boxes are never re-marshalled: they are copied
		// byte-for-byte, so media data is never re-encoded.
		if !bi.IsSupportedType() || bi.Type == BoxTypeMdat() || !hasChildBoxes(bi.Type) {
			// Keep chunk offsets consistent when bytes before the mdat move.
			if layout.delta != 0 && layout.moovBeforeMdat {
				switch bi.Type {
				case BoxTypeStco(), BoxTypeCo64():
					return nil, rewriteOffsetBox(r, writer, h, layout.delta)
				}
			}
			return nil, writer.CopyBox(r, bi)
		}

		// Stream container boxes recursively.
		if err := startContainerAndCopyFixedPayload(r, writer, h); err != nil {
			return nil, err
		}
		if _, err := h.Expand(); err != nil {
			return nil, err
		}

		// Append a new udta/chpl at the end of a moov box which had
		// neither box.
		if bi.Type == BoxTypeMoov() && layout.udta == nil && len(chapters) > 0 {
			if err := writeUdtaWithChpl(writer, chapters); err != nil {
				return nil, err
			}
		}
		// Append the new chpl at the end of an existing udta box.
		if bi.Type == BoxTypeUdta() && layout.chpl == nil && len(chapters) > 0 {
			if err := writeNewChpl(writer, chapters); err != nil {
				return nil, err
			}
		}

		if _, err := writer.EndBox(); err != nil {
			return nil, err
		}
		return nil, nil
	})
	if err != nil {
		return err
	}

	// Verify that the produced size matches the computed delta. This guards
	// against offset corruption.
	outEnd, err := w.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	inEnd, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	if int64(outEnd) != int64(inEnd)+layout.delta {
		return fmt.Errorf("%w: unexpected output size: got %d want %d", ErrUnsupportedLayout, outEnd, int64(inEnd)+layout.delta)
	}
	return nil
}

// containerBoxTypes is the whitelist of supported boxes whose payload
// consists of child boxes rather than fixed fields. Their children are
// streamed recursively through the rewrite handler.
var containerBoxTypes = map[BoxType]struct{}{
	BoxTypeMoov():        {},
	BoxTypeTrak():        {},
	BoxTypeEdts():        {},
	BoxTypeMdia():        {},
	BoxTypeMinf():        {},
	BoxTypeDinf():        {},
	BoxTypeDref():        {},
	BoxTypeStbl():        {},
	BoxTypeStsd():        {},
	BoxTypeUdta():        {},
	BoxTypeMeta():        {},
	BoxTypeIlst():        {},
	BoxTypeMvex():        {},
	BoxTypeMoof():        {},
	BoxTypeTraf():        {},
	BoxTypeMfra():        {},
	BoxTypeWave():        {},
	BoxTypeSinf():        {},
	BoxTypeSchi():        {},
	BoxTypeVttc():        {},
	BoxTypeVttC():        {},
	StrToBoxType("----"): {},
}

// hasChildBoxes reports whether the box is defined as a container whose
// children must be streamed individually.
func hasChildBoxes(t BoxType) bool {
	_, ok := containerBoxTypes[t]
	return ok
}

// startContainerAndCopyFixedPayload writes the container box header and copies
// its fixed payload bytes (e.g. FullBox version/flags and stsd entry count)
// verbatim, stopping just before its first child box. The fixed payload is
// parsed only to learn where the children begin; the original bytes are kept
// so QuickTime-style and ISO-style containers are preserved identically.
func startContainerAndCopyFixedPayload(r io.ReadSeeker, w *Writer, h *ReadHandle) error {
	if _, err := w.StartBox(&h.BoxInfo); err != nil {
		return err
	}

	// Parse the fixed payload to learn where the child boxes begin.
	_, n, err := h.ReadPayload()
	if err != nil {
		return err
	}

	// Plain containers (Box with no fixed fields) consume nothing.
	if n == 0 {
		return nil
	}

	bi := &h.BoxInfo
	if _, err := bi.SeekToPayload(r); err != nil {
		return err
	}
	// Keep the original fixed bytes verbatim (version/flags, counts, etc.)
	// so QuickTime-style and ISO-style containers are preserved identically.
	if _, err := io.CopyN(w, r, int64(n)); err != nil {
		return err
	}
	return nil
}

func buildChplBox(chapters []Chapter) *Chpl {
	chpl := &Chpl{
		Version: 1,
		Entries: make([]ChplEntry, 0, len(chapters)),
	}
	for _, chapter := range chapters {
		chpl.Entries = append(chpl.Entries, ChplEntry{
			StartTime: durationToChplTime(chapter.StartTime),
			Title:     chapter.Title,
		})
	}
	return chpl
}

func writeNewChpl(w *Writer, chapters []Chapter) error {
	if _, err := w.StartBox(&BoxInfo{
		Type:       BoxTypeChpl(),
		HeaderSize: SmallHeaderSize,
		Size:       SmallHeaderSize,
	}); err != nil {
		return err
	}
	if _, err := Marshal(w, buildChplBox(chapters), Context{}); err != nil {
		return err
	}
	if _, err := w.EndBox(); err != nil {
		return err
	}
	return nil
}

func writeUdtaWithChpl(w *Writer, chapters []Chapter) error {
	if _, err := w.StartBox(&BoxInfo{
		Type:       BoxTypeUdta(),
		HeaderSize: SmallHeaderSize,
		Size:       SmallHeaderSize,
	}); err != nil {
		return err
	}
	if err := writeNewChpl(w, chapters); err != nil {
		return err
	}
	if _, err := w.EndBox(); err != nil {
		return err
	}
	return nil
}

// rewriteOffsetBox re-marshals an stco or co64 box after shifting every chunk
// offset by delta. stco entries that overflow uint32 are rejected.
func rewriteOffsetBox(r io.ReadSeeker, w *Writer, h *ReadHandle, delta int64) error {
	box, _, err := h.ReadPayload()
	if err != nil {
		return err
	}

	switch b := box.(type) {
	case *Stco:
		for i, offset := range b.ChunkOffset {
			shifted := int64(offset) + delta
			if shifted < 0 || shifted > int64(^uint32(0)) {
				return fmt.Errorf("%w: stco offset overflow at index %d", ErrUnsupportedLayout, i)
			}
			b.ChunkOffset[i] = uint32(shifted)
		}
	case *Co64:
		for i, offset := range b.ChunkOffset {
			shifted := int64(offset) + delta
			if shifted < 0 {
				return fmt.Errorf("%w: co64 offset underflow at index %d", ErrUnsupportedLayout, i)
			}
			b.ChunkOffset[i] = uint64(shifted)
		}
	default:
		return fmt.Errorf("%w: unexpected offset box type %s", ErrUnsupportedLayout, h.BoxInfo.Type.String())
	}

	if _, err := w.StartBox(&h.BoxInfo); err != nil {
		return err
	}
	if _, err := Marshal(w, box, h.BoxInfo.Context); err != nil {
		return err
	}
	if _, err := w.EndBox(); err != nil {
		return err
	}
	return nil
}
