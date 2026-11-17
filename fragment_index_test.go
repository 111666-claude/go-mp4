package mp4

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// assertErrorIs works with the pinned testify version, which has no ErrorIs.
func assertErrorIs(t *testing.T, err, target error) {
	t.Helper()
	assert.True(t, errors.Is(err, target), "want error %v, got %v", target, err)
}

// ---------------------------------------------------------------------------
// Handcrafted fMP4 fixture builder
// ---------------------------------------------------------------------------

// memWriteSeeker is a bytes.Buffer-backed io.WriteSeeker. Unlike
// writerseeker.WriterSeeker it exposes the live backing slice, so in-place
// patches to already-written bytes persist.
type memWriteSeeker struct {
	buf bytes.Buffer
	pos int
}

func (m *memWriteSeeker) Write(p []byte) (int, error) {
	if extra := m.pos - m.buf.Len(); extra > 0 {
		if _, err := m.buf.Write(make([]byte, extra)); err != nil {
			return 0, err
		}
	}
	if m.pos < m.buf.Len() {
		n := copy(m.buf.Bytes()[m.pos:], p)
		p = p[n:]
		m.pos += n
		if len(p) == 0 {
			return n, nil
		}
	}
	n, err := m.buf.Write(p)
	m.pos += n
	return n, err
}

func (m *memWriteSeeker) Seek(offset int64, whence int) (int64, error) {
	newPos := int(offset)
	if whence == io.SeekCurrent {
		newPos = m.pos + int(offset)
	} else if whence == io.SeekEnd {
		newPos = m.buf.Len() + int(offset)
	}
	if newPos < 0 {
		return 0, errors.New("negative position")
	}
	m.pos = newPos
	return int64(newPos), nil
}

func (m *memWriteSeeker) bytes() []byte { return m.buf.Bytes() }

type fmp4Builder struct {
	buf *memWriteSeeker
	w   *Writer
}

func newFMP4Builder() *fmp4Builder {
	buf := &memWriteSeeker{}
	return &fmp4Builder{buf: buf, w: NewWriter(buf)}
}

// box starts a box, marshals the payload and ends it, returning the finalized BoxInfo.
func (b *fmp4Builder) box(box IBox) *BoxInfo {
	if _, err := b.w.StartBox(&BoxInfo{Type: box.GetType()}); err != nil {
		panic(err)
	}
	if _, err := Marshal(b.w, box, Context{}); err != nil {
		panic(err)
	}
	bi, err := b.w.EndBox()
	if err != nil {
		panic(err)
	}
	return bi
}

func (b *fmp4Builder) start(typ BoxType) uint64 {
	bi, err := b.w.StartBox(&BoxInfo{Type: typ})
	if err != nil {
		panic(err)
	}
	return bi.Offset
}

func (b *fmp4Builder) payload(box IBox) {
	if _, err := Marshal(b.w, box, Context{}); err != nil {
		panic(err)
	}
}

func (b *fmp4Builder) end() *BoxInfo {
	bi, err := b.w.EndBox()
	if err != nil {
		panic(err)
	}
	return bi
}

func (b *fmp4Builder) raw(data []byte) {
	if _, err := b.w.Write(data); err != nil {
		panic(err)
	}
}

func (b *fmp4Builder) bytes() []byte {
	return b.buf.bytes()
}

// patchTrunDataOffset writes the trun DataOffset field.
// trun layout: 8 box header | 4 full box | 4 sample count | 4 data offset ...
func patchTrunDataOffset(data []byte, endBI *BoxInfo, value int32) {
	binary.BigEndian.PutUint32(data[endBI.Offset+16:], uint32(value))
}

// patchTfhdBaseDataOffset writes the BaseDataOffset field of a tfhd box.
// tfhd layout: 8 header | 4 full box | 4 track id | 8 base data offset ...
func patchTfhdBaseDataOffset(data []byte, endBI *BoxInfo, value uint64) {
	binary.BigEndian.PutUint64(data[endBI.Offset+16:], value)
}

