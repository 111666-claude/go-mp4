package mp4

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// boxBytes encodes a single box with an 8-byte header and the given payload.
func boxBytes(t testing.TB, boxType BoxType, payload []byte) []byte {
	t.Helper()
	buf := bytes.NewBuffer(nil)
	if _, err := buf.Write(EncodeBoxInfo(&BoxInfo{
		Size:       SmallHeaderSize + uint64(len(payload)),
		HeaderSize: SmallHeaderSize,
		Type:       boxType,
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := buf.Write(payload); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// emsgPayload encodes an emsg box payload (without the box header).
func emsgPayload(t testing.TB, emsg *Emsg) []byte {
	t.Helper()
	buf := bytes.NewBuffer(nil)
	if _, err := Marshal(buf, emsg, Context{}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// fragmentFile builds a synthetic fragmented MP4:
//
//	ftyp
//	for each segment: styp, emsg..., moof, mdat
//
// emsgs are placed before the moof box of their segment, as DASH packagers do.
func fragmentFile(t testing.TB, emsgsPerSegment [][]*Emsg, mdatSize int) []byte {
	t.Helper()
	file := bytes.NewBuffer(nil)

	ftyp := &Ftyp{
		MajorBrand:       BrandISOM(),
		MinorVersion:     0,
		CompatibleBrands: []CompatibleBrandElem{{CompatibleBrand: BrandISO6()}},
	}
	buf := bytes.NewBuffer(nil)
	_, err := Marshal(buf, ftyp, Context{})
	require.NoError(t, err)
	file.Write(boxBytes(t, BoxTypeFtyp(), buf.Bytes()))

	for _, emsgs := range emsgsPerSegment {
		file.Write(boxBytes(t, BoxTypeStyp(), []byte{'i', 's', 'o', '6', 0, 0, 0, 0}))
		for _, emsg := range emsgs {
			file.Write(boxBytes(t, BoxTypeEmsg(), emsgPayload(t, emsg)))
		}
		// A moof with a single opaque child is enough: the reader only needs
		// the box boundaries to delimit a segment.
		file.Write(boxBytes(t, BoxTypeMoof(), boxBytes(t, BoxTypeMfhd(), []byte{0, 0, 0, 0, 0, 0, 0, 1})))
		file.Write(boxBytes(t, BoxTypeMdat(), make([]byte, mdatSize)))
	}
	return file.Bytes()
}

func v0Emsg(schemeID, value, message string, timescale, delta, duration, id uint32) *Emsg {
	return &Emsg{
		FullBox:               FullBox{Version: 0},
		SchemeIdUri:           schemeID,
		Value:                 value,
		Timescale:             timescale,
		PresentationTimeDelta: delta,
		EventDuration:         duration,
		Id:                    id,
		MessageData:           []byte(message),
	}
}

func v1Emsg(schemeID, value, message string, timescale uint32, presentation uint64, duration, id uint32) *Emsg {
	return &Emsg{
		FullBox:          FullBox{Version: 1},
		SchemeIdUri:      schemeID,
		Value:            value,
		Timescale:        timescale,
		PresentationTime: presentation,
		EventDuration:    duration,
		Id:               id,
		MessageData:      []byte(message),
	}
}

func TestReadEmsgsSyntheticFragments(t *testing.T) {
	// Three segments. Segment 0 has two version 0 events, segment 1 one
	// version 0 event, segment 2 one version 1 event with an absolute time.
	// Segments start at 0s, 10s and 20s; segment 2's absolute time is
	// independent of its start time.
	file := fragmentFile(t, [][]*Emsg{
		{
			v0Emsg("urn:a", "1", "first-payload", 1000, 500, 2000, 1),
			v0Emsg("urn:a", "1", "second-payload", 1000, 1500, 4000, 2),
		},
		{
			v0Emsg("urn:b", "2", "third-payload", 90000, 90000, 180000, 3),
		},
		{
			v1Emsg("urn:c", "3", "fourth-payload", 1000, 23500, 7000, 4),
		},
	}, 128*1024)

	events, err := ReadEmsgs(bytes.NewReader(file), WithEmsgSegmentStartTimes([]time.Duration{
		0,
		10 * time.Second,
		20 * time.Second,
	}))
	require.NoError(t, err)
	require.Len(t, events, 4)

	assert.Equal(t, uint32(1), events[0].Id)
	assert.Equal(t, "urn:a", events[0].SchemeIdUri)
	assert.Equal(t, "1", events[0].Value)
	assert.Equal(t, uint8(0), events[0].Version)
	assert.Equal(t, 500*time.Millisecond, events[0].PresentationTime)
	assert.Equal(t, 2*time.Second, events[0].Duration)
	assert.Equal(t, []byte("first-payload"), events[0].MessageData)

	assert.Equal(t, uint32(2), events[1].Id)
	assert.Equal(t, 1500*time.Millisecond, events[1].PresentationTime)
	assert.Equal(t, 4*time.Second, events[1].Duration)
	assert.Equal(t, []byte("second-payload"), events[1].MessageData)

	// Segment 1 starts at 10s; its timescale is 90000 (one tick = 1ms).
	assert.Equal(t, uint32(3), events[2].Id)
	assert.Equal(t, 10*time.Second+time.Second, events[2].PresentationTime)
	assert.Equal(t, 2*time.Second, events[2].Duration)
	assert.Equal(t, []byte("third-payload"), events[2].MessageData)

	// Version 1 ignores the segment start time and uses its own value.
	assert.Equal(t, uint32(4), events[3].Id)
	assert.Equal(t, uint8(1), events[3].Version)
	assert.Equal(t, 23500*time.Millisecond, events[3].PresentationTime)
	assert.Equal(t, 7*time.Second, events[3].Duration)
	assert.Equal(t, []byte("fourth-payload"), events[3].MessageData)

	// Payloads are not concatenated or shared between events.
	assert.NotEqual(t, events[0].MessageData, events[1].MessageData)
	events[0].MessageData[0] = 'X'
	assert.Equal(t, []byte("second-payload"), events[1].MessageData)
}

func TestReadEmsgsBinaryAndEmptyPayload(t *testing.T) {
	binaryPayload := []byte{0x00, 0xff, 0x10, 0x00, 0x7f, 0x00}
	file := fragmentFile(t, [][]*Emsg{
		{
			v1Emsg("urn:bin", "", string(binaryPayload), 1000, 0, 0, 1),
			v1Emsg("urn:bin", "", "", 1000, 0, 500, 2),
		},
	}, 8)

	events, err := ReadEmsgs(bytes.NewReader(file))
	require.NoError(t, err)
	require.Len(t, events, 2)
	assert.Equal(t, binaryPayload, events[0].MessageData)
	assert.Empty(t, events[1].MessageData)
	assert.Equal(t, 500*time.Millisecond, events[1].Duration)
}

func TestReadEmsgsSegmentStartCallback(t *testing.T) {
	file := fragmentFile(t, [][]*Emsg{
		{v0Emsg("urn:s", "v", "a", 1000, 100, 100, 1)},
		{v0Emsg("urn:s", "v", "b", 1000, 200, 100, 2)},
	}, 16)

	var calls []int
	events, err := ReadEmsgs(bytes.NewReader(file), WithEmsgSegmentStartTime(
		func(segment EmsgSegment) (time.Duration, error) {
			calls = append(calls, segment.Index)
			// Sanity: the moof offset must point at a moof box.
			assert.Equal(t, BoxTypeMoof(), segment.Moof.Type)
			return time.Duration(segment.Index) * 3 * time.Second, nil
		}))
	require.NoError(t, err)
	require.Len(t, events, 2)
	assert.Equal(t, []int{0, 1}, calls)
	assert.Equal(t, 100*time.Millisecond, events[0].PresentationTime)
	assert.Equal(t, 3*time.Second+200*time.Millisecond, events[1].PresentationTime)
}

func TestReadEmsgsRealFragmentedFile(t *testing.T) {
	// The sample contains moof/mdat pairs but no emsg boxes: the scan must
	// traverse it without error and without reading the media payload.
	f, err := os.Open("./testdata/sample_fragmented.mp4")
	require.NoError(t, err)
	defer f.Close()

	events, err := ReadEmsgs(f, WithEmsgSegmentStartTimes([]time.Duration{
		0, time.Second, 2 * time.Second, 3 * time.Second,
		4 * time.Second, 5 * time.Second, 6 * time.Second, 7 * time.Second,
		8 * time.Second, 9 * time.Second, 10 * time.Second, 11 * time.Second,
	}))
	require.NoError(t, err)
	assert.Empty(t, events)
}

func TestReadEmsgsLargeBoxHeader(t *testing.T) {
	// A 16-byte (64-bit) box header must be handled the same way as the
	// common 8-byte header, both for the emsg box and its container.
	payload := emsgPayload(t, v1Emsg("urn:large", "", "large", 1000, 1234, 0, 9))

	largeBox := func(boxType BoxType, body []byte) []byte {
		buf := bytes.NewBuffer(nil)
		buf.Write(EncodeBoxInfo(&BoxInfo{
			Size:       LargeHeaderSize + uint64(len(body)),
			HeaderSize: LargeHeaderSize,
			Type:       boxType,
		}))
		buf.Write(body)
		return buf.Bytes()
	}

	child := largeBox(BoxTypeEmsg(), payload)
	moof := largeBox(BoxTypeMoof(), child)
	file := append(append([]byte{}, moof...), largeBox(BoxTypeMdat(), []byte{1, 2, 3})...)

	events, err := ReadEmsgs(bytes.NewReader(file))
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, uint32(9), events[0].Id)
	assert.Equal(t, 1234*time.Millisecond, events[0].PresentationTime)
	assert.Equal(t, []byte("large"), events[0].MessageData)
	assert.Equal(t, uint64(LargeHeaderSize), events[0].Offset)
}

func TestReadEmsgsEventOffsets(t *testing.T) {
	file := fragmentFile(t, [][]*Emsg{
		{v1Emsg("urn:a", "", "a", 1000, 0, 0, 1)},
		{v1Emsg("urn:a", "", "b", 1000, 0, 0, 2)},
	}, 8)

	events, err := ReadEmsgs(bytes.NewReader(file))
	require.NoError(t, err)
	require.Len(t, events, 2)

	// Each event offset must point at a box header whose type is emsg, and
	// offsets must be strictly increasing in file order.
	for i, event := range events {
		bi, err := ReadBoxInfo(bytes.NewReader(file[event.Offset:]))
		require.NoError(t, err)
		assert.Equal(t, BoxTypeEmsg(), bi.Type)
		if i > 0 {
			assert.Greater(t, event.Offset, events[i-1].Offset)
		}
	}
}

func TestReadEmsgsMultipleEventsInOneSegment(t *testing.T) {
	// Both emsg boxes follow the moof box of segment 0 and precede its mdat,
	// so their segment start is known as soon as they are read. Segment 1
	// then contributes an event placed before its moof box.
	file := bytes.NewBuffer(nil)
	file.Write(boxBytes(t, BoxTypeMoof(), boxBytes(t, BoxTypeMfhd(), []byte{0, 0, 0, 0, 0, 0, 0, 1})))
	file.Write(boxBytes(t, BoxTypeEmsg(), emsgPayload(t, v0Emsg("urn:a", "1", "a", 1000, 0, 0, 1))))
	file.Write(boxBytes(t, BoxTypeEmsg(), emsgPayload(t, v0Emsg("urn:a", "2", "b", 1000, 400, 100, 2))))
	file.Write(boxBytes(t, BoxTypeMdat(), make([]byte, 8)))
	file.Write(boxBytes(t, BoxTypeEmsg(), emsgPayload(t, v0Emsg("urn:a", "3", "c", 1000, 50, 0, 3))))
	file.Write(boxBytes(t, BoxTypeMoof(), boxBytes(t, BoxTypeMfhd(), []byte{0, 0, 0, 0, 0, 0, 0, 2})))
	file.Write(boxBytes(t, BoxTypeMdat(), make([]byte, 8)))

	events, err := ReadEmsgs(bytes.NewReader(file.Bytes()), WithEmsgSegmentStartTimes(
		[]time.Duration{time.Second, 5 * time.Second}))
	require.NoError(t, err)
	require.Len(t, events, 3)
	assert.Equal(t, []uint32{1, 2, 3}, []uint32{events[0].Id, events[1].Id, events[2].Id})
	assert.Equal(t, time.Second, events[0].PresentationTime)
	assert.Equal(t, time.Second+400*time.Millisecond, events[1].PresentationTime)
	assert.Equal(t, 5*time.Second+50*time.Millisecond, events[2].PresentationTime)
	assert.Equal(t, []byte("a"), events[0].MessageData)
	assert.Equal(t, []byte("b"), events[1].MessageData)
	assert.Equal(t, []byte("c"), events[2].MessageData)
}

func TestReadEmsgsEmptyFile(t *testing.T) {
	events, err := ReadEmsgs(bytes.NewReader(nil))
	require.NoError(t, err)
	assert.Empty(t, events)
}

func TestReadEmsgsEmsgInsideMoof(t *testing.T) {
	file := bytes.NewBuffer(nil)
	emsg := v0Emsg("urn:m", "x", "in-moof", 1000, 250, 500, 7)

	child := boxBytes(t, BoxTypeEmsg(), emsgPayload(t, emsg))
	moof := boxBytes(t, BoxTypeMoof(), child)
	file.Write(moof)
	file.Write(boxBytes(t, BoxTypeMdat(), make([]byte, 32)))

	events, err := ReadEmsgs(bytes.NewReader(file.Bytes()),
		WithEmsgSegmentStartTimes([]time.Duration{time.Second}))
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, uint32(7), events[0].Id)
	assert.Equal(t, time.Second+250*time.Millisecond, events[0].PresentationTime)
	assert.Equal(t, []byte("in-moof"), events[0].MessageData)
}

func TestReadEmsgsSkipsMdatPayload(t *testing.T) {
	// The reader must never touch the bytes of an mdat payload. A read inside
	// that region fails the test, while reading the surrounding box headers is
	// allowed.
	const mdatPayloadSize = 4 * 1024 * 1024
	file := fragmentFile(t, [][]*Emsg{
		{v0Emsg("urn:a", "1", "one", 1000, 0, 0, 1)},
		{v0Emsg("urn:a", "2", "two", 1000, 0, 0, 2)},
	}, mdatPayloadSize)

	// Locate every mdat payload range.
	var mdatRanges [][2]int64
	for offset := int64(0); offset < int64(len(file)); {
		bi, err := ReadBoxInfo(bytes.NewReader(file[offset:]))
		require.NoError(t, err)
		if bi.Type == BoxTypeMdat() {
			mdatRanges = append(mdatRanges, [2]int64{
				offset + int64(bi.HeaderSize),
				offset + int64(bi.Size),
			})
		}
		offset += int64(bi.Size)
	}
	require.Len(t, mdatRanges, 2)

	r := &guardedReadSeeker{
		r:      bytes.NewReader(file),
		ranges: mdatRanges,
	}
	events, err := ReadEmsgs(r, WithEmsgSegmentStartTimes([]time.Duration{0, time.Second}))
	require.NoError(t, err)
	require.Len(t, events, 2)
	assert.Equal(t, []byte("one"), events[0].MessageData)
	assert.Equal(t, []byte("two"), events[1].MessageData)
	assert.NoError(t, r.failure())
}

type guardedReadSeeker struct {
	r      io.ReadSeeker
	ranges [][2]int64
	err    error
}

func (g *guardedReadSeeker) Seek(offset int64, whence int) (int64, error) {
	return g.r.Seek(offset, whence)
}

func (g *guardedReadSeeker) Read(p []byte) (int, error) {
	start, err := g.r.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	end := start + int64(len(p))
	for _, r := range g.ranges {
		if start < r[1] && end > r[0] && g.err == nil {
			g.err = fmt.Errorf("read at [%d,%d) overlaps mdat payload [%d,%d)", start, end, r[0], r[1])
		}
	}
	return g.r.Read(p)
}

func (g *guardedReadSeeker) failure() error {
	return g.err
}

func TestReadEmsgsZeroTimescale(t *testing.T) {
	file := bytes.NewBuffer(nil)
	file.Write(boxBytes(t, BoxTypeEmsg(), emsgPayload(t, v1Emsg("urn:z", "", "x", 0, 0, 0, 1))))

	_, err := ReadEmsgs(bytes.NewReader(file.Bytes()))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrEmsgZeroTimescale), err)
}

func TestReadEmsgsCorruptBoxes(t *testing.T) {
	valid := boxBytes(t, BoxTypeEmsg(), emsgPayload(t, v1Emsg("urn:a", "", "x", 1000, 0, 0, 1)))

	t.Run("truncated header", func(t *testing.T) {
		_, err := ReadEmsgs(bytes.NewReader([]byte{0, 0, 0}))
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrEmsgInvalidBox), err)
	})

	t.Run("size beyond file", func(t *testing.T) {
		corrupt := append([]byte{}, valid...)
		corrupt[3] += 0x10 // grow the declared size
		_, err := ReadEmsgs(bytes.NewReader(corrupt))
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrEmsgInvalidBox), err)
	})

	t.Run("size smaller than header", func(t *testing.T) {
		_, err := ReadEmsgs(bytes.NewReader([]byte{
			0, 0, 0, 1, // size 1 -> large header
			0, 0, 0, 0,
			'e', 'm', 's', 'g',
			0, 0, 0, 0, 0, 0, 0, 8, // large size smaller than header
		}))
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrEmsgInvalidBox), err)
	})

	t.Run("child outside moof", func(t *testing.T) {
		// A moof that claims a child larger than the remaining moof bytes.
		moof := []byte{
			0, 0, 0, 16, 'm', 'o', 'o', 'f',
			0, 0, 0, 32, 'e', 'm', 's', 'g',
		}
		_, err := ReadEmsgs(bytes.NewReader(moof))
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrEmsgInvalidBox), err)
	})

	t.Run("missing string terminator in v1", func(t *testing.T) {
		// version=1, timescale, presentation time, duration, id, then a
		// scheme id uri without a NUL terminator.
		payload := []byte{
			1, 0, 0, 0,
			0, 0, 3, 0xe8,
			0, 0, 0, 0, 0, 0, 0, 0,
			0, 0, 0, 0,
			0, 0, 0, 1,
			'u', 'r', 'n', ':', 'x',
		}
		_, err := ReadEmsgs(bytes.NewReader(boxBytes(t, BoxTypeEmsg(), payload)))
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrEmsgInvalidBox), err)
	})
}

