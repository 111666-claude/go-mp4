package mp4

import (
	"errors"
	"os"
	"testing"

	"gopkg.in/src-d/go-billy.v4/memfs"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// trakSpec describes a synthetic trak box for TrackView fixtures.
type trakSpec struct {
	trackID uint32
	handler string  // 4cc handler type; empty with noHdlr
	lang    [3]byte // packed 5-bit ISO 639-2/T code
	group   int16   // tkhd alternate_group
	enabled bool    // tkhd track_enabled flag
	noMdhd  bool    // omit mdia/mdhd
	noHdlr  bool    // omit mdia/hdlr
}

func langCode(s string) [3]byte {
	return [3]byte{s[0] - 'a', s[1] - 'a', s[2] - 'a'}
}

func buildTrackViewFixture(t *testing.T, specs []trakSpec) *TrackView {
	t.Helper()
	f, err := memfs.New().Create("fixture.mp4")
	require.NoError(t, err)
	defer f.Close()

	w := NewWriter(f)
	_, err = w.StartBox(&BoxInfo{Type: BoxTypeMoov()})
	require.NoError(t, err)
	for _, spec := range specs {
		_, err = w.StartBox(&BoxInfo{Type: BoxTypeTrak()})
		require.NoError(t, err)

		_, err = w.StartBox(&BoxInfo{Type: BoxTypeTkhd()})
		require.NoError(t, err)
		tkhd := &Tkhd{TrackID: spec.trackID, AlternateGroup: spec.group}
		if spec.enabled {
			tkhd.SetFlags(0x1)
		}
		_, err = Marshal(w, tkhd, Context{})
		require.NoError(t, err)
		_, err = w.EndBox() // tkhd
		require.NoError(t, err)

		_, err = w.StartBox(&BoxInfo{Type: BoxTypeMdia()})
		require.NoError(t, err)
		if !spec.noMdhd {
			_, err = w.StartBox(&BoxInfo{Type: BoxTypeMdhd()})
			require.NoError(t, err)
			_, err = Marshal(w, &Mdhd{Timescale: 90000, Language: spec.lang}, Context{})
			require.NoError(t, err)
			_, err = w.EndBox() // mdhd
			require.NoError(t, err)
		}
		if !spec.noHdlr {
			var handler [4]byte
			copy(handler[:], spec.handler)
			_, err = w.StartBox(&BoxInfo{Type: BoxTypeHdlr()})
			require.NoError(t, err)
			_, err = Marshal(w, &Hdlr{HandlerType: handler, Name: "test"}, Context{})
			require.NoError(t, err)
			_, err = w.EndBox() // hdlr
			require.NoError(t, err)
		}
		_, err = w.EndBox() // mdia
		require.NoError(t, err)

		_, err = w.EndBox() // trak
		require.NoError(t, err)
	}
	_, err = w.EndBox() // moov
	require.NoError(t, err)

	_, err = f.Seek(0, 0)
	require.NoError(t, err)
	view, err := ProbeTrackView(f)
	require.NoError(t, err)
	return view
}

func findGroup(view *TrackView, mediaType string, group int16) *TrackGroup {
	for _, g := range view.Groups {
		if g.MediaType == mediaType && g.AlternateGroup == group {
			return g
		}
	}
	return nil
}

func TestTrackViewDualAudio(t *testing.T) {
	view := buildTrackViewFixture(t, []trakSpec{
		{trackID: 1, handler: MediaTypeVideo, lang: langCode("eng"), enabled: true},
		{trackID: 2, handler: MediaTypeAudio, lang: langCode("eng"), group: 1, enabled: true},
		{trackID: 3, handler: MediaTypeAudio, lang: langCode("zho"), group: 1, enabled: true},
	})

	require.Len(t, view.Tracks, 3)
	assert.Empty(t, view.Issues)

	assert.Equal(t, uint32(1), view.Tracks[0].TrackID)
	assert.Equal(t, MediaTypeVideo, view.Tracks[0].MediaType)
	assert.Equal(t, "eng", view.Tracks[0].Language)
	assert.True(t, view.Tracks[0].LanguageValid)
	assert.True(t, view.Tracks[0].Enabled)
	assert.Equal(t, int16(0), view.Tracks[0].AlternateGroup)

	assert.Equal(t, "zho", view.Tracks[2].Language)
	assert.Equal(t, int16(1), view.Tracks[2].AlternateGroup)

	// zero alternate group must not be merged: the video track is a singleton
	require.Len(t, view.Groups, 2)
	vg := findGroup(view, MediaTypeVideo, 0)
	require.NotNil(t, vg)
	require.Len(t, vg.Candidates, 1)
	assert.Equal(t, uint32(1), vg.Candidates[0].TrackID)

	// both audio dubs are candidates of the same alternate group, in file order
	ag := findGroup(view, MediaTypeAudio, 1)
	require.NotNil(t, ag)
	require.Len(t, ag.Candidates, 2)
	assert.Equal(t, uint32(2), ag.Candidates[0].TrackID)
	assert.Equal(t, uint32(3), ag.Candidates[1].TrackID)

	// language priority drives the selection
	assert.Equal(t, uint32(3), view.Select(MediaTypeAudio, "zho", "eng").TrackID)
	assert.Equal(t, uint32(2), view.Select(MediaTypeAudio, "eng", "zho").TrackID)
	assert.Equal(t, uint32(3), view.Select(MediaTypeAudio, "ZHO").TrackID)
	// unlisted languages fall back to file order
	assert.Equal(t, uint32(2), view.Select(MediaTypeAudio, "jpn").TrackID)
	assert.Equal(t, uint32(2), view.Select(MediaTypeAudio).TrackID)
	// video and unknown media types
	assert.Equal(t, uint32(1), view.Select(MediaTypeVideo).TrackID)
	assert.Nil(t, view.Select(MediaTypeMeta))
}

func TestTrackViewDisabledTrack(t *testing.T) {
	view := buildTrackViewFixture(t, []trakSpec{
		{trackID: 1, handler: MediaTypeAudio, lang: langCode("eng"), group: 1, enabled: false},
		{trackID: 2, handler: MediaTypeAudio, lang: langCode("zho"), group: 1, enabled: true},
	})

	assert.False(t, view.Tracks[0].Enabled)
	assert.True(t, view.Tracks[1].Enabled)

	// both tracks remain candidates of the same group
	ag := findGroup(view, MediaTypeAudio, 1)
	require.NotNil(t, ag)
	require.Len(t, ag.Candidates, 2)

	// the enabled track wins even when the disabled one matches the language
	assert.Equal(t, uint32(2), view.Select(MediaTypeAudio, "eng").TrackID)
}

func TestTrackViewAllTracksDisabled(t *testing.T) {
	view := buildTrackViewFixture(t, []trakSpec{
		{trackID: 1, handler: MediaTypeAudio, lang: langCode("eng"), group: 1},
		{trackID: 2, handler: MediaTypeAudio, lang: langCode("zho"), group: 1},
	})

	// with no enabled track, language priority applies to disabled ones
	assert.Equal(t, uint32(2), view.Select(MediaTypeAudio, "zho").TrackID)
	assert.Equal(t, uint32(1), view.Select(MediaTypeAudio).TrackID)
}

func TestTrackViewMissingMetadata(t *testing.T) {
	view := buildTrackViewFixture(t, []trakSpec{
		{trackID: 1, handler: MediaTypeAudio, lang: langCode("eng"), noMdhd: true, enabled: true},
		{trackID: 2, lang: langCode("eng"), noHdlr: true, enabled: true},
	})

	require.Len(t, view.Tracks, 2)
	assert.Empty(t, view.Tracks[0].Language)
	assert.False(t, view.Tracks[0].LanguageValid)
	assert.Empty(t, view.Tracks[1].MediaType)

	require.Len(t, view.Issues, 2)
	assert.True(t, errors.Is(view.Issues[0], ErrMdhdNotFound))
	assert.True(t, errors.Is(view.Issues[1], ErrHdlrNotFound))

	// tracks still degrade into singleton groups instead of failing
	require.Len(t, view.Groups, 2)
	// the soun track without mdhd stays selectable; the track without hdlr
	// has no media type and matches nothing
	assert.Equal(t, uint32(1), view.Select(MediaTypeAudio, "eng").TrackID)
}

func TestTrackViewDuplicateTrackID(t *testing.T) {
	f, err := memfs.New().Create("dup.mp4")
	require.NoError(t, err)
	defer f.Close()

	w := NewWriter(f)
	_, err = w.StartBox(&BoxInfo{Type: BoxTypeMoov()})
	require.NoError(t, err)
	for i := 0; i < 2; i++ {
		_, err = w.StartBox(&BoxInfo{Type: BoxTypeTrak()})
		require.NoError(t, err)
		_, err = w.StartBox(&BoxInfo{Type: BoxTypeTkhd()})
		require.NoError(t, err)
		_, err = Marshal(w, &Tkhd{TrackID: 1}, Context{})
		require.NoError(t, err)
		_, err = w.EndBox() // tkhd
		require.NoError(t, err)
		_, err = w.EndBox() // trak
		require.NoError(t, err)
	}
	_, err = w.EndBox() // moov
	require.NoError(t, err)

	_, err = f.Seek(0, 0)
	require.NoError(t, err)
	_, err = ProbeTrackView(f)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrDuplicateTrackID))
}