// buildTwoFragmentFMP4 creates an fMP4 file with two tracks and two fragments.
//
// Track 1 (video, timescale 90000):
//   - fragment 1: one traf with two trun boxes, per-trun sizes/durations and
//     signed composition time offsets (trun version 1); data offset points
//     into the following mdat.
//   - fragment 2: tfhd with explicit base-data-offset and default sample
//     duration/size; the first trun has no data offset (cursor inheritance)
//     and its sample relies on the tfhd/trex default size; the second trun
//     resets the cursor with its own data offset.
//
// Track 2 (audio, timescale 44100):
//   - both fragments use a single trun whose entries give no per-sample
//     duration/size, so tfhd and trex defaults are inherited.
func buildTwoFragmentFMP4(t *testing.T) ([]byte, map[string]uint64) {
	b := newFMP4Builder()
	layout := make(map[string]uint64)

	// ftyp
	b.box(&Ftyp{
		MajorBrand:       [4]byte{'i', 's', 'o', 'm'},
		MinorVersion:     0x200,
		CompatibleBrands: []CompatibleBrandElem{{CompatibleBrand: [4]byte{'i', 's', 'o', 'm'}}},
	})

	// moov
	b.start(BoxTypeMoov())
	b.box(&Mvhd{Timescale: 1000, DurationV0: 1000, NextTrackID: 3})

	// trak #1 (video)
	b.start(BoxTypeTrak())
	b.box(&Tkhd{TrackID: 1, DurationV0: 1000, Width: 0x01400000, Height: 0x00d20000})
	b.start(BoxTypeMdia())
	b.box(&Mdhd{Timescale: 90000, DurationV0: 90000, Language: [3]byte{0x55, 0xc4}})
	b.end() // mdia
	b.end() // trak

	// trak #2 (audio)
	b.start(BoxTypeTrak())
	b.box(&Tkhd{TrackID: 2, DurationV0: 1000})
	b.start(BoxTypeMdia())
	b.box(&Mdhd{Timescale: 44100, DurationV0: 44100, Language: [3]byte{0x55, 0xc4}})
	b.end() // mdia
	b.end() // trak

	// mvex with trex defaults
	b.start(BoxTypeMvex())
	b.box(&Trex{TrackID: 1, DefaultSampleDescriptionIndex: 1, DefaultSampleDuration: 3000, DefaultSampleSize: 7, DefaultSampleFlags: 0x10000})
	b.box(&Trex{TrackID: 2, DefaultSampleDescriptionIndex: 1, DefaultSampleDuration: 1024, DefaultSampleSize: 4, DefaultSampleFlags: 0x2000000})
	b.end() // mvex
	b.end() // moov

	// ---------------- fragment 1 ----------------
	moof1Off := b.start(BoxTypeMoof())
	layout["moof1"] = moof1Off
	b.box(&Mfhd{SequenceNumber: 1})

	// video traf: tfhd default-duration + default-base-is-moof, tfdt v0, two truns
	b.start(BoxTypeTraf())
	b.box(&Tfhd{FullBox: FullBox{Version: 0, Flags: [3]byte{0x02, 0x00, 0x08}}, TrackID: 1, DefaultSampleDuration: 3000})
	b.box(&Tfdt{BaseMediaDecodeTimeV0: 0})
	trun11 := &Trun{
		FullBox:     FullBox{Version: 1, Flags: [3]byte{0x00, 0x0f, 0x01}}, // 0xf01
		SampleCount: 2,
		Entries: []TrunEntry{
			{SampleDuration: 3000, SampleSize: 10, SampleCompositionTimeOffsetV1: 1000},
			{SampleDuration: 3000, SampleSize: 20, SampleCompositionTimeOffsetV1: -500},
		},
	}
	b.start(BoxTypeTrun())
	b.payload(trun11)
	trun11End := b.end()
	trun12 := &Trun{
		FullBox:     FullBox{Version: 1, Flags: [3]byte{0x00, 0x0f, 0x00}}, // 0xf00, no data offset
		SampleCount: 1,
		Entries: []TrunEntry{
			{SampleDuration: 3000, SampleSize: 30, SampleCompositionTimeOffsetV1: 0},
		},
	}
	b.box(trun12)
	b.end() // traf

	// audio traf: tfhd default duration+size, tfdt, trun with only data offset
	b.start(BoxTypeTraf())
	b.box(&Tfhd{FullBox: FullBox{Version: 0, Flags: [3]byte{0x00, 0x00, 0x18}}, TrackID: 2, DefaultSampleDuration: 1024, DefaultSampleSize: 5})
	b.box(&Tfdt{BaseMediaDecodeTimeV0: 0})
	trun13 := &Trun{
		FullBox:     FullBox{Version: 0, Flags: [3]byte{0x00, 0x00, 0x01}},
		SampleCount: 2,
		Entries:     make([]TrunEntry, 2),
	}
	b.start(BoxTypeTrun())
	b.payload(trun13)
	trun13End := b.end()
	b.end() // traf
	b.end() // moof

	// mdat #1: video 10+20+30 bytes then audio 5+5 bytes
	mdat1Off := b.start(BoxTypeMdat())
	layout["mdat1"] = mdat1Off
	videoPayload1 := make([]byte, 0, 60)
	for i := 0; i < 10; i++ {
		videoPayload1 = append(videoPayload1, byte(0x10+i))
	}
	for i := 0; i < 20; i++ {
		videoPayload1 = append(videoPayload1, byte(0x20+i))
	}
	for i := 0; i < 30; i++ {
		videoPayload1 = append(videoPayload1, byte(0x30+i))
	}
	b.raw(videoPayload1)
	audioPayload1 := make([]byte, 0, 10)
	for i := 0; i < 5; i++ {
		audioPayload1 = append(audioPayload1, byte(0xa0+i))
	}
	for i := 0; i < 5; i++ {
		audioPayload1 = append(audioPayload1, byte(0xb0+i))
	}
	b.raw(audioPayload1)
	b.end() // mdat

	data := b.bytes()
	// patch trun data offsets relative to moof start
	patchTrunDataOffset(data, trun11End, int32(mdat1Off+8-moof1Off))
	patchTrunDataOffset(data, trun13End, int32(mdat1Off+8+60-moof1Off))

	// ---------------- fragment 2 ----------------
	moof2Off := b.start(BoxTypeMoof())
	layout["moof2"] = moof2Off
	b.box(&Mfhd{SequenceNumber: 2})

	// video traf: explicit base-data-offset; first trun has no data offset and
	// no per-sample size (inherit tfhd default size 7).
	b.start(BoxTypeTraf())
	tfhd21 := &Tfhd{
		FullBox:               FullBox{Version: 0, Flags: [3]byte{0x00, 0x00, 0x19}}, // base-data-offset + default-duration + default-size
		TrackID:               1,
		DefaultSampleDuration: 3000,
		DefaultSampleSize:     7,
	}
	b.start(BoxTypeTfhd())
	b.payload(tfhd21)
	tfhd21End := b.end()
	b.box(&Tfdt{BaseMediaDecodeTimeV0: 9000})
	b.box(&Trun{
		FullBox:     FullBox{Version: 0, Flags: [3]byte{0x00, 0x01, 0x00}}, // durations only, no data offset
		SampleCount: 1,
		Entries:     []TrunEntry{{SampleDuration: 3000}},
	})
	trun22 := &Trun{
		FullBox:     FullBox{Version: 0, Flags: [3]byte{0x00, 0x03, 0x01}}, // data offset + durations + sizes
		SampleCount: 1,
		DataOffset:  0, // patched below
		Entries:     []TrunEntry{{SampleDuration: 3000, SampleSize: 40}},
	}
	b.start(BoxTypeTrun())
	b.payload(trun22)
	trun22End := b.end()
	b.end() // traf

	// audio traf: only trex defaults (no tfhd defaults, no per-sample fields)
	b.start(BoxTypeTraf())
	b.box(&Tfhd{FullBox: FullBox{Version: 0, Flags: [3]byte{0x02, 0x00, 0x00}}, TrackID: 2})
	b.box(&Tfdt{BaseMediaDecodeTimeV0: 2048})
	trun23 := &Trun{
		FullBox:     FullBox{Version: 0, Flags: [3]byte{0x00, 0x00, 0x01}},
		SampleCount: 1,
		Entries:     make([]TrunEntry, 1),
	}
	b.start(BoxTypeTrun())
	b.payload(trun23)
	trun23End := b.end()
	b.end() // traf
	b.end() // moof

	mdat2Off := b.start(BoxTypeMdat())
	layout["mdat2"] = mdat2Off
	videoPayload2 := make([]byte, 0, 47)
	for i := 0; i < 7; i++ {
		videoPayload2 = append(videoPayload2, byte(0x40+i))
	}
	for i := 0; i < 40; i++ {
		videoPayload2 = append(videoPayload2, byte(0x50+i))
	}
	b.raw(videoPayload2)
	audioPayload2 := make([]byte, 4)
	for i := range audioPayload2 {
		audioPayload2[i] = byte(0xc0 + i)
	}
	b.raw(audioPayload2)
	b.end() // mdat

	data = b.bytes()
	mdat2Payload := mdat2Off + 8
	patchTfhdBaseDataOffset(data, tfhd21End, mdat2Payload)
	// the video trun data offset is relative to the tfhd base data offset
	patchTrunDataOffset(data, trun22End, int32(mdat2Payload+7)-int32(mdat2Payload))
	// the audio trun uses default-base-is-moof, so its offset is moof-relative
	patchTrunDataOffset(data, trun23End, int32(mdat2Payload+47-moof2Off))

	return data, layout
}

