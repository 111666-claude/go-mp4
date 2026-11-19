package mp4

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// memWS is an in-memory io.WriteSeeker used to build test fixtures.
type memWS struct {
	buf []byte
	off int64
}

func (m *memWS) Write(p []byte) (int, error) {
	end := m.off + int64(len(p))
	if end > int64(len(m.buf)) {
		nb := make([]byte, end)
		copy(nb, m.buf)
		m.buf = nb
	}
	copy(m.buf[m.off:], p)
	m.off = end
	return len(p), nil
}

func (m *memWS) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		m.off = offset
	case io.SeekCurrent:
		m.off += offset
	case io.SeekEnd:
		m.off = int64(len(m.buf)) + offset
	}
	return m.off, nil
}

func marshalBox(t *testing.T, w *Writer, boxType BoxType, payload IBox) {
	t.Helper()
	_, err := w.StartBox(&BoxInfo{Type: boxType})
	require.NoError(t, err)
	_, err = Marshal(w, payload, Context{})
	require.NoError(t, err)
	_, err = w.EndBox()
	require.NoError(t, err)
}

// rebaseTestSample describes one sample of a synthetic fragment.
type rebaseTestSample struct {
	duration uint32
	flags    uint32
	cto      int32
	data     []byte
}

// rebaseTestFragOpts describes how a synthetic fragment is encoded.
type rebaseTestFragOpts struct {
	seq     uint32
	trackID uint32

	tfdtVersion int // -1 omits the tfdt box
	baseTime    uint64

	tfhdDefaultDuration uint32 // written when non-zero
	tfhdDefaultFlags    uint32 // written when non-zero
	tfhdBaseDataOffset  bool   // include a (placeholder) base-data-offset

	trunPerSampleDuration bool
	trunPerSampleFlags    bool

	unknownInMoof []byte // payload of an unknown box placed inside the moof
	samples       []rebaseTestSample
}

func buildInitSegment(t *testing.T, trackID uint32, trexDefaultDuration uint32) []byte {
	t.Helper()
	m := &memWS{}
	w := NewWriter(m)
	marshalBox(t, w, BoxTypeFtyp(), &Ftyp{
		MajorBrand:   [4]byte{'c', 'm', 'f', 'c'},
		MinorVersion: 0,
		CompatibleBrands: []CompatibleBrandElem{
			{CompatibleBrand: [4]byte{'c', 'm', 'f', 'c'}},
			{CompatibleBrand: [4]byte{'i', 's', 'o', 'm'}},
		},
	})
	_, err := w.StartBox(&BoxInfo{Type: BoxTypeMoov()})
	require.NoError(t, err)
	_, err = w.StartBox(&BoxInfo{Type: BoxTypeMvex()})
	require.NoError(t, err)
	marshalBox(t, w, BoxTypeTrex(), &Trex{
		TrackID:                       trackID,
		DefaultSampleDescriptionIndex: 1,
		DefaultSampleDuration:         trexDefaultDuration,
		DefaultSampleSize:             0,
		DefaultSampleFlags:            0,
	})
	_, err = w.EndBox() // mvex
	require.NoError(t, err)
	_, err = w.EndBox() // moov
	require.NoError(t, err)
	return m.buf
}

