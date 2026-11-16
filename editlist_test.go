package mp4

import (
	"bytes"
	"errors"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/src-d/go-billy.v4/memfs"
)

func TestEditListTimelineEmptyEditAndTimescales(t *testing.T) {
	// movie timescale 1000, media timescale 48000:
	//   [0, 500)    empty edit (0.5s of nothing)
	//   [500, 2500) media time 96000 (2s of media)
	elst := &Elst{
		FullBox:    FullBox{Version: 0, Flags: [3]byte{0, 0, 0}},
		EntryCount: 2,
		Entries: []ElstEntry{
			{SegmentDurationV0: 500, MediaTimeV0: -1, MediaRateInteger: 0, MediaRateFraction: 0},
			{SegmentDurationV0: 2000, MediaTimeV0: 96000, MediaRateInteger: 1, MediaRateFraction: 0},
		},
	}
	tl, err := NewEditListTimeline(elst, 1000, 48000)
	require.NoError(t, err)
	assert.Equal(t, uint32(1000), tl.MovieTimescale())
	assert.Equal(t, uint32(48000), tl.MediaTimescale())

	// inside the empty edit
	for _, pt := range []uint64{0, 1, 499} {
		_, err := tl.MediaTimeAt(pt)
		assert.True(t, errors.Is(err, ErrPlaybackTimeInEmptyEdit), "playbackTime=%d: %v", pt, err)
	}

	// normal edit, half-open interval [500, 2500)
	cases := []struct {
		playbackTime uint64
		mediaTime    int64
	}{
		{500, 96000},
		{501, 96048},
		{1500, 96000 + 48000},
		{2499, 96000 + 1999*48},
	}
	for _, c := range cases {
		mt, err := tl.MediaTimeAt(c.playbackTime)
		require.NoError(t, err, "playbackTime=%d", c.playbackTime)
		assert.Equal(t, c.mediaTime, mt, "playbackTime=%d", c.playbackTime)

		pts, err := tl.PlaybackTimesAt(c.mediaTime)
		require.NoError(t, err, "mediaTime=%d", c.mediaTime)
		assert.Equal(t, []uint64{c.playbackTime}, pts, "mediaTime=%d", c.mediaTime)
	}

	// past the end of the edit list
	_, err = tl.MediaTimeAt(2500)
	assert.True(t, errors.Is(err, ErrPlaybackTimeOutOfRange), "%v", err)

	// media times not presented by any edit
	for _, mt := range []int64{-1, 0, 95999, 96000 + 2000*48} {
		_, err := tl.PlaybackTimesAt(mt)
		assert.True(t, errors.Is(err, ErrMediaTimeNotPresented), "mediaTime=%d: %v", mt, err)
	}
}

func TestEditListTimelineDuplicateMediaTime(t *testing.T) {
	// version 1 elst, identical timescales, two normal edits presenting the
	// same media range [500, 1500) twice.
	elst := &Elst{
		FullBox:    FullBox{Version: 1, Flags: [3]byte{0, 0, 0}},
		EntryCount: 2,
		Entries: []ElstEntry{
			{SegmentDurationV1: 1000, MediaTimeV1: 500, MediaRateInteger: 1, MediaRateFraction: 0},
			{SegmentDurationV1: 1000, MediaTimeV1: 500, MediaRateInteger: 1, MediaRateFraction: 0},
		},
	}
	tl, err := NewEditListTimeline(elst, 1000, 1000)
	require.NoError(t, err)

	mt, err := tl.MediaTimeAt(0)
	require.NoError(t, err)
	assert.Equal(t, int64(500), mt)
	mt, err = tl.MediaTimeAt(999)
	require.NoError(t, err)
	assert.Equal(t, int64(1499), mt)
	mt, err = tl.MediaTimeAt(1000)
	require.NoError(t, err)
	assert.Equal(t, int64(500), mt)
	mt, err = tl.MediaTimeAt(1999)
	require.NoError(t, err)
	assert.Equal(t, int64(1499), mt)
	_, err = tl.MediaTimeAt(2000)
	assert.True(t, errors.Is(err, ErrPlaybackTimeOutOfRange), "%v", err)

	// the reverse query reports both edits instead of silently picking one
	pts, err := tl.PlaybackTimesAt(500)
	require.NoError(t, err)
	assert.Equal(t, []uint64{0, 1000}, pts)
	pts, err = tl.PlaybackTimesAt(1499)
	require.NoError(t, err)
	assert.Equal(t, []uint64{999, 1999}, pts)
	_, err = tl.PlaybackTimesAt(1500)
	assert.True(t, errors.Is(err, ErrMediaTimeNotPresented), "%v", err)
}