// ---------------------------------------------------------------------------
// Tests against the handcrafted two-fragment fixture
// ---------------------------------------------------------------------------

func TestBuildFragmentIndexHandcrafted(t *testing.T) {
	data, layout := buildTwoFragmentFMP4(t)
	r := bytes.NewReader(data)

	idx, err := BuildFragmentIndex(r)
	require.NoError(t, err)
	require.Len(t, idx.Tracks, 2)
	// tracks must follow moov order, not nondeterministic map iteration
	assert.Equal(t, uint32(1), idx.Tracks[0].TrackID)
	assert.Equal(t, uint32(2), idx.Tracks[1].TrackID)

	video := idx.FindTrack(1)
	require.NotNil(t, video)
	assert.Equal(t, uint32(90000), video.Timescale)
	require.Len(t, video.Samples, 5)

	audio := idx.FindTrack(2)
	require.NotNil(t, audio)
	assert.Equal(t, uint32(44100), audio.Timescale)
	require.Len(t, audio.Samples, 3)

	mdat1Payload := layout["mdat1"] + 8
	moof1Off := layout["moof1"]
	moof2Off := layout["moof2"]
	mdat2Payload := layout["mdat2"] + 8

	type expect struct {
		off      uint64
		size     uint32
		dts      uint64
		pts      int64
		duration uint32
		cto      int64
		moofOff  uint64
	}
	videoExpects := []expect{
		{mdat1Payload, 10, 0, 1000, 3000, 1000, moof1Off},
		{mdat1Payload + 10, 20, 3000, 2500, 3000, -500, moof1Off},
		{mdat1Payload + 30, 30, 6000, 6000, 3000, 0, moof1Off},
		// fragment 2: tfdt=9000, first trun inherits size 7 and starts at the
		// tfhd base data offset
		{mdat2Payload, 7, 9000, 9000, 3000, 0, moof2Off},
		{mdat2Payload + 7, 40, 12000, 12000, 3000, 0, moof2Off},
	}
	for i, e := range videoExpects {
		s := video.Samples[i]
		assert.Equalf(t, uint32(1), s.TrackID, "video sample %d track id", i)
		assert.Equalf(t, e.off, s.DataOffset, "video sample %d offset", i)
		assert.Equalf(t, e.size, s.Size, "video sample %d size", i)
		assert.Equalf(t, e.dts, s.DTS, "video sample %d dts", i)
		assert.Equalf(t, e.pts, s.PTS, "video sample %d pts", i)
		assert.Equalf(t, e.duration, s.Duration, "video sample %d duration", i)
		assert.Equalf(t, e.cto, s.CompositionTimeOffset, "video sample %d cto", i)
		assert.Equalf(t, e.moofOff, s.MoofOffset, "video sample %d moof offset", i)
	}

	audioExpects := []expect{
		{mdat1Payload + 60, 5, 0, 0, 1024, 0, moof1Off},
		{mdat1Payload + 65, 5, 1024, 1024, 1024, 0, moof1Off},
		// fragment 2: tfdt=2048, trex defaults (size 4, duration 1024)
		{mdat2Payload + 47, 4, 2048, 2048, 1024, 0, moof2Off},
	}
	for i, e := range audioExpects {
		s := audio.Samples[i]
		assert.Equalf(t, uint32(2), s.TrackID, "audio sample %d track id", i)
		assert.Equalf(t, e.off, s.DataOffset, "audio sample %d offset", i)
		assert.Equalf(t, e.size, s.Size, "audio sample %d size", i)
		assert.Equalf(t, e.dts, s.DTS, "audio sample %d dts", i)
		assert.Equalf(t, e.pts, s.PTS, "audio sample %d pts", i)
		assert.Equalf(t, e.duration, s.Duration, "audio sample %d duration", i)
		assert.Equalf(t, e.moofOff, s.MoofOffset, "audio sample %d moof offset", i)
	}

	// sample byte content round-trip through the indexed offsets
	buf, err := ReadFragmentSampleData(bytes.NewReader(data), video.Samples[1])
	require.NoError(t, err)
	require.Len(t, buf, 20)
	assert.Equal(t, byte(0x20), buf[0])
	assert.Equal(t, byte(0x20+19), buf[19])

	buf, err = ReadFragmentSampleData(bytes.NewReader(data), audio.Samples[2])
	require.NoError(t, err)
	require.Len(t, buf, 4)
	assert.Equal(t, []byte{0xc0, 0xc1, 0xc2, 0xc3}, buf)
}