func TestReadEmsgsPayloadTooLarge(t *testing.T) {
	file := bytes.NewBuffer(nil)
	payload := emsgPayload(t, v1Emsg("urn:a", "", "0123456789", 1000, 0, 0, 1))
	file.Write(boxBytes(t, BoxTypeEmsg(), payload))

	_, err := ReadEmsgs(bytes.NewReader(file.Bytes()), WithEmsgMaxPayloadSize(8))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrEmsgPayloadTooLarge), err)

	// The default limit is generous: the same file is readable without an
	// option.
	events, err := ReadEmsgs(bytes.NewReader(file.Bytes()))
	require.NoError(t, err)
	require.Len(t, events, 1)
}

func TestReadEmsgsMissingSegmentStartTime(t *testing.T) {
	t.Run("no callback", func(t *testing.T) {
		file := fragmentFile(t, [][]*Emsg{
			{v0Emsg("urn:a", "", "x", 1000, 100, 100, 1)},
		}, 8)
		_, err := ReadEmsgs(bytes.NewReader(file))
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrEmsgMissingSegmentStartTime), err)
	})

	t.Run("segments out of range", func(t *testing.T) {
		file := fragmentFile(t, [][]*Emsg{
			{v0Emsg("urn:a", "", "x", 1000, 100, 100, 1)},
			{v0Emsg("urn:a", "", "y", 1000, 100, 100, 2)},
		}, 8)
		_, err := ReadEmsgs(bytes.NewReader(file), WithEmsgSegmentStartTimes([]time.Duration{0}))
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrEmsgMissingSegmentStartTime), err)
	})

	t.Run("v0 without any moof", func(t *testing.T) {
		file := bytes.NewBuffer(nil)
		file.Write(boxBytes(t, BoxTypeEmsg(), emsgPayload(t, v0Emsg("urn:a", "", "x", 1000, 100, 100, 1))))
		_, err := ReadEmsgs(bytes.NewReader(file.Bytes()),
			WithEmsgSegmentStartTimes([]time.Duration{0}))
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrEmsgMissingSegmentStartTime), err)
	})

	t.Run("callback error", func(t *testing.T) {
		file := fragmentFile(t, [][]*Emsg{
			{v0Emsg("urn:a", "", "x", 1000, 100, 100, 1)},
		}, 8)
		callbackErr := errors.New("boom")
		_, err := ReadEmsgs(bytes.NewReader(file), WithEmsgSegmentStartTime(
			func(EmsgSegment) (time.Duration, error) { return 0, callbackErr }))
		require.Error(t, err)
		assert.True(t, errors.Is(err, callbackErr), err)
	})
}

