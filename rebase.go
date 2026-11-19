package mp4

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

// Error categories returned by FragmentRebaser. Errors returned by
// AppendFragment are wrapped in a *FragmentError carrying the fragment index,
// and can be classified with errors.Is against these sentinels.
var (
	// ErrFragmentTrackMismatch indicates that a traf belongs to a different
	// track than the track established by the init segment / first fragment.
	ErrFragmentTrackMismatch = errors.New("mp4: fragment track ID mismatch")
	// ErrFragmentMissingDuration indicates that a sample duration can be
	// derived neither from trun entries, tfhd defaults, nor trex defaults.
	ErrFragmentMissingDuration = errors.New("mp4: missing sample duration")
	// ErrFragmentTimeOverflow indicates that a sequence number or a base
	// media decode time does not fit into its target field.
	ErrFragmentTimeOverflow = errors.New("mp4: decode time overflow")
	// ErrFragmentOffsetOverflow indicates that a data offset does not fit
	// into its target field.
	ErrFragmentOffsetOverflow = errors.New("mp4: data offset overflow")
	// ErrFragmentCorrupt indicates a structurally broken fragment.
	ErrFragmentCorrupt = errors.New("mp4: corrupt fragment")
)

// FragmentError is returned by FragmentRebaser.AppendFragment and carries
// the zero-based index of the fragment that caused the failure.
type FragmentError struct {
	Index int
	Err   error
}

func (e *FragmentError) Error() string {
	return fmt.Sprintf("mp4: fragment %d: %s", e.Index, e.Err)
}

func (e *FragmentError) Unwrap() error { return e.Err }

// trun flags (ISO/IEC 14496-12)
const (
	trunDataOffsetPresent                  = 0x000001
	trunSampleDurationPresent              = 0x000100
	trunSampleSizePresent                  = 0x000200
	trunSampleFlagsPresent                 = 0x000400
	trunSampleCompositionTimeOffsetPresent = 0x000800
)

// FragmentRebaser concatenates CMAF/fMP4 media fragments of a single track
// into one continuous stream. Simply concatenating the bytes of two live
// fragments makes their timestamps overlap or go backwards; FragmentRebaser
// rewrites the movie fragments so that
//
//   - mfhd sequence numbers are consecutive,
//   - tfdt base media decode times accumulate fragment durations,
//   - tfhd base data offsets and trun data offsets stay correct.
//
// Sample bytes, sample durations, sample flags and unknown boxes are passed
// through untouched. The typical usage is:
//
//	rb := mp4.NewFragmentRebaser(w)
//	err := rb.WriteInitSegment(initSegment)
//	for _, frag := range fragments {
//		err = rb.AppendFragment(frag)
//	}
//
// It is not safe for concurrent use.
type FragmentRebaser struct {
	w      io.Writer
	offset uint64 // number of bytes written to w so far

	seqNum  uint64 // next mfhd sequence number
	base    uint64 // next tfdt base media decode time
	hasBase bool   // base was initialized from the first fragment

	trackID  uint32
	hasTrack bool
	trex     map[uint32]*Trex // default sample durations from the init segment

	fragIndex int // index of the fragment being processed
	initDone  bool
}

// NewFragmentRebaser creates a FragmentRebaser writing the rebased stream to w.
func NewFragmentRebaser(w io.Writer) *FragmentRebaser {
	return &FragmentRebaser{w: w, trex: make(map[uint32]*Trex)}
}