func TestFragmentIndexMatchesRawBoxes(t *testing.T) {
	// The indexed values must agree with the independently parsed moof/traf
	// tfhd/tfdt/trun boxes (the original boxes).
	data, _ := buildTwoFragmentFMP4(t)
	r := bytes.NewReader(data)

	idx, err := BuildFragmentIndex(r)
	require.NoError(t, err)

	trunBIs, err := ExtractBoxesWithPayload(r, nil, []BoxPath{
		{BoxTypeMoof(), BoxTypeTraf(), BoxTypeTrun()},
	})
	require.NoError(t, err)
	var trunSampleCount uint32
	var trunExplicitSize uint64
	for _, bip := range trunBIs {
		trun := bip.Payload.(*Trun)
		trunSampleCount += trun.SampleCount
		if trun.CheckFlag(0x200) {
			for _, e := range trun.Entries {
				trunExplicitSize += uint64(e.SampleSize)
			}
		}
	}
	var indexedCount uint32
	var indexedExplicitSize uint64
	for _, track := range idx.Tracks {
		for _, s := range track.Samples {
			indexedCount++
			// inherited default sizes are 7 (video), 5 and 4 (audio)
			if s.Size != 7 && s.Size != 5 && s.Size != 4 {
				indexedExplicitSize += uint64(s.Size)
			}
		}
	}
	assert.Equal(t, trunSampleCount, indexedCount)
	assert.Equal(t, trunExplicitSize, indexedExplicitSize)

	// every indexed offset must point inside an mdat box
	mdatBIs, err := ExtractBox(r, nil, BoxPath{BoxTypeMdat()})
	require.NoError(t, err)
	for _, track := range idx.Tracks {
		for _, s := range track.Samples {
			var inside bool
			for _, m := range mdatBIs {
				payloadStart := m.Offset + m.HeaderSize
				payloadEnd := m.Offset + m.Size
				if s.DataOffset >= payloadStart && s.DataOffset+uint64(s.Size) <= payloadEnd {
					inside = true
					break
				}
			}
			assert.Truef(t, inside, "sample offset %d size %d not inside any mdat", s.DataOffset, s.Size)
		}
	}

	// tfdt base media decode times must equal the first sample DTS of each fragment
	assert.Equal(t, uint64(0), idx.FindTrack(1).Samples[0].DTS)
	assert.Equal(t, uint64(9000), idx.FindTrack(1).Samples[3].DTS)
	assert.Equal(t, uint64(0), idx.FindTrack(2).Samples[0].DTS)
	assert.Equal(t, uint64(2048), idx.FindTrack(2).Samples[2].DTS)
}