func TestReadEmsgsTimeOverflow(t *testing.T) {
	t.Run("version 1 presentation time", func(t *testing.T) {
		file := bytes.NewBuffer(nil)
		file.Write(boxBytes(t, BoxTypeEmsg(),
			emsgPayload(t, v1Emsg("urn:a", "", "x", 1, math.MaxUint64, 0, 1))))
		_, err := ReadEmsgs(bytes.NewReader(file.Bytes()))
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrEmsgTimeOverflow), err)
	})

	t.Run("version 0 delta plus segment start", func(t *testing.T) {
		// A segment start near the maximum duration plus a non-zero delta
		// cannot be represented.
		file := fragmentFile(t, [][]*Emsg{
			{v0Emsg("urn:a", "", "x", 1, math.MaxUint32, 0, 1)},
		}, 8)
		_, err := ReadEmsgs(bytes.NewReader(file), WithEmsgSegmentStartTimes(
			[]time.Duration{time.Duration(math.MaxInt64) - time.Second}))
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrEmsgTimeOverflow), err)
	})

	t.Run("representable large values", func(t *testing.T) {
		// MaxUint32 ticks at timescale 1 is ~4295 seconds, well in range.
		file := fragmentFile(t, [][]*Emsg{
			{v0Emsg("urn:a", "", "x", 1, math.MaxUint32, math.MaxUint32, 1)},
		}, 8)
		events, err := ReadEmsgs(bytes.NewReader(file), WithEmsgSegmentStartTimes([]time.Duration{0}))
		require.NoError(t, err)
		require.Len(t, events, 1)
		assert.Equal(t, time.Duration(math.MaxUint32)*time.Second, events[0].PresentationTime)
		assert.Equal(t, time.Duration(math.MaxUint32)*time.Second, events[0].Duration)
	})
}