// buildMoof builds the moof box; trunDataOffset is written into the trun.
func buildMoof(t *testing.T, opts rebaseTestFragOpts, trunDataOffset int32) []byte {
	t.Helper()
	m := &memWS{}
	w := NewWriter(m)
	_, err := w.StartBox(&BoxInfo{Type: BoxTypeMoof()})
	require.NoError(t, err)

	marshalBox(t, w, BoxTypeMfhd(), &Mfhd{SequenceNumber: opts.seq})

	_, err = w.StartBox(&BoxInfo{Type: BoxTypeTraf()})
	require.NoError(t, err)

	tfhd := &Tfhd{TrackID: opts.trackID}
	tfhd.AddFlag(TfhdDefaultBaseIsMoof)
	if opts.tfhdBaseDataOffset {
		tfhd.AddFlag(TfhdBaseDataOffsetPresent)
		tfhd.BaseDataOffset = 0xdeadbeef // placeholder, must be rebased
	}
	if opts.tfhdDefaultDuration != 0 {
		tfhd.AddFlag(TfhdDefaultSampleDurationPresent)
		tfhd.DefaultSampleDuration = opts.tfhdDefaultDuration
	}
	if opts.tfhdDefaultFlags != 0 {
		tfhd.AddFlag(TfhdDefaultSampleFlagsPresent)
		tfhd.DefaultSampleFlags = opts.tfhdDefaultFlags
	}
	marshalBox(t, w, BoxTypeTfhd(), tfhd)

	if opts.tfdtVersion >= 0 {
		tfdt := &Tfdt{}
		if opts.tfdtVersion == 0 {
			tfdt.SetVersion(0)
			tfdt.BaseMediaDecodeTimeV0 = uint32(opts.baseTime)
		} else {
			tfdt.SetVersion(1)
			tfdt.BaseMediaDecodeTimeV1 = opts.baseTime
		}
		marshalBox(t, w, BoxTypeTfdt(), tfdt)
	}

	trun := &Trun{SampleCount: uint32(len(opts.samples))}
	trun.SetVersion(1)
	trun.AddFlag(trunDataOffsetPresent)
	trun.DataOffset = trunDataOffset
	if opts.trunPerSampleDuration {
		trun.AddFlag(trunSampleDurationPresent)
	}
	trun.AddFlag(trunSampleSizePresent)
	if opts.trunPerSampleFlags {
		trun.AddFlag(trunSampleFlagsPresent)
	}
	trun.AddFlag(trunSampleCompositionTimeOffsetPresent)
	for _, s := range opts.samples {
		e := TrunEntry{
			SampleSize:                    uint32(len(s.data)),
			SampleCompositionTimeOffsetV1: s.cto,
		}
		if opts.trunPerSampleDuration {
			e.SampleDuration = s.duration
		}
		if opts.trunPerSampleFlags {
			e.SampleFlags = s.flags
		}
		trun.Entries = append(trun.Entries, e)
	}
	marshalBox(t, w, BoxTypeTrun(), trun)

	_, err = w.EndBox() // traf
	require.NoError(t, err)

	if opts.unknownInMoof != nil {
		_, err := w.StartBox(&BoxInfo{Type: StrToBoxType("zzyz")})
		require.NoError(t, err)
		_, err = w.Write(opts.unknownInMoof)
		require.NoError(t, err)
		_, err = w.EndBox()
		require.NoError(t, err)
	}

	_, err = w.EndBox() // moof
	require.NoError(t, err)
	return m.buf
}

// buildFragment builds one moof+mdat fragment with a correct trun data offset.
func buildFragment(t *testing.T, opts rebaseTestFragOpts) []byte {
	t.Helper()
	// first pass to measure the moof size, second pass with the real offset
	moof := buildMoof(t, opts, 0)
	moof = buildMoof(t, opts, int32(len(moof))+8)

	var mdatPayload []byte
	for _, s := range opts.samples {
		mdatPayload = append(mdatPayload, s.data...)
	}
	mdatHeader := make([]byte, 8)
	binary.BigEndian.PutUint32(mdatHeader, uint32(len(mdatPayload))+8)
	copy(mdatHeader[4:], "mdat")

	out := append([]byte{}, moof...)
	out = append(out, mdatHeader...)
	out = append(out, mdatPayload...)
	return out
}

// parsedFragment is the result of walking one rebased moof in the output.
type parsedFragment struct {
	moofOffset uint64
	seq        uint32
	trackID    uint32
	tfhd       *Tfhd
	baseTime   uint64
	truns      []*Trun
	trunInfos  []BoxInfo
}