// ---------------------------------------------------------------------------
// Error handling fixtures
// ---------------------------------------------------------------------------

// minimalMoov writes a moov with one track and a matching trex default.
func minimalMoov(b *fmp4Builder, trackID uint32, timescale uint32) {
	b.start(BoxTypeMoov())
	b.box(&Mvhd{Timescale: 1000, DurationV0: 1000, NextTrackID: 2})
	b.start(BoxTypeTrak())
	b.box(&Tkhd{TrackID: trackID, DurationV0: 1000})
	b.start(BoxTypeMdia())
	b.box(&Mdhd{Timescale: timescale, DurationV0: 1000, Language: [3]byte{0x55, 0xc4}})
	b.end() // mdia
	b.end() // trak
	b.start(BoxTypeMvex())
	b.box(&Trex{TrackID: trackID, DefaultSampleDescriptionIndex: 1, DefaultSampleDuration: 100, DefaultSampleSize: 4})
	b.end() // mvex
	b.end() // moov
}

// simpleFragment writes one moof (single traf, tfhd+tfdt+trun v1) plus an mdat.
// The trun carries one sample with the given payload size and cto; the data
// offset is patched relative to the moof start, plus an optional adjustment.
func simpleFragment(b *fmp4Builder, trackID uint32, baseDTS uint32, size uint32, cto int32, offsetAdjust int32) {
	moofOff := b.start(BoxTypeMoof())
	b.box(&Mfhd{SequenceNumber: 1})
	b.start(BoxTypeTraf())
	b.box(&Tfhd{FullBox: FullBox{Flags: [3]byte{0x02, 0x00, 0x00}}, TrackID: trackID})
	b.box(&Tfdt{BaseMediaDecodeTimeV0: baseDTS})
	trun := &Trun{
		FullBox:     FullBox{Version: 1, Flags: [3]byte{0x00, 0x0b, 0x01}},
		SampleCount: 1,
		DataOffset:  0, // patched below
		Entries: []TrunEntry{
			{SampleDuration: 100, SampleSize: size, SampleCompositionTimeOffsetV1: cto},
		},
	}
	b.start(BoxTypeTrun())
	b.payload(trun)
	trunEnd := b.end()
	b.end() // traf
	mdatOff := b.start(BoxTypeMdat())
	b.raw(make([]byte, 4))
	b.end() // mdat
	b.end() // moof
	patchTrunDataOffset(b.bytes(), trunEnd, int32(mdatOff+8-moofOff)+offsetAdjust)
}

