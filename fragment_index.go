package mp4

import (
	"errors"
	"fmt"
	"io"
	"math"
)

// Errors returned by BuildFragmentIndex. Callers can use errors.Is to test them.
var (
	// ErrMoovBoxNotFound means that the file has no moov box.
	ErrMoovBoxNotFound = errors.New("moov box not found")

	// ErrTrexNotFound means that a fragmented track has no matching trex box in mvex.
	ErrTrexNotFound = errors.New("trex box not found")

	// ErrDuplicateTrackID means that the same track ID is declared more than once.
	ErrDuplicateTrackID = errors.New("duplicate track ID")

	// ErrTfhdNotFound means that a traf box does not contain a tfhd box.
	ErrTfhdNotFound = errors.New("tfhd box not found")

	// ErrUnknownTrackID means that a fragment references a track which is not declared in moov.
	ErrUnknownTrackID = errors.New("unknown track ID")

	// ErrInvalidDataOffset means that a trun data offset resolves to a negative
	// or otherwise invalid file position.
	ErrInvalidDataOffset = errors.New("invalid data offset")

	// ErrSampleOutOfRange means that a sample extends beyond the end of the file.
	ErrSampleOutOfRange = errors.New("sample out of file range")

	// ErrTimeOverflow means that DTS/PTS arithmetic overflows the representable range.
	ErrTimeOverflow = errors.New("time overflow")

	// ErrDecodeTimeDiscontinuity means that a fragment's base media decode time is
	// earlier than the end of the previous fragment of the same track.
	ErrDecodeTimeDiscontinuity = errors.New("decode time discontinuity between fragments")
)

// FragmentSample describes a single sample inside a fragmented movie (fMP4).
type FragmentSample struct {
	// TrackID is the track identifier declared by the enclosing tfhd box.
	TrackID uint32

	// DataOffset is the absolute byte offset of the sample payload in the file.
	DataOffset uint64

	// Size is the sample payload length in bytes.
	Size uint32

	// DTS is the decode time in the track's media timescale.
	DTS uint64

	// PTS is the presentation time in the track's media timescale.
	// It equals DTS plus the (possibly negative) composition time offset.
	PTS int64

	// Duration is the sample duration in the track's media timescale.
	Duration uint32

	// CompositionTimeOffset is the signed composition time offset (PTS - DTS).
	CompositionTimeOffset int64

	// MoofOffset is the file offset of the enclosing moof box.
	MoofOffset uint64
}

// FragmentTrack groups all indexed samples of one track, in decode order.
type FragmentTrack struct {
	TrackID   uint32
	Timescale uint32
	Samples   []*FragmentSample
}

// FragmentIndex is the per-track sample index built from a fragmented MP4 file.
type FragmentIndex struct {
	Tracks []*FragmentTrack
}

// FindTrack returns the track with the given ID, or nil if it is not indexed.
func (idx *FragmentIndex) FindTrack(trackID uint32) *FragmentTrack {
	for _, track := range idx.Tracks {
		if track.TrackID == trackID {
			return track
		}
	}
	return nil
}

type fragTrackState struct {
	track      *FragmentTrack
	trex       *Trex
	lastEndDTS uint64
	hasLastDTS bool
}

// BuildFragmentIndex builds a per-track sample index for a fragmented MP4 file.
//
// Sample file offsets, sizes, DTS and PTS are reconstructed from the trex
// defaults in moov/mvex and the moof/traf/tfhd/tfdt/trun boxes, including
// default value inheritance, multiple traf/trun boxes, signed composition
// time offsets and inter-fragment decode time continuity.
func BuildFragmentIndex(r io.ReadSeeker) (*FragmentIndex, error) {
	fileSize, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}

	topBoxes, err := ExtractBoxes(r, nil, []BoxPath{
		{BoxTypeMoov()},
		{BoxTypeMoof()},
	})
	if err != nil {
		return nil, err
	}

	var moovBI *BoxInfo
	moofBIs := make([]*BoxInfo, 0, 8)
	for _, bi := range topBoxes {
		switch bi.Type {
		case BoxTypeMoov():
			if moovBI == nil {
				moovBI = bi
			}
		case BoxTypeMoof():
			moofBIs = append(moofBIs, bi)
		}
	}
	if moovBI == nil {
		return nil, ErrMoovBoxNotFound
	}

	states, order, err := buildFragmentTrackStates(r, moovBI)
	if err != nil {
		return nil, err
	}

	for _, moofBI := range moofBIs {
		if err := indexMoof(r, moofBI, states, uint64(fileSize)); err != nil {
			return nil, err
		}
	}

	index := &FragmentIndex{Tracks: make([]*FragmentTrack, 0, len(order))}
	for _, trackID := range order {
		index.Tracks = append(index.Tracks, states[trackID].track)
	}
	return index, nil
}