// walkOutput parses init+fragments produced by FragmentRebaser.
func walkOutput(t *testing.T, data []byte, initLen int) (initBytes []byte, frags []parsedFragment) {
	t.Helper()
	initBytes = data[:initLen]
	boxes, err := splitBoxes(data[initLen:])
	require.NoError(t, err)
	for _, b := range boxes {
		if b.info.Type != BoxTypeMoof() {
			continue
		}
		pf := parsedFragment{moofOffset: uint64(initLen) + b.info.Offset}
		children, err := splitBoxes(b.payload())
		require.NoError(t, err)
		for _, c := range children {
			switch c.info.Type {
			case BoxTypeMfhd():
				box, _, err := UnmarshalAny(bytes.NewReader(c.payload()), BoxTypeMfhd(),
					c.info.Size-c.info.HeaderSize, Context{})
				require.NoError(t, err)
				pf.seq = box.(*Mfhd).SequenceNumber
			case BoxTypeTraf():
				grands, err := splitBoxes(c.payload())
				require.NoError(t, err)
				for _, g := range grands {
					switch g.info.Type {
					case BoxTypeTfhd():
						box, _, err := UnmarshalAny(bytes.NewReader(g.payload()), BoxTypeTfhd(),
							g.info.Size-g.info.HeaderSize, Context{})
						require.NoError(t, err)
						pf.tfhd = box.(*Tfhd)
						pf.trackID = pf.tfhd.TrackID
					case BoxTypeTfdt():
						box, _, err := UnmarshalAny(bytes.NewReader(g.payload()), BoxTypeTfdt(),
							g.info.Size-g.info.HeaderSize, Context{})
						require.NoError(t, err)
						pf.baseTime = box.(*Tfdt).GetBaseMediaDecodeTime()
					case BoxTypeTrun():
						box, _, err := UnmarshalAny(bytes.NewReader(g.payload()), BoxTypeTrun(),
							g.info.Size-g.info.HeaderSize, Context{})
						require.NoError(t, err)
						pf.truns = append(pf.truns, box.(*Trun))
						pf.trunInfos = append(pf.trunInfos, g.info)
					}
				}
			}
		}
		frags = append(frags, pf)
	}
	return initBytes, frags
}

// sampleBytesAt reads size bytes of sample data addressed by a moof-relative offset.
func sampleBytesAt(t *testing.T, output []byte, moofOffset uint64, dataOffset int32, pos, size uint64) []byte {
	t.Helper()
	off := int64(moofOffset) + int64(dataOffset) + int64(pos)
	require.GreaterOrEqual(t, off, int64(0))
	require.LessOrEqual(t, off+int64(size), int64(len(output)))
	return output[off : off+int64(size)]
}