func TestBuildFragmentIndexErrors(t *testing.T) {
	good, _ := buildTwoFragmentFMP4(t)

	t.Run("missing moov", func(t *testing.T) {
		moofPos := bytes.Index(good, []byte{'m', 'o', 'o', 'f'}) - 4
		require.True(t, moofPos > 0)
		_, err := BuildFragmentIndex(bytes.NewReader(good[moofPos:]))
		assertErrorIs(t, err, ErrMoovBoxNotFound)
	})

	t.Run("missing trex", func(t *testing.T) {
		b := newFMP4Builder()
		b.start(BoxTypeMoov())
		b.box(&Mvhd{Timescale: 1000, DurationV0: 1000, NextTrackID: 2})
		b.start(BoxTypeTrak())
		b.box(&Tkhd{TrackID: 1, DurationV0: 1000})
		b.start(BoxTypeMdia())
		b.box(&Mdhd{Timescale: 90000, DurationV0: 1000, Language: [3]byte{0x55, 0xc4}})
		b.end()
		b.end()
		b.end() // moov without mvex
		_, err := BuildFragmentIndex(bytes.NewReader(b.bytes()))
		assertErrorIs(t, err, ErrTrexNotFound)
	})

	t.Run("duplicate track id in moov", func(t *testing.T) {
		b := newFMP4Builder()
		b.start(BoxTypeMoov())
		b.box(&Mvhd{Timescale: 1000, DurationV0: 1000, NextTrackID: 2})
		for i := 0; i < 2; i++ {
			b.start(BoxTypeTrak())
			b.box(&Tkhd{TrackID: 1, DurationV0: 1000})
			b.start(BoxTypeMdia())
			b.box(&Mdhd{Timescale: 90000, DurationV0: 1000, Language: [3]byte{0x55, 0xc4}})
			b.end() // mdia
			b.end() // trak
		}
		b.end() // moov
		_, err := BuildFragmentIndex(bytes.NewReader(b.bytes()))
		assertErrorIs(t, err, ErrDuplicateTrackID)
	})

	t.Run("unknown track id in tfhd", func(t *testing.T) {
		b := newFMP4Builder()
		minimalMoov(b, 1, 90000)
		simpleFragment(b, 99, 0, 4, 0, 0)
		_, err := BuildFragmentIndex(bytes.NewReader(b.bytes()))
		assertErrorIs(t, err, ErrUnknownTrackID)
	})

	t.Run("invalid negative data offset", func(t *testing.T) {
		b := newFMP4Builder()
		minimalMoov(b, 1, 90000)
		simpleFragment(b, 1, 0, 4, 0, -100000)
		_, err := BuildFragmentIndex(bytes.NewReader(b.bytes()))
		assertErrorIs(t, err, ErrInvalidDataOffset)
	})

	t.Run("sample size out of range", func(t *testing.T) {
		b := newFMP4Builder()
		minimalMoov(b, 1, 90000)

		moofOff := b.start(BoxTypeMoof())
		b.box(&Mfhd{SequenceNumber: 1})
		b.start(BoxTypeTraf())
		b.box(&Tfhd{FullBox: FullBox{Flags: [3]byte{0x02, 0x00, 0x00}}, TrackID: 1})
		b.box(&Tfdt{BaseMediaDecodeTimeV0: 0})
		trun := &Trun{
			FullBox:     FullBox{Version: 0, Flags: [3]byte{0x00, 0x03, 0x01}},
			SampleCount: 1,
			DataOffset:  0, // patched below
			Entries:     []TrunEntry{{SampleDuration: 100, SampleSize: math.MaxUint32}},
		}
		b.start(BoxTypeTrun())
		b.payload(trun)
		trunEnd := b.end()
		b.end() // traf
		mdatOff := b.start(BoxTypeMdat())
		b.raw([]byte{1, 2, 3, 4})
		b.end()
		patchTrunDataOffset(b.bytes(), trunEnd, int32(mdatOff+8-moofOff))

		_, err := BuildFragmentIndex(bytes.NewReader(b.bytes()))
		assertErrorIs(t, err, ErrSampleOutOfRange)
	})

	t.Run("decode time discontinuity", func(t *testing.T) {
		b := newFMP4Builder()
		minimalMoov(b, 1, 90000)
		simpleFragment(b, 1, 1000, 4, 0, 0)
		// second fragment starts before the first one ended (first ends at 1100)
		simpleFragment(b, 1, 500, 4, 0, 0)
		_, err := BuildFragmentIndex(bytes.NewReader(b.bytes()))
		assertErrorIs(t, err, ErrDecodeTimeDiscontinuity)
	})

	t.Run("time overflow in dts", func(t *testing.T) {
		b := newFMP4Builder()
		minimalMoov(b, 1, 90000)

		moofOff := b.start(BoxTypeMoof())
		b.box(&Mfhd{SequenceNumber: 1})
		b.start(BoxTypeTraf())
		b.box(&Tfhd{FullBox: FullBox{Flags: [3]byte{0x02, 0x00, 0x00}}, TrackID: 1})
		b.box(&Tfdt{FullBox: FullBox{Version: 1}, BaseMediaDecodeTimeV1: math.MaxUint64 - 10})
		trun := &Trun{
			FullBox:     FullBox{Version: 0, Flags: [3]byte{0x00, 0x03, 0x01}},
			SampleCount: 1,
			DataOffset:  0, // patched below
			Entries:     []TrunEntry{{SampleDuration: 100, SampleSize: 4}},
		}
		b.start(BoxTypeTrun())
		b.payload(trun)
		trunEnd := b.end()
		b.end()
		mdatOff := b.start(BoxTypeMdat())
		b.raw([]byte{1, 2, 3, 4})
		b.end()
		patchTrunDataOffset(b.bytes(), trunEnd, int32(mdatOff+8-moofOff))

		_, err := BuildFragmentIndex(bytes.NewReader(b.bytes()))
		assertErrorIs(t, err, ErrTimeOverflow)
	})

	t.Run("truncated file sample out of range", func(t *testing.T) {
		_, err := BuildFragmentIndex(bytes.NewReader(good[:len(good)-30]))
		assertErrorIs(t, err, ErrSampleOutOfRange)
	})

	t.Run("negative composition offset below dts", func(t *testing.T) {
		b := newFMP4Builder()
		minimalMoov(b, 1, 90000)
		simpleFragment(b, 1, 100, 4, -200, 0)
		_, err := BuildFragmentIndex(bytes.NewReader(b.bytes()))
		assertErrorIs(t, err, ErrTimeOverflow)
	})

	t.Run("missing tfhd", func(t *testing.T) {
		b := newFMP4Builder()
		minimalMoov(b, 1, 90000)
		b.start(BoxTypeMoof())
		b.box(&Mfhd{SequenceNumber: 1})
		b.start(BoxTypeTraf())
		b.box(&Tfdt{BaseMediaDecodeTimeV0: 0})
		b.end() // traf
		b.start(BoxTypeMdat())
		b.raw([]byte{1, 2, 3, 4})
		b.end() // mdat
		b.end() // moof
		_, err := BuildFragmentIndex(bytes.NewReader(b.bytes()))
		assertErrorIs(t, err, ErrTfhdNotFound)
	})
}

