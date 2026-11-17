package mp4

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests below build the boxes byte by byte instead of using Marshal, so
// that the values read by the parser do not depend on the same code paths
// that are being verified.

func testRawBox(boxType string, payload []byte) []byte {
	box := make([]byte, 8, 8+len(payload))
	binary.BigEndian.PutUint32(box, uint32(8+len(payload)))
	copy(box[4:8], boxType)
	return append(box, payload...)
}

func testVersionFlags(version byte, flags uint32) []byte {
	return []byte{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}
}

func testU16(v uint16) []byte {
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, v)
	return b
}

func testU32(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

func testU64(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}

func testI16(v int16) []byte {
	return testU16(uint16(v))
}

func testI32(v int32) []byte {
	return testU32(uint32(v))
}

func testI64(v int64) []byte {
	return testU64(uint64(v))
}

func testConcat(parts ...[]byte) []byte {
	var out []byte
	for _, part := range parts {
		out = append(out, part...)
	}
	return out
}

func testMvhdBox(version byte, timescale uint32, duration uint64) []byte {
	body := testConcat(
		testVersionFlags(version, 0),
	)
	if version == 0 {
		body = testConcat(body,
			testU32(0),         // creation_time
			testU32(0),         // modification_time
			testU32(timescale), // timescale
			testU32(uint32(duration)),
		)
	} else {
		body = testConcat(body,
			testU64(0), // creation_time
			testU64(0), // modification_time
			testU32(timescale),
			testU64(duration),
		)
	}
	body = testConcat(body,
		testU32(0x00010000), // rate = 1.0
		testU16(0x0100),     // volume = 1.0
		testU16(0),          // reserved
		testU32(0), testU32(0),
		testI32(0x00010000), testI32(0), testI32(0),
		testI32(0), testI32(0x00010000), testI32(0),
		testI32(0), testI32(0), testI32(0x40000000),
		testU32(0), testU32(0), testU32(0), testU32(0), testU32(0), testU32(0),
		testU32(1), // next_track_ID
	)
	return testRawBox("mvhd", body)
}

func testMdhdBox(version byte, timescale uint32, duration uint64) []byte {
	body := testVersionFlags(version, 0)
	if version == 0 {
		body = testConcat(body,
			testU32(0), // creation_time
			testU32(0), // modification_time
			testU32(timescale),
			testU32(uint32(duration)),
		)
	} else {
		body = testConcat(body,
			testU64(0),
			testU64(0),
			testU32(timescale),
			testU64(duration),
		)
	}
	body = testConcat(body,
		testU16(0x55c4), // language "und"
		testU16(0),      // pre_defined
	)
	return testRawBox("mdhd", body)
}

type testElstEntry struct {
	duration  uint64
	mediaTime int64
	rateInt   int16
	rateFrac  int16
}

// testNormalEdit returns a 1.0x edit list entry.
func testNormalEdit(duration uint64, mediaTime int64) testElstEntry {
	return testElstEntry{duration: duration, mediaTime: mediaTime, rateInt: 1}
}

func testEmptyEdit(duration uint64) testElstEntry {
	return testElstEntry{duration: duration, mediaTime: -1}
}

func testElstBox(version byte, entries ...testElstEntry) []byte {
	body := testConcat(
		testVersionFlags(version, 0),
		testU32(uint32(len(entries))),
	)
	for _, entry := range entries {
		if version == 0 {
			body = testConcat(body,
				testU32(uint32(entry.duration)),
				testI32(int32(entry.mediaTime)),
			)
		} else {
			body = testConcat(body,
				testU64(entry.duration),
				testI64(entry.mediaTime),
			)
		}
		body = testConcat(body,
			testI16(entry.rateInt),
			testI16(entry.rateFrac),
		)
	}
	return testRawBox("elst", body)
}

func testMoovBox(mvhd, mdhd, elst []byte) []byte {
	trak := testConcat(testRawBox("mdia", mdhd))
	if elst != nil {
		trak = testConcat(trak, testRawBox("edts", elst))
	}
	return testRawBox("moov", testConcat(mvhd, testRawBox("trak", trak)))
}

// testParseTrack parses a hand-assembled moov box through the regular reader.
func testParseTrack(t *testing.T, moov []byte) (*Mvhd, *Mdhd, *Elst) {
	t.Helper()
	bips, err := ExtractBoxesWithPayload(bytes.NewReader(moov), nil, []BoxPath{
		{BoxTypeMoov(), BoxTypeMvhd()},
		{BoxTypeMoov(), BoxTypeTrak(), BoxTypeMdia(), BoxTypeMdhd()},
		{BoxTypeMoov(), BoxTypeTrak(), BoxTypeEdts(), BoxTypeElst()},
	})
	require.NoError(t, err)
	var mvhd *Mvhd
	var mdhd *Mdhd
	var elst *Elst
	for _, bip := range bips {
		switch bip.Info.Type {
		case BoxTypeMvhd():
			mvhd = bip.Payload.(*Mvhd)
		case BoxTypeMdhd():
			mdhd = bip.Payload.(*Mdhd)
		case BoxTypeElst():
			elst = bip.Payload.(*Elst)
		}
	}
	require.NotNil(t, mvhd)
	require.NotNil(t, mdhd)
	return mvhd, mdhd, elst
}

func TestEditListTimelineLeadingEmptyEditAndTwoEdits(t *testing.T) {
	// movie timescale 1000 (milliseconds), media timescale 48000 (audio)
	//   [0, 500)      empty edit (0.5s of silence)
	//   [500, 1500)   media [96000, 144000)   (2.0s starting at media 2s)
	//   [1500, 3500)  media [48000, 144000)   (2.0s starting at media 1s)
	elstBox := testElstBox(0,
		testEmptyEdit(500),
		testNormalEdit(1000, 96000),
		testNormalEdit(2000, 48000),
	)
	moov := testMoovBox(
		testMvhdBox(0, 1000, 3500),
		testMdhdBox(0, 48000, 240000),
		elstBox,
	)
	mvhd, mdhd, elst := testParseTrack(t, moov)

	// verify that the hand-built values reached the parser intact
	assert.Equal(t, uint32(1000), mvhd.Timescale)
	assert.Equal(t, uint32(48000), mdhd.Timescale)
	require.NotNil(t, elst)
	require.Len(t, elst.Entries, 3)
	assert.Equal(t, uint64(500), elst.GetSegmentDuration(0))
	assert.Equal(t, int64(-1), elst.GetMediaTime(0))
	assert.Equal(t, uint64(1000), elst.GetSegmentDuration(1))
	assert.Equal(t, int64(96000), elst.GetMediaTime(1))
	assert.Equal(t, uint64(2000), elst.GetSegmentDuration(2))
	assert.Equal(t, int64(48000), elst.GetMediaTime(2))
	assert.Equal(t, int16(1), elst.Entries[1].MediaRateInteger)

	tl, err := NewEditListTimeline(elst, mvhd, mdhd)
	require.NoError(t, err)
	assert.True(t, tl.HasEditList())
	assert.Equal(t, uint32(1000), tl.MovieTimescale())
	assert.Equal(t, uint32(48000), tl.MediaTimescale())
	assert.Equal(t, uint64(3500), tl.MovieDuration())

	assert.Equal(t, []EditListSegment{
		{MovieStart: 0, MovieEnd: 500, MediaTime: -1, MediaEnd: -1, Empty: true},
		{MovieStart: 500, MovieEnd: 1500, MediaTime: 96000, MediaEnd: 144000},
		{MovieStart: 1500, MovieEnd: 3500, MediaTime: 48000, MediaEnd: 144000},
	}, tl.Segments())

	// the leading blank is not media time 0
	for _, playbackTime := range []uint64{0, 1, 499} {
		_, err := tl.MediaTimeAt(playbackTime)
		assert.True(t, errors.Is(err, ErrEditListPlaybackTimeInEmptyEdit),
			"playbackTime=%d: %v", playbackTime, err)
	}

	// 1 movie tick = 48 media ticks in the first edit
	mt, err := tl.MediaTimeAt(500)
	require.NoError(t, err)
	assert.Equal(t, int64(96000), mt)
	mt, err = tl.MediaTimeAt(501)
	require.NoError(t, err)
	assert.Equal(t, int64(96048), mt)
	mt, err = tl.MediaTimeAt(1499)
	require.NoError(t, err)
	assert.Equal(t, int64(96000+999*48), mt)

	// the second edit starts again at an earlier media time
	mt, err = tl.MediaTimeAt(1500)
	require.NoError(t, err)
	assert.Equal(t, int64(48000), mt)
	mt, err = tl.MediaTimeAt(2499)
	require.NoError(t, err)
	assert.Equal(t, int64(48000+999*48), mt)
	mt, err = tl.MediaTimeAt(3499)
	require.NoError(t, err)
	assert.Equal(t, int64(48000+1999*48), mt)

	// the end of the last edit is exclusive
	_, err = tl.MediaTimeAt(3500)
	assert.True(t, errors.Is(err, ErrEditListPlaybackTimeOutOfRange), "%v", err)
}

func TestEditListTimelineVersion1(t *testing.T) {
	// the same timeline as above, but written as v1 boxes with 64-bit fields
	elstBox := testElstBox(1,
		testEmptyEdit(500),
		testNormalEdit(1000, 96000),
		testNormalEdit(2000, 48000),
	)
	moov := testMoovBox(
		testMvhdBox(1, 1000, 3500),
		testMdhdBox(1, 48000, 240000),
		elstBox,
	)
	mvhd, mdhd, elst := testParseTrack(t, moov)
	require.NotNil(t, elst)
	assert.EqualValues(t, 1, elst.GetVersion())

	tl, err := NewEditListTimeline(elst, mvhd, mdhd)
	require.NoError(t, err)
	assert.Equal(t, uint64(3500), tl.MovieDuration())

	_, err = tl.MediaTimeAt(499)
	assert.True(t, errors.Is(err, ErrEditListPlaybackTimeInEmptyEdit), "%v", err)
	mt, err := tl.MediaTimeAt(500)
	require.NoError(t, err)
	assert.Equal(t, int64(96000), mt)
	mt, err = tl.MediaTimeAt(1500)
	require.NoError(t, err)
	assert.Equal(t, int64(48000), mt)
	mt, err = tl.MediaTimeAt(3499)
	require.NoError(t, err)
	assert.Equal(t, int64(48000+1999*48), mt)
	_, err = tl.MediaTimeAt(3500)
	assert.True(t, errors.Is(err, ErrEditListPlaybackTimeOutOfRange), "%v", err)
}

func TestEditListTimelineOverlappingMediaRanges(t *testing.T) {
	// two edits present the same media range; media ranges may overlap, only
	// the movie-time ranges are required to be contiguous
	elstBox := testElstBox(0,
		testNormalEdit(1000, 500),
		testNormalEdit(1000, 500),
	)
	moov := testMoovBox(
		testMvhdBox(0, 1000, 2000),
		testMdhdBox(0, 1000, 2000),
		elstBox,
	)
	mvhd, mdhd, elst := testParseTrack(t, moov)
	tl, err := NewEditListTimeline(elst, mvhd, mdhd)
	require.NoError(t, err)

	// a boundary playback time belongs to the following edit
	cases := []struct {
		playbackTime uint64
		mediaTime    int64
	}{
		{0, 500},
		{999, 1499},
		{1000, 500}, // start of the second edit, not the end of the first
		{1999, 1499},
	}
	for _, c := range cases {
		mt, err := tl.MediaTimeAt(c.playbackTime)
		require.NoError(t, err, "playbackTime=%d", c.playbackTime)
		assert.Equal(t, c.mediaTime, mt, "playbackTime=%d", c.playbackTime)
	}
	_, err = tl.MediaTimeAt(2000)
	assert.True(t, errors.Is(err, ErrEditListPlaybackTimeOutOfRange), "%v", err)
}

func TestEditListTimelineRounding(t *testing.T) {
	// 3 movie ticks map to 100 media ticks: conversions round down
	elstBox := testElstBox(0, testNormalEdit(3, 0))
	moov := testMoovBox(
		testMvhdBox(0, 3, 3),
		testMdhdBox(0, 100, 100),
		elstBox,
	)
	mvhd, mdhd, elst := testParseTrack(t, moov)
	tl, err := NewEditListTimeline(elst, mvhd, mdhd)
	require.NoError(t, err)

	assert.Equal(t, EditListSegment{
		MovieStart: 0, MovieEnd: 3, MediaTime: 0, MediaEnd: 100,
	}, tl.Segments()[0])

	for playbackTime, want := range map[uint64]int64{0: 0, 1: 33, 2: 66} {
		mt, err := tl.MediaTimeAt(playbackTime)
		require.NoError(t, err, "playbackTime=%d", playbackTime)
		assert.Equal(t, want, mt, "playbackTime=%d", playbackTime)
	}
}

func TestEditListTimelineNoEditList(t *testing.T) {
	// missing elst: media time follows the movie time through the timescales
	moov := testMoovBox(
		testMvhdBox(0, 1000, 2000),
		testMdhdBox(0, 48000, 96000),
		nil,
	)
	mvhd, mdhd, elst := testParseTrack(t, moov)
	require.Nil(t, elst)

	for _, box := range []*Elst{nil, {FullBox: FullBox{Version: 0}}} {
		tl, err := NewEditListTimeline(box, mvhd, mdhd)
		require.NoError(t, err)
		assert.False(t, tl.HasEditList())
		assert.Nil(t, tl.Segments())
		assert.Equal(t, uint64(0), tl.MovieDuration())

		mt, err := tl.MediaTimeAt(0)
		require.NoError(t, err)
		assert.Equal(t, int64(0), mt)
		mt, err = tl.MediaTimeAt(2)
		require.NoError(t, err)
		assert.Equal(t, int64(96), mt)
		mt, err = tl.MediaTimeAt(2000)
		require.NoError(t, err)
		assert.Equal(t, int64(96000), mt)
	}
}

func TestEditListTimelineInvalidTimescale(t *testing.T) {
	elstBox := testElstBox(0, testNormalEdit(1000, 0))

	// zero movie timescale
	moov := testMoovBox(
		testMvhdBox(0, 0, 1000),
		testMdhdBox(0, 48000, 48000),
		elstBox,
	)
	mvhd, mdhd, elst := testParseTrack(t, moov)
	_, err := NewEditListTimeline(elst, mvhd, mdhd)
	assert.True(t, errors.Is(err, ErrEditListInvalidTimescale), "%v", err)

	// zero media timescale
	moov = testMoovBox(
		testMvhdBox(0, 1000, 1000),
		testMdhdBox(0, 0, 48000),
		elstBox,
	)
	mvhd, mdhd, elst = testParseTrack(t, moov)
	_, err = NewEditListTimeline(elst, mvhd, mdhd)
	assert.True(t, errors.Is(err, ErrEditListInvalidTimescale), "%v", err)

	// missing mvhd / mdhd
	assert.True(t, errors.Is(mustTimelineError(t, elst, nil, mdhd), ErrEditListInvalidTimescale))
	assert.True(t, errors.Is(mustTimelineError(t, elst, mvhd, nil), ErrEditListInvalidTimescale))

	// the identity timeline still needs valid timescales
	assert.True(t, errors.Is(mustTimelineError(t, nil, nil, nil), ErrEditListInvalidTimescale))
}

func mustTimelineError(t *testing.T, elst *Elst, mvhd *Mvhd, mdhd *Mdhd) error {
	t.Helper()
	_, err := NewEditListTimeline(elst, mvhd, mdhd)
	require.Error(t, err)
	return err
}

func TestEditListTimelineInvalidEntries(t *testing.T) {
	mvhdBox := testMvhdBox(0, 1000, 1000)
	mdhdBox := testMdhdBox(0, 48000, 48000)
	parseElst := func(t *testing.T, entries ...testElstEntry) (*Mvhd, *Mdhd, *Elst) {
		t.Helper()
		moov := testMoovBox(mvhdBox, mdhdBox, testElstBox(0, entries...))
		return testParseTrack(t, moov)
	}

	// unsupported elst version (not reachable through the parser, which only
	// accepts v0/v1, but callers can construct the struct)
	elst := &Elst{FullBox: FullBox{Version: 2}}
	_, err := NewEditListTimeline(elst, &Mvhd{Timescale: 1000}, &Mdhd{Timescale: 48000})
	assert.True(t, errors.Is(err, ErrEditListUnsupportedVersion), "%v", err)

	// media rate other than 1.0
	for _, entry := range []testElstEntry{
		{duration: 1000, mediaTime: 0, rateInt: 2},
		{duration: 1000, mediaTime: 0, rateInt: 0, rateFrac: -32768}, // 0.5x
		{duration: 1000, mediaTime: 0, rateInt: 0, rateFrac: 16384},  // 1.25x
	} {
		mvhd, mdhd, elst := parseElst(t, entry)
		_, err := NewEditListTimeline(elst, mvhd, mdhd)
		assert.True(t, errors.Is(err, ErrEditListUnsupportedRate),
			"rate=%d+%d: %v", entry.rateInt, entry.rateFrac, err)
	}

	// an empty edit's rate is irrelevant because it presents no media
	mvhd, mdhd, elst := parseElst(t, testElstEntry{duration: 500, mediaTime: -1, rateInt: 0})
	_, err = NewEditListTimeline(elst, mvhd, mdhd)
	assert.NoError(t, err)

	// media time below -1 is corrupt
	mvhd, mdhd, elst = parseElst(t, testNormalEdit(1000, -2))
	_, err = NewEditListTimeline(elst, mvhd, mdhd)
	assert.True(t, errors.Is(err, ErrEditListInvalidEntry), "%v", err)

	// zero-length edits are rejected
	mvhd, mdhd, elst = parseElst(t, testNormalEdit(0, 0))
	_, err = NewEditListTimeline(elst, mvhd, mdhd)
	assert.True(t, errors.Is(err, ErrEditListInvalidEntry), "%v", err)
	mvhd, mdhd, elst = parseElst(t, testEmptyEdit(0))
	_, err = NewEditListTimeline(elst, mvhd, mdhd)
	assert.True(t, errors.Is(err, ErrEditListInvalidEntry), "%v", err)
}

func TestEditListTimelineOverflow(t *testing.T) {
	// accumulation of the movie durations overflows uint64
	elstBox := testElstBox(1,
		testNormalEdit(math.MaxUint64, 0),
		testNormalEdit(1, 0),
	)
	moov := testMoovBox(
		testMvhdBox(1, math.MaxUint32, 0),
		testMdhdBox(1, 1, 0),
		elstBox,
	)
	mvhd, mdhd, elst := testParseTrack(t, moov)
	_, err := NewEditListTimeline(elst, mvhd, mdhd)
	assert.True(t, errors.Is(err, ErrEditListOverflow), "%v", err)

	// conversion of a segment duration to media time overflows uint64
	elstBox = testElstBox(1, testNormalEdit(math.MaxUint64, 0))
	moov = testMoovBox(
		testMvhdBox(1, 1, 0),
		testMdhdBox(1, math.MaxUint32, 0),
		elstBox,
	)
	mvhd, mdhd, elst = testParseTrack(t, moov)
	_, err = NewEditListTimeline(elst, mvhd, mdhd)
	assert.True(t, errors.Is(err, ErrEditListOverflow), "%v", err)

	// accumulating the media time overflows int64
	elstBox = testElstBox(1, testNormalEdit(1000, math.MaxInt64))
	moov = testMoovBox(
		testMvhdBox(1, 1000, 1000),
		testMdhdBox(1, 1000, 1000),
		elstBox,
	)
	mvhd, mdhd, elst = testParseTrack(t, moov)
	_, err = NewEditListTimeline(elst, mvhd, mdhd)
	assert.True(t, errors.Is(err, ErrEditListOverflow), "%v", err)

	// an identity timeline (no elst) also reports conversion overflow
	tl, err := NewEditListTimeline(nil, &Mvhd{Timescale: 1}, &Mdhd{Timescale: math.MaxUint32})
	require.NoError(t, err)
	_, err = tl.MediaTimeAt(math.MaxUint64)
	assert.True(t, errors.Is(err, ErrEditListOverflow), "%v", err)
}
