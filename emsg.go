package mp4

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"time"
)

// Errors reported by ReadEmsgs and WalkEmsgs.
//
// They are wrapped with contextual information such as the file offset of the
// offending box, so callers should test them with errors.Is instead of
// comparing the returned error directly.
var (
	// ErrEmsgInvalidBox means that a box header is broken (truncated, has a
	// size smaller than its own header, points outside of the file, ...) or
	// that an emsg payload cannot be decoded.
	ErrEmsgInvalidBox = errors.New("emsg: invalid or corrupt box")

	// ErrEmsgZeroTimescale means that an emsg box declares timescale 0, so its
	// presentation time and duration cannot be converted to a time.Duration.
	ErrEmsgZeroTimescale = errors.New("emsg: timescale must not be zero")

	// ErrEmsgMissingSegmentStartTime means that a version 0 emsg box needs the
	// start time of its media segment, but the caller did not supply one (or
	// the emsg box is not associated with any segment).
	ErrEmsgMissingSegmentStartTime = errors.New("emsg: version 0 event requires a media segment start time")

	// ErrEmsgPayloadTooLarge means that an emsg payload exceeds the limit set
	// with WithEmsgMaxPayloadSize. The payload is rejected before it is read,
	// so a corrupt size field cannot trigger a large allocation.
	ErrEmsgPayloadTooLarge = errors.New("emsg: payload exceeds the maximum size")

	// ErrEmsgTimeOverflow means that a presentation time or an event duration
	// cannot be represented as a time.Duration.
	ErrEmsgTimeOverflow = errors.New("emsg: time value overflows time.Duration")
)

// DefaultEmsgMaxPayloadSize is the maximum emsg payload size, including the
// scheme_id_uri and value strings, when WithEmsgMaxPayloadSize is not used.
// Event payloads are normally at most a few hundred bytes; the limit only
// exists to reject corrupt or malicious box sizes.
const DefaultEmsgMaxPayloadSize = 1 << 20 // 1 MiB

const (
	// maxEmsgDuration is the largest value representable by time.Duration.
	maxEmsgDuration = time.Duration(1<<63 - 1)

	// minEmsgDuration is the smallest value representable by time.Duration.
	minEmsgDuration = time.Duration(-1 << 63)

	// emsgNanosPerSecond is the number of nanoseconds in one second.
	emsgNanosPerSecond = uint64(time.Second)
)

// EmsgEvent is a timed event message carried by an emsg box.
//
// Id, SchemeIdUri, Value and MessageData are copied from the box, while
// PresentationTime and Duration are converted from emsg ticks using Timescale.
type EmsgEvent struct {
	// Offset is the file offset of the emsg box.
	Offset uint64

	// Version is the emsg box version (0 or 1).
	Version uint8

	// Id is the event identifier (emsg.id).
	Id uint32

	// SchemeIdUri identifies the event scheme (emsg.scheme_id_uri).
	SchemeIdUri string

	// Value is the scheme-specific value (emsg.value).
	Value string

	// PresentationTime is the absolute presentation time of the event.
	//
	// For version 1 boxes it is emsg.presentation_time. For version 0 boxes it
	// is the start time of the media segment reported by
	// EmsgSegmentStartTimeFunc plus emsg.presentation_time_delta. Both are
	// converted using Timescale.
	PresentationTime time.Duration

	// Duration is emsg.event_duration converted using Timescale.
	Duration time.Duration

	// MessageData is the raw event payload (emsg.message_data). It is
	// allocated for this event and is not shared with the reader or with other
	// events.
	MessageData []byte

	// Timescale is the emsg timescale used for PresentationTime and Duration.
	Timescale uint32
}

// EmsgSegment identifies a media segment for a version 0 emsg box.
//
// Media segments are delimited by top-level moof boxes and are numbered in
// file order starting at zero. An emsg box placed before the moof box of a
// segment (the usual layout styp/sidx/emsg/moof/mdat) or after the media data
// of the previous segment belongs to the segment introduced by the following
// moof box.
type EmsgSegment struct {
	// Index is the zero-based index of the segment in file order.
	Index int

	// Moof is the moof box that starts the segment.
	Moof BoxInfo
}