func TestAddCompositionTimeOverflow(t *testing.T) {
	_, err := addCompositionTime(1, math.MaxInt64-1, 10)
	assertErrorIs(t, err, ErrTimeOverflow)
	_, err = addCompositionTime(1, 5, -10)
	assertErrorIs(t, err, ErrTimeOverflow)

	pts, err := addCompositionTime(1, 1000, -500)
	require.NoError(t, err)
	assert.Equal(t, int64(500), pts)
	pts, err = addCompositionTime(1, 1000, 250)
	require.NoError(t, err)
	assert.Equal(t, int64(1250), pts)
}

func TestFragmentIndexEmptyTrun(t *testing.T) {
	// a trun with zero samples is legal and produces no samples for the track
	b := newFMP4Builder()
	minimalMoov(b, 1, 90000)
	b.start(BoxTypeMoof())
	b.box(&Mfhd{SequenceNumber: 1})
	b.start(BoxTypeTraf())
	b.box(&Tfhd{FullBox: FullBox{Flags: [3]byte{0x02, 0x00, 0x00}}, TrackID: 1})
	b.box(&Tfdt{BaseMediaDecodeTimeV0: 0})
	b.box(&Trun{FullBox: FullBox{Version: 0, Flags: [3]byte{0x00, 0x00, 0x01}}, SampleCount: 0, DataOffset: 8})
	b.end() // traf
	b.start(BoxTypeMdat())
	b.raw([]byte{1, 2, 3, 4})
	b.end() // mdat
	b.end() // moof

	idx, err := BuildFragmentIndex(bytes.NewReader(b.bytes()))
	require.NoError(t, err)
	require.Len(t, idx.Tracks, 1)
	assert.Empty(t, idx.FindTrack(1).Samples)
}

