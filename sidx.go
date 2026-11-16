package mp4

import (
	"errors"
	"fmt"
	"io"
	"math"
)

// Errors returned by GenerateSidx.
var (
	// ErrSidxNotSeekable is returned when the input does not implement io.ReadSeeker.
	ErrSidxNotSeekable = errors.New("mp4: sidx generation requires a seekable input")
	// ErrSidxNoFragments is returned when no moof/mdat fragment is found.
	ErrSidxNoFragments = errors.New("mp4: no moof/mdat fragments found")
	// ErrSidxMissingMoof is returned when an mdat box has no preceding moof box.
	ErrSidxMissingMoof = errors.New("mp4: mdat box without a preceding moof box")
	// ErrSidxMissingMdat is returned when a moof box is not followed by an mdat box.
	ErrSidxMissingMdat = errors.New("mp4: moof box without a following mdat box")
	// ErrSidxNoTimescale is returned when the media timescale can be determined
	// neither from SidxOptions.Timescale nor from the moov box.
	ErrSidxNoTimescale = errors.New("mp4: media timescale not found")
	// ErrSidxInconsistentTimescale is returned when the media timescale is ambiguous
	// or contradicts the timescale declared by the moov box.
	ErrSidxInconsistentTimescale = errors.New("mp4: inconsistent media timescale")
	// ErrSidxOverlappingFragments is returned when fragments overlap in the byte
	// stream or on the media timeline.
	ErrSidxOverlappingFragments = errors.New("mp4: overlapping fragments")
	// ErrSidxOverflow is returned when a value does not fit into the corresponding
	// sidx field.
	ErrSidxOverflow = errors.New("mp4: sidx field overflow")
	// ErrSidxInvalidOffset is returned when the sidx box would be placed after the
	// first fragment, which cannot be expressed by first_offset.
	ErrSidxInvalidOffset = errors.New("mp4: sidx box cannot be placed after the first fragment")
)

// SidxOptions configures GenerateSidx.
type SidxOptions struct {
	// Timescale is the media timescale used for the reference durations.
	// If zero, the timescale is taken from the mdhd box(es) of the moov box,
	// which must then be present and unambiguous.
	Timescale uint32

	// ReferenceID is the reference_ID stored in the sidx box.
	// If zero, the track ID of the first fragment's tfhd box is used.
	ReferenceID uint32

	// Offset is the byte position at which the generated sidx box will be
	// written in the final file. first_offset is computed relative to the end
	// of the sidx box at this position, so inserting the sidx box (and any
	// boxes preceding the fragments) does not invalidate the references.
	// If zero, the sidx box is assumed to be placed immediately before the
	// first moof box.
	Offset uint64
}

// sidxFragment describes one moof/mdat pair of a fragmented MP4 stream.
type sidxFragment struct {
	moof       *BoxInfo
	mdat       *BoxInfo
	decodeTime uint64 // base media decode time in the media timescale
	duration   uint64 // sum of sample durations in the media timescale
}

