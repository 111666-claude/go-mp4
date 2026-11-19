package mp4

import (
	"errors"
	"fmt"
	"io"
	"math"
	"sync"
)

// Errors returned by SampleReader. All errors returned by SampleReader wrap
// one of these sentinel errors, so they can be inspected with errors.Is.
var (
	// ErrUnsupportedSampleLayout is returned when the file uses a layout
	// which SampleReader does not support, such as a fragmented MP4
	// (moof/mvex) or the stz2 sample size box.
	ErrUnsupportedSampleLayout = errors.New("mp4: unsupported sample layout")

	// ErrInvalidSampleTable is returned when the sample tables are missing,
	// duplicated, or inconsistent with each other.
	ErrInvalidSampleTable = errors.New("mp4: invalid sample table")

	// ErrTrackNotFound is returned when the requested track ID does not exist
	// in the file.
	ErrTrackNotFound = errors.New("mp4: track not found")

	// ErrEmptyTrack is returned when samples are requested from a track which
	// holds no sample.
	ErrEmptyTrack = errors.New("mp4: empty track")

	// ErrSampleIndexOutOfRange is returned when the sample index is equal to
	// or larger than the sample count of the track.
	ErrSampleIndexOutOfRange = errors.New("mp4: sample index out of range")

	// ErrSampleDataOutOfBounds is returned when the byte range of a sample is
	// outside the file, outside every mdat payload, or cannot be represented
	// as a seek offset.
	ErrSampleDataOutOfBounds = errors.New("mp4: sample data out of bounds")

	// ErrTruncatedMdat is returned when an mdat box declares more bytes than
	// the file contains, or when the payload of a sample cannot be read
	// completely.
	ErrTruncatedMdat = errors.New("mp4: truncated mdat")

	// ErrSampleTimeOverflow is returned when a decode time or the resulting
	// presentation time cannot be represented by the types of MediaSample.
	ErrSampleTimeOverflow = errors.New("mp4: sample time overflow")
)

// MediaSample describes a single sample of a non-fragmented MP4 track.
//
// DTS, PTS and Duration are expressed in the media timescale of the track
// (see SampleReader.Timescale).
type MediaSample struct {
	// TrackID is the ID of the track which owns the sample.
	TrackID uint32

	// Index is the zero-based sample number in the track.
	Index uint32

	// Offset is the absolute file offset of the sample payload.
	Offset uint64

	// Size is the payload size of the sample in bytes.
	Size uint32

	// DTS is the decode timestamp of the sample.
	DTS uint64

	// PTS is the presentation timestamp of the sample. It is DTS plus the
	// composition time offset from the ctts box, and therefore may be
	// negative or predate DTS.
	PTS int64

	// Duration is the duration of the sample.
	Duration uint32

	// Sync reports whether the sample is a sync sample. It is true for every
	// sample when the track has no stss box.
	Sync bool
}

// MediaSampleData is a MediaSample together with its payload.
type MediaSampleData struct {
	MediaSample

	// Data is the payload of the sample.
	Data []byte
}

type byteRange struct {
	start uint64
	end   uint64 // exclusive
}

type sampleTrack struct {
	trackID   uint32
	timescale uint32
	samples   []MediaSample
}

// SampleReader reads samples of non-fragmented MP4 tracks.
//
// NewSampleReader parses the sample tables (stsc, stco/co64, stsz, stts, ctts
// and stss) once, but it does not read the media payload. Each call to
// ReadSample/ReadSampleData reads only the requested byte range from the
// underlying io.ReadSeeker, so the mdat box is never cached as a whole.
//
// SampleReader is safe for concurrent use by multiple goroutines, provided
// that the underlying io.ReadSeeker is not used by other code at the same
// time.
type SampleReader struct {
	r        io.ReadSeeker
	fileSize uint64
	mdats    []byteRange
	tracks   map[uint32]*sampleTrack
	trackIDs []uint32

	mu sync.Mutex
}

