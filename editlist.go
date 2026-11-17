package mp4

import (
	"errors"
	"math"
	"math/bits"
)

// Errors returned while building an EditListTimeline.
var (
	// ErrEditListInvalidTimescale is returned when the movie (mvhd) or the
	// media (mdhd) box is missing, or when either timescale is zero.
	ErrEditListInvalidTimescale = errors.New("edit list requires non-zero movie and media timescales")
	// ErrEditListUnsupportedVersion is returned when the elst box version is
	// neither 0 nor 1.
	ErrEditListUnsupportedVersion = errors.New("unsupported elst box version")
	// ErrEditListUnsupportedRate is returned when a normal edit has a media
	// rate other than 1.0. Empty edits ignore their media rate.
	ErrEditListUnsupportedRate = errors.New("edit list entry has a media rate other than 1.0")
	// ErrEditListInvalidEntry is returned when an edit list entry is corrupt:
	// a media time below -1, a zero-length edit, or a segment duration of 0.
	ErrEditListInvalidEntry = errors.New("invalid edit list entry")
	// ErrEditListOverflow is returned when a time calculation does not fit
	// into 64 bits.
	ErrEditListOverflow = errors.New("edit list time calculation overflow")
)

// Errors returned by EditListTimeline queries.
var (
	// ErrEditListPlaybackTimeOutOfRange is returned when a playback time is
	// not covered by any edit of the track.
	ErrEditListPlaybackTimeOutOfRange = errors.New("playback time is outside the edit list")
	// ErrEditListPlaybackTimeInEmptyEdit is returned when a playback time
	// falls into an empty edit, which presents no media.
	ErrEditListPlaybackTimeInEmptyEdit = errors.New("playback time falls into an empty edit")
)

// EditListTimeline maps a track's playback time to its media time using the
// track's edit list (moov/trak/edts/elst) together with the movie (mvhd) and
// media (mdhd) timescales. It is read-only.
//
// Playback (movie) time is expressed in movie timescale units, relative to
// the start of the track's presentation, and media time is expressed in media
// timescale units. tkhd track offsets and the movie header's own time base
// are not applied here: the first edit always starts at playback time 0.
//
// Every edit covers the half-open interval [MovieStart, MovieEnd) in movie
// time, so a playback time on a boundary belongs to the following edit and a
// playback time equal to the end of the edit list is out of range. A normal
// edit starting at media time M with segment duration D presents the
// half-open media interval
//
//	[M, M + floor(D * mediaTimescale / movieTimescale))
//
// Timescale conversions are rounded down. Different edits are allowed to
// present overlapping media ranges (for example to repeat a part of the
// media); because movie-time intervals do not overlap, every playback time
// still maps to exactly one edit.
type EditListTimeline struct {
	movieTimescale uint32
	mediaTimescale uint32
	segments       []EditListSegment
	identity       bool
}

// EditListSegment describes one edit list entry.
//
// MovieStart is inclusive and MovieEnd is exclusive, both in movie timescale
// units. MediaTime is the first media time presented by the segment and
// MediaEnd is the exclusive end of the presented media range, both in media
// timescale units. An empty edit (media_time = -1) presents no media, so
// Empty is true and both MediaTime and MediaEnd are -1.
type EditListSegment struct {
	MovieStart uint64
	MovieEnd   uint64
	MediaTime  int64
	MediaEnd   int64
	Empty      bool
}