// GenerateSidx scans r for consecutive moof/mdat fragments and builds a sidx
// (segment index) box referencing them. The input is only read; the media
// payload bytes are never modified.
//
// Each sidx reference covers one moof/mdat pair. Reference durations and the
// earliest presentation time are expressed in the media timescale (see
// SidxOptions.Timescale) and are derived from the tfdt/tfhd/trun boxes of the
// first traf of each moof. first_offset points from the end of the sidx box
// to the first moof box; use SidxOptions.Offset to declare where the sidx box
// will be written so that boxes prepended or appended around the fragments
// are accounted for. Version 1 of the sidx box is selected automatically when
// the earliest presentation time or first_offset exceeds 32 bits.
func GenerateSidx(r io.Reader, opts *SidxOptions) (*Sidx, error) {
	if opts == nil {
		opts = &SidxOptions{}
	}
	rs, ok := r.(io.ReadSeeker)
	if !ok {
		return nil, ErrSidxNotSeekable
	}

	frags, trackID, err := scanSidxFragments(rs)
	if err != nil {
		return nil, err
	}

	timescale, err := resolveSidxTimescale(rs, opts.Timescale)
	if err != nil {
		return nil, err
	}

	// The sidx box must not be placed after the first fragment: first_offset
	// is unsigned and relative to the end of the sidx box.
	placement := opts.Offset
	if placement == 0 {
		placement = frags[0].moof.Offset
	}
	if placement > frags[0].moof.Offset {
		return nil, fmt.Errorf("%w: offset %d is behind the first moof box at %d",
			ErrSidxInvalidOffset, placement, frags[0].moof.Offset)
	}
	// Boxes between the placement position and the first moof (e.g. ftyp or
	// styp) are covered by first_offset; boxes appended after the fragments
	// do not affect the references.
	firstOffset := frags[0].moof.Offset - placement

	ept := frags[0].decodeTime
	version := uint8(0)
	if ept > math.MaxUint32 || firstOffset > math.MaxUint32 {
		version = 1
	}

	if len(frags) > math.MaxUint16 {
		return nil, fmt.Errorf("%w: %d fragments exceed the 16-bit reference_count field",
			ErrSidxOverflow, len(frags))
	}

	refID := opts.ReferenceID
	if refID == 0 {
		refID = trackID
	}

	sidx := &Sidx{
		FullBox:        FullBox{Version: version},
		Timescale:      timescale,
		ReferenceCount: uint16(len(frags)),
		References:     make([]SidxReference, len(frags)),
	}
	if version == 0 {
		sidx.EarliestPresentationTimeV0 = uint32(ept)
		sidx.FirstOffsetV0 = uint32(firstOffset)
	} else {
		sidx.EarliestPresentationTimeV1 = ept
		sidx.FirstOffsetV1 = firstOffset
	}

	for i, frag := range frags {
		size := frag.moof.Size + frag.mdat.Size
		if size > math.MaxInt32 {
			return nil, fmt.Errorf("%w: referenced size of fragment %d (%d bytes) exceeds the 31-bit referenced_size field",
				ErrSidxOverflow, i, size)
		}
		if frag.duration > math.MaxUint32 {
			return nil, fmt.Errorf("%w: duration of fragment %d (%d) exceeds the 32-bit subsegment_duration field",
				ErrSidxOverflow, i, frag.duration)
		}
		sidx.References[i] = SidxReference{
			ReferencedSize:     uint32(size),
			SubsegmentDuration: uint32(frag.duration),
		}
	}

	sidx.ReferenceID = refID

	return sidx, nil
}

// scanSidxFragments pairs top-level moof and mdat boxes in file order and
// extracts the decode time and duration of each fragment. It also returns the
// track ID shared by all fragments.
func scanSidxFragments(r io.ReadSeeker) ([]*sidxFragment, uint32, error) {
	bis, err := ExtractBoxes(r, nil, []BoxPath{
		{BoxTypeMoof()},
		{BoxTypeMdat()},
	})
	if err != nil {
		return nil, 0, err
	}

	frags := make([]*sidxFragment, 0, 8)
	var pendingMoof *BoxInfo
	for _, bi := range bis {
		switch bi.Type {
		case BoxTypeMoof():
			if pendingMoof != nil {
				return nil, 0, fmt.Errorf("%w at offset %d", ErrSidxMissingMdat, pendingMoof.Offset)
			}
			pendingMoof = bi
		case BoxTypeMdat():
			if pendingMoof == nil {
				return nil, 0, fmt.Errorf("%w at offset %d", ErrSidxMissingMoof, bi.Offset)
			}
			frags = append(frags, &sidxFragment{moof: pendingMoof, mdat: bi})
			pendingMoof = nil
		}
	}
	if pendingMoof != nil {
		return nil, 0, fmt.Errorf("%w at offset %d", ErrSidxMissingMdat, pendingMoof.Offset)
	}
	if len(frags) == 0 {
		return nil, 0, ErrSidxNoFragments
	}

	var trackID uint32
	for i, frag := range frags {
		if i > 0 {
			if prevEnd := frags[i-1].mdat.Offset + frags[i-1].mdat.Size; frag.moof.Offset < prevEnd {
				return nil, 0, fmt.Errorf("%w: fragment %d starts at %d but the previous fragment ends at %d",
					ErrSidxOverlappingFragments, i, frag.moof.Offset, prevEnd)
			}
		}

		id, decodeTime, duration, err := readSidxFragmentTiming(r, frag.moof)
		if err != nil {
			return nil, 0, err
		}
		if i == 0 {
			trackID = id
		} else if id != trackID {
			return nil, 0, fmt.Errorf("mp4: inconsistent track ID: fragment 0 has %d but fragment %d has %d",
				trackID, i, id)
		}
		frag.decodeTime = decodeTime
		frag.duration = duration

		if i > 0 {
			if prevEnd := frags[i-1].decodeTime + frags[i-1].duration; frag.decodeTime < prevEnd {
				return nil, 0, fmt.Errorf("%w: decode time of fragment %d (%d) is before the end of the previous fragment (%d)",
					ErrSidxOverlappingFragments, i, frag.decodeTime, prevEnd)
			}
		}
	}
	return frags, trackID, nil
}