func TestReadEmsgsOrderAcrossSegments(t *testing.T) {
	// Version 1 events before any moof must keep their file-order position
	// relative to queued version 0 events, even though their time is known
	// immediately.
	file := bytes.NewBuffer(nil)
	file.Write(boxBytes(t, BoxTypeEmsg(), emsgPayload(t, v1Emsg("urn:a", "", "v1-first", 1000, 5000, 0, 10))))
	file.Write(boxBytes(t, BoxTypeEmsg(), emsgPayload(t, v0Emsg("urn:a", "", "v0-second", 1000, 100, 0, 11))))
	file.Write(boxBytes(t, BoxTypeMoof(), boxBytes(t, BoxTypeMfhd(), []byte{0, 0, 0, 0, 0, 0, 0, 1})))
	file.Write(boxBytes(t, BoxTypeMdat(), make([]byte, 8)))

	events, err := ReadEmsgs(bytes.NewReader(file.Bytes()), WithEmsgSegmentStartTimes([]time.Duration{0}))
	require.NoError(t, err)
	require.Len(t, events, 2)
	assert.Equal(t, uint32(10), events[0].Id)
	assert.Equal(t, 5*time.Second, events[0].PresentationTime)
	assert.Equal(t, uint32(11), events[1].Id)
	assert.Equal(t, 100*time.Millisecond, events[1].PresentationTime)
}