// WriteInitSegment copies an initialization segment (ftyp, moov, ...) to the
// output unchanged and records the trex default sample durations used to
// resolve fragment durations. It must be called before the first
// AppendFragment. It is optional, but without it fragments must carry sample
// durations in their trun entries or tfhd defaults.
func (rb *FragmentRebaser) WriteInitSegment(r io.Reader) error {
	if rb.initDone {
		return errors.New("mp4: init segment written twice")
	}
	if rb.fragIndex != 0 {
		return errors.New("mp4: init segment must be written before media fragments")
	}
	rb.initDone = true

	data, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("mp4: init segment: read: %w", err)
	}
	boxes, err := splitBoxes(data)
	if err != nil {
		return fmt.Errorf("mp4: init segment: %w", err)
	}
	var moov *boxSlice
	for i := range boxes {
		if boxes[i].info.Type == BoxTypeMoov() {
			moov = &boxes[i]
			break
		}
	}
	if moov == nil {
		return errors.New("mp4: init segment: moov box not found")
	}
	moovChildren, err := splitBoxes(moov.payload())
	if err != nil {
		return fmt.Errorf("mp4: init segment: %w", err)
	}
	for i := range moovChildren {
		if moovChildren[i].info.Type != BoxTypeMvex() {
			continue
		}
		mvexChildren, err := splitBoxes(moovChildren[i].payload())
		if err != nil {
			return fmt.Errorf("mp4: init segment: %w", err)
		}
		for j := range mvexChildren {
			if mvexChildren[j].info.Type != BoxTypeTrex() {
				continue
			}
			box, _, err := UnmarshalAny(bytes.NewReader(mvexChildren[j].payload()),
				BoxTypeTrex(), mvexChildren[j].info.Size-mvexChildren[j].info.HeaderSize, Context{})
			if err != nil {
				return fmt.Errorf("mp4: init segment: trex: %w", err)
			}
			trex := box.(*Trex)
			rb.trex[trex.TrackID] = trex
		}
	}
	if _, err := rb.w.Write(data); err != nil {
		return fmt.Errorf("mp4: init segment: write: %w", err)
	}
	rb.offset += uint64(len(data))
	return nil
}

// AppendFragment reads one media fragment (styp, moof, mdat, and any other
// top-level boxes) from r and appends it to the output with rebased sequence
// numbers, decode times and data offsets. The first fragment establishes the
// track ID and the initial base media decode time. Any failure is returned
// as a *FragmentError carrying the zero-based fragment index.
func (rb *FragmentRebaser) AppendFragment(r io.Reader) (retErr error) {
	idx := rb.fragIndex
	rb.fragIndex++
	defer func() {
		if retErr != nil {
			retErr = &FragmentError{Index: idx, Err: retErr}
		}
	}()

	data, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	boxes, err := splitBoxes(data)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrFragmentCorrupt, err)
	}
	if len(boxes) == 0 {
		return fmt.Errorf("%w: empty fragment", ErrFragmentCorrupt)
	}

	var out bytes.Buffer
	sawMoof := false
	for i := range boxes {
		if boxes[i].info.Type != BoxTypeMoof() {
			out.Write(boxes[i].raw)
			continue
		}
		sawMoof = true
		rebased, err := rb.rebaseMoof(&boxes[i], rb.offset+uint64(out.Len()))
		if err != nil {
			return err
		}
		out.Write(rebased)
	}
	if !sawMoof {
		return fmt.Errorf("%w: moof box not found", ErrFragmentCorrupt)
	}
	if _, err := rb.w.Write(out.Bytes()); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	rb.offset += uint64(out.Len())
	return nil
}

// boxSlice is a box together with its raw bytes (header + payload).
type boxSlice struct {
	info BoxInfo
	raw  []byte
}

func (b *boxSlice) payload() []byte {
	return b.raw[b.info.HeaderSize:]
}

// splitBoxes parses the consecutive boxes contained in data.
func splitBoxes(data []byte) ([]boxSlice, error) {
	r := bytes.NewReader(data)
	var boxes []boxSlice
	for r.Len() > 0 {
		bi, err := ReadBoxInfo(r)
		if err != nil {
			return nil, err
		}
		if bi.Size < bi.HeaderSize || bi.Size-bi.HeaderSize > uint64(r.Len()) {
			return nil, io.ErrUnexpectedEOF
		}
		boxes = append(boxes, boxSlice{
			info: *bi,
			raw:  data[bi.Offset : bi.Offset+bi.Size],
		})
		if _, err := bi.SeekToEnd(r); err != nil {
			return nil, err
		}
	}
	return boxes, nil
}