// readSidxFragmentTiming reads the tfhd/tfdt/trun boxes of the first traf of
// the given moof box and returns the track ID, the base media decode time and
// the fragment duration in the media timescale.
func readSidxFragmentTiming(r io.ReadSeeker, moof *BoxInfo) (trackID uint32, decodeTime, duration uint64, err error) {
	trafs, err := ExtractBox(r, moof, BoxPath{BoxTypeTraf()})
	if err != nil {
		return 0, 0, 0, err
	}
	if len(trafs) == 0 {
		return 0, 0, 0, fmt.Errorf("mp4: moof box at offset %d contains no traf box", moof.Offset)
	}

	bips, err := ExtractBoxesWithPayload(r, trafs[0], []BoxPath{
		{BoxTypeTfhd()},
		{BoxTypeTfdt()},
		{BoxTypeTrun()},
	})
	if err != nil {
		return 0, 0, 0, err
	}

	var tfhd *Tfhd
	var tfdt *Tfdt
	var truns []*Trun
	for _, bip := range bips {
		switch payload := bip.Payload.(type) {
		case *Tfhd:
			tfhd = payload
		case *Tfdt:
			tfdt = payload
		case *Trun:
			truns = append(truns, payload)
		}
	}
	if tfhd == nil {
		return 0, 0, 0, fmt.Errorf("mp4: tfhd box not found in moof box at offset %d", moof.Offset)
	}
	trackID = tfhd.TrackID

	if tfdt != nil {
		decodeTime = tfdt.GetBaseMediaDecodeTime()
	}

	for _, trun := range truns {
		if trun.CheckFlag(0x000100) { // sample-duration-present
			for ei := range trun.Entries {
				duration += uint64(trun.Entries[ei].SampleDuration)
			}
		} else {
			duration += uint64(tfhd.DefaultSampleDuration) * uint64(trun.SampleCount)
		}
	}
	return trackID, decodeTime, duration, nil
}

// resolveSidxTimescale determines the media timescale from opts and, when
// present, the mdhd boxes of the moov box.
func resolveSidxTimescale(r io.ReadSeeker, optTimescale uint32) (uint32, error) {
	bips, err := ExtractBoxesWithPayload(r, nil, []BoxPath{
		{BoxTypeMoov(), BoxTypeTrak(), BoxTypeMdia(), BoxTypeMdhd()},
	})
	if err != nil {
		return 0, err
	}

	timescales := make([]uint32, 0, len(bips))
	for _, bip := range bips {
		timescales = append(timescales, bip.Payload.(*Mdhd).Timescale)
	}

	if optTimescale != 0 {
		for _, ts := range timescales {
			if ts == optTimescale {
				return optTimescale, nil
			}
		}
		if len(timescales) > 0 {
			return 0, fmt.Errorf("%w: timescale %d matches no mdhd box (%v)",
				ErrSidxInconsistentTimescale, optTimescale, timescales)
		}
		return optTimescale, nil
	}

	if len(timescales) == 0 {
		return 0, ErrSidxNoTimescale
	}
	for _, ts := range timescales[1:] {
		if ts != timescales[0] {
			return 0, fmt.Errorf("%w: mdhd boxes declare %v; specify SidxOptions.Timescale",
				ErrSidxInconsistentTimescale, timescales)
		}
	}
	return timescales[0], nil
}