func TestEditListTimelineNoEditList(t *testing.T) {
	// no elst box: identity mapping between the two time bases
	tl, err := NewEditListTimeline(nil, 1000, 48000)
	require.NoError(t, err)
	mt, err := tl.MediaTimeAt(0)
	require.NoError(t, err)
	assert.Equal(t, int64(0), mt)
	mt, err = tl.MediaTimeAt(2)
	require.NoError(t, err)
	assert.Equal(t, int64(96), mt)
	pts, err := tl.PlaybackTimesAt(96)
	require.NoError(t, err)
	assert.Equal(t, []uint64{2}, pts)
}

func TestEditListTimelineInvalid(t *testing.T) {
	validEntry := ElstEntry{SegmentDurationV0: 1000, MediaTimeV0: 0, MediaRateInteger: 1}
	makeElst := func(version byte, entry ElstEntry) *Elst {
		return &Elst{
			FullBox:    FullBox{Version: version, Flags: [3]byte{0, 0, 0}},
			EntryCount: 1,
			Entries:    []ElstEntry{entry},
		}
	}

	// zero timescales
	_, err := NewEditListTimeline(makeElst(0, validEntry), 0, 48000)
	assert.True(t, errors.Is(err, ErrEditListZeroTimescale), "%v", err)
	_, err = NewEditListTimeline(makeElst(0, validEntry), 1000, 0)
	assert.True(t, errors.Is(err, ErrEditListZeroTimescale), "%v", err)
	_, err = NewEditListTimeline(nil, 0, 0)
	assert.True(t, errors.Is(err, ErrEditListZeroTimescale), "%v", err)

	// unsupported elst version
	_, err = NewEditListTimeline(makeElst(2, validEntry), 1000, 48000)
	assert.True(t, errors.Is(err, ErrEditListUnsupportedVersion), "%v", err)

	// non 1x media rates
	entry := validEntry
	entry.MediaRateInteger = 2
	_, err = NewEditListTimeline(makeElst(0, entry), 1000, 48000)
	assert.True(t, errors.Is(err, ErrEditListUnsupportedRate), "%v", err)
	entry = validEntry
	entry.MediaRateInteger = 0
	entry.MediaRateFraction = -32768 // 0.5x
	_, err = NewEditListTimeline(makeElst(0, entry), 1000, 48000)
	assert.True(t, errors.Is(err, ErrEditListUnsupportedRate), "%v", err)

	// corrupt entries
	entry = validEntry
	entry.MediaTimeV0 = -2
	_, err = NewEditListTimeline(makeElst(0, entry), 1000, 48000)
	assert.True(t, errors.Is(err, ErrEditListInvalidEntry), "%v", err)
	entry = validEntry
	entry.SegmentDurationV0 = 0
	_, err = NewEditListTimeline(makeElst(0, entry), 1000, 48000)
	assert.True(t, errors.Is(err, ErrEditListInvalidEntry), "%v", err)

	// segment duration accumulation overflow
	elst := &Elst{
		FullBox:    FullBox{Version: 1, Flags: [3]byte{0, 0, 0}},
		EntryCount: 2,
		Entries: []ElstEntry{
			{SegmentDurationV1: math.MaxUint64, MediaTimeV1: 0, MediaRateInteger: 1},
			{SegmentDurationV1: 1, MediaTimeV1: 0, MediaRateInteger: 1},
		},
	}
	_, err = NewEditListTimeline(elst, 1000, 48000)
	assert.True(t, errors.Is(err, ErrEditListOverflow), "%v", err)
}