// trafPlan collects the modifications for one traf box.
type trafPlan struct {
	payload   []byte // patched copy of the traf payload
	insert    []byte // tfdt box to insert (nil if the traf already has one)
	insertPos int    // offset within payload where insert goes
	trunOffs  []int  // offsets of trun data-offset fields within payload
}

// rebaseMoof rewrites one moof box (given with its raw bytes) and returns the
// new raw bytes. moofOutOffset is the absolute offset the moof will have in
// the output stream.
func (rb *FragmentRebaser) rebaseMoof(moof *boxSlice, moofOutOffset uint64) ([]byte, error) {
	if moof.info.ExtendToEOF {
		return nil, fmt.Errorf("%w: moof extends to EOF", ErrFragmentCorrupt)
	}
	children, err := splitBoxes(moof.payload())
	if err != nil {
		return nil, fmt.Errorf("%w: moof: %s", ErrFragmentCorrupt, err)
	}

	var mfhd *boxSlice
	var plans []*trafPlan
	for i := range children {
		child := &children[i]
		switch child.info.Type {
		case BoxTypeMfhd():
			mfhd = child
		case BoxTypeTraf():
			plan, err := rb.planTraf(child, moofOutOffset)
			if err != nil {
				return nil, err
			}
			plans = append(plans, plan)
		}
	}
	if mfhd == nil {
		return nil, fmt.Errorf("%w: mfhd box not found", ErrFragmentCorrupt)
	}
	if len(plans) == 0 {
		return nil, fmt.Errorf("%w: traf box not found", ErrFragmentCorrupt)
	}

	// total size change of the moof, used to fix trun data offsets
	var totalDelta int64
	for _, p := range plans {
		totalDelta += int64(len(p.insert))
	}

	// patch trun data offsets now that the total delta is known
	for _, p := range plans {
		for _, off := range p.trunOffs {
			old := int32(binary.BigEndian.Uint32(p.payload[off:]))
			newOff := int64(old) + totalDelta
			if newOff > math.MaxInt32 || newOff < math.MinInt32 {
				return nil, fmt.Errorf("%w: trun data offset %d overflows int32",
					ErrFragmentOffsetOverflow, newOff)
			}
			binary.BigEndian.PutUint32(p.payload[off:], uint32(int32(newOff)))
		}
	}

	// rewrite the mfhd sequence number
	if rb.seqNum > math.MaxUint32 {
		return nil, fmt.Errorf("%w: sequence number exceeds 32 bits", ErrFragmentTimeOverflow)
	}
	mfhdRaw := make([]byte, len(mfhd.raw))
	copy(mfhdRaw, mfhd.raw)
	binary.BigEndian.PutUint32(mfhdRaw[mfhd.info.HeaderSize+4:], uint32(rb.seqNum))
	rb.seqNum++

	// assemble the new moof
	newSize := moof.info.Size + uint64(totalDelta)
	header, err := encodeBoxHeader(moof.info.Type, newSize, moof.info.HeaderSize)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, newSize)
	out = append(out, header...)
	planIdx := 0
	for i := range children {
		child := &children[i]
		switch child.info.Type {
		case BoxTypeMfhd():
			out = append(out, mfhdRaw...)
		case BoxTypeTraf():
			p := plans[planIdx]
			planIdx++
			trafHeader, err := encodeBoxHeader(child.info.Type,
				child.info.Size+uint64(len(p.insert)), child.info.HeaderSize)
			if err != nil {
				return nil, err
			}
			out = append(out, trafHeader...)
			out = append(out, p.payload[:p.insertPos]...)
			out = append(out, p.insert...)
			out = append(out, p.payload[p.insertPos:]...)
		default:
			// unknown boxes are preserved as-is
			out = append(out, child.raw...)
		}
	}
	return out, nil
}