func TestFragmentRebaserConcatenate(t *testing.T) {
	const trackID = 1
	init := buildInitSegment(t, trackID, 512)

	samplesA := []rebaseTestSample{
		{duration: 1000, flags: 0x02000000, cto: 5, data: []byte("AAAA")},
		{duration: 1000, flags: 0x01010000, cto: 6, data: []byte("BBBBB")},
		{duration: 1000, flags: 0x01010000, cto: 7, data: []byte("CCC")},
	}
	samplesB := []rebaseTestSample{
		{duration: 512, flags: 0x02000000, cto: 1, data: []byte("dd")},
		{duration: 512, flags: 0x01010000, cto: 2, data: []byte("eee")},
	}
	samplesC := []rebaseTestSample{
		{duration: 512, flags: 0x02000000, cto: 0, data: []byte("F")},
		{duration: 512, flags: 0x01010000, cto: 3, data: []byte("GG")},
	}
	unknownPayload := []byte{0xde, 0xad, 0xbe, 0xef, 0x42}

	// fragment A: tfdt v1, per-sample durations, base time starts at 1000
	fragA := buildFragment(t, rebaseTestFragOpts{
		seq:                   7,
		trackID:               trackID,
		tfdtVersion:           1,
		baseTime:              1000,
		trunPerSampleDuration: true,
		trunPerSampleFlags:    true,
		samples:               samplesA,
	})
	// fragment B: tfdt v0 with a backwards timestamp, tfhd default duration,
	// an unknown box inside the moof
	fragB := buildFragment(t, rebaseTestFragOpts{
		seq:                 3, // lower than A's: must be renumbered
		trackID:             trackID,
		tfdtVersion:         0,
		baseTime:            900, // goes backwards: must be rebased
		tfhdDefaultDuration: 512,
		tfhdDefaultFlags:    0x01010000,
		trunPerSampleFlags:  false,
		unknownInMoof:       unknownPayload,
		samples:             samplesB,
	})
	// fragment C: no tfdt at all (one must be inserted), tfhd base-data-offset,
	// durations resolved from trex
	fragC := buildFragment(t, rebaseTestFragOpts{
		seq:                4,
		trackID:            trackID,
		tfdtVersion:        -1,
		tfhdBaseDataOffset: true,
		trunPerSampleFlags: true,
		samples:            samplesC,
	})

	var out bytes.Buffer
	rb := NewFragmentRebaser(&out)
	require.NoError(t, rb.WriteInitSegment(bytes.NewReader(init)))
	require.NoError(t, rb.AppendFragment(bytes.NewReader(fragA)))
	require.NoError(t, rb.AppendFragment(bytes.NewReader(fragB)))
	require.NoError(t, rb.AppendFragment(bytes.NewReader(fragC)))
	output := out.Bytes()

	// the init segment is copied verbatim
	initOut, frags := walkOutput(t, output, len(init))
	assert.Equal(t, init, initOut)
	require.Len(t, frags, 3)

	// consecutive sequence numbers starting from 0
	assert.Equal(t, uint32(0), frags[0].seq)
	assert.Equal(t, uint32(1), frags[1].seq)
	assert.Equal(t, uint32(2), frags[2].seq)

	// continuous decode times: A keeps its origin, B and C accumulate
	// durations 3000 (A), 1024 (B), 1024 (C)
	assert.Equal(t, uint64(1000), frags[0].baseTime)
	assert.Equal(t, uint64(4000), frags[1].baseTime)
	assert.Equal(t, uint64(5024), frags[2].baseTime)

	// tfhd defaults survive
	assert.True(t, frags[1].tfhd.CheckFlag(TfhdDefaultSampleDurationPresent))
	assert.Equal(t, uint32(512), frags[1].tfhd.DefaultSampleDuration)
	assert.Equal(t, uint32(0x01010000), frags[1].tfhd.DefaultSampleFlags)

	// tfhd base-data-offset of fragment C points at its own moof
	require.True(t, frags[2].tfhd.CheckFlag(TfhdBaseDataOffsetPresent))
	assert.Equal(t, frags[2].moofOffset, frags[2].tfhd.BaseDataOffset)

	// per-sample data is preserved, decode times are continuous and the
	// trun data offsets address the samples correctly
	allSamples := [][]rebaseTestSample{samplesA, samplesB, samplesC}
	wantDTS := []uint64{1000, 2000, 3000, 4000, 4512, 5024, 5536}
	hash := sha256.New()
	sampleIdx := 0
	for i, pf := range frags {
		require.Len(t, pf.truns, 1)
		trun := pf.truns[0]
		require.Len(t, trun.Entries, len(allSamples[i]))
		dts := pf.baseTime
		var pos uint64
		for j, s := range allSamples[i] {
			e := trun.Entries[j]
			// decode time continuity
			assert.Equal(t, wantDTS[sampleIdx], dts, "fragment %d sample %d", i, j)
			dts += uint64(s.duration)
			// duration, flags and cto preserved
			if i == 0 { // only fragment A carries per-sample durations
				assert.Equal(t, s.duration, e.SampleDuration, "fragment %d sample %d", i, j)
			}
			if i != 1 { // fragment B resolves flags via tfhd default
				assert.Equal(t, s.flags, e.SampleFlags, "fragment %d sample %d", i, j)
			}
			assert.Equal(t, int64(s.cto), trun.GetSampleCompositionTimeOffset(j))
			// sample bytes reachable via the rebased data offset
			got := sampleBytesAt(t, output, pf.moofOffset, trun.DataOffset, pos, uint64(len(s.data)))
			assert.Equal(t, s.data, got, "fragment %d sample %d", i, j)
			hash.Write(s.data)
			pos += uint64(len(s.data))
			sampleIdx++
		}
	}

	// byte hash of the reordered sample stream
	wantHash := sha256.New()
	for _, samples := range allSamples {
		for _, s := range samples {
			wantHash.Write(s.data)
		}
	}
	assert.Equal(t, wantHash.Sum(nil), hash.Sum(nil))

	// the unknown box inside fragment B's moof is preserved
	assert.Contains(t, string(output), string(append([]byte("zzyz"), unknownPayload...)))
}