// EmsgSegmentStartTimeFunc returns the presentation time at which segment
// starts. It is called at most once per segment and only when a version 0
// emsg box needs the value; version 1 boxes carry absolute times and do not
// need it. Returning an error aborts the scan with that error.
type EmsgSegmentStartTimeFunc func(segment EmsgSegment) (time.Duration, error)

// EmsgOption customizes the behavior of ReadEmsgs and WalkEmsgs.
type EmsgOption func(*emsgOptions)

type emsgOptions struct {
	segmentStartTime EmsgSegmentStartTimeFunc
	maxPayloadSize   uint64
}

// WithEmsgSegmentStartTime sets the function used to resolve the absolute
// start time of media segments for version 0 emsg boxes. Without it, version 0
// events fail with ErrEmsgMissingSegmentStartTime; version 1 events still work
// because they carry absolute presentation times.
func WithEmsgSegmentStartTime(fn EmsgSegmentStartTimeFunc) EmsgOption {
	return func(o *emsgOptions) {
		o.segmentStartTime = fn
	}
}

// WithEmsgSegmentStartTimes is a convenience version of
// WithEmsgSegmentStartTime that maps the segment index to an element of
// startTimes. Requesting an index outside of the slice fails with
// ErrEmsgMissingSegmentStartTime.
func WithEmsgSegmentStartTimes(startTimes []time.Duration) EmsgOption {
	times := append([]time.Duration(nil), startTimes...)
	return WithEmsgSegmentStartTime(func(segment EmsgSegment) (time.Duration, error) {
		if segment.Index < 0 || segment.Index >= len(times) {
			return 0, fmt.Errorf("%w: no start time for segment %d",
				ErrEmsgMissingSegmentStartTime, segment.Index)
		}
		return times[segment.Index], nil
	})
}

// WithEmsgMaxPayloadSize overrides the maximum accepted emsg payload size in
// bytes (see DefaultEmsgMaxPayloadSize). A non-positive size is ignored.
func WithEmsgMaxPayloadSize(size int) EmsgOption {
	return func(o *emsgOptions) {
		if size > 0 {
			o.maxPayloadSize = uint64(size)
		}
	}
}

// ReadEmsgs scans r in file order and returns every timed event message carried
// by an emsg box.
//
// Only the top level of the file is traversed; moof boxes are additionally
// inspected for emsg children. Media data (mdat) is skipped by seeking, so its
// payload is never loaded into memory. Every emsg payload is limited by
// DefaultEmsgMaxPayloadSize or WithEmsgMaxPayloadSize.
//
// Version 1 emsg boxes are self-contained. Version 0 boxes need the start time
// of their media segment, which the caller supplies with
// WithEmsgSegmentStartTime or WithEmsgSegmentStartTimes.
//
// Events are delivered in file order. Use WalkEmsgs to process events without
// collecting them in a slice.
func ReadEmsgs(r io.ReadSeeker, options ...EmsgOption) ([]*EmsgEvent, error) {
	events := make([]*EmsgEvent, 0, 8)
	err := WalkEmsgs(r, func(event *EmsgEvent) error {
		events = append(events, event)
		return nil
	}, options...)
	if err != nil {
		return nil, err
	}
	return events, nil
}

// WalkEmsgs scans r in file order and calls handler exactly once for every
// timed event message carried by an emsg box.
//
// If handler returns an error, scanning stops and that error is returned
// unchanged. See ReadEmsgs for how emsg boxes are located, how media segments
// are assigned and which options are available.
func WalkEmsgs(r io.ReadSeeker, handler func(*EmsgEvent) error, options ...EmsgOption) error {
	if r == nil {
		return errors.New("mp4: emsg reader must not be nil")
	}
	if handler == nil {
		return errors.New("mp4: emsg handler must not be nil")
	}

	opts := emsgOptions{maxPayloadSize: DefaultEmsgMaxPayloadSize}
	for _, option := range options {
		if option != nil {
			option(&opts)
		}
	}
	if opts.maxPayloadSize == 0 {
		opts.maxPayloadSize = DefaultEmsgMaxPayloadSize
	}

	s := &emsgScanner{
		r:            r,
		opts:         opts,
		handler:      handler,
		segmentIndex: -1,
	}
	return s.scan()
}