func TestEditListTimelineOverflow(t *testing.T) {
	// timescale conversion overflow on the forward query
	elst := &Elst{
		FullBox:    FullBox{Version: 1, Flags: [3]byte{0, 0, 0}},
		EntryCount: 1,
		Entries: []ElstEntry{
			{SegmentDurationV1: math.MaxUint64, MediaTimeV1: 0, MediaRateInteger: 1},
		},
	}
	tl, err := NewEditListTimeline(elst, 1, 0xffffffff)
	require.NoError(t, err)
	_, err = tl.MediaTimeAt(math.MaxUint64 - 1)
	assert.True(t, errors.Is(err, ErrEditListOverflow), "%v", err)

	// media time accumulation overflow on the forward query
	elst = &Elst{
		FullBox:    FullBox{Version: 1, Flags: [3]byte{0, 0, 0}},
		EntryCount: 1,
		Entries: []ElstEntry{
			{SegmentDurationV1: math.MaxUint64, MediaTimeV1: math.MaxInt64, MediaRateInteger: 1},
		},
	}
	tl, err = NewEditListTimeline(elst, 1, 1)
	require.NoError(t, err)
	mt, err := tl.MediaTimeAt(0)
	require.NoError(t, err)
	assert.Equal(t, int64(math.MaxInt64), mt)
	_, err = tl.MediaTimeAt(1)
	assert.True(t, errors.Is(err, ErrEditListOverflow), "%v", err)
}