func TestFragmentRebaserDefaultCombinations(t *testing.T) {
	// The same logical fragment (2 samples, duration 300 each) encoded with
	// different trun/tfhd/trex default combinations must rebase to the same
	// decode times and the same sample bytes.
	const trackID = 1
	samples := []rebaseTestSample{
		{duration: 300, flags: 0x02000000, cto: 0, data: []byte("xy")},
		{duration: 300, flags: 0x01010000, cto: 1, data: []byte("z")},
	}
	first := buildFragment(t, rebaseTestFragOpts{
		seq:                   0,
		trackID:               trackID,
		tfdtVersion:           1,
		baseTime:              0,
		trunPerSampleDuration: true,
		samples: []rebaseTestSample{
			{duration: 700, data: []byte("first")},
		},
	})

	cases := []struct {
		name string
		init bool // write init segment carrying trex default 300
		opts rebaseTestFragOpts
	}{
		{
			name: "trun carries durations",
			opts: rebaseTestFragOpts{trunPerSampleDuration: true},
		},
		{
			name: "tfhd default duration",
			opts: rebaseTestFragOpts{tfhdDefaultDuration: 300},
		},
		{
			name: "trex default duration",
			init: true,
			opts: rebaseTestFragOpts{},
		},
		{
			name: "no tfdt, tfhd default duration",
			opts: rebaseTestFragOpts{tfdtVersion: -1, tfhdDefaultDuration: 300},
		},
	}

	wantHash := sha256.New()
	for _, s := range samples {
		wantHash.Write(s.data)
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			opts := c.opts
			opts.seq = 0
			opts.trackID = trackID
			if c.opts.tfdtVersion != -1 {
				opts.tfdtVersion = 1
				opts.baseTime = 12345 // arbitrary, must be rebased to 700
			}
			opts.samples = samples
			frag := buildFragment(t, opts)

			var out bytes.Buffer
			rb := NewFragmentRebaser(&out)
			initLen := 0
			if c.init {
				init := buildInitSegment(t, trackID, 300)
				require.NoError(t, rb.WriteInitSegment(bytes.NewReader(init)))
				initLen = len(init)
			}
			require.NoError(t, rb.AppendFragment(bytes.NewReader(first)))
			require.NoError(t, rb.AppendFragment(bytes.NewReader(frag)))

			_, frags := walkOutput(t, out.Bytes(), initLen)
			require.Len(t, frags, 2)
			// second fragment continues at 700 regardless of the encoding
			assert.Equal(t, uint64(700), frags[1].baseTime)
			assert.Equal(t, uint32(1), frags[1].seq)

			// sample bytes identical across encodings
			hash := sha256.New()
			trun := frags[1].truns[0]
			var pos uint64
			for _, s := range samples {
				got := sampleBytesAt(t, out.Bytes(), frags[1].moofOffset, trun.DataOffset, pos, uint64(len(s.data)))
				hash.Write(got)
				pos += uint64(len(s.data))
			}
			assert.Equal(t, wantHash.Sum(nil), hash.Sum(nil))
		})
	}
}