// emsgPending is an event that has been read but not delivered yet. Version 0
// events that appear before the moof box of their segment cannot be resolved
// until that box is seen, so later events queue behind them to keep file order.
type emsgPending struct {
	event      *EmsgEvent
	delta      time.Duration
	needsStart bool
}

type emsgScanner struct {
	r       io.ReadSeeker
	opts    emsgOptions
	handler func(*EmsgEvent) error

	fileSize uint64
	pending  []emsgPending

	segmentIndex int // -1 until the first moof box
	segmentMoof  BoxInfo
	haveSegment  bool
	mdatSeen     bool

	segmentStart      time.Duration
	segmentStartKnown bool
}

func (s *emsgScanner) scan() error {
	end, err := s.r.Seek(0, io.SeekEnd)
	if err != nil {
		return fmt.Errorf("mp4: emsg scan: %w", err)
	}
	if end < 0 {
		return fmt.Errorf("%w: invalid file size %d", ErrEmsgInvalidBox, end)
	}
	s.fileSize = uint64(end)

	offset := uint64(0)
	for offset < s.fileSize {
		bi, err := s.readBoxAt(offset, s.fileSize)
		if err != nil {
			return err
		}

		switch bi.Type {
		case BoxTypeEmsg():
			if err := s.onEmsg(bi); err != nil {
				return err
			}
		case BoxTypeMoof():
			if err := s.onMoof(bi); err != nil {
				return err
			}
		case BoxTypeMdat():
			// Media data is never read: the loop only advances over the
			// payload. An emsg box after it belongs to the next segment.
			s.mdatSeen = true
		}

		offset = bi.Offset + bi.Size
	}

	return s.finish()
}

// readBoxAt reads and validates a box header at offset. limit is the end of
// the containing box, or the file size at the top level.
func (s *emsgScanner) readBoxAt(offset, limit uint64) (*BoxInfo, error) {
	if offset >= limit {
		return nil, fmt.Errorf("%w: box offset %d is outside of its container (end %d)",
			ErrEmsgInvalidBox, offset, limit)
	}
	if _, err := s.r.Seek(int64(offset), io.SeekStart); err != nil {
		return nil, fmt.Errorf("%w: seek to offset %d: %v", ErrEmsgInvalidBox, offset, err)
	}
	bi, err := ReadBoxInfo(s.r)
	if err != nil {
		return nil, fmt.Errorf("%w: failed to read box header at offset %d: %v",
			ErrEmsgInvalidBox, offset, err)
	}
	if bi.Offset != offset {
		return nil, fmt.Errorf("%w: unexpected box offset %d at offset %d",
			ErrEmsgInvalidBox, bi.Offset, offset)
	}
	if bi.Size < bi.HeaderSize {
		return nil, fmt.Errorf("%w: box %s at offset %d has size %d smaller than its %d byte header",
			ErrEmsgInvalidBox, bi.Type.String(), offset, bi.Size, bi.HeaderSize)
	}
	if bi.Size > limit-offset {
		return nil, fmt.Errorf("%w: box %s at offset %d with size %d exceeds its container (end %d)",
			ErrEmsgInvalidBox, bi.Type.String(), offset, bi.Size, limit)
	}
	return bi, nil
}

func (s *emsgScanner) onMoof(bi *BoxInfo) error {
	s.segmentIndex++
	s.segmentMoof = *bi
	s.haveSegment = true
	s.mdatSeen = false
	s.segmentStart = 0
	s.segmentStartKnown = false

	// Events seen before this moof box belong to the segment it starts.
	if err := s.flushPending(); err != nil {
		return err
	}

	return s.scanMoofChildren(bi)
}

// scanMoofChildren looks for emsg boxes placed directly inside a moof box
// (ISO/IEC 23009-1 allows both top-level and moof-level placement). Child
// payloads themselves are never read.
func (s *emsgScanner) scanMoofChildren(bi *BoxInfo) error {
	end := bi.Offset + bi.Size
	for offset := bi.Offset + bi.HeaderSize; offset < end; {
		child, err := s.readBoxAt(offset, end)
		if err != nil {
			return err
		}
		if child.Type == BoxTypeEmsg() {
			if err := s.onEmsg(child); err != nil {
				return err
			}
		}
		offset = child.Offset + child.Size
	}
	return nil
}