// planTraf inspects one traf box, advances the base media decode time and
// returns the patches to apply to the traf payload.
func (rb *FragmentRebaser) planTraf(traf *boxSlice, moofOutOffset uint64) (*trafPlan, error) {
	if traf.info.ExtendToEOF {
		return nil, fmt.Errorf("%w: traf extends to EOF", ErrFragmentCorrupt)
	}
	children, err := splitBoxes(traf.payload())
	if err != nil {
		return nil, fmt.Errorf("%w: traf: %s", ErrFragmentCorrupt, err)
	}

	plan := &trafPlan{}
	plan.payload = make([]byte, len(traf.payload()))
	copy(plan.payload, traf.payload())

	var tfhdBox *Tfhd
	var tfhdChild *boxSlice
	var tfdtChild *boxSlice
	var truns []*Trun
	var trunChildren []*boxSlice
	for i := range children {
		child := &children[i]
		switch child.info.Type {
		case BoxTypeTfhd():
			box, _, err := UnmarshalAny(bytes.NewReader(child.payload()),
				BoxTypeTfhd(), child.info.Size-child.info.HeaderSize, Context{})
			if err != nil {
				return nil, fmt.Errorf("%w: tfhd: %s", ErrFragmentCorrupt, err)
			}
			tfhdBox = box.(*Tfhd)
			tfhdChild = child
		case BoxTypeTfdt():
			tfdtChild = child
		case BoxTypeTrun():
			box, _, err := UnmarshalAny(bytes.NewReader(child.payload()),
				BoxTypeTrun(), child.info.Size-child.info.HeaderSize, Context{})
			if err != nil {
				return nil, fmt.Errorf("%w: trun: %s", ErrFragmentCorrupt, err)
			}
			truns = append(truns, box.(*Trun))
			trunChildren = append(trunChildren, child)
		}
	}
	if tfhdBox == nil {
		return nil, fmt.Errorf("%w: tfhd box not found", ErrFragmentCorrupt)
	}

	// track consistency
	if !rb.hasTrack {
		rb.trackID = tfhdBox.TrackID
		rb.hasTrack = true
		if len(rb.trex) != 0 {
			if _, ok := rb.trex[rb.trackID]; !ok {
				return nil, fmt.Errorf("%w: track ID %d not present in init segment",
					ErrFragmentTrackMismatch, rb.trackID)
			}
		}
	} else if tfhdBox.TrackID != rb.trackID {
		return nil, fmt.Errorf("%w: got track ID %d, want %d",
			ErrFragmentTrackMismatch, tfhdBox.TrackID, rb.trackID)
	}

	// fragment duration from the truns
	trex := rb.trex[rb.trackID]
	var duration uint64
	for _, trun := range truns {
		d, err := trunDuration(trun, tfhdBox, trex)
		if err != nil {
			return nil, err
		}
		if duration > math.MaxUint64-d {
			return nil, fmt.Errorf("%w: accumulated duration overflows 64 bits", ErrFragmentTimeOverflow)
		}
		duration += d
	}

	// tfdt: patch in place, or schedule an insertion
	if tfdtChild != nil {
		box, _, err := UnmarshalAny(bytes.NewReader(tfdtChild.payload()),
			BoxTypeTfdt(), tfdtChild.info.Size-tfdtChild.info.HeaderSize, Context{})
		if err != nil {
			return nil, fmt.Errorf("%w: tfdt: %s", ErrFragmentCorrupt, err)
		}
		tfdt := box.(*Tfdt)
		if !rb.hasBase {
			// adopt the decode time of the first fragment as the origin
			rb.base = tfdt.GetBaseMediaDecodeTime()
		}
		off := tfdtChild.info.Offset + tfdtChild.info.HeaderSize + 4 // full box header
		switch tfdt.GetVersion() {
		case 0:
			if rb.base > math.MaxUint32 {
				return nil, fmt.Errorf("%w: base media decode time %d does not fit tfdt version 0",
					ErrFragmentTimeOverflow, rb.base)
			}
			binary.BigEndian.PutUint32(plan.payload[off:], uint32(rb.base))
		default:
			binary.BigEndian.PutUint64(plan.payload[off:], rb.base)
		}
	} else {
		if !rb.hasBase {
			rb.base = 0
		}
		// insert a version 1 tfdt right after the tfhd box
		tfdt := &Tfdt{BaseMediaDecodeTimeV1: rb.base}
		tfdt.SetVersion(1)
		var buf bytes.Buffer
		if _, err := Marshal(&buf, tfdt, Context{}); err != nil {
			return nil, err
		}
		header := EncodeBoxInfo(&BoxInfo{
			Type: BoxTypeTfdt(),
			Size: uint64(buf.Len()) + SmallHeaderSize,
		})
		plan.insert = append(header, buf.Bytes()...)
		plan.insertPos = int(tfhdChild.info.Offset + tfhdChild.info.Size)
	}
	rb.hasBase = true
	if rb.base > math.MaxUint64-duration {
		return nil, fmt.Errorf("%w: base media decode time overflows 64 bits", ErrFragmentTimeOverflow)
	}
	rb.base += duration

	// tfhd base data offset: point at the moof in the output stream
	if tfhdBox.CheckFlag(TfhdBaseDataOffsetPresent) {
		off := tfhdChild.info.Offset + tfhdChild.info.HeaderSize + 8 // full box header + track ID
		binary.BigEndian.PutUint64(plan.payload[off:], moofOutOffset)
	}

	// record trun data offset locations; they are patched once the total
	// moof size change is known
	for i, trun := range truns {
		if trun.GetFlags()&trunDataOffsetPresent != 0 {
			child := trunChildren[i]
			off := child.info.Offset + child.info.HeaderSize + 8 // full box header + sample count
			plan.trunOffs = append(plan.trunOffs, int(off))
		}
	}
	return plan, nil
}

