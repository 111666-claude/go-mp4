package mp4

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"

	"github.com/abema/go-mp4/internal/bitio"
)

/*************************** chpl ****************************/

// ChplTimeScale is the time unit of chpl chapter start times:
// timestamps are measured in 100-nanosecond units (1/10,000,000 second),
// independently of the movie timescale in the mvhd box.
const ChplTimeScale uint64 = 10_000_000

// MaxChapterTitleLen is the maximum byte length of a chpl chapter title.
const MaxChapterTitleLen = 255

// Box type and definition -------------------------------------------------

func BoxTypeChpl() BoxType { return StrToBoxType("chpl") }

func init() {
	AddBoxDef(&Chpl{})
}

// Chpl is the Nero/QuickTime chapter list box located at moov/udta/chpl.
//
// The payload layout (matching FFmpeg's mov_read_chpl/mov_write_chpl_tag) is:
//
//	byte     Version (1 for files written by common tools)
//	byte[3]  Flags
//	byte[4]  Unknown (written as zero)
//	uint8    chapter count
//	entry[]  uint64 start time in 100-nanosecond units (ChplTimeScale),
//	         uint8 title length, UTF-8 title bytes
//
// The title length is a single unsigned byte, therefore a title cannot
// exceed 255 bytes.
type Chpl struct {
	Box
	// Version is the first payload byte. Common tools write 1.
	Version uint8 `mp4:"0,size=8"`
	// Flags are the following three bytes (usually zero).
	Flags [3]uint8 `mp4:"1,size=8"`
	// Unknown is a four-byte field written as zero.
	Unknown [4]uint8 `mp4:"2,size=8"`
	// Entries is fully handled by OnReadField and OnWriteField.
	Entries []ChplEntry `mp4:"3,hidden"`
}

// ChplEntry is a single chapter of the chpl box.
type ChplEntry struct {
	// StartTime is the chapter start time in 100-nanosecond units
	// (see ChplTimeScale), independent of the movie timescale.
	StartTime uint64
	// Title is the UTF-8 encoded chapter title.
	Title string
}

// GetType returns the BoxType
func (*Chpl) GetType() BoxType {
	return BoxTypeChpl()
}

// OnReadField parses the chapter count and all chapter entries.
func (chpl *Chpl) OnReadField(name string, r bitio.ReadSeeker, leftBits uint64, ctx Context) (rbits uint64, override bool, err error) {
	if name != "Entries" {
		return 0, false, nil
	}

	if leftBits < 8 {
		return 0, true, errors.New("chpl: truncated chapter count")
	}
	countBuf := make([]byte, 1)
	if _, err = io.ReadFull(r, countBuf); err != nil {
		return 0, true, err
	}
	rbits += 8
	count := int(countBuf[0])

	entries := make([]ChplEntry, 0, count)
	for i := 0; i < count; i++ {
		var entry ChplEntry

		if leftBits-rbits < 72 {
			return rbits, true, fmt.Errorf("chpl: truncated chapter entry at index %d", i)
		}
		timeBuf := make([]byte, 8)
		if _, err = io.ReadFull(r, timeBuf); err != nil {
			return rbits, true, err
		}
		entry.StartTime = binary.BigEndian.Uint64(timeBuf)
		rbits += 64

		lenBuf := make([]byte, 1)
		if _, err = io.ReadFull(r, lenBuf); err != nil {
			return rbits, true, err
		}
		rbits += 8
		titleLen := int(lenBuf[0])

		if uint64(titleLen)*8 > leftBits-rbits {
			return rbits, true, fmt.Errorf("chpl: title length exceeds box size at index %d", i)
		}
		titleBuf := make([]byte, titleLen)
		if _, err = io.ReadFull(r, titleBuf); err != nil {
			return rbits, true, err
		}
		rbits += uint64(titleLen) * 8
		if !utf8.Valid(titleBuf) {
			return rbits, true, fmt.Errorf("chpl: title is not valid UTF-8 at index %d", i)
		}
		entry.Title = string(titleBuf)

		entries = append(entries, entry)
	}

	chpl.Entries = entries
	return rbits, true, nil
}

// OnWriteField writes the chapter count and all chapter entries.
func (chpl *Chpl) OnWriteField(name string, w bitio.Writer, ctx Context) (wbits uint64, override bool, err error) {
	if name != "Entries" {
		return 0, false, nil
	}

	if len(chpl.Entries) > 255 {
		return 0, true, errors.New("chpl: too many chapters")
	}
	if _, err = w.Write([]byte{uint8(len(chpl.Entries))}); err != nil {
		return 0, true, err
	}
	wbits += 8

	for i := range chpl.Entries {
		timeBuf := make([]byte, 8)
		binary.BigEndian.PutUint64(timeBuf, chpl.Entries[i].StartTime)
		if _, err = w.Write(timeBuf); err != nil {
			return wbits, true, err
		}
		wbits += 64

		titleLen := len(chpl.Entries[i].Title)
		if titleLen > MaxChapterTitleLen {
			return wbits, true, fmt.Errorf("chpl: title too long at index %d: %d bytes", i, titleLen)
		}
		if _, err = w.Write([]byte{uint8(titleLen)}); err != nil {
			return wbits, true, err
		}
		wbits += 8

		if _, err = w.Write([]byte(chpl.Entries[i].Title)); err != nil {
			return wbits, true, err
		}
		wbits += uint64(titleLen) * 8
	}

	return wbits, true, nil
}