func buildFragmentTrackStates(r io.ReadSeeker, moovBI *BoxInfo) (map[uint32]*fragTrackState, []uint32, error) {
	trakBIs, err := ExtractBox(r, moovBI, BoxPath{BoxTypeTrak()})
	if err != nil {
		return nil, nil, err
	}

	states := make(map[uint32]*fragTrackState, len(trakBIs))
	order := make([]uint32, 0, len(trakBIs))
	for _, trakBI := range trakBIs {
		bips, err := ExtractBoxesWithPayload(r, trakBI, []BoxPath{
			{BoxTypeTkhd()},
			{BoxTypeMdia(), BoxTypeMdhd()},
		})
		if err != nil {
			return nil, nil, err
		}

		var tkhd *Tkhd
		var mdhd *Mdhd
		for _, bip := range bips {
			switch bip.Info.Type {
			case BoxTypeTkhd():
				tkhd = bip.Payload.(*Tkhd)
			case BoxTypeMdhd():
				mdhd = bip.Payload.(*Mdhd)
			}
		}
		if tkhd == nil {
			return nil, nil, errors.New("tkhd box not found")
		}
		if mdhd == nil {
			return nil, nil, fmt.Errorf("mdhd box not found: trackID=%d", tkhd.TrackID)
		}
		if _, exists := states[tkhd.TrackID]; exists {
			return nil, nil, fmt.Errorf("%w: trackID=%d", ErrDuplicateTrackID, tkhd.TrackID)
		}
		states[tkhd.TrackID] = &fragTrackState{
			track: &FragmentTrack{
				TrackID:   tkhd.TrackID,
				Timescale: mdhd.Timescale,
				Samples:   make([]*FragmentSample, 0, 16),
			},
		}
		order = append(order, tkhd.TrackID)
	}

	trexBIs, err := ExtractBoxesWithPayload(r, moovBI, []BoxPath{
		{BoxTypeMvex(), BoxTypeTrex()},
	})
	if err != nil {
		return nil, nil, err
	}
	for _, bip := range trexBIs {
		trex := bip.Payload.(*Trex)
		state, ok := states[trex.TrackID]
		if !ok {
			return nil, nil, fmt.Errorf("%w: trex trackID=%d", ErrUnknownTrackID, trex.TrackID)
		}
		if state.trex != nil {
			return nil, nil, fmt.Errorf("%w: trackID=%d", ErrDuplicateTrackID, trex.TrackID)
		}
		state.trex = trex
	}
	for _, trackID := range order {
		if states[trackID].trex == nil {
			return nil, nil, fmt.Errorf("%w: trackID=%d", ErrTrexNotFound, trackID)
		}
	}

	return states, order, nil
}

func indexMoof(r io.ReadSeeker, moofBI *BoxInfo, states map[uint32]*fragTrackState, fileSize uint64) error {
	trafBIs, err := ExtractBox(r, moofBI, BoxPath{BoxTypeTraf()})
	if err != nil {
		return err
	}

	for _, trafBI := range trafBIs {
		if err := indexTraf(r, trafBI, moofBI, states, fileSize); err != nil {
			return err
		}
	}
	return nil
}