// NewSampleReader parses the moov box of r and validates the sample tables of
// every track. The reader must stay open while the SampleReader is in use.
//
// The whole buffer is never read: only box headers, sample tables and the
// explicitly requested sample payloads are read from r.
func NewSampleReader(r io.ReadSeeker) (*SampleReader, error) {
	if r == nil {
		return nil, errors.New("mp4: nil reader")
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("mp4: failed to seek: %w", err)
	}
	fileSize, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, fmt.Errorf("mp4: failed to get file size: %w", err)
	}
	if fileSize < 0 {
		return nil, fmt.Errorf("mp4: invalid file size: %d", fileSize)
	}

	// Collect the top-level boxes needed to build the sample index. The
	// handler does not expand moov here, so its children are not read yet.
	bis, err := ExtractBoxes(r, nil, []BoxPath{
		{BoxTypeMoov()},
		{BoxTypeMoof()},
		{BoxTypeMvex()},
		{BoxTypeMdat()},
	})
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, fmt.Errorf("%w: unexpected end of file: %v", ErrTruncatedMdat, err)
		}
		return nil, err
	}

	sr := &SampleReader{
		r:        r,
		fileSize: uint64(fileSize),
		tracks:   make(map[uint32]*sampleTrack, 4),
	}
	var moovs []*BoxInfo
	for _, bi := range bis {
		switch bi.Type {
		case BoxTypeMoof(), BoxTypeMvex():
			return nil, fmt.Errorf("%w: fragmented MP4 is not supported (box %s at offset %d)", ErrUnsupportedSampleLayout, bi.Type.String(), bi.Offset)
		case BoxTypeMdat():
			end, ok := addUint64(bi.Offset, bi.Size)
			if !ok || end > sr.fileSize {
				return nil, fmt.Errorf("%w: mdat box at offset %d declares size %d but the file size is %d", ErrTruncatedMdat, bi.Offset, bi.Size, sr.fileSize)
			}
			if bi.Size < bi.HeaderSize {
				return nil, fmt.Errorf("%w: mdat box at offset %d has invalid size %d", ErrInvalidSampleTable, bi.Offset, bi.Size)
			}
			sr.mdats = append(sr.mdats, byteRange{
				start: bi.Offset + bi.HeaderSize,
				end:   end,
			})
		case BoxTypeMoov():
			moovs = append(moovs, bi)
		}
	}

	if len(moovs) == 0 {
		return nil, fmt.Errorf("%w: moov box not found", ErrUnsupportedSampleLayout)
	}
	if len(moovs) > 1 {
		return nil, fmt.Errorf("%w: multiple moov boxes found", ErrUnsupportedSampleLayout)
	}

	// A movie fragment extension means that samples are stored in movie
	// fragments even if the file happens to contain no moof box (e.g. an
	// initialization segment).
	mvex, err := ExtractBox(r, moovs[0], BoxPath{BoxTypeMvex()})
	if err != nil {
		return nil, err
	}
	if len(mvex) > 0 {
		return nil, fmt.Errorf("%w: fragmented MP4 is not supported (mvex box at offset %d)", ErrUnsupportedSampleLayout, mvex[0].Offset)
	}

	traks, err := ExtractBox(r, moovs[0], BoxPath{BoxTypeTrak()})
	if err != nil {
		return nil, err
	}
	if len(traks) == 0 {
		return nil, fmt.Errorf("%w: moov box contains no trak box", ErrInvalidSampleTable)
	}

	for _, trak := range traks {
		track, err := sr.loadTrack(trak)
		if err != nil {
			return nil, err
		}
		if _, ok := sr.tracks[track.trackID]; ok {
			return nil, fmt.Errorf("%w: duplicate track ID %d", ErrInvalidSampleTable, track.trackID)
		}
		sr.tracks[track.trackID] = track
		sr.trackIDs = append(sr.trackIDs, track.trackID)
	}

	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("mp4: failed to seek: %w", err)
	}
	return sr, nil
}

// TrackIDs returns the track IDs in the order in which the track boxes appear
// in the moov box.
func (sr *SampleReader) TrackIDs() []uint32 {
	ids := make([]uint32, len(sr.trackIDs))
	copy(ids, sr.trackIDs)
	return ids
}

// SampleCount returns the number of samples in the track.
func (sr *SampleReader) SampleCount(trackID uint32) (uint32, error) {
	track, err := sr.track(trackID)
	if err != nil {
		return 0, err
	}
	return uint32(len(track.samples)), nil
}

// Timescale returns the media timescale of the track. DTS, PTS and Duration
// of the samples are expressed in this unit.
func (sr *SampleReader) Timescale(trackID uint32) (uint32, error) {
	track, err := sr.track(trackID)
	if err != nil {
		return 0, err
	}
	return track.timescale, nil
}