func TestTrackViewBadLanguageCode(t *testing.T) {
	view := buildTrackViewFixture(t, []trakSpec{
		{trackID: 1, handler: MediaTypeAudio, lang: [3]byte{0x1f, 0x1f, 0x1f}, enabled: true},
	})

	require.Len(t, view.Tracks, 1)
	assert.Empty(t, view.Tracks[0].Language)
	assert.False(t, view.Tracks[0].LanguageValid)
	require.Len(t, view.Issues, 1)
	assert.True(t, errors.Is(view.Issues[0], ErrInvalidLanguageCode))

	// invalid language ranks below listed ones but the track stays selectable
	assert.Equal(t, uint32(1), view.Select(MediaTypeAudio, "eng").TrackID)
}

func TestTrackViewConflictingAlternateGroup(t *testing.T) {
	view := buildTrackViewFixture(t, []trakSpec{
		{trackID: 1, handler: MediaTypeVideo, lang: langCode("und"), group: 1, enabled: true},
		{trackID: 2, handler: MediaTypeAudio, lang: langCode("eng"), group: 1, enabled: true},
	})

	require.Len(t, view.Issues, 1)
	assert.True(t, errors.Is(view.Issues[0], ErrConflictingAlternateGroup))

	// the conflicting tracks are kept in separate groups
	require.Len(t, view.Groups, 2)
	vg := findGroup(view, MediaTypeVideo, 1)
	require.NotNil(t, vg)
	require.Len(t, vg.Candidates, 1)
	ag := findGroup(view, MediaTypeAudio, 1)
	require.NotNil(t, ag)
	require.Len(t, ag.Candidates, 1)
}