func (s *emsgScanner) onEmsg(bi *BoxInfo) error {
	payloadSize := bi.Size - bi.HeaderSize
	if payloadSize > s.opts.maxPayloadSize {
		return fmt.Errorf("%w: emsg at offset %d has a %d byte payload, limit is %d",
			ErrEmsgPayloadTooLarge, bi.Offset, payloadSize, s.opts.maxPayloadSize)
	}

	payload := make([]byte, payloadSize)
	if _, err := bi.SeekToPayload(s.r); err != nil {
		return fmt.Errorf("%w: emsg at offset %d: %v", ErrEmsgInvalidBox, bi.Offset, err)
	}
	if _, err := io.ReadFull(s.r, payload); err != nil {
		return fmt.Errorf("%w: emsg at offset %d: truncated payload: %v",
			ErrEmsgInvalidBox, bi.Offset, err)
	}

	// Unmarshal from the bounded payload. In particular, version 1 reads
	// null-terminated strings without knowing the box size, so a missing
	// terminator must hit the end of this payload instead of consuming the
	// bytes of the following box.
	box, n, err := UnmarshalAny(bytes.NewReader(payload), BoxTypeEmsg(), payloadSize, Context{})
	if err != nil {
		return fmt.Errorf("%w: emsg at offset %d: %v", ErrEmsgInvalidBox, bi.Offset, err)
	}
	if n != payloadSize {
		return fmt.Errorf("%w: emsg at offset %d: decoded %d of %d payload bytes",
			ErrEmsgInvalidBox, bi.Offset, n, payloadSize)
	}
	emsg, ok := box.(*Emsg)
	if !ok {
		return fmt.Errorf("%w: emsg at offset %d: unexpected box type",
			ErrEmsgInvalidBox, bi.Offset)
	}
	if version := emsg.GetVersion(); version != 0 && version != 1 {
		return fmt.Errorf("%w: emsg at offset %d has unsupported version %d",
			ErrEmsgInvalidBox, bi.Offset, version)
	}

	if emsg.Timescale == 0 {
		return fmt.Errorf("%w: emsg at offset %d", ErrEmsgZeroTimescale, bi.Offset)
	}

	duration, err := emsgTicksToDuration(uint64(emsg.EventDuration), emsg.Timescale)
	if err != nil {
		return fmt.Errorf("%w: emsg at offset %d: event duration",
			ErrEmsgTimeOverflow, bi.Offset)
	}

	event := &EmsgEvent{
		Offset:      bi.Offset,
		Version:     emsg.GetVersion(),
		Id:          emsg.Id,
		SchemeIdUri: emsg.SchemeIdUri,
		Value:       emsg.Value,
		Duration:    duration,
		MessageData: emsg.MessageData,
		Timescale:   emsg.Timescale,
	}

	if emsg.GetVersion() == 1 {
		presentationTime, err := emsgTicksToDuration(emsg.PresentationTime, emsg.Timescale)
		if err != nil {
			return fmt.Errorf("%w: emsg at offset %d: presentation time",
				ErrEmsgTimeOverflow, bi.Offset)
		}
		event.PresentationTime = presentationTime
		return s.queue(emsgPending{event: event})
	}

	delta, err := emsgTicksToDuration(uint64(emsg.PresentationTimeDelta), emsg.Timescale)
	if err != nil {
		return fmt.Errorf("%w: emsg at offset %d: presentation time delta",
			ErrEmsgTimeOverflow, bi.Offset)
	}
	if s.haveSegment && !s.mdatSeen {
		// The emsg box follows the moof box of its segment and precedes the
		// media data, so the segment is already known.
		start, err := s.resolveSegmentStart(bi.Offset)
		if err != nil {
			return err
		}
		event.PresentationTime, err = addEmsgDurations(start, delta)
		if err != nil {
			return fmt.Errorf("%w: emsg at offset %d: presentation time",
				ErrEmsgTimeOverflow, bi.Offset)
		}
		return s.queue(emsgPending{event: event})
	}

	// The event belongs to the segment that the next moof box starts.
	return s.queue(emsgPending{event: event, delta: delta, needsStart: true})
}