// Samples returns the metadata of every sample of the track in decode order.
// The sample payloads are not read.
func (sr *SampleReader) Samples(trackID uint32) ([]MediaSample, error) {
	track, err := sr.track(trackID)
	if err != nil {
		return nil, err
	}
	if len(track.samples) == 0 {
		return nil, fmt.Errorf("%w: track %d", ErrEmptyTrack, trackID)
	}
	samples := make([]MediaSample, len(track.samples))
	copy(samples, track.samples)
	return samples, nil
}

// ReadSample returns the metadata and the payload of the sample at index.
// Only the payload of this sample is read from the underlying reader.
func (sr *SampleReader) ReadSample(trackID uint32, index uint32) (*MediaSampleData, error) {
	track, err := sr.track(trackID)
	if err != nil {
		return nil, err
	}
	if len(track.samples) == 0 {
		return nil, fmt.Errorf("%w: track %d", ErrEmptyTrack, trackID)
	}
	if uint64(index) >= uint64(len(track.samples)) {
		return nil, fmt.Errorf("%w: track %d index %d (sample count %d)", ErrSampleIndexOutOfRange, trackID, index, len(track.samples))
	}
	sample := track.samples[index]
	data, err := sr.ReadSampleData(&sample)
	if err != nil {
		return nil, err
	}
	return &MediaSampleData{
		MediaSample: sample,
		Data:        data,
	}, nil
}

// ReadSampleData reads the payload of the given sample and returns it as a
// newly allocated byte slice.
func (sr *SampleReader) ReadSampleData(sample *MediaSample) ([]byte, error) {
	if sample == nil {
		return nil, errors.New("mp4: nil sample")
	}
	if err := sr.validateSampleRange(sample); err != nil {
		return nil, err
	}
	if uint64(sample.Size) > uint64(maxInt()) {
		return nil, fmt.Errorf("%w: sample size %d is too large", ErrSampleDataOutOfBounds, sample.Size)
	}
	data := make([]byte, int(sample.Size))
	if _, err := sr.readAt(sample.Offset, data); err != nil {
		return nil, err
	}
	return data, nil
}

// ReadSampleDataTo reads the payload of the given sample and writes it to w.
// It uses io.CopyN, so the payload is streamed instead of being buffered as a
// whole.
func (sr *SampleReader) ReadSampleDataTo(sample *MediaSample, w io.Writer) (int64, error) {
	if sample == nil {
		return 0, errors.New("mp4: nil sample")
	}
	if w == nil {
		return 0, errors.New("mp4: nil writer")
	}
	if err := sr.validateSampleRange(sample); err != nil {
		return 0, err
	}
	if sample.Size == 0 {
		return 0, nil
	}
	if sample.Offset > uint64(math.MaxInt64) {
		return 0, fmt.Errorf("%w: sample offset %d is too large", ErrSampleDataOutOfBounds, sample.Offset)
	}

	sr.mu.Lock()
	defer sr.mu.Unlock()
	if _, err := sr.r.Seek(int64(sample.Offset), io.SeekStart); err != nil {
		return 0, fmt.Errorf("mp4: failed to seek to sample payload: %w", err)
	}
	n, err := io.CopyN(w, sr.r, int64(sample.Size))
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return n, fmt.Errorf("%w: failed to read %d bytes at offset %d: %v", ErrTruncatedMdat, sample.Size, sample.Offset, err)
		}
		return n, err
	}
	return n, nil
}

func (sr *SampleReader) track(trackID uint32) (*sampleTrack, error) {
	track, ok := sr.tracks[trackID]
	if !ok {
		return nil, fmt.Errorf("%w: track ID %d", ErrTrackNotFound, trackID)
	}
	return track, nil
}

func (sr *SampleReader) readAt(offset uint64, p []byte) (int, error) {
	if offset > uint64(math.MaxInt64) {
		return 0, fmt.Errorf("%w: sample offset %d is too large", ErrSampleDataOutOfBounds, offset)
	}
	sr.mu.Lock()
	defer sr.mu.Unlock()
	if _, err := sr.r.Seek(int64(offset), io.SeekStart); err != nil {
		return 0, fmt.Errorf("mp4: failed to seek to sample payload: %w", err)
	}
	n, err := io.ReadFull(sr.r, p)
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return n, fmt.Errorf("%w: failed to read %d bytes at offset %d: %v", ErrTruncatedMdat, len(p), offset, err)
		}
		return n, err
	}
	return n, nil
}

