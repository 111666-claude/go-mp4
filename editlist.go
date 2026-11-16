package mp4

import (
	"errors"
	"math"
	"math/bits"
)

// Errors returned by NewEditListTimeline.
var (
	// ErrEditListZeroTimescale is returned when the movie or media timescale is zero.
	ErrEditListZeroTimescale = errors.New("go-mp4: edit list timeline requires non-zero movie and media timescales")
	// ErrEditListUnsupportedVersion is returned when the elst box version is neither 0 nor 1.
	ErrEditListUnsupportedVersion = errors.New("go-mp4: unsupported elst box version")
	// ErrEditListUnsupportedRate is returned when a normal edit has a media rate other than 1.0.
	ErrEditListUnsupportedRate = errors.New("go-mp4: edit list entry has a non 1x media rate")
	// ErrEditListInvalidEntry is returned when an edit list entry is corrupt
	// (e.g. a media time smaller than -1, or a zero-duration normal edit).
	ErrEditListInvalidEntry = errors.New("go-mp4: edit list entry is invalid")
	// ErrEditListOverflow is returned when a time computation overflows 64 bits.
	ErrEditListOverflow = errors.New("go-mp4: edit list time computation overflow")
)

// Errors returned by EditListTimeline queries.
var (
	// ErrPlaybackTimeOutOfRange is returned when the playback time is not
	// covered by any edit of the timeline.
	ErrPlaybackTimeOutOfRange = errors.New("go-mp4: playback time is outside the edit list timeline")
	// ErrPlaybackTimeInEmptyEdit is returned when the playback time falls into
	// an empty edit (media time -1), which presents no media.
	ErrPlaybackTimeInEmptyEdit = errors.New("go-mp4: playback time falls into an empty edit")
	// ErrMediaTimeNotPresented is returned when no normal edit presents the
	// given media time.
	ErrMediaTimeNotPresented = errors.New("go-mp4: media time is not presented by the edit list timeline")
)

// EditListTimeline maps track playback time (in movie timescale units) to
// media time (in media timescale units) and back, based on the track's edit
// list (moov/trak/edts/elst). It is read-only: it never modifies the file.
//
// Interval convention: each edit covers the half-open interval
// [start, end) in movie timescale, where start is the sum of the durations of
// all preceding edits. A playback time exactly equal to the total duration of
// the edit list is therefore out of range. A normal edit starting at media
// time M with segment duration D presents the half-open media time interval
// [M, M + D*mediaTimescale/movieTimescale) (the product is rounded down).
type EditListTimeline struct {
	movieTimescale uint32
	mediaTimescale uint32
	edits          []timelineEdit
}

type timelineEdit struct {
	start     uint64 // inclusive, in movie timescale
	end       uint64 // exclusive, in movie timescale
	mediaTime int64  // media time at start; -1 marks an empty edit
}

// NewEditListTimeline builds an EditListTimeline from a track's elst box and
// the movie (mvhd) and media (mdhd) timescales. A nil elst box or an elst box
// without entries yields the identity timeline: playback time and media time
// coincide (up to the timescale conversion) for every playback time >= 0.
//
// Only elst version 0 and 1 are supported. Normal edits must have a media
// rate of exactly 1.0; any other rate is rejected with
// ErrEditListUnsupportedRate. Corrupt entries (media time < -1, zero-duration
// normal edits) are rejected with ErrEditListInvalidEntry.
func NewEditListTimeline(elst *Elst, movieTimescale, mediaTimescale uint32) (*EditListTimeline, error) {
	if movieTimescale == 0 || mediaTimescale == 0 {
		return nil, ErrEditListZeroTimescale
	}
	tl := &EditListTimeline{
		movieTimescale: movieTimescale,
		mediaTimescale: mediaTimescale,
	}
	if elst == nil || len(elst.Entries) == 0 {
		tl.edits = []timelineEdit{{start: 0, end: math.MaxUint64, mediaTime: 0}}
		return tl, nil
	}
	if v := elst.GetVersion(); v != 0 && v != 1 {
		return nil, ErrEditListUnsupportedVersion
	}
	tl.edits = make([]timelineEdit, 0, len(elst.Entries))
	var start uint64
	for i := range elst.Entries {
		duration := elst.GetSegmentDuration(i)
		mediaTime := elst.GetMediaTime(i)
		if mediaTime < -1 {
			return nil, ErrEditListInvalidEntry
		}
		if mediaTime == -1 {
			// Empty edit: no media is presented, the media rate is
			// meaningless (commonly 0) and therefore not checked.
			if duration == 0 {
				continue
			}
		} else {
			entry := &elst.Entries[i]
			if entry.MediaRateInteger != 1 || entry.MediaRateFraction != 0 {
				return nil, ErrEditListUnsupportedRate
			}
			if duration == 0 {
				return nil, ErrEditListInvalidEntry
			}
		}
		if math.MaxUint64-start < duration {
			return nil, ErrEditListOverflow
		}
		tl.edits = append(tl.edits, timelineEdit{
			start:     start,
			end:       start + duration,
			mediaTime: mediaTime,
		})
		start += duration
	}
	return tl, nil
}