func TestFragmentIndexContinuousWithoutTfdt(t *testing.T) {
	// second fragment has no tfdt: its base DTS must continue from the first
	b := newFMP4Builder()
	minimalMoov(b, 1, 90000)
	simpleFragment(b, 1, 0, 4, 0, 0)

	moofOff := b.start(BoxTypeMoof())
	b.box(&Mfhd{SequenceNumber: 2})
	b.start(BoxTypeTraf())
	b.box(&Tfhd{FullBox: FullBox{Flags: [3]byte{0x02, 0x00, 0x00}}, TrackID: 1})
	trun := &Trun{
		FullBox:     FullBox{Version: 0, Flags: [3]byte{0x00, 0x03, 0x01}},
		SampleCount: 1,
		DataOffset:  0, // patched below
		Entries:     []TrunEntry{{SampleDuration: 100, SampleSize: 4}},
	}
	b.start(BoxTypeTrun())
	b.payload(trun)
	trunEnd := b.end()
	b.end()
	mdatOff := b.start(BoxTypeMdat())
	b.raw([]byte{5, 6, 7, 8})
	b.end()
	b.end()
	patchTrunDataOffset(b.bytes(), trunEnd, int32(mdatOff+8-moofOff))

	idx, err := BuildFragmentIndex(bytes.NewReader(b.bytes()))
	require.NoError(t, err)
	samples := idx.FindTrack(1).Samples
	require.Len(t, samples, 2)
	assert.Equal(t, uint64(0), samples[0].DTS)
	assert.Equal(t, uint64(100), samples[1].DTS)
}

func TestFragmentIndexOnRealFixtures(t *testing.T) {
	// cross-check against the repository's existing fragmented sample files
	for _, name := range []string{"./testdata/sample_fragmented.mp4", "./testdata/sample_fragmented_muxed.mp4"} {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(name)
			require.NoError(t, err)

			idx, err := BuildFragmentIndex(bytes.NewReader(raw))
			require.NoError(t, err)
			require.NotEmpty(t, idx.Tracks)

			info, err := Probe(bytes.NewReader(raw))
			require.NoError(t, err)

			for _, track := range idx.Tracks {
				assert.NotZero(t, track.Timescale)
				var segCount uint32
				var segSize uint32
				for _, seg := range info.Segments {
					if seg.TrackID == track.TrackID {
						segCount += seg.SampleCount
						segSize += seg.Size
					}
				}
				assert.Equalf(t, segCount, uint32(len(track.Samples)), "track %d sample count", track.TrackID)
				var indexedSize uint64
				for _, s := range track.Samples {
					indexedSize += uint64(s.Size)
					assert.NotZero(t, s.Size)
				}
				assert.Equalf(t, uint64(segSize), indexedSize, "track %d total size", track.TrackID)

				for i := 1; i < len(track.Samples); i++ {
					assert.GreaterOrEqualf(t, track.Samples[i].DTS, track.Samples[i-1].DTS,
						"track %d DTS monotonic at %d", track.TrackID, i)
				}
			}
		})
	}
}