func (sr *SampleReader) validateSampleRange(sample *MediaSample) error {
	end, ok := addUint64(sample.Offset, uint64(sample.Size))
	if !ok || end > uint64(math.MaxInt64) {
		return fmt.Errorf("%w: track %d sample %d: offset %d size %d", ErrSampleDataOutOfBounds, sample.TrackID, sample.Index, sample.Offset, sample.Size)
	}
	for _, mdat := range sr.mdats {
		if sample.Offset >= mdat.start && end <= mdat.end {
			return nil
		}
	}
	return fmt.Errorf("%w: track %d sample %d: range [%d,%d) is not inside any mdat payload", ErrSampleDataOutOfBounds, sample.TrackID, sample.Index, sample.Offset, end)
}

// loadTrack reads the sample tables of one trak box and builds the sample
// index of the track.
func (sr *SampleReader) loadTrack(trak *BoxInfo) (*sampleTrack, error) {
	bips, err := ExtractBoxesWithPayload(sr.r, trak, []BoxPath{
		{BoxTypeTkhd()},
		{BoxTypeMdia(), BoxTypeMdhd()},
		{BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeStsc()},
		{BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeStco()},
		{BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeCo64()},
		{BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeStsz()},
		{BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeStts()},
		{BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeCtts()},
		{BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeStss()},
	})
	if err != nil {
		return nil, err
	}

	var (
		tkhd *Tkhd
		mdhd *Mdhd
		stsc *Stsc
		stco *Stco
		co64 *Co64
		stsz *Stsz
		stts *Stts
		ctts *Ctts
		stss *Stss
	)
	for _, bip := range bips {
		switch bip.Info.Type {
		case BoxTypeTkhd():
			if tkhd != nil {
				return nil, fmt.Errorf("%w: multiple tkhd boxes", ErrInvalidSampleTable)
			}
			tkhd = bip.Payload.(*Tkhd)
		case BoxTypeMdhd():
			if mdhd != nil {
				return nil, fmt.Errorf("%w: multiple mdhd boxes", ErrInvalidSampleTable)
			}
			mdhd = bip.Payload.(*Mdhd)
		case BoxTypeStsc():
			if stsc != nil {
				return nil, fmt.Errorf("%w: multiple stsc boxes", ErrInvalidSampleTable)
			}
			stsc = bip.Payload.(*Stsc)
		case BoxTypeStco():
			if stco != nil {
				return nil, fmt.Errorf("%w: multiple stco boxes", ErrInvalidSampleTable)
			}
			stco = bip.Payload.(*Stco)
		case BoxTypeCo64():
			if co64 != nil {
				return nil, fmt.Errorf("%w: multiple co64 boxes", ErrInvalidSampleTable)
			}
			co64 = bip.Payload.(*Co64)
		case BoxTypeStsz():
			if stsz != nil {
				return nil, fmt.Errorf("%w: multiple stsz boxes", ErrInvalidSampleTable)
			}
			stsz = bip.Payload.(*Stsz)
		case BoxTypeStts():
			if stts != nil {
				return nil, fmt.Errorf("%w: multiple stts boxes", ErrInvalidSampleTable)
			}
			stts = bip.Payload.(*Stts)
		case BoxTypeCtts():
			if ctts != nil {
				return nil, fmt.Errorf("%w: multiple ctts boxes", ErrInvalidSampleTable)
			}
			ctts = bip.Payload.(*Ctts)
		case BoxTypeStss():
			if stss != nil {
				return nil, fmt.Errorf("%w: multiple stss boxes", ErrInvalidSampleTable)
			}
			stss = bip.Payload.(*Stss)
		}
	}

	if tkhd == nil {
		return nil, fmt.Errorf("%w: tkhd box not found", ErrInvalidSampleTable)
	}
	if mdhd == nil {
		return nil, fmt.Errorf("%w: track %d: mdhd box not found", ErrInvalidSampleTable, tkhd.TrackID)
	}

	// stz2 stores sample sizes in a different layout. It is rejected
	// explicitly instead of being silently treated as an empty track.
	stz2, err := ExtractBox(sr.r, trak, BoxPath{BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), StrToBoxType("stz2")})
	if err != nil {
		return nil, err
	}
	if len(stz2) > 0 {
		return nil, fmt.Errorf("%w: track %d: stz2 sample size box is not supported", ErrUnsupportedSampleLayout, tkhd.TrackID)
	}

	samples, err := sr.buildSamples(tkhd.TrackID, stsc, stco, co64, stsz, stts, ctts, stss)
	if err != nil {
		return nil, err
	}

	return &sampleTrack{
		trackID:   tkhd.TrackID,
		timescale: mdhd.Timescale,
		samples:   samples,
	}, nil
}