func TestWalkEmsgs(t *testing.T) {
	file := fragmentFile(t, [][]*Emsg{
		{v0Emsg("urn:a", "", "a", 1000, 0, 0, 1)},
		{v1Emsg("urn:a", "", "b", 1000, 0, 0, 2)},
	}, 8)

	var ids []uint32
	err := WalkEmsgs(bytes.NewReader(file), func(event *EmsgEvent) error {
		ids = append(ids, event.Id)
		if event.Id == 1 {
			return nil
		}
		return nil
	}, WithEmsgSegmentStartTimes([]time.Duration{0, time.Second}))
	require.NoError(t, err)
	assert.Equal(t, []uint32{1, 2}, ids)

	// A handler error stops the scan and is returned unchanged.
	childErr := errors.New("stop")
	err = WalkEmsgs(bytes.NewReader(file), func(event *EmsgEvent) error {
		return childErr
	}, WithEmsgSegmentStartTimes([]time.Duration{0, time.Second}))
	assert.True(t, errors.Is(err, childErr), err)
}

func TestReadEmsgsInvalidArguments(t *testing.T) {
	_, err := ReadEmsgs(nil)
	require.Error(t, err)

	err = WalkEmsgs(bytes.NewReader(nil), nil)
	require.Error(t, err)
}