func TestEditListTimelineRoundTrip(t *testing.T) {
	// build a moov box, write it out, re-read it, and verify that the
	// timeline built from the re-read boxes behaves identically
	elst := &Elst{
		FullBox:    FullBox{Version: 0, Flags: [3]byte{0, 0, 0}},
		EntryCount: 3,
		Entries: []ElstEntry{
			{SegmentDurationV0: 500, MediaTimeV0: -1, MediaRateInteger: 0, MediaRateFraction: 0},
			{SegmentDurationV0: 1000, MediaTimeV0: 48000, MediaRateInteger: 1, MediaRateFraction: 0},
			{SegmentDurationV0: 1000, MediaTimeV0: 192000, MediaRateInteger: 1, MediaRateFraction: 0},
		},
	}
	mvhd := &Mvhd{
		FullBox:    FullBox{Version: 0, Flags: [3]byte{0, 0, 0}},
		Timescale:  1000,
		DurationV0: 2500,
	}
	mdhd := &Mdhd{
		FullBox:    FullBox{Version: 0, Flags: [3]byte{0, 0, 0}},
		Timescale:  48000,
		DurationV0: 240000,
	}

	fs := memfs.New()
	f, err := fs.Create("moov.mp4")
	require.NoError(t, err)
	w := NewWriter(f)
	_, err = w.StartBox(&BoxInfo{Type: BoxTypeMoov()})
	require.NoError(t, err)
	_, err = w.StartBox(&BoxInfo{Type: BoxTypeMvhd()})
	require.NoError(t, err)
	_, err = Marshal(w, mvhd, Context{})
	require.NoError(t, err)
	_, err = w.EndBox() // mvhd
	require.NoError(t, err)
	_, err = w.StartBox(&BoxInfo{Type: BoxTypeTrak()})
	require.NoError(t, err)
	_, err = w.StartBox(&BoxInfo{Type: BoxTypeMdia()})
	require.NoError(t, err)
	_, err = w.StartBox(&BoxInfo{Type: BoxTypeMdhd()})
	require.NoError(t, err)
	_, err = Marshal(w, mdhd, Context{})
	require.NoError(t, err)
	_, err = w.EndBox() // mdhd
	require.NoError(t, err)
	_, err = w.EndBox() // mdia
	require.NoError(t, err)
	_, err = w.StartBox(&BoxInfo{Type: BoxTypeEdts()})
	require.NoError(t, err)
	_, err = w.StartBox(&BoxInfo{Type: BoxTypeElst()})
	require.NoError(t, err)
	_, err = Marshal(w, elst, Context{})
	require.NoError(t, err)
	_, err = w.EndBox() // elst
	require.NoError(t, err)
	_, err = w.EndBox() // edts
	require.NoError(t, err)
	_, err = w.EndBox() // trak
	require.NoError(t, err)
	_, err = w.EndBox() // moov
	require.NoError(t, err)
	require.NoError(t, f.Close())

	f, err = fs.Open("moov.mp4")
	require.NoError(t, err)
	defer f.Close()
	bips, err := ExtractBoxesWithPayload(f, nil, []BoxPath{
		{BoxTypeMoov(), BoxTypeMvhd()},
		{BoxTypeMoov(), BoxTypeTrak(), BoxTypeMdia(), BoxTypeMdhd()},
		{BoxTypeMoov(), BoxTypeTrak(), BoxTypeEdts(), BoxTypeElst()},
	})
	require.NoError(t, err)
	require.Len(t, bips, 3)
	var gotMvhd *Mvhd
	var gotMdhd *Mdhd
	var gotElst *Elst
	for _, bip := range bips {
		switch bip.Info.Type {
		case BoxTypeMvhd():
			gotMvhd = bip.Payload.(*Mvhd)
		case BoxTypeMdhd():
			gotMdhd = bip.Payload.(*Mdhd)
		case BoxTypeElst():
			gotElst = bip.Payload.(*Elst)
		}
	}
	require.NotNil(t, gotMvhd)
	require.NotNil(t, gotMdhd)
	require.NotNil(t, gotElst)

	// the re-read elst box is identical to the original
	assert.Equal(t, *elst, *gotElst)

	tl, err := NewEditListTimeline(gotElst, gotMvhd.Timescale, gotMdhd.Timescale)
	require.NoError(t, err)

	// empty edit at the head of the timeline
	_, err = tl.MediaTimeAt(499)
	assert.True(t, errors.Is(err, ErrPlaybackTimeInEmptyEdit), "%v", err)

	// first trimmed segment: media time 48000 for 1s of playback
	mt, err := tl.MediaTimeAt(500)
	require.NoError(t, err)
	assert.Equal(t, int64(48000), mt)
	mt, err = tl.MediaTimeAt(1499)
	require.NoError(t, err)
	assert.Equal(t, int64(48000+999*48), mt)

	// second trimmed segment: media time 192000 for 1s of playback
	mt, err = tl.MediaTimeAt(1500)
	require.NoError(t, err)
	assert.Equal(t, int64(192000), mt)
	mt, err = tl.MediaTimeAt(2499)
	require.NoError(t, err)
	assert.Equal(t, int64(192000+999*48), mt)

	_, err = tl.MediaTimeAt(2500)
	assert.True(t, errors.Is(err, ErrPlaybackTimeOutOfRange), "%v", err)

	// reverse queries on the re-read timeline
	pts, err := tl.PlaybackTimesAt(48000)
	require.NoError(t, err)
	assert.Equal(t, []uint64{500}, pts)
	pts, err = tl.PlaybackTimesAt(192000)
	require.NoError(t, err)
	assert.Equal(t, []uint64{1500}, pts)
	// the gap between the two trimmed segments is not presented
	_, err = tl.PlaybackTimesAt(96000)
	assert.True(t, errors.Is(err, ErrMediaTimeNotPresented), "%v", err)

	// the elst box payload is written back byte-identically
	var buf bytes.Buffer
	n, err := Marshal(&buf, gotElst, Context{})
	require.NoError(t, err)
	var want bytes.Buffer
	wn, err := Marshal(&want, elst, Context{})
	require.NoError(t, err)
	assert.Equal(t, wn, n)
	assert.Equal(t, want.Bytes(), buf.Bytes())
}