// buildSamples joins stsc, stco/co64, stsz, stts, ctts and stss into a flat
// sample index.
func (sr *SampleReader) buildSamples(
	trackID uint32,
	stsc *Stsc,
	stco *Stco,
	co64 *Co64,
	stsz *Stsz,
	stts *Stts,
	ctts *Ctts,
	stss *Stss,
) ([]MediaSample, error) {
	invalid := func(format string, args ...interface{}) error {
		return fmt.Errorf("%w: track %d: %s", ErrInvalidSampleTable, trackID, fmt.Sprintf(format, args...))
	}

	if stsz == nil {
		return nil, invalid("stsz box not found")
	}
	sampleCount := stsz.SampleCount
	hasConstantSize := stsz.SampleSize != 0
	if !hasConstantSize && uint64(len(stsz.EntrySize)) != uint64(sampleCount) {
		return nil, invalid("stsz declares %d samples but has %d sample sizes", sampleCount, len(stsz.EntrySize))
	}

	var chunkOffsets []uint64
	switch {
	case stco != nil && co64 != nil:
		return nil, invalid("both stco and co64 boxes found")
	case stco != nil:
		chunkOffsets = make([]uint64, len(stco.ChunkOffset))
		for i, offset := range stco.ChunkOffset {
			chunkOffsets[i] = uint64(offset)
		}
	case co64 != nil:
		chunkOffsets = make([]uint64, len(co64.ChunkOffset))
		copy(chunkOffsets, co64.ChunkOffset)
	}

	if sampleCount == 0 {
		return nil, sr.validateEmptyTrack(trackID, stts, ctts, stsc, chunkOffsets)
	}

	if stts == nil {
		return nil, invalid("stts box not found")
	}
	if stsc == nil {
		return nil, invalid("stsc box not found")
	}
	if stco == nil && co64 == nil {
		return nil, invalid("stco/co64 box not found")
	}

	// Validate stts and reject tracks whose decode timeline cannot be
	// represented by the int64 presentation timestamps before allocating the
	// sample index.
	var sttsCount uint64
	var totalDuration uint64
	for _, entry := range stts.Entries {
		add := uint64(entry.SampleCount) * uint64(entry.SampleDelta)
		if totalDuration > math.MaxUint64-add {
			return nil, fmt.Errorf("%w: track %d: total duration overflows uint64", ErrSampleTimeOverflow, trackID)
		}
		totalDuration += add
		if sttsCount > math.MaxUint64-uint64(entry.SampleCount) {
			return nil, invalid("stts sample count overflows")
		}
		sttsCount += uint64(entry.SampleCount)
	}
	if sttsCount != uint64(sampleCount) {
		return nil, invalid("stts declares %d samples but stsz declares %d", sttsCount, sampleCount)
	}
	if totalDuration > uint64(math.MaxInt64) {
		return nil, fmt.Errorf("%w: track %d: total duration %d exceeds %d", ErrSampleTimeOverflow, trackID, totalDuration, int64(math.MaxInt64))
	}

	if ctts != nil {
		var cttsCount uint64
		for _, entry := range ctts.Entries {
			if cttsCount > math.MaxUint64-uint64(entry.SampleCount) {
				return nil, invalid("ctts sample count overflows")
			}
			cttsCount += uint64(entry.SampleCount)
		}
		if cttsCount != uint64(sampleCount) {
			return nil, invalid("ctts declares %d samples but stsz declares %d", cttsCount, sampleCount)
		}
	}

	var syncSamples map[uint32]struct{}
	if stss != nil {
		syncSamples = make(map[uint32]struct{}, len(stss.SampleNumber))
		for _, number := range stss.SampleNumber {
			if number == 0 || number > sampleCount {
				return nil, invalid("stss sample number %d is out of range (sample count %d)", number, sampleCount)
			}
			syncSamples[number] = struct{}{}
		}
	}

	// Validate stsc and count the samples mapped onto the chunks.
	if len(stsc.Entries) == 0 {
		return nil, invalid("stsc box has no entry")
	}
	if stsc.Entries[0].FirstChunk != 1 {
		return nil, invalid("stsc first entry starts at chunk %d instead of 1", stsc.Entries[0].FirstChunk)
	}
	var mappedSamples uint64
	prevFirstChunk := uint32(0)
	for i := range stsc.Entries {
		entry := &stsc.Entries[i]
		if entry.SamplesPerChunk == 0 {
			return nil, invalid("stsc entry %d has no sample per chunk", i)
		}
		if entry.FirstChunk <= prevFirstChunk {
			return nil, invalid("stsc entry %d starts at chunk %d after chunk %d", i, entry.FirstChunk, prevFirstChunk)
		}
		if uint64(entry.FirstChunk) > uint64(len(chunkOffsets)) {
			return nil, invalid("stsc entry %d starts at chunk %d but only %d chunks exist", i, entry.FirstChunk, len(chunkOffsets))
		}
		prevFirstChunk = entry.FirstChunk

		endChunk := uint64(len(chunkOffsets)) + 1
		if i+1 < len(stsc.Entries) {
			endChunk = uint64(stsc.Entries[i+1].FirstChunk)
		}
		chunks := endChunk - uint64(entry.FirstChunk)
		// chunks and SamplesPerChunk are both at most MaxUint32, so the
		// product fits into uint64.
		add := chunks * uint64(entry.SamplesPerChunk)
		if mappedSamples > math.MaxUint64-add {
			return nil, invalid("stsc sample count overflows")
		}
		mappedSamples += add
	}
	if mappedSamples != uint64(sampleCount) {
		return nil, invalid("stsc maps %d samples but stsz declares %d", mappedSamples, sampleCount)
	}

	if uint64(sampleCount) > uint64(maxInt()) {
		return nil, invalid("too many samples: %d", sampleCount)
	}

	samples := make([]MediaSample, 0, int(sampleCount))

	var (
		sampleIndex  uint32
		dts          uint64
		sttsEntry    int
		sttsRemain   uint32
		cttsEntry    int
		cttsRemain   uint32
		stscEntryIdx int
	)
	nextDuration := func() (uint32, error) {
		for sttsRemain == 0 {
			for sttsEntry < len(stts.Entries) && stts.Entries[sttsEntry].SampleCount == 0 {
				sttsEntry++
			}
			if sttsEntry >= len(stts.Entries) {
				return 0, invalid("stts entries ended before sample %d", sampleIndex)
			}
			sttsRemain = stts.Entries[sttsEntry].SampleCount
		}
		return stts.Entries[sttsEntry].SampleDelta, nil
	}
	nextCompositionOffset := func() (int64, error) {
		if ctts == nil {
			return 0, nil
		}
		for cttsRemain == 0 {
			for cttsEntry < len(ctts.Entries) && ctts.Entries[cttsEntry].SampleCount == 0 {
				cttsEntry++
			}
			if cttsEntry >= len(ctts.Entries) {
				return 0, invalid("ctts entries ended before sample %d", sampleIndex)
			}
			cttsRemain = ctts.Entries[cttsEntry].SampleCount
		}
		return ctts.GetSampleOffset(cttsEntry), nil
	}

	for chunkIndex, chunkOffset := range chunkOffsets {
		chunkNumber := uint64(chunkIndex) + 1
		for stscEntryIdx+1 < len(stsc.Entries) && uint64(stsc.Entries[stscEntryIdx+1].FirstChunk) <= chunkNumber {
			stscEntryIdx++
		}
		samplesPerChunk := stsc.Entries[stscEntryIdx].SamplesPerChunk
		offset := chunkOffset
		for i := uint32(0); i < samplesPerChunk; i++ {
			if sampleIndex >= sampleCount {
				return nil, invalid("chunks contain more samples than stsz declares (%d)", sampleCount)
			}

			var size uint32
			if hasConstantSize {
				size = stsz.SampleSize
			} else {
				size = stsz.EntrySize[sampleIndex]
			}

			duration, err := nextDuration()
			if err != nil {
				return nil, err
			}
			sttsRemain--
			if sttsRemain == 0 {
				sttsEntry++
			}

			compositionOffset, err := nextCompositionOffset()
			if err != nil {
				return nil, err
			}
			if ctts != nil {
				cttsRemain--
				if cttsRemain == 0 {
					cttsEntry++
				}
			}

			if dts > math.MaxUint64-uint64(duration) {
				return nil, fmt.Errorf("%w: track %d: decode time overflows uint64", ErrSampleTimeOverflow, trackID)
			}
			pts, err := composePTS(dts, compositionOffset)
			if err != nil {
				return nil, fmt.Errorf("%w: track %d sample %d: DTS %d and composition offset %d: %v", ErrSampleTimeOverflow, trackID, sampleIndex, dts, compositionOffset, err)
			}

			sync := syncSamples == nil
			if !sync {
				_, sync = syncSamples[sampleIndex+1]
			}

			samples = append(samples, MediaSample{
				TrackID:  trackID,
				Index:    sampleIndex,
				Offset:   offset,
				Size:     size,
				DTS:      dts,
				PTS:      pts,
				Duration: duration,
				Sync:     sync,
			})

			nextOffset, ok := addUint64(offset, uint64(size))
			if !ok {
				return nil, fmt.Errorf("%w: track %d sample %d: chunk offset %d and size %d overflow", ErrSampleDataOutOfBounds, trackID, sampleIndex, offset, size)
			}
			offset = nextOffset

			dts += uint64(duration)
			sampleIndex++
		}
	}

	if sampleIndex != sampleCount {
		return nil, invalid("chunks contain %d samples but stsz declares %d", sampleIndex, sampleCount)
	}

	for i := range samples {
		if err := sr.validateSampleRange(&samples[i]); err != nil {
			return nil, err
		}
	}

	return samples, nil
}