// resolvePendingStart converts the presentation_time_delta of a pending
// version 0 event using the start time of the current segment.
func (s *emsgScanner) resolvePendingStart(pending *emsgPending, emsgOffset uint64) error {
	start, err := s.resolveSegmentStart(emsgOffset)
	if err != nil {
		return err
	}
	presentationTime, err := addEmsgDurations(start, pending.delta)
	if err != nil {
		return fmt.Errorf("%w: emsg at offset %d: presentation time",
			ErrEmsgTimeOverflow, pending.event.Offset)
	}
	pending.event.PresentationTime = presentationTime
	pending.needsStart = false
	return nil
}

// queue delivers an event, keeping file order. If an earlier event is still
// waiting for its segment start time, the new event is queued behind it.
func (s *emsgScanner) queue(pending emsgPending) error {
	if len(s.pending) == 0 && !pending.needsStart {
		return s.handler(pending.event)
	}
	s.pending = append(s.pending, pending)
	return nil
}

// flushPending resolves and delivers events queued before the moof box that
// has just been seen.
func (s *emsgScanner) flushPending() error {
	if len(s.pending) == 0 {
		return nil
	}

	for i := range s.pending {
		if !s.pending[i].needsStart {
			continue
		}
		if err := s.resolvePendingStart(&s.pending[i], s.pending[i].event.Offset); err != nil {
			return err
		}
	}

	for _, pending := range s.pending {
		if err := s.handler(pending.event); err != nil {
			return err
		}
	}
	s.pending = s.pending[:0]
	return nil
}

// finish delivers events that remain after the last box and reports version 0
// events for which no segment was ever seen.
func (s *emsgScanner) finish() error {
	for _, pending := range s.pending {
		if pending.needsStart {
			return fmt.Errorf("%w: emsg at offset %d is not followed by a moof box",
				ErrEmsgMissingSegmentStartTime, pending.event.Offset)
		}
	}
	for _, pending := range s.pending {
		if err := s.handler(pending.event); err != nil {
			return err
		}
	}
	s.pending = nil
	return nil
}

func (s *emsgScanner) resolveSegmentStart(emsgOffset uint64) (time.Duration, error) {
	if s.segmentStartKnown {
		return s.segmentStart, nil
	}
	if !s.haveSegment {
		return 0, fmt.Errorf("%w: emsg at offset %d", ErrEmsgMissingSegmentStartTime, emsgOffset)
	}
	if s.opts.segmentStartTime == nil {
		return 0, fmt.Errorf("%w: segment %d (moof at offset %d), emsg at offset %d",
			ErrEmsgMissingSegmentStartTime, s.segmentIndex, s.segmentMoof.Offset, emsgOffset)
	}

	start, err := s.opts.segmentStartTime(EmsgSegment{Index: s.segmentIndex, Moof: s.segmentMoof})
	if err != nil {
		return 0, fmt.Errorf("mp4: emsg at offset %d: %w", emsgOffset, err)
	}
	s.segmentStart = start
	s.segmentStartKnown = true
	return start, nil
}

// emsgTicksToDuration converts ticks with the given timescale to nanoseconds
// without overflowing. Sub-nanosecond remainders are truncated.
func emsgTicksToDuration(ticks uint64, timescale uint32) (time.Duration, error) {
	ts := uint64(timescale)
	if ts == 0 {
		return 0, ErrEmsgZeroTimescale
	}

	seconds := ticks / ts
	remainder := ticks % ts
	if seconds > uint64(maxEmsgDuration)/emsgNanosPerSecond {
		return 0, ErrEmsgTimeOverflow
	}
	nanos := seconds * emsgNanosPerSecond

	// remainder < timescale <= math.MaxUint32, so the product fits in uint64.
	fraction := remainder * emsgNanosPerSecond / ts
	if nanos > uint64(maxEmsgDuration)-fraction {
		return 0, ErrEmsgTimeOverflow
	}
	return time.Duration(nanos + fraction), nil
}

// addEmsgDurations adds a non-negative delta to start without wrapping around.
func addEmsgDurations(start, delta time.Duration) (time.Duration, error) {
	if delta > 0 && start > maxEmsgDuration-delta {
		return 0, ErrEmsgTimeOverflow
	}
	if delta < 0 && start < minEmsgDuration-delta {
		return 0, ErrEmsgTimeOverflow
	}
	return start + delta, nil
}