func indexTraf(r io.ReadSeeker, trafBI, moofBI *BoxInfo, states map[uint32]*fragTrackState, fileSize uint64) error {
	bips, err := ExtractBoxesWithPayload(r, trafBI, []BoxPath{
		{BoxTypeTfhd()},
		{BoxTypeTfdt()},
		{BoxTypeTrun()},
	})
	if err != nil {
		return err
	}

	var tfhd *Tfhd
	var tfdt *Tfdt
	truns := make([]*Trun, 0, 2)
	for _, bip := range bips {
		switch bip.Info.Type {
		case BoxTypeTfhd():
			if tfhd != nil {
				return errors.New("multiple tfhd boxes in one traf")
			}
			tfhd = bip.Payload.(*Tfhd)
		case BoxTypeTfdt():
			if tfdt != nil {
				return errors.New("multiple tfdt boxes in one traf")
			}
			tfdt = bip.Payload.(*Tfdt)
		case BoxTypeTrun():
			truns = append(truns, bip.Payload.(*Trun))
		}
	}
	if tfhd == nil {
		return ErrTfhdNotFound
	}

	state, ok := states[tfhd.TrackID]
	if !ok {
		return fmt.Errorf("%w: tfhd trackID=%d", ErrUnknownTrackID, tfhd.TrackID)
	}
	trex := state.trex

	// Resolve the base data offset (ISO/IEC 14496-12 8.8.3).
	var baseDataOffset int64
	if tfhd.CheckFlag(TfhdBaseDataOffsetPresent) {
		if tfhd.BaseDataOffset > math.MaxInt64 {
			return fmt.Errorf("%w: base data offset exceeds int64: %d", ErrInvalidDataOffset, tfhd.BaseDataOffset)
		}
		baseDataOffset = int64(tfhd.BaseDataOffset)
	} else {
		// Both the default-base-is-moof case and the plain default resolve to
		// the start of the enclosing moof box in practice and per spec.
		baseDataOffset = int64(moofBI.Offset)
	}

	// Determine the fragment base decode time and check continuity with the
	// previous fragment of the same track.
	var dts uint64
	if tfdt != nil {
		dts = tfdt.GetBaseMediaDecodeTime()
		if state.hasLastDTS && dts < state.lastEndDTS {
			return fmt.Errorf("%w: trackID=%d baseMediaDecodeTime=%d previousEnd=%d",
				ErrDecodeTimeDiscontinuity, tfhd.TrackID, dts, state.lastEndDTS)
		}
	} else if state.hasLastDTS {
		dts = state.lastEndDTS
	}

	// Byte cursor shared by all trun boxes of this traf.
	var cursor int64
	haveCursor := false
	for _, trun := range truns {
		trunFlags := trun.GetFlags()

		if trunFlags&0x000001 != 0 {
			cursor = baseDataOffset + int64(trun.DataOffset)
			haveCursor = true
		} else if !haveCursor {
			// No explicit data offset on the first trun: samples start at the
			// base data offset.
			cursor = baseDataOffset
			haveCursor = true
		}
		if cursor < 0 {
			return fmt.Errorf("%w: trackID=%d offset=%d", ErrInvalidDataOffset, tfhd.TrackID, cursor)
		}

		for i := uint32(0); i < trun.SampleCount; i++ {
			entry := &trun.Entries[i]
			var duration, size uint32
			var cto int64
			if trunFlags&0x000100 != 0 {
				duration = entry.SampleDuration
			} else if tfhd.CheckFlag(TfhdDefaultSampleDurationPresent) {
				duration = tfhd.DefaultSampleDuration
			} else {
				duration = trex.DefaultSampleDuration
			}
			if trunFlags&0x000200 != 0 {
				size = entry.SampleSize
			} else if tfhd.CheckFlag(TfhdDefaultSampleSizePresent) {
				size = tfhd.DefaultSampleSize
			} else {
				size = trex.DefaultSampleSize
			}
			if trunFlags&0x000800 != 0 {
				cto = trun.GetSampleCompositionTimeOffset(int(i))
			}

			// Bounds-check the sample payload against the file.
			if cursor > int64(fileSize) {
				return fmt.Errorf("%w: trackID=%d offset=%d fileSize=%d",
					ErrSampleOutOfRange, tfhd.TrackID, cursor, fileSize)
			}
			if cursor > int64(fileSize)-int64(size) {
				return fmt.Errorf("%w: trackID=%d offset=%d size=%d fileSize=%d",
					ErrSampleOutOfRange, tfhd.TrackID, cursor, size, fileSize)
			}

			// DTS / PTS with explicit overflow detection.
			if dts > math.MaxUint64-uint64(duration) {
				return fmt.Errorf("%w: trackID=%d dts=%d duration=%d",
					ErrTimeOverflow, tfhd.TrackID, dts, duration)
			}
			pts, err := addCompositionTime(tfhd.TrackID, dts, cto)
			if err != nil {
				return err
			}

			state.track.Samples = append(state.track.Samples, &FragmentSample{
				TrackID:               tfhd.TrackID,
				DataOffset:            uint64(cursor),
				Size:                  size,
				DTS:                   dts,
				PTS:                   pts,
				Duration:              duration,
				CompositionTimeOffset: cto,
				MoofOffset:            moofBI.Offset,
			})

			cursor += int64(size)
			dts += uint64(duration)
		}
	}

	state.lastEndDTS = dts
	state.hasLastDTS = true
	return nil
}

func addCompositionTime(trackID uint32, dts uint64, cto int64) (int64, error) {
	if cto >= 0 {
		if dts > math.MaxInt64-uint64(cto) {
			return 0, fmt.Errorf("%w: trackID=%d dts=%d cto=%d", ErrTimeOverflow, trackID, dts, cto)
		}
		return int64(dts) + cto, nil
	}
	abs := uint64(-cto)
	if dts < abs {
		return 0, fmt.Errorf("%w: trackID=%d dts=%d cto=%d", ErrTimeOverflow, trackID, dts, cto)
	}
	return int64(dts - abs), nil
}

// ReadFragmentSampleData reads and returns the payload bytes of one sample.
func ReadFragmentSampleData(r io.ReadSeeker, sample *FragmentSample) ([]byte, error) {
	if _, err := r.Seek(int64(sample.DataOffset), io.SeekStart); err != nil {
		return nil, err
	}
	buf := make([]byte, sample.Size)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}