// NewEditListTimeline builds an EditListTimeline for one track.
//
// elst may be nil: a track without an edit list (or with an empty one) uses
// the identity timeline in which media time is playback time converted with
// the two timescales. mvhd and mdhd must not be nil and their timescales must
// be non-zero, even for the identity timeline, because the conversion is not
// defined otherwise.
//
// Only elst versions 0 and 1 are supported. Normal edits must have a media
// rate of exactly 1.0; any other rate yields ErrEditListUnsupportedRate. The
// media rate of an empty edit is ignored, because it presents no media.
// Entries with a media time below -1 or a zero segment duration are rejected
// with ErrEditListInvalidEntry. Any time computation that does not fit into
// 64 bits yields ErrEditListOverflow.
func NewEditListTimeline(elst *Elst, mvhd *Mvhd, mdhd *Mdhd) (*EditListTimeline, error) {
	if mvhd == nil || mdhd == nil || mvhd.Timescale == 0 || mdhd.Timescale == 0 {
		return nil, ErrEditListInvalidTimescale
	}

	tl := &EditListTimeline{
		movieTimescale: mvhd.Timescale,
		mediaTimescale: mdhd.Timescale,
	}
	if elst == nil {
		tl.identity = true
		return tl, nil
	}
	if version := elst.GetVersion(); version != 0 && version != 1 {
		return nil, ErrEditListUnsupportedVersion
	}
	if len(elst.Entries) == 0 {
		tl.identity = true
		return tl, nil
	}

	tl.segments = make([]EditListSegment, 0, len(elst.Entries))
	var movieStart uint64
	for i := range elst.Entries {
		duration := elst.GetSegmentDuration(i)
		mediaTime := elst.GetMediaTime(i)
		if duration == 0 {
			return nil, ErrEditListInvalidEntry
		}
		if mediaTime < -1 {
			return nil, ErrEditListInvalidEntry
		}

		segment := EditListSegment{
			MovieStart: movieStart,
			MediaTime:  mediaTime,
			MediaEnd:   -1,
		}
		if mediaTime == -1 {
			segment.Empty = true
		} else {
			entry := &elst.Entries[i]
			if entry.MediaRateInteger != 1 || entry.MediaRateFraction != 0 {
				return nil, ErrEditListUnsupportedRate
			}
			mediaDuration, err := mulDivFloor(duration, uint64(tl.mediaTimescale), uint64(tl.movieTimescale))
			if err != nil {
				return nil, err
			}
			mediaEnd, err := addMediaDuration(mediaTime, mediaDuration)
			if err != nil {
				return nil, err
			}
			segment.MediaEnd = mediaEnd
		}

		if math.MaxUint64-movieStart < duration {
			return nil, ErrEditListOverflow
		}
		movieStart += duration
		segment.MovieEnd = movieStart
		tl.segments = append(tl.segments, segment)
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

// HasEditList reports whether the timeline was built from at least one edit
// list entry. It is false for the identity timeline.
func (tl *EditListTimeline) HasEditList() bool {
	return !tl.identity
}

// Segments returns a copy of the track's playback segments in edit list
// order. It returns nil for the identity timeline.
func (tl *EditListTimeline) Segments() []EditListSegment {
	if len(tl.segments) == 0 {
		return nil
	}
	segments := make([]EditListSegment, len(tl.segments))
	copy(segments, tl.segments)
	return segments
}

// MovieDuration returns the total playback duration in movie timescale units,
// that is the sum of all segment durations. It returns 0 for the identity
// timeline, whose duration is not defined by an edit list.
func (tl *EditListTimeline) MovieDuration() uint64 {
	if len(tl.segments) == 0 {
		return 0
	}
	return tl.segments[len(tl.segments)-1].MovieEnd
}

// MediaTimeAt returns the media time presented at the given playback time.
//
// playbackTime is in movie timescale units. The returned media time is in
// media timescale units. It returns ErrEditListPlaybackTimeInEmptyEdit if the
// playback time falls into an empty edit, and
// ErrEditListPlaybackTimeOutOfRange if no edit covers it.
func (tl *EditListTimeline) MediaTimeAt(playbackTime uint64) (int64, error) {
	if tl.identity {
		return identityMediaTime(playbackTime, tl.movieTimescale, tl.mediaTimescale)
	}
	for i := range tl.segments {
		segment := &tl.segments[i]
		if playbackTime < segment.MovieStart || playbackTime >= segment.MovieEnd {
			continue
		}
		if segment.Empty {
			return 0, ErrEditListPlaybackTimeInEmptyEdit
		}
		offset, err := mulDivFloor(
			playbackTime-segment.MovieStart,
			uint64(tl.mediaTimescale),
			uint64(tl.movieTimescale),
		)
		if err != nil {
			return 0, err
		}
		return addMediaDuration(segment.MediaTime, offset)
	}
	return 0, ErrEditListPlaybackTimeOutOfRange
}

func identityMediaTime(playbackTime uint64, movieTimescale, mediaTimescale uint32) (int64, error) {
	mediaTime, err := mulDivFloor(playbackTime, uint64(mediaTimescale), uint64(movieTimescale))
	if err != nil {
		return 0, err
	}
	if mediaTime > math.MaxInt64 {
		return 0, ErrEditListOverflow
	}
	return int64(mediaTime), nil
}

// addMediaDuration adds d (which must fit into int64) to a non-negative media
// time and reports an overflow of the signed result.
func addMediaDuration(mediaTime int64, d uint64) (int64, error) {
	if d > math.MaxInt64 {
		return 0, ErrEditListOverflow
	}
	if mediaTime > math.MaxInt64-int64(d) {
		return 0, ErrEditListOverflow
	}
	return mediaTime + int64(d), nil
}

// mulDivFloor computes v*mul/div, rounding down, and reports an overflow if
// the quotient does not fit into 64 bits. div must be non-zero.
func mulDivFloor(v, mul, div uint64) (uint64, error) {
	hi, lo := bits.Mul64(v, mul)
	if hi >= div {
		return 0, ErrEditListOverflow
	}
	quotient, _ := bits.Div64(hi, lo, div)
	return quotient, nil
}