func TestTrackViewInvalidHierarchy(t *testing.T) {
	f, err := memfs.New().Create("broken.mp4")
	require.NoError(t, err)
	defer f.Close()

	w := NewWriter(f)
	_, err = w.StartBox(&BoxInfo{Type: BoxTypeMoov()})
	require.NoError(t, err)
	_, err = w.StartBox(&BoxInfo{Type: BoxTypeTrak()})
	require.NoError(t, err)
	_, err = w.StartBox(&BoxInfo{Type: BoxTypeTkhd()})
	require.NoError(t, err)
	_, err = Marshal(w, &Tkhd{TrackID: 1}, Context{})
	require.NoError(t, err)
	_, err = w.EndBox() // tkhd
	require.NoError(t, err)
	// mdhd directly under trak: illegal nesting
	_, err = w.StartBox(&BoxInfo{Type: BoxTypeMdhd()})
	require.NoError(t, err)
	_, err = Marshal(w, &Mdhd{Timescale: 90000}, Context{})
	require.NoError(t, err)
	_, err = w.EndBox() // mdhd
	_, err = w.EndBox() // trak
	require.NoError(t, err)
	_, err = w.EndBox() // moov
	require.NoError(t, err)

	_, err = f.Seek(0, 0)
	require.NoError(t, err)
	_, err = ProbeTrackView(f)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidTrackHierarchy))
}

func TestTrackViewSampleFile(t *testing.T) {
	f, err := os.Open("./testdata/sample.mp4")
	require.NoError(t, err)
	defer f.Close()

	view, err := ProbeTrackView(f)
	require.NoError(t, err)

	require.Len(t, view.Tracks, 2)
	assert.Equal(t, uint32(1), view.Tracks[0].TrackID)
	assert.Equal(t, MediaTypeVideo, view.Tracks[0].MediaType)
	assert.True(t, view.Tracks[0].Enabled)
	assert.Equal(t, uint32(2), view.Tracks[1].TrackID)
	assert.Equal(t, MediaTypeAudio, view.Tracks[1].MediaType)
	assert.True(t, view.Tracks[1].Enabled)

	// the file has no shared alternate group: two singleton groups
	// (the audio track carries alternate_group=1 but no alternate)
	require.Len(t, view.Groups, 2)
	for _, g := range view.Groups {
		require.Len(t, g.Candidates, 1)
	}

	assert.Equal(t, uint32(1), view.Select(MediaTypeVideo).TrackID)
	assert.Equal(t, uint32(2), view.Select(MediaTypeAudio).TrackID)
}