func BenchmarkReadEmsgs(b *testing.B) {
	var segments [][]*Emsg
	for i := 0; i < 100; i++ {
		segments = append(segments, []*Emsg{
			v0Emsg("urn:a", "1", "payload", 1000, uint32(i), 100, uint32(i)),
		})
	}
	file := bytes.NewBuffer(nil)
	ftyp := &Ftyp{MajorBrand: BrandISOM()}
	buf := bytes.NewBuffer(nil)
	if _, err := Marshal(buf, ftyp, Context{}); err != nil {
		b.Fatal(err)
	}
	file.Write(boxBytes(b, BoxTypeFtyp(), buf.Bytes()))
	for _, emsgs := range segments {
		payload := emsgPayload(b, emsgs[0])
		file.Write(boxBytes(b, BoxTypeEmsg(), payload))
		file.Write(boxBytes(b, BoxTypeMoof(), boxBytes(b, BoxTypeMfhd(), []byte{0, 0, 0, 0, 0, 0, 0, 1})))
		file.Write(boxBytes(b, BoxTypeMdat(), make([]byte, 64*1024)))
	}
	data := file.Bytes()
	startTimes := make([]time.Duration, len(segments))
	for i := range startTimes {
		startTimes[i] = time.Duration(i) * time.Second
	}
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ReadEmsgs(bytes.NewReader(data), WithEmsgSegmentStartTimes(startTimes)); err != nil {
			b.Fatal(err)
		}
	}
}