func TestFragmentRebaserErrors(t *testing.T) {
	const trackID = 1

	simpleFrag := func(trackID uint32) []byte {
		return buildFragment(t, rebaseTestFragOpts{
			seq:                   0,
			trackID:               trackID,
			tfdtVersion:           1,
			baseTime:              0,
			trunPerSampleDuration: true,
			samples:               []rebaseTestSample{{duration: 100, data: []byte("q")}},
		})
	}

	t.Run("track mismatch", func(t *testing.T) {
		var out bytes.Buffer
		rb := NewFragmentRebaser(&out)
		require.NoError(t, rb.AppendFragment(bytes.NewReader(simpleFrag(1))))
		err := rb.AppendFragment(bytes.NewReader(simpleFrag(2)))
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrFragmentTrackMismatch))
		var ferr *FragmentError
		require.True(t, errors.As(err, &ferr))
		assert.Equal(t, 1, ferr.Index)
		assert.Contains(t, err.Error(), "fragment 1")
	})

	t.Run("track not in init segment", func(t *testing.T) {
		var out bytes.Buffer
		rb := NewFragmentRebaser(&out)
		require.NoError(t, rb.WriteInitSegment(bytes.NewReader(buildInitSegment(t, 9, 100))))
		err := rb.AppendFragment(bytes.NewReader(simpleFrag(1)))
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrFragmentTrackMismatch))
	})

	t.Run("missing duration", func(t *testing.T) {
		// no init segment, no tfhd default, no per-sample durations
		frag := buildFragment(t, rebaseTestFragOpts{
			seq:         0,
			trackID:     trackID,
			tfdtVersion: 1,
			samples:     []rebaseTestSample{{data: []byte("q")}},
		})
		var out bytes.Buffer
		rb := NewFragmentRebaser(&out)
		err := rb.AppendFragment(bytes.NewReader(frag))
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrFragmentMissingDuration))
		var ferr *FragmentError
		require.True(t, errors.As(err, &ferr))
		assert.Equal(t, 0, ferr.Index)
	})

	t.Run("tfdt version 0 overflow", func(t *testing.T) {
		frag0 := buildFragment(t, rebaseTestFragOpts{
			seq:                   0,
			trackID:               trackID,
			tfdtVersion:           0,
			baseTime:              math.MaxUint32 - 10,
			trunPerSampleDuration: true,
			samples:               []rebaseTestSample{{duration: 100, data: []byte("q")}},
		})
		var out bytes.Buffer
		rb := NewFragmentRebaser(&out)
		require.NoError(t, rb.AppendFragment(bytes.NewReader(frag0)))
		err := rb.AppendFragment(bytes.NewReader(frag0)) // base time no longer fits v0
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrFragmentTimeOverflow))
		var ferr *FragmentError
		require.True(t, errors.As(err, &ferr))
		assert.Equal(t, 1, ferr.Index)
	})

	t.Run("data offset overflow", func(t *testing.T) {
		// tfdt is missing so a 16 byte tfdt is inserted; a data offset near
		// math.MaxInt32 then overflows
		opts := rebaseTestFragOpts{
			seq:                   0,
			trackID:               trackID,
			tfdtVersion:           -1,
			trunPerSampleDuration: true,
			samples:               []rebaseTestSample{{duration: 100, data: []byte("q")}},
		}
		moof := buildMoof(t, opts, math.MaxInt32-4)
		frag := append([]byte{}, moof...)
		frag = append(frag, 0, 0, 0, 9, 'm', 'd', 'a', 't', 'q')
		var out bytes.Buffer
		rb := NewFragmentRebaser(&out)
		err := rb.AppendFragment(bytes.NewReader(frag))
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrFragmentOffsetOverflow))
	})

	t.Run("corrupt fragment", func(t *testing.T) {
		frag := simpleFrag(trackID)
		var out bytes.Buffer
		rb := NewFragmentRebaser(&out)
		err := rb.AppendFragment(bytes.NewReader(frag[:len(frag)/2]))
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrFragmentCorrupt))
		var ferr *FragmentError
		require.True(t, errors.As(err, &ferr))
		assert.Equal(t, 0, ferr.Index)
	})

	t.Run("interrupted input", func(t *testing.T) {
		r := io.MultiReader(bytes.NewReader(simpleFrag(trackID)[:16]),
			&errorReader{err: errors.New("connection reset")})
		var out bytes.Buffer
		rb := NewFragmentRebaser(&out)
		err := rb.AppendFragment(r)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "fragment 0")
		assert.Contains(t, err.Error(), "connection reset")
	})

	t.Run("missing moof", func(t *testing.T) {
		var out bytes.Buffer
		rb := NewFragmentRebaser(&out)
		err := rb.AppendFragment(bytes.NewReader([]byte{0, 0, 0, 8, 'f', 'r', 'e', 'e'}))
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrFragmentCorrupt))
	})
}

type errorReader struct{ err error }

func (r *errorReader) Read([]byte) (int, error) { return 0, r.err }
