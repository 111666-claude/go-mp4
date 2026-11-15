package mp4

import (
	"encoding/hex"
	"testing"

	"github.com/orcaman/writerseeker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildAudioTrak constructs a minimal but valid audio trak containing an
// mp4a sample entry with the given AudioSpecificConfig in its esds box.
func buildAudioTrak(t *testing.T, w *Writer, trackID uint32, channelCount uint16, ascHex string) {
	t.Helper()
	asc, err := hex.DecodeString(ascHex)
	require.NoError(t, err)

	_, err = w.StartBox(&BoxInfo{Type: BoxTypeTrak()})
	require.NoError(t, err)

	// tkhd
	_, err = w.StartBox(&BoxInfo{Type: BoxTypeTkhd()})
	require.NoError(t, err)
	_, err = Marshal(w, &Tkhd{
		TrackID:    trackID,
		DurationV0: 1024,
		Volume:     0x0100,
		Matrix:     [9]int32{0x10000, 0, 0, 0, 0x10000, 0, 0, 0, 0x40000000},
	}, Context{})
	require.NoError(t, err)
	_, err = w.EndBox()
	require.NoError(t, err)

	// mdia
	_, err = w.StartBox(&BoxInfo{Type: BoxTypeMdia()})
	require.NoError(t, err)

	_, err = w.StartBox(&BoxInfo{Type: BoxTypeMdhd()})
	require.NoError(t, err)
	_, err = Marshal(w, &Mdhd{
		Timescale:  44100,
		DurationV0: 44100,
		Language:   [3]byte{'u', 'n', 'd'},
	}, Context{})
	require.NoError(t, err)
	_, err = w.EndBox()
	require.NoError(t, err)

	_, err = w.StartBox(&BoxInfo{Type: BoxTypeHdlr()})
	require.NoError(t, err)
	_, err = Marshal(w, &Hdlr{
		HandlerType: [4]byte{'s', 'o', 'u', 'n'},
		Name:        "sound",
	}, Context{})
	require.NoError(t, err)
	_, err = w.EndBox()
	require.NoError(t, err)

	// minf
	_, err = w.StartBox(&BoxInfo{Type: BoxTypeMinf()})
	require.NoError(t, err)

	_, err = w.StartBox(&BoxInfo{Type: BoxTypeSmhd()})
	require.NoError(t, err)
	_, err = Marshal(w, &Smhd{}, Context{})
	require.NoError(t, err)
	_, err = w.EndBox()
	require.NoError(t, err)

	_, err = w.StartBox(&BoxInfo{Type: BoxTypeDinf()})
	require.NoError(t, err)
	_, err = w.StartBox(&BoxInfo{Type: BoxTypeDref()})
	require.NoError(t, err)
	_, err = Marshal(w, &Dref{EntryCount: 1}, Context{})
	require.NoError(t, err)
	_, err = w.StartBox(&BoxInfo{Type: BoxTypeUrl()})
	require.NoError(t, err)
	_, err = Marshal(w, &Url{
		FullBox: FullBox{Flags: [3]byte{0x00, 0x00, 0x01}},
	}, Context{})
	require.NoError(t, err)
	_, err = w.EndBox()
	require.NoError(t, err) // url
	_, err = w.EndBox()
	require.NoError(t, err) // dref
	_, err = w.EndBox()
	require.NoError(t, err) // dinf

	// stbl
	_, err = w.StartBox(&BoxInfo{Type: BoxTypeStbl()})
	require.NoError(t, err)

	// stsd/mp4a/esds
	_, err = w.StartBox(&BoxInfo{Type: BoxTypeStsd()})
	require.NoError(t, err)
	_, err = Marshal(w, &Stsd{EntryCount: 1}, Context{})
	require.NoError(t, err)

	_, err = w.StartBox(&BoxInfo{Type: BoxTypeMp4a()})
	require.NoError(t, err)
	_, err = Marshal(w, &AudioSampleEntry{
		SampleEntry: SampleEntry{
			AnyTypeBox: AnyTypeBox{Type: BoxTypeMp4a()},
			DataReferenceIndex: 1,
		},
		ChannelCount: channelCount,
		SampleSize:   16,
		SampleRate:   44100 << 16,
	}, Context{})
	require.NoError(t, err)

	_, err = w.StartBox(&BoxInfo{Type: BoxTypeEsds()})
	require.NoError(t, err)
	_, err = Marshal(w, &Esds{
		Descriptors: []Descriptor{
			{
				Tag:  ESDescrTag,
				Size: 3,
				ESDescriptor: &ESDescriptor{
					ESID: uint16(trackID),
				},
			},
			{
				Tag:  DecoderConfigDescrTag,
				Size: 13 + uint32(len(asc)),
				DecoderConfigDescriptor: &DecoderConfigDescriptor{
					ObjectTypeIndication: 0x40,
					StreamType:           5,
					Reserved:             true,
				},
			},
			{
				Tag:  DecSpecificInfoTag,
				Size: uint32(len(asc)),
				Data: asc,
			},
			{
				Tag:  SLConfigDescrTag,
				Size: 1,
				Data: []byte{0x02},
			},
		},
	}, Context{})
	require.NoError(t, err)
	_, err = w.EndBox()
	require.NoError(t, err) // esds
	_, err = w.EndBox()
	require.NoError(t, err) // mp4a
	_, err = w.EndBox()
	require.NoError(t, err) // stsd

	marshalSimpleTable := func(boxType BoxType, v IImmutableBox) {
		t.Helper()
		_, err = w.StartBox(&BoxInfo{Type: boxType})
		require.NoError(t, err)
		_, err = Marshal(w, v, Context{})
		require.NoError(t, err)
		_, err = w.EndBox()
	require.NoError(t, err)
	}

	marshalSimpleTable(BoxTypeStts(), &Stts{
		EntryCount: 1,
		Entries:    []SttsEntry{{SampleCount: 1, SampleDelta: 1024}},
	})
	marshalSimpleTable(BoxTypeStsc(), &Stsc{
		EntryCount: 1,
		Entries:    []StscEntry{{FirstChunk: 1, SamplesPerChunk: 1, SampleDescriptionIndex: 1}},
	})
	marshalSimpleTable(BoxTypeStsz(), &Stsz{
		SampleSize:  0,
		SampleCount: 1,
		EntrySize:   []uint32{10},
	})
	marshalSimpleTable(BoxTypeStco(), &Stco{
		EntryCount:  1,
		ChunkOffset: []uint32{0},
	})

	_, err = w.EndBox()
	require.NoError(t, err) // stbl
	_, err = w.EndBox()
	require.NoError(t, err) // minf
	_, err = w.EndBox()
	require.NoError(t, err) // mdia
	_, err = w.EndBox()
	require.NoError(t, err) // trak
}

// TestProbeMultiAudioTracks builds an MP4 with an AAC-LC track and an
// HE-AAC v2 (PS) track and verifies Probe reports each track's own
// object type indication and audio object type without mixing esds data.
func TestProbeMultiAudioTracks(t *testing.T) {
	buf := &writerseeker.WriterSeeker{}
	w := NewWriter(buf)

	// ftyp
	_, err := w.StartBox(&BoxInfo{Type: BoxTypeFtyp()})
	require.NoError(t, err)
	_, err = Marshal(w, &Ftyp{
		MajorBrand:       BrandISOM(),
		MinorVersion:     0x200,
		CompatibleBrands: []CompatibleBrandElem{{CompatibleBrand: BrandISOM()}},
	}, Context{})
	require.NoError(t, err)
	_, err = w.EndBox()
	require.NoError(t, err)

	// moov with two audio traks
	_, err = w.StartBox(&BoxInfo{Type: BoxTypeMoov()})
	require.NoError(t, err)

	_, err = w.StartBox(&BoxInfo{Type: BoxTypeMvhd()})
	require.NoError(t, err)
	_, err = Marshal(w, &Mvhd{
		Timescale:   1000,
		DurationV0:  1024,
		Rate:        0x10000,
		Volume:      0x0100,
		Matrix:      [9]int32{0x10000, 0, 0, 0, 0x10000, 0, 0, 0, 0x40000000},
		NextTrackID: 3,
	}, Context{})
	require.NoError(t, err)
	_, err = w.EndBox()
	require.NoError(t, err)

	// track 1: AAC-LC 44.1kHz stereo
	buildAudioTrak(t, w, 1, 2, "1210")
	// track 2: HE-AAC v2 (PS), mono core
	buildAudioTrak(t, w, 2, 1, "eb8a10")

	_, err = w.EndBox()
	require.NoError(t, err) // moov

	info, err := Probe(buf.BytesReader())
	require.NoError(t, err)
	require.Len(t, info.Tracks, 2)

	assert.Equal(t, uint32(1), info.Tracks[0].TrackID)
	assert.Equal(t, CodecMP4A, info.Tracks[0].Codec)
	require.NotNil(t, info.Tracks[0].MP4A)
	assert.Equal(t, uint8(0x40), info.Tracks[0].MP4A.OTI)
	assert.Equal(t, uint8(2), info.Tracks[0].MP4A.AudOTI)
	assert.Equal(t, uint16(2), info.Tracks[0].MP4A.ChannelCount)

	assert.Equal(t, uint32(2), info.Tracks[1].TrackID)
	assert.Equal(t, CodecMP4A, info.Tracks[1].Codec)
	require.NotNil(t, info.Tracks[1].MP4A)
	assert.Equal(t, uint8(0x40), info.Tracks[1].MP4A.OTI)
	assert.Equal(t, uint8(29), info.Tracks[1].MP4A.AudOTI)
	assert.Equal(t, uint16(1), info.Tracks[1].MP4A.ChannelCount)
}
