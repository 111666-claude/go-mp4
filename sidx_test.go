package mp4

import (
	"bytes"
	"errors"
	"io"
	"math"
	"testing"

	"gopkg.in/src-d/go-billy.v4/memfs"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type sidxTestFragment struct {
	trackID     uint32
	decodeTime  uint64
	tfdtVersion uint8
	durations   []uint32
	mdatSize    int
}

type sidxBuiltStream struct {
	data       []byte
	moofOffset []uint64
	moofSize   []uint64
	mdatSize   []uint64
}

// buildSidxStream creates a synthetic fragmented MP4 stream: an ftyp box,
// an optional moov box with one trak per entry of mdhdTimescales, an optional
// styp box, and the given moof/mdat fragments.
func buildSidxStream(t *testing.T, mdhdTimescales []uint32, styp bool, frags []sidxTestFragment) sidxBuiltStream {
	t.Helper()
	fs := memfs.New()
	f, err := fs.Create("out.mp4")
	require.NoError(t, err)
	w := NewWriter(f)

	_, err = w.StartBox(&BoxInfo{Type: BoxTypeFtyp()})
	require.NoError(t, err)
	_, err = Marshal(w, &Ftyp{
		MajorBrand:   [4]byte{'i', 's', 'o', '6'},
		MinorVersion: 1,
		CompatibleBrands: []CompatibleBrandElem{
			{CompatibleBrand: [4]byte{'i', 's', 'o', '6'}},
			{CompatibleBrand: [4]byte{'d', 'a', 's', 'h'}},
		},
	}, Context{})
	require.NoError(t, err)
	_, err = w.EndBox()
	require.NoError(t, err)

	for _, ts := range mdhdTimescales {
		_, err = w.StartBox(&BoxInfo{Type: BoxTypeMoov()})
		require.NoError(t, err)
		_, err = w.StartBox(&BoxInfo{Type: BoxTypeTrak()})
		require.NoError(t, err)
		_, err = w.StartBox(&BoxInfo{Type: BoxTypeMdia()})
		require.NoError(t, err)
		_, err = w.StartBox(&BoxInfo{Type: BoxTypeMdhd()})
		require.NoError(t, err)
		_, err = Marshal(w, &Mdhd{Timescale: ts}, Context{})
		require.NoError(t, err)
		_, err = w.EndBox() // mdhd
		require.NoError(t, err)
		_, err = w.EndBox() // mdia
		require.NoError(t, err)
		_, err = w.EndBox() // trak
		require.NoError(t, err)
		_, err = w.EndBox() // moov
		require.NoError(t, err)
	}

	if styp {
		_, err = w.StartBox(&BoxInfo{Type: BoxTypeStyp()})
		require.NoError(t, err)
		_, err = Marshal(w, &Styp{
			MajorBrand:   [4]byte{'m', 's', 'd', 'h'},
			MinorVersion: 0,
		}, Context{})
		require.NoError(t, err)
		_, err = w.EndBox()
		require.NoError(t, err)
	}

	out := sidxBuiltStream{}
	for i, frag := range frags {
		bi, err := w.StartBox(&BoxInfo{Type: BoxTypeMoof()})
		require.NoError(t, err)
		out.moofOffset = append(out.moofOffset, bi.Offset)

		_, err = w.StartBox(&BoxInfo{Type: BoxTypeMfhd()})
		require.NoError(t, err)
		_, err = Marshal(w, &Mfhd{SequenceNumber: uint32(i + 1)}, Context{})
		require.NoError(t, err)
		_, err = w.EndBox()
		require.NoError(t, err)

		_, err = w.StartBox(&BoxInfo{Type: BoxTypeTraf()})
		require.NoError(t, err)

		_, err = w.StartBox(&BoxInfo{Type: BoxTypeTfhd()})
		require.NoError(t, err)
		_, err = Marshal(w, &Tfhd{TrackID: frag.trackID}, Context{})
		require.NoError(t, err)
		_, err = w.EndBox()
		require.NoError(t, err)

		_, err = w.StartBox(&BoxInfo{Type: BoxTypeTfdt()})
		require.NoError(t, err)
		tfdt := &Tfdt{}
		tfdt.SetVersion(frag.tfdtVersion)
		if frag.tfdtVersion == 0 {
			tfdt.BaseMediaDecodeTimeV0 = uint32(frag.decodeTime)
		} else {
			tfdt.BaseMediaDecodeTimeV1 = frag.decodeTime
		}
		_, err = Marshal(w, tfdt, Context{})
		require.NoError(t, err)
		_, err = w.EndBox()
		require.NoError(t, err)

		_, err = w.StartBox(&BoxInfo{Type: BoxTypeTrun()})
		require.NoError(t, err)
		trun := &Trun{SampleCount: uint32(len(frag.durations))}
		trun.SetFlags(0x000100) // sample-duration-present
		for _, d := range frag.durations {
			trun.Entries = append(trun.Entries, TrunEntry{SampleDuration: d})
		}
		_, err = Marshal(w, trun, Context{})
		require.NoError(t, err)
		_, err = w.EndBox()
		require.NoError(t, err)

		_, err = w.EndBox() // traf
		require.NoError(t, err)
		bi, err = w.EndBox() // moof
		require.NoError(t, err)
		out.moofSize = append(out.moofSize, bi.Size)

		_, err = w.StartBox(&BoxInfo{Type: BoxTypeMdat()})
		require.NoError(t, err)
		_, err = w.Write(make([]byte, frag.mdatSize))
		require.NoError(t, err)
		bi, err = w.EndBox() // mdat
		require.NoError(t, err)
		out.mdatSize = append(out.mdatSize, bi.Size)
	}
	require.NoError(t, f.Close())

	r, err := fs.Open("out.mp4")
	require.NoError(t, err)
	defer r.Close()
	out.data, err = io.ReadAll(r)
	require.NoError(t, err)
	return out
}

// marshalSidx serializes the sidx box including its box header.
func marshalSidx(t *testing.T, sidx *Sidx) []byte {
	t.Helper()
	fs := memfs.New()
	f, err := fs.Create("sidx.bin")
	require.NoError(t, err)
	w := NewWriter(f)
	_, err = w.StartBox(&BoxInfo{Type: BoxTypeSidx()})
	require.NoError(t, err)
	_, err = Marshal(w, sidx, Context{})
	require.NoError(t, err)
	bi, err := w.EndBox()
	require.NoError(t, err)
	require.NoError(t, f.Close())

	r, err := fs.Open("sidx.bin")
	require.NoError(t, err)
	defer r.Close()
	data, err := io.ReadAll(r)
	require.NoError(t, err)
	require.Equal(t, bi.Size, uint64(len(data)))
	return data
}

func TestGenerateSidx(t *testing.T) {
	frags := []sidxTestFragment{
		{trackID: 1, decodeTime: 0, durations: []uint32{3000, 3000}, mdatSize: 100},
		{trackID: 1, decodeTime: 6000, durations: []uint32{1500}, mdatSize: 250},
		{trackID: 1, decodeTime: 7500, durations: []uint32{2000, 2000, 2000}, mdatSize: 80},
	}
	stream := buildSidxStream(t, nil, false, frags)

	sidx, err := GenerateSidx(bytes.NewReader(stream.data), &SidxOptions{Timescale: 90000})
	require.NoError(t, err)

	assert.Equal(t, uint8(0), sidx.GetVersion())
	assert.Equal(t, uint32(90000), sidx.Timescale)
	assert.Equal(t, uint32(1), sidx.ReferenceID)
	assert.Equal(t, uint64(0), sidx.GetEarliestPresentationTime())
	// the sidx box is placed immediately before the first moof box
	assert.Equal(t, uint64(0), sidx.GetFirstOffset())
	require.Len(t, sidx.References, 3)
	assert.Equal(t, int(sidx.ReferenceCount), len(sidx.References))

	expectedDurations := []uint32{6000, 1500, 6000}
	for i := range sidx.References {
		assert.False(t, sidx.References[i].ReferenceType)
		assert.Equal(t, uint32(stream.moofSize[i]+stream.mdatSize[i]), sidx.References[i].ReferencedSize)
		assert.Equal(t, expectedDurations[i], sidx.References[i].SubsegmentDuration)
	}

	// check the serialized sidx box
	data := marshalSidx(t, sidx)
	// 8 (header) + 4 (fullbox) + 4 (reference_ID) + 4 (timescale)
	// + 4 (ept v0) + 4 (first_offset v0) + 2 (reserved) + 2 (count) + 12*3
	require.Len(t, data, 8+4+4+4+4+4+2+2+12*3)
	assert.Equal(t, "sidx", string(data[4:8]))
	assert.Equal(t, byte(0), data[8]) // version

	// the binary must be decodable and yield the same values
	r := bytes.NewReader(data)
	bi, err := ReadBoxInfo(r)
	require.NoError(t, err)
	require.Equal(t, BoxTypeSidx(), bi.Type)
	decoded, _, err := UnmarshalAny(r, bi.Type, bi.Size-bi.HeaderSize, Context{})
	require.NoError(t, err)
	decodedSidx := decoded.(*Sidx)
	assert.Equal(t, sidx.Timescale, decodedSidx.Timescale)
	assert.Equal(t, sidx.GetEarliestPresentationTime(), decodedSidx.GetEarliestPresentationTime())
	assert.Equal(t, sidx.GetFirstOffset(), decodedSidx.GetFirstOffset())
	assert.Equal(t, sidx.References, decodedSidx.References)
}

func TestGenerateSidxRandomAccess(t *testing.T) {
	frags := []sidxTestFragment{
		{trackID: 1, decodeTime: 1000, durations: []uint32{2048, 2048}, mdatSize: 500},
		{trackID: 1, decodeTime: 5096, durations: []uint32{1024}, mdatSize: 120},
		{trackID: 1, decodeTime: 6120, durations: []uint32{1024, 1024, 1024}, mdatSize: 333},
	}
	stream := buildSidxStream(t, nil, false, frags)

	sidx, err := GenerateSidx(bytes.NewReader(stream.data), &SidxOptions{Timescale: 10240})
	require.NoError(t, err)
	assert.Equal(t, uint64(1000), sidx.GetEarliestPresentationTime())

	// assemble the final file: everything before the first moof, then the
	// sidx box, then the fragments
	sidxData := marshalSidx(t, sidx)
	head := stream.data[:stream.moofOffset[0]]
	final := make([]byte, 0, len(stream.data)+len(sidxData))
	final = append(final, head...)
	final = append(final, sidxData...)
	final = append(final, stream.data[stream.moofOffset[0]:]...)

	// random access: locate every fragment through the sidx references and
	// read back its decode time
	r := bytes.NewReader(final)
	anchor := uint64(len(head)) + uint64(len(sidxData)) + sidx.GetFirstOffset()
	decodeTime := sidx.GetEarliestPresentationTime()
	// visit references in random order
	for _, i := range []int{2, 0, 1} {
		offset := anchor
		for j := 0; j < i; j++ {
			offset += uint64(sidx.References[j].ReferencedSize)
		}
		_, err := r.Seek(int64(offset), io.SeekStart)
		require.NoError(t, err)
		bi, err := ReadBoxInfo(r)
		require.NoError(t, err)
		require.Equal(t, BoxTypeMoof(), bi.Type, "reference %d must point to a moof box", i)

		bips, err := ExtractBoxesWithPayload(r, bi, []BoxPath{{BoxTypeTraf(), BoxTypeTfdt()}})
		require.NoError(t, err)
		require.Len(t, bips, 1)
		expected := decodeTime
		for j := 0; j < i; j++ {
			expected += uint64(sidx.References[j].SubsegmentDuration)
		}
		assert.Equal(t, expected, bips[0].Payload.(*Tfdt).GetBaseMediaDecodeTime(),
			"decode time of fragment %d must be recoverable from the sidx", i)
	}
}

func TestGenerateSidxPlacement(t *testing.T) {
	frags := []sidxTestFragment{
		{trackID: 1, decodeTime: 0, durations: []uint32{1000}, mdatSize: 10},
		{trackID: 1, decodeTime: 1000, durations: []uint32{1000}, mdatSize: 20},
	}
	// ftyp + styp before the fragments: the sidx box is written right after
	// ftyp, so first_offset must span the styp box
	stream := buildSidxStream(t, nil, true, frags)

	// locate the styp box
	bis, err := ExtractBoxes(bytes.NewReader(stream.data), nil, []BoxPath{{BoxTypeStyp()}})
	require.NoError(t, err)
	require.Len(t, bis, 1)
	styp := bis[0]

	sidx, err := GenerateSidx(bytes.NewReader(stream.data), &SidxOptions{
		Timescale: 1000,
		Offset:    styp.Offset, // sidx takes the place in front of styp
	})
	require.NoError(t, err)
	assert.Equal(t, styp.Size, sidx.GetFirstOffset())

	// assemble the final file and verify the first reference
	sidxData := marshalSidx(t, sidx)
	final := make([]byte, 0, len(stream.data)+len(sidxData))
	final = append(final, stream.data[:styp.Offset]...)
	final = append(final, sidxData...)
	final = append(final, stream.data[styp.Offset:]...)

	r := bytes.NewReader(final)
	_, err = r.Seek(int64(styp.Offset+uint64(len(sidxData))+sidx.GetFirstOffset()), io.SeekStart)
	require.NoError(t, err)
	bi, err := ReadBoxInfo(r)
	require.NoError(t, err)
	assert.Equal(t, BoxTypeMoof(), bi.Type)

	// a placement behind the first fragment cannot be expressed
	_, err = GenerateSidx(bytes.NewReader(stream.data), &SidxOptions{
		Timescale: 1000,
		Offset:    stream.moofOffset[0] + 1,
	})
	requireSidxError(t, err, ErrSidxInvalidOffset)
}

func TestGenerateSidxVersion1(t *testing.T) {
	frags := []sidxTestFragment{
		{trackID: 1, decodeTime: math.MaxUint32 + 5, tfdtVersion: 1, durations: []uint32{90000}, mdatSize: 64},
		{trackID: 1, decodeTime: math.MaxUint32 + 5 + 90000, tfdtVersion: 1, durations: []uint32{45000}, mdatSize: 32},
	}
	stream := buildSidxStream(t, nil, false, frags)

	sidx, err := GenerateSidx(bytes.NewReader(stream.data), &SidxOptions{Timescale: 90000})
	require.NoError(t, err)
	assert.Equal(t, uint8(1), sidx.GetVersion())
	assert.Equal(t, uint64(math.MaxUint32)+5, sidx.GetEarliestPresentationTime())

	// the version 1 binary must round-trip
	data := marshalSidx(t, sidx)
	assert.Equal(t, byte(1), data[8]) // version
	r := bytes.NewReader(data)
	bi, err := ReadBoxInfo(r)
	require.NoError(t, err)
	decoded, _, err := UnmarshalAny(r, bi.Type, bi.Size-bi.HeaderSize, Context{})
	require.NoError(t, err)
	assert.Equal(t, uint64(math.MaxUint32)+5, decoded.(*Sidx).GetEarliestPresentationTime())
	assert.Equal(t, sidx.References, decoded.(*Sidx).References)
}

func TestGenerateSidxTimescaleFromMoov(t *testing.T) {
	frags := []sidxTestFragment{
		{trackID: 1, decodeTime: 0, durations: []uint32{48000}, mdatSize: 50},
		{trackID: 1, decodeTime: 48000, durations: []uint32{48000}, mdatSize: 50},
	}
	stream := buildSidxStream(t, []uint32{48000}, false, frags)

	sidx, err := GenerateSidx(bytes.NewReader(stream.data), nil)
	require.NoError(t, err)
	assert.Equal(t, uint32(48000), sidx.Timescale)

	// a matching explicit timescale is accepted
	sidx, err = GenerateSidx(bytes.NewReader(stream.data), &SidxOptions{Timescale: 48000})
	require.NoError(t, err)
	assert.Equal(t, uint32(48000), sidx.Timescale)

	// a contradicting timescale is rejected
	_, err = GenerateSidx(bytes.NewReader(stream.data), &SidxOptions{Timescale: 90000})
	requireSidxError(t, err, ErrSidxInconsistentTimescale)
}

func TestGenerateSidxErrors(t *testing.T) {
	t.Run("not seekable", func(t *testing.T) {
		stream := buildSidxStream(t, nil, false, []sidxTestFragment{
			{trackID: 1, decodeTime: 0, durations: []uint32{1000}, mdatSize: 10},
		})
		// bytes.Buffer does not implement io.Seeker
		_, err := GenerateSidx(bytes.NewBuffer(stream.data), &SidxOptions{Timescale: 1000})
		requireSidxError(t, err, ErrSidxNotSeekable)
	})

	t.Run("no fragments", func(t *testing.T) {
		stream := buildSidxStream(t, nil, false, nil)
		_, err := GenerateSidx(bytes.NewReader(stream.data), &SidxOptions{Timescale: 1000})
		requireSidxError(t, err, ErrSidxNoFragments)
	})

	t.Run("missing mdat", func(t *testing.T) {
		stream := buildSidxStream(t, nil, false, []sidxTestFragment{
			{trackID: 1, decodeTime: 0, durations: []uint32{1000}, mdatSize: 10},
			{trackID: 1, decodeTime: 1000, durations: []uint32{1000}, mdatSize: 10},
		})
		// drop the second mdat box
		secondMdatOffset := stream.moofOffset[1] + stream.moofSize[1]
		truncated := stream.data[:secondMdatOffset]
		_, err := GenerateSidx(bytes.NewReader(truncated), &SidxOptions{Timescale: 1000})
		requireSidxError(t, err, ErrSidxMissingMdat)
	})

	t.Run("missing moof", func(t *testing.T) {
		stream := buildSidxStream(t, nil, false, []sidxTestFragment{
			{trackID: 1, decodeTime: 0, durations: []uint32{1000}, mdatSize: 10},
		})
		// drop the moof box, keep ftyp and mdat
		mdatOffset := stream.moofOffset[0] + stream.moofSize[0]
		data := append([]byte{}, stream.data[:stream.moofOffset[0]]...)
		data = append(data, stream.data[mdatOffset:]...)
		_, err := GenerateSidx(bytes.NewReader(data), &SidxOptions{Timescale: 1000})
		requireSidxError(t, err, ErrSidxMissingMoof)
	})

	t.Run("inconsistent timescale", func(t *testing.T) {
		stream := buildSidxStream(t, []uint32{90000, 48000}, false, []sidxTestFragment{
			{trackID: 1, decodeTime: 0, durations: []uint32{1000}, mdatSize: 10},
		})
		// ambiguous without an explicit timescale
		_, err := GenerateSidx(bytes.NewReader(stream.data), nil)
		requireSidxError(t, err, ErrSidxInconsistentTimescale)
	})

	t.Run("no timescale", func(t *testing.T) {
		stream := buildSidxStream(t, nil, false, []sidxTestFragment{
			{trackID: 1, decodeTime: 0, durations: []uint32{1000}, mdatSize: 10},
		})
		_, err := GenerateSidx(bytes.NewReader(stream.data), nil)
		requireSidxError(t, err, ErrSidxNoTimescale)
	})

	t.Run("overlapping fragments", func(t *testing.T) {
		stream := buildSidxStream(t, nil, false, []sidxTestFragment{
			{trackID: 1, decodeTime: 0, durations: []uint32{3000, 3000}, mdatSize: 10},
			// starts before the first fragment ends (0 + 6000)
			{trackID: 1, decodeTime: 3000, durations: []uint32{1000}, mdatSize: 10},
		})
		_, err := GenerateSidx(bytes.NewReader(stream.data), &SidxOptions{Timescale: 1000})
		requireSidxError(t, err, ErrSidxOverlappingFragments)
	})

	t.Run("duration overflow", func(t *testing.T) {
		stream := buildSidxStream(t, nil, false, []sidxTestFragment{
			{trackID: 1, decodeTime: 0, durations: []uint32{math.MaxUint32, 1}, mdatSize: 10},
		})
		_, err := GenerateSidx(bytes.NewReader(stream.data), &SidxOptions{Timescale: 1000})
		requireSidxError(t, err, ErrSidxOverflow)
	})

	t.Run("inconsistent track id", func(t *testing.T) {
		stream := buildSidxStream(t, nil, false, []sidxTestFragment{
			{trackID: 1, decodeTime: 0, durations: []uint32{1000}, mdatSize: 10},
			{trackID: 2, decodeTime: 1000, durations: []uint32{1000}, mdatSize: 10},
		})
		_, err := GenerateSidx(bytes.NewReader(stream.data), &SidxOptions{Timescale: 1000})
		require.Error(t, err)
	})
}

// requireSidxError asserts that err matches target via errors.Is.
func requireSidxError(t *testing.T, err, target error) {
	t.Helper()
	require.Error(t, err)
	require.True(t, errors.Is(err, target), "expected %v, got %v", target, err)
}

func TestGenerateSidxErrorUnwrapping(t *testing.T) {
	// errors must be usable with errors.Is even when wrapped with context
	stream := buildSidxStream(t, nil, false, nil)
	_, err := GenerateSidx(bytes.NewReader(stream.data), &SidxOptions{Timescale: 1000})
	require.True(t, errors.Is(err, ErrSidxNoFragments))
}