// validateEmptyTrack checks that no table which is present declares samples
// for an empty track.
func (sr *SampleReader) validateEmptyTrack(trackID uint32, stts *Stts, ctts *Ctts, stsc *Stsc, chunkOffsets []uint64) error {
	invalid := func(format string, args ...interface{}) error {
		return fmt.Errorf("%w: track %d: %s", ErrInvalidSampleTable, trackID, fmt.Sprintf(format, args...))
	}
	if stts != nil {
		for _, entry := range stts.Entries {
			if entry.SampleCount != 0 {
				return invalid("stsz declares no sample but stts declares samples")
			}
		}
	}
	if ctts != nil {
		for _, entry := range ctts.Entries {
			if entry.SampleCount != 0 {
				return invalid("stsz declares no sample but ctts declares samples")
			}
		}
	}
	if stsc != nil && len(stsc.Entries) != 0 {
		// stsc entries alone are harmless: without any chunk offset they do
		// not map a sample, so they are not treated as an inconsistency.
		for _, entry := range stsc.Entries {
			if entry.FirstChunk == 0 {
				return invalid("stsc entry has invalid first chunk 0")
			}
		}
	}
	if len(chunkOffsets) != 0 {
		return invalid("stsz declares no sample but stco/co64 has entries")
	}
	return nil
}

func composePTS(dts uint64, compositionOffset int64) (int64, error) {
	if compositionOffset >= 0 {
		offset := uint64(compositionOffset)
		if dts > uint64(math.MaxInt64)-offset {
			return 0, ErrSampleTimeOverflow
		}
		return int64(dts + offset), nil
	}
	// Composition offsets come from a 32-bit field, so the magnitude fits
	// into a uint64 even for math.MinInt64. The result of -int64(diff) can
	// represent every value down to math.MinInt64.
	magnitude := uint64(-(compositionOffset + 1)) + 1
	if dts >= magnitude {
		value := dts - magnitude
		if value > uint64(math.MaxInt64) {
			return 0, ErrSampleTimeOverflow
		}
		return int64(value), nil
	}
	diff := magnitude - dts
	if diff > uint64(math.MaxInt64) {
		// Only diff == 1<<63 gives math.MinInt64, which is representable.
		if diff == uint64(math.MaxInt64)+1 {
			return math.MinInt64, nil
		}
		return 0, ErrSampleTimeOverflow
	}
	return -int64(diff), nil
}

func addUint64(a, b uint64) (uint64, bool) {
	sum := a + b
	return sum, sum >= a
}

func maxInt() int {
	return int(^uint(0) >> 1)
}