// MovieTimescale returns the movie (mvhd) timescale of the timeline.
func (tl *EditListTimeline) MovieTimescale() uint32 {
	return tl.movieTimescale
}

// MediaTimescale returns the media (mdhd) timescale of the timeline.
func (tl *EditListTimeline) MediaTimescale() uint32 {
	return tl.mediaTimescale
}

// MediaTimeAt returns the media time (in media timescale units) presented at
// the given playback time (in movie timescale units).
//
// It returns ErrPlaybackTimeOutOfRange if the playback time is not covered by
// any edit, and ErrPlaybackTimeInEmptyEdit if it falls into an empty edit.
func (tl *EditListTimeline) MediaTimeAt(playbackTime uint64) (int64, error) {
	for i := range tl.edits {
		e := &tl.edits[i]
		if playbackTime < e.start || playbackTime >= e.end {
			continue
		}
		if e.mediaTime == -1 {
			return 0, ErrPlaybackTimeInEmptyEdit
		}
		offset, err := mulDivTimescale(playbackTime-e.start, uint64(tl.mediaTimescale), uint64(tl.movieTimescale))
		if err != nil {
			return 0, err
		}
		if offset > uint64(math.MaxInt64-e.mediaTime) {
			return 0, ErrEditListOverflow
		}
		return e.mediaTime + int64(offset), nil
	}
	return 0, ErrPlaybackTimeOutOfRange
}

// PlaybackTimesAt returns every playback time (in movie timescale units) at
// which the given media time (in media timescale units) is presented. The
// result contains one entry per matching normal edit, in edit list order,
// because the same media time may be presented by several edits. It returns
// ErrMediaTimeNotPresented if no normal edit covers the media time.
func (tl *EditListTimeline) PlaybackTimesAt(mediaTime int64) ([]uint64, error) {
	if mediaTime < 0 {
		return nil, ErrMediaTimeNotPresented
	}
	var playbackTimes []uint64
	for i := range tl.edits {
		e := &tl.edits[i]
		if e.mediaTime == -1 || mediaTime < e.mediaTime {
			continue
		}
		// the media time span presented by this edit; saturate on overflow,
		// which only makes the edit cover every later media time as well
		span, err := mulDivTimescale(e.end-e.start, uint64(tl.mediaTimescale), uint64(tl.movieTimescale))
		if err != nil {
			span = math.MaxUint64
		}
		if uint64(mediaTime-e.mediaTime) >= span {
			continue
		}
		offset, err := mulDivTimescale(uint64(mediaTime-e.mediaTime), uint64(tl.movieTimescale), uint64(tl.mediaTimescale))
		if err != nil {
			return nil, err
		}
		if math.MaxUint64-e.start < offset {
			return nil, ErrEditListOverflow
		}
		playbackTimes = append(playbackTimes, e.start+offset)
	}
	if playbackTimes == nil {
		return nil, ErrMediaTimeNotPresented
	}
	return playbackTimes, nil
}

// mulDivTimescale computes v*mul/div, rounding down, and reports an overflow
// if the exact result does not fit into 64 bits. div must not be zero.
func mulDivTimescale(v, mul, div uint64) (uint64, error) {
	hi, lo := bits.Mul64(v, mul)
	if hi >= div {
		return 0, ErrEditListOverflow
	}
	q, _ := bits.Div64(hi, lo, div)
	return q, nil
}
