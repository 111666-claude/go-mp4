package mp4

import (
	"bytes"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/orcaman/writerseeker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stblFixtureConfig describes the fixed MP4 boxes used by the recovery point
// tests. The sample table contains 8 samples; the roll sgpd defines two
// groups (distances -2 and -1) and several samples remain ungrouped.
type stblFixtureConfig struct {
	rollSgpd       *Sgpd
	prolSgpd       *Sgpd
	rollSbgp       *Sbgp
	prolSbgp       *Sbgp
	stss           *Stss
	omitStss       bool
	additionalSgpd []*Sgpd
	additionalSbgp []*Sbgp
}

func writeBox(t *testing.T, w *Writer, box IImmutableBox) {
	t.Helper()
	_, err := w.StartBox(&BoxInfo{Type: box.GetType()})
	require.NoError(t, err)
	_, err = Marshal(w, box, Context{})
	require.NoError(t, err)
	_, err = w.EndBox()
	require.NoError(t, err)
}

// buildStblFixture writes an stbl box containing stsz/stss/sgpd/sbgp boxes and
// returns a reader over the serialized bytes together with the parsed stbl
// BoxInfo.
func buildStblFixture(t *testing.T, cfg stblFixtureConfig) (*bytes.Reader, *BoxInfo) {
	t.Helper()

	ws := &writerseeker.WriterSeeker{}
	w := NewWriter(ws)
	_, err := w.StartBox(&BoxInfo{Type: BoxTypeStbl()})
	require.NoError(t, err)

	// stsz: 8 samples with individual sizes
	writeBox(t, w, &Stsz{
		FullBox:     FullBox{Version: 0},
		SampleSize:  0,
		SampleCount: 8,
		EntrySize:   []uint32{10, 11, 12, 13, 14, 15, 16, 17},
	})

	if cfg.stss != nil {
		writeBox(t, w, cfg.stss)
	} else if !cfg.omitStss {
		writeBox(t, w, &Stss{
			FullBox:      FullBox{Version: 0},
			EntryCount:   4,
			SampleNumber: []uint32{1, 3, 5, 7},
		})
	}

	if cfg.rollSgpd != nil {
		writeBox(t, w, cfg.rollSgpd)
	}
	if cfg.prolSgpd != nil {
		writeBox(t, w, cfg.prolSgpd)
	}
	for _, box := range cfg.additionalSgpd {
		writeBox(t, w, box)
	}
	if cfg.rollSbgp != nil {
		writeBox(t, w, cfg.rollSbgp)
	}
	if cfg.prolSbgp != nil {
		writeBox(t, w, cfg.prolSbgp)
	}
	for _, box := range cfg.additionalSbgp {
		writeBox(t, w, box)
	}

	_, err = w.EndBox()
	require.NoError(t, err)

	raw := ws.BytesReader()
	data, err := io.ReadAll(raw)
	require.NoError(t, err)

	bis, err := ExtractBox(bytes.NewReader(data), nil, BoxPath{BoxTypeStbl()})
	require.NoError(t, err)
	require.Len(t, bis, 1)
	return bytes.NewReader(data), bis[0]
}

func defaultRollSgpd() *Sgpd {
	return &Sgpd{
		FullBox:       FullBox{Version: 1},
		GroupingType:  groupingTypeRoll,
		DefaultLength: 2,
		EntryCount:    2,
		RollDistances: []int16{-2, -1},
	}
}

func defaultProlSgpd() *Sgpd {
	return &Sgpd{
		FullBox:       FullBox{Version: 1},
		GroupingType:  groupingTypeProl,
		DefaultLength: 2,
		EntryCount:    1,
		RollDistances: []int16{-1},
	}
}

// defaultRollSbgp maps (stss sync samples are 0, 2, 4, 6):
// samples 0, 2, 4, 6 belong to roll groups while the non-sync samples
// 1, 3, 5, 7 are skipped by index-0 (no group) runs.
func defaultRollSbgp() *Sbgp {
	return &Sbgp{
		FullBox:      FullBox{Version: 0},
		GroupingType: 0x726F6C6C, // "roll"
		EntryCount:   8,
		Entries: []SbgpEntry{
			{SampleCount: 1, GroupDescriptionIndex: 1}, // sample 0: distance -2
			{SampleCount: 1, GroupDescriptionIndex: 0}, // sample 1: no group
			{SampleCount: 1, GroupDescriptionIndex: 2}, // sample 2: distance -1
			{SampleCount: 1, GroupDescriptionIndex: 0}, // sample 3: no group
			{SampleCount: 1, GroupDescriptionIndex: 1}, // sample 4: distance -2
			{SampleCount: 1, GroupDescriptionIndex: 0}, // sample 5: no group
			{SampleCount: 1, GroupDescriptionIndex: 2}, // sample 6: distance -1
			{SampleCount: 1, GroupDescriptionIndex: 0}, // sample 7: no group
		},
	}
}

func defaultProlSbgp() *Sbgp {
	// The first two samples are skipped so that the prol group lands on
	// sample 2, avoiding overlap with the roll run covering sample 0.
	return &Sbgp{
		FullBox:      FullBox{Version: 0},
		GroupingType: 0x70726F6C, // "prol"
		EntryCount:   2,
		Entries: []SbgpEntry{
			{SampleCount: 2, GroupDescriptionIndex: 0},
			{SampleCount: 1, GroupDescriptionIndex: 1},
		},
	}
}

func TestBuildRecoveryPointIndexMapping(t *testing.T) {
	r, stbl := buildStblFixture(t, stblFixtureConfig{
		rollSgpd: defaultRollSgpd(),
		prolSgpd: defaultProlSgpd(),
		rollSbgp: defaultRollSbgp(),
		prolSbgp: defaultProlSbgp(), // sample 2 additionally belongs to prol group 1
	})

	idx, err := BuildRecoveryPointIndex(r, stbl)
	require.NoError(t, err)
	require.Equal(t, 8, idx.SampleCount)

	// sample 0: roll distance -2; the prol group starts at sample 2.
	rp, err := idx.GetRecoveryPoint(0)
	require.NoError(t, err)
	assert.Equal(t, &RecoveryPoint{
		SampleNumber:      0,
		DecodeStartIndex:  0,
		PrerollStartIndex: 0,
		PrerollEndIndex:   0,
		DisplayStartIndex: 0,
		IsRandomAccess:    true,
		RollDistance:      -2,
		Degraded:          true,
	}, rp)
	assert.Equal(t, 0, rp.PrerollSampleCount())

	// sample 1: ungrouped non-sync sample.
	_, err = idx.GetRecoveryPoint(1)
	assert.True(t, errors.Is(err, ErrNoRecoveryPoint))

	// sample 2: roll group 2 with distance -1 and the prol group (-1).
	rp, err = idx.GetRecoveryPoint(2)
	require.NoError(t, err)
	assert.Equal(t, &RecoveryPoint{
		SampleNumber:      2,
		DecodeStartIndex:  1,
		PrerollStartIndex: 1,
		PrerollEndIndex:   2,
		DisplayStartIndex: 2,
		IsRandomAccess:    true,
		RollDistance:      -1,
		PrerollDistance:   -1,
	}, rp)
	assert.Equal(t, 1, rp.PrerollSampleCount())

	// sample 3: ungrouped non-sync sample.
	_, err = idx.GetRecoveryPoint(3)
	assert.True(t, errors.Is(err, ErrNoRecoveryPoint))

	// sample 4: roll group 1 with distance -2.
	rp, err = idx.GetRecoveryPoint(4)
	require.NoError(t, err)
	assert.Equal(t, &RecoveryPoint{
		SampleNumber:      4,
		DecodeStartIndex:  2,
		PrerollStartIndex: 2,
		PrerollEndIndex:   4,
		DisplayStartIndex: 4,
		IsRandomAccess:    true,
		RollDistance:      -2,
	}, rp)
	assert.Equal(t, 2, rp.PrerollSampleCount())

	// sample 5: ungrouped non-sync sample.
	_, err = idx.GetRecoveryPoint(5)
	assert.True(t, errors.Is(err, ErrNoRecoveryPoint))

	// sample 6: grouped sync sample (roll group 2, distance -1).
	rp, err = idx.GetRecoveryPoint(6)
	require.NoError(t, err)
	assert.True(t, rp.IsRandomAccess)
	assert.Equal(t, 5, rp.DecodeStartIndex)
	assert.Equal(t, 6, rp.DisplayStartIndex)
	assert.Equal(t, int16(-1), rp.RollDistance)

	// sample 7: ungrouped non-sync sample.
	_, err = idx.GetRecoveryPoint(7)
	assert.True(t, errors.Is(err, ErrNoRecoveryPoint))

	// out of range.
	_, err = idx.GetRecoveryPoint(8)
	assert.True(t, errors.Is(err, ErrSampleIndexOutOfRange))
	_, err = idx.GetRecoveryPoint(-1)
	assert.True(t, errors.Is(err, ErrSampleIndexOutOfRange))
}

func TestBuildRecoveryPointIndexNoStss(t *testing.T) {
	// Without an stss box every sample is a sync sample, so even ungrouped
	// samples provide ordinary recovery points.
	r, stbl := buildStblFixture(t, stblFixtureConfig{
		rollSgpd: defaultRollSgpd(),
		rollSbgp: defaultRollSbgp(),
		omitStss: true,
	})

	idx, err := BuildRecoveryPointIndex(r, stbl)
	require.NoError(t, err)

	rp, err := idx.GetRecoveryPoint(7)
	require.NoError(t, err)
	assert.True(t, rp.IsRandomAccess)
	assert.False(t, rp.Degraded)
	assert.Equal(t, 7, rp.DecodeStartIndex)
}

func TestBuildRecoveryPointIndexSbgpIndexZero(t *testing.T) {
	// An sbgp entry with group description index 0 means that the samples of
	// the run do not belong to any group.
	r, stbl := buildStblFixture(t, stblFixtureConfig{
		rollSgpd: defaultRollSgpd(),
		rollSbgp: &Sbgp{
			FullBox:      FullBox{Version: 0},
			GroupingType: 0x726F6C6C,
			EntryCount:   1,
			Entries: []SbgpEntry{
				{SampleCount: 1, GroupDescriptionIndex: 0},
			},
		},
	})

	idx, err := BuildRecoveryPointIndex(r, stbl)
	require.NoError(t, err)

	rp, err := idx.GetRecoveryPoint(0)
	require.NoError(t, err)
	assert.Equal(t, int16(0), rp.RollDistance)
	assert.False(t, rp.Degraded)
}

func TestBuildRecoveryPointIndexSgpdDefaultGroup(t *testing.T) {
	// sgpd v2 with a default group description and no sbgp box: every sample
	// belongs to the default group.
	r, stbl := buildStblFixture(t, stblFixtureConfig{
		rollSgpd: &Sgpd{
			FullBox:                       FullBox{Version: 2},
			GroupingType:                  groupingTypeRoll,
			DefaultSampleDescriptionIndex: 2,
			EntryCount:                    2,
			RollDistances:                 []int16{-2, -1},
		},
		omitStss: true,
	})

	idx, err := BuildRecoveryPointIndex(r, stbl)
	require.NoError(t, err)

	rp, err := idx.GetRecoveryPoint(2)
	require.NoError(t, err)
	assert.Equal(t, int16(-1), rp.RollDistance)
	assert.Equal(t, 1, rp.DecodeStartIndex)
}

func TestBuildRecoveryPointIndexNoDefaultLength(t *testing.T) {
	// sgpd with DefaultLength=0 stores a per-entry description length.
	r, stbl := buildStblFixture(t, stblFixtureConfig{
		rollSgpd: &Sgpd{
			FullBox:       FullBox{Version: 1},
			GroupingType:  groupingTypeRoll,
			DefaultLength: 0,
			EntryCount:    1,
			RollDistancesL: []RollDistanceWithLength{
				{DescriptionLength: 2, RollDistance: -2},
			},
		},
		rollSbgp: &Sbgp{
			FullBox:      FullBox{Version: 0},
			GroupingType: 0x726F6C6C,
			EntryCount:   1,
			Entries: []SbgpEntry{
				{SampleCount: 1, GroupDescriptionIndex: 1},
			},
		},
	})

	idx, err := BuildRecoveryPointIndex(r, stbl)
	require.NoError(t, err)

	rp, err := idx.GetRecoveryPoint(0)
	require.NoError(t, err)
	assert.Equal(t, int16(-2), rp.RollDistance)
	assert.Equal(t, 0, rp.DecodeStartIndex)
	assert.True(t, rp.Degraded)
}

func TestBuildRecoveryPointIndexMissingGroup(t *testing.T) {
	// sbgp references a group description index that does not exist.
	r, stbl := buildStblFixture(t, stblFixtureConfig{
		rollSgpd: defaultRollSgpd(),
		rollSbgp: &Sbgp{
			FullBox:      FullBox{Version: 0},
			GroupingType: 0x726F6C6C,
			EntryCount:   1,
			Entries: []SbgpEntry{
				{SampleCount: 1, GroupDescriptionIndex: 3},
			},
		},
	})

	_, err := BuildRecoveryPointIndex(r, stbl)
	assert.True(t, errors.Is(err, ErrMissingGroupDescription), err)

	// sbgp without a paired sgpd.
	r, stbl = buildStblFixture(t, stblFixtureConfig{
		rollSbgp: defaultRollSbgp(),
	})
	_, err = BuildRecoveryPointIndex(r, stbl)
	assert.True(t, errors.Is(err, ErrMissingGroupDescription), err)
}

func TestBuildRecoveryPointIndexConflictingGroup(t *testing.T) {
	// A roll group assigned to a non-sync sample (only sample 1 is listed in
	// stss) conflicts with the random access point semantics.
	r, stbl := buildStblFixture(t, stblFixtureConfig{
		rollSgpd: defaultRollSgpd(),
		rollSbgp: &Sbgp{
			FullBox:      FullBox{Version: 0},
			GroupingType: 0x726F6C6C,
			EntryCount:   1,
			Entries: []SbgpEntry{
				{SampleCount: 3, GroupDescriptionIndex: 1},
			},
		},
		stss: &Stss{
			FullBox:      FullBox{Version: 0},
			EntryCount:   1,
			SampleNumber: []uint32{1},
		},
	})

	_, err := BuildRecoveryPointIndex(r, stbl)
	assert.True(t, errors.Is(err, ErrGroupConflict), err)
}

func TestBuildRecoveryPointIndexDuplicateBoxes(t *testing.T) {
	r, stbl := buildStblFixture(t, stblFixtureConfig{
		rollSgpd:       defaultRollSgpd(),
		additionalSgpd: []*Sgpd{defaultRollSgpd()},
		rollSbgp:       defaultRollSbgp(),
	})
	_, err := BuildRecoveryPointIndex(r, stbl)
	assert.True(t, errors.Is(err, ErrDuplicateSampleGroupBox), err)

	r, stbl = buildStblFixture(t, stblFixtureConfig{
		rollSgpd:       defaultRollSgpd(),
		rollSbgp:       defaultRollSbgp(),
		additionalSbgp: []*Sbgp{defaultRollSbgp()},
	})
	_, err = BuildRecoveryPointIndex(r, stbl)
	assert.True(t, errors.Is(err, ErrDuplicateSampleGroupBox), err)
}

func TestBuildRecoveryPointIndexRunOverflow(t *testing.T) {
	r, stbl := buildStblFixture(t, stblFixtureConfig{
		rollSgpd: defaultRollSgpd(),
		rollSbgp: &Sbgp{
			FullBox:      FullBox{Version: 0},
			GroupingType: 0x726F6C6C,
			EntryCount:   1,
			Entries: []SbgpEntry{
				{SampleCount: 9, GroupDescriptionIndex: 1},
			},
		},
	})

	_, err := BuildRecoveryPointIndex(r, stbl)
	assert.True(t, errors.Is(err, ErrGroupRunOverflow), err)
}

func TestBuildRecoveryPointIndexInvalidRollDistance(t *testing.T) {
	r, stbl := buildStblFixture(t, stblFixtureConfig{
		rollSgpd: &Sgpd{
			FullBox:       FullBox{Version: 1},
			GroupingType:  groupingTypeRoll,
			DefaultLength: 2,
			EntryCount:    1,
			RollDistances: []int16{1},
		},
		rollSbgp: &Sbgp{
			FullBox:      FullBox{Version: 0},
			GroupingType: 0x726F6C6C,
			EntryCount:   1,
			Entries: []SbgpEntry{
				{SampleCount: 1, GroupDescriptionIndex: 1},
			},
		},
	})

	_, err := BuildRecoveryPointIndex(r, stbl)
	assert.True(t, errors.Is(err, ErrInvalidRollDistance), err)
}

func TestBuildRecoveryPointIndexProlAllowsPositiveDistance(t *testing.T) {
	// Positive-roll (prol) descriptions are allowed to carry a positive
	// distance, unlike roll descriptions.
	r, stbl := buildStblFixture(t, stblFixtureConfig{
		prolSgpd: &Sgpd{
			FullBox:       FullBox{Version: 1},
			GroupingType:  groupingTypeProl,
			DefaultLength: 2,
			EntryCount:    1,
			RollDistances: []int16{1},
		},
		prolSbgp: &Sbgp{
			FullBox:      FullBox{Version: 0},
			GroupingType: 0x70726F6C,
			EntryCount:   1,
			Entries: []SbgpEntry{
				{SampleCount: 1, GroupDescriptionIndex: 1},
			},
		},
	})

	idx, err := BuildRecoveryPointIndex(r, stbl)
	require.NoError(t, err)

	rp, err := idx.GetRecoveryPoint(0)
	require.NoError(t, err)
	assert.Equal(t, int16(1), rp.PrerollDistance)
	// A positive prol distance does not move the decode start backwards.
	assert.Equal(t, 0, rp.DecodeStartIndex)
	assert.False(t, rp.Degraded)
}

func TestBuildRecoveryPointIndexCorruptedBox(t *testing.T) {
	// Build a valid stbl with a roll sgpd box, then truncate the sgpd payload
	// while keeping its declared box size, simulating a corrupted box.
	ws := &writerseeker.WriterSeeker{}
	w := NewWriter(ws)
	_, err := w.StartBox(&BoxInfo{Type: BoxTypeStbl()})
	require.NoError(t, err)
	writeBox(t, w, &Stsz{
		FullBox:     FullBox{Version: 0},
		SampleSize:  10,
		SampleCount: 8,
	})
	// locate the sgpd box start and write a valid roll sgpd
	sgpdStart, err := w.Seek(0, io.SeekCurrent)
	require.NoError(t, err)
	_, err = w.StartBox(&BoxInfo{Type: BoxTypeSgpd()})
	require.NoError(t, err)
	_, err = Marshal(w, &Sgpd{
		FullBox:       FullBox{Version: 1},
		GroupingType:  groupingTypeRoll,
		DefaultLength: 2,
		EntryCount:    2,
		RollDistances: []int16{-2, -1},
	}, Context{})
	require.NoError(t, err)
	sgpdInfo, err := w.EndBox()
	require.NoError(t, err)
	_, err = w.EndBox()
	require.NoError(t, err)

	// remove the second roll distance from the sgpd payload
	data := ws.BytesReader()
	raw, err := io.ReadAll(data)
	require.NoError(t, err)
	cutAt := sgpdStart + int64(sgpdInfo.Size) - 2
	raw = append(raw[:cutAt], raw[cutAt+2:]...)

	r := bytes.NewReader(raw)
	bis, err := ExtractBox(r, nil, BoxPath{BoxTypeStbl()})
	require.NoError(t, err)
	require.Len(t, bis, 1)

	_, err = BuildRecoveryPointIndex(bytes.NewReader(raw), bis[0])
	assert.Error(t, err)
	assert.False(t, errors.Is(err, ErrNoRecoveryPoint))
}

func TestBuildRecoveryPointIndexNilStbl(t *testing.T) {
	_, err := BuildRecoveryPointIndex(bytes.NewReader(nil), nil)
	assert.Error(t, err)
}

// buildTrafFixture writes a traf box with tfhd/trun/sbgp boxes and returns a
// reader over its serialized bytes, the parsed traf BoxInfo and the
// movie-level sgpd boxes.
func buildTrafFixture(t *testing.T, trackID uint32, trun *Trun, sbgpBoxes ...*Sbgp) (*bytes.Reader, *BoxInfo) {
	t.Helper()

	ws := &writerseeker.WriterSeeker{}
	w := NewWriter(ws)
	_, err := w.StartBox(&BoxInfo{Type: BoxTypeTraf()})
	require.NoError(t, err)

	writeBox(t, w, &Tfhd{
		FullBox: FullBox{
			Version: 0,
			Flags:   [3]byte{0x00, 0x00, 0x20}, // default-sample-flags present
		},
		TrackID:            trackID,
		DefaultSampleFlags: 0x00000000, // sync by default
	})
	writeBox(t, w, trun)
	for _, box := range sbgpBoxes {
		writeBox(t, w, box)
	}
	_, err = w.EndBox()
	require.NoError(t, err)

	raw, err := io.ReadAll(ws.BytesReader())
	require.NoError(t, err)

	bis, err := ExtractBox(bytes.NewReader(raw), nil, BoxPath{BoxTypeTraf()})
	require.NoError(t, err)
	require.Len(t, bis, 1)
	return bytes.NewReader(raw), bis[0]
}

func TestBuildFragmentRecoveryPointIndex(t *testing.T) {
	// 4 fragment samples; sample 2 is marked non-sync via its trun flags.
	trun := &Trun{
		FullBox:     FullBox{Version: 0, Flags: [3]byte{0x00, 0x04, 0x00}}, // sample-flags present
		SampleCount: 4,
		Entries: []TrunEntry{
			{SampleFlags: 0x00000000}, // sync
			{SampleFlags: 0x00000000}, // sync
			{SampleFlags: 0x00008000}, // sample_depends_on = 2 -> non-sync
			{SampleFlags: 0x00000000}, // sync
		},
	}
	sbgp := &Sbgp{
		FullBox:      FullBox{Version: 0},
		GroupingType: 0x726F6C6C,
		EntryCount:   1,
		Entries: []SbgpEntry{
			{SampleCount: 1, GroupDescriptionIndex: 1},
		},
	}

	r, traf := buildTrafFixture(t, 7, trun, sbgp)

	idx, err := BuildFragmentRecoveryPointIndex(r, traf, []*Sgpd{defaultRollSgpd()})
	require.NoError(t, err)
	assert.Equal(t, uint32(7), idx.TrackID)
	assert.Equal(t, 4, idx.SampleCount)

	// sample 0: roll distance -2, clamped to the fragment start -> degraded.
	rp, err := idx.GetRecoveryPoint(0)
	require.NoError(t, err)
	assert.Equal(t, int16(-2), rp.RollDistance)
	assert.Equal(t, 0, rp.DecodeStartIndex)
	assert.True(t, rp.Degraded)

	// sample 1: ordinary sync sample.
	rp, err = idx.GetRecoveryPoint(1)
	require.NoError(t, err)
	assert.Equal(t, 1, rp.DecodeStartIndex)
	assert.False(t, rp.Degraded)

	// sample 2: non-sync and ungrouped -> no recovery point.
	_, err = idx.GetRecoveryPoint(2)
	assert.True(t, errors.Is(err, ErrNoRecoveryPoint), err)

	// sample 3: ordinary sync sample.
	rp, err = idx.GetRecoveryPoint(3)
	require.NoError(t, err)
	assert.Equal(t, 3, rp.DecodeStartIndex)
}

func TestBuildFragmentRecoveryPointIndexTrackIsolation(t *testing.T) {
	trunA := &Trun{
		FullBox:     FullBox{Version: 0, Flags: [3]byte{0x00, 0x04, 0x00}},
		SampleCount: 2,
		Entries: []TrunEntry{
			{SampleFlags: 0x00000000},
			{SampleFlags: 0x00000000},
		},
	}
	sbgpA := &Sbgp{
		FullBox:      FullBox{Version: 0},
		GroupingType: 0x726F6C6C,
		EntryCount:   1,
		Entries:      []SbgpEntry{{SampleCount: 1, GroupDescriptionIndex: 1}},
	}
	rA, trafA := buildTrafFixture(t, 1, trunA, sbgpA)

	trunB := &Trun{
		FullBox:     FullBox{Version: 0, Flags: [3]byte{0x00, 0x04, 0x00}},
		SampleCount: 1,
		Entries:     []TrunEntry{{SampleFlags: 0x00000000}},
	}
	rB, trafB := buildTrafFixture(t, 2, trunB)

	idxA, err := BuildFragmentRecoveryPointIndex(rA, trafA, []*Sgpd{defaultRollSgpd()})
	require.NoError(t, err)
	idxB, err := BuildFragmentRecoveryPointIndex(rB, trafB, nil)
	require.NoError(t, err)

	assert.Equal(t, uint32(1), idxA.TrackID)
	assert.Equal(t, uint32(2), idxB.TrackID)
	assert.Equal(t, 2, idxA.SampleCount)
	assert.Equal(t, 1, idxB.SampleCount)

	rp, err := idxA.GetRecoveryPoint(0)
	require.NoError(t, err)
	assert.Equal(t, int16(-2), rp.RollDistance)

	// Track B has no sample groups: its sample is an ordinary sync sample.
	rp, err = idxB.GetRecoveryPoint(0)
	require.NoError(t, err)
	assert.Equal(t, int16(0), rp.RollDistance)
	assert.Equal(t, 0, rp.DecodeStartIndex)
}

func TestBuildFragmentRecoveryPointIndexMissingDescription(t *testing.T) {
	trun := &Trun{
		FullBox:     FullBox{Version: 0},
		SampleCount: 1,
		Entries:     []TrunEntry{{}},
	}
	sbgp := &Sbgp{
		FullBox:      FullBox{Version: 0},
		GroupingType: 0x726F6C6C,
		EntryCount:   1,
		Entries:      []SbgpEntry{{SampleCount: 1, GroupDescriptionIndex: 1}},
	}
	r, traf := buildTrafFixture(t, 1, trun, sbgp)

	_, err := BuildFragmentRecoveryPointIndex(r, traf, nil)
	assert.True(t, errors.Is(err, ErrMissingGroupDescription), err)
}

func TestBuildFragmentRecoveryPointIndexNilTraf(t *testing.T) {
	_, err := BuildFragmentRecoveryPointIndex(bytes.NewReader(nil), nil, nil)
	assert.Error(t, err)
}

// TestBuildRecoveryPointIndexSampleMp4 exercises the API against the fixed
// sample.mp4 fixture, whose audio track carries a roll sgpd/sbgp pair.
func TestBuildRecoveryPointIndexSampleMp4(t *testing.T) {
	// sample.mp4: audio track (track 2) contains
	// sgpd roll DefaultLength=2 RollDistances=[-1] and
	// sbgp roll EntryCount=1 SampleCount=44 GroupDescriptionIndex=1.
	// The audio sample table has 44 samples and no stss box.
	f, err := os.Open("testdata/sample.mp4")
	require.NoError(t, err)
	defer f.Close()

	stbls, err := ExtractBox(f, nil, BoxPath{
		BoxTypeMoov(), BoxTypeTrak(), BoxTypeMdia(),
		BoxTypeMinf(), BoxTypeStbl(),
	})
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(stbls), 2)

	var found bool
	for _, stbl := range stbls {
		idx, err := BuildRecoveryPointIndex(f, stbl)
		require.NoError(t, err)
		if idx.SampleCount != 44 {
			continue
		}
		found = true

		// All 44 samples belong to the roll group with distance -1.
		rp, err := idx.GetRecoveryPoint(0)
		require.NoError(t, err)
		assert.Equal(t, int16(-1), rp.RollDistance)
		assert.True(t, rp.Degraded) // distance -1 at the track start
		assert.Equal(t, 0, rp.DecodeStartIndex)

		rp, err = idx.GetRecoveryPoint(10)
		require.NoError(t, err)
		assert.Equal(t, int16(-1), rp.RollDistance)
		assert.Equal(t, 9, rp.DecodeStartIndex)
		assert.Equal(t, 9, rp.PrerollStartIndex)
		assert.Equal(t, 10, rp.PrerollEndIndex)
		assert.Equal(t, 10, rp.DisplayStartIndex)
		assert.False(t, rp.Degraded)
	}
	assert.True(t, found, "audio sample table with 44 samples not found")
}