// trunDuration returns the total duration of the samples in a trun, resolving
// defaults from tfhd and trex.
func trunDuration(trun *Trun, tfhd *Tfhd, trex *Trex) (uint64, error) {
	flags := trun.GetFlags()
	if flags&trunSampleDurationPresent != 0 {
		var sum uint64
		for i := range trun.Entries {
			d := uint64(trun.Entries[i].SampleDuration)
			if sum > math.MaxUint64-d {
				return 0, fmt.Errorf("%w: accumulated duration overflows 64 bits", ErrFragmentTimeOverflow)
			}
			sum += d
		}
		return sum, nil
	}
	switch {
	case tfhd.CheckFlag(TfhdDefaultSampleDurationPresent):
		return uint64(tfhd.DefaultSampleDuration) * uint64(trun.SampleCount), nil
	case tfhd.CheckFlag(TfhdDurationIsEmpty):
		return 0, nil
	case trex != nil:
		return uint64(trex.DefaultSampleDuration) * uint64(trun.SampleCount), nil
	}
	return 0, fmt.Errorf("%w: trun carries no sample durations and no tfhd/trex default exists",
		ErrFragmentMissingDuration)
}

// encodeBoxHeader encodes a box header keeping the original header form.
func encodeBoxHeader(boxType BoxType, size, headerSize uint64) ([]byte, error) {
	if headerSize == SmallHeaderSize && size > math.MaxUint32 {
		return nil, fmt.Errorf("%w: box size %d does not fit a 32 bit box header",
			ErrFragmentOffsetOverflow, size)
	}
	return EncodeBoxInfo(&BoxInfo{Type: boxType, Size: size, HeaderSize: headerSize}), nil
}
