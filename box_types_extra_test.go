package mp4

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildSgpd assembles a version-1 sgpd box payload for the given grouping
// type, default length and raw entry bytes.
func buildSgpdPayload(version uint8, grouping [4]byte, defaultLength uint32, entries []byte) []byte {
	buf := bytes.NewBuffer(nil)
	buf.WriteByte(version)
	buf.Write([]byte{0x00, 0x00, 0x00}) // flags
	buf.Write(grouping[:])
	if version >= 1 {
		tmp := make([]byte, 4)
		putU32(tmp, defaultLength)
		buf.Write(tmp)
	}
	if version >= 2 {
		tmp := make([]byte, 4)
		putU32(tmp, 0) // default sample description index
		buf.Write(tmp)
	}
	cnt := make([]byte, 4)
	if defaultLength != 0 {
		putU32(cnt, uint32(len(entries))/defaultLength)
	} else {
		putU32(cnt, 0)
	}
	buf.Write(cnt)
	buf.Write(entries)
	return buf.Bytes()
}

func putU32(b []byte, v uint32) {
	b[0] = byte(v >> 24)
	b[1] = byte(v >> 16)
	b[2] = byte(v >> 8)
	b[3] = byte(v)
}

// TestSgpdEntryCounts covers the structured entry count for both a small (2)
// and a large (256) number of fixed-length "roll" descriptions, including a
// full read/write byte round-trip.
func TestSgpdEntryCounts(t *testing.T) {
	for _, count := range []int{2, 256} {
		t.Run("roll", func(t *testing.T) {
			src := &Sgpd{
				FullBox:       FullBox{Version: 1},
				GroupingType:  [4]byte{'r', 'o', 'l', 'l'},
				DefaultLength: 2,
				EntryCount:    uint32(count),
				RollDistances: make([]int16, count),
			}
			for i := 0; i < count; i++ {
				src.RollDistances[i] = int16(i - count/2)
			}

			// write
			bin := bytes.NewBuffer(nil)
			n, err := Marshal(bin, src)
			require.NoError(t, err)
			assert.Equal(t, uint64(4+4+4+4+count*2), n)

			// read
			dst := &Sgpd{}
			rn, err := Unmarshal(bytes.NewReader(bin.Bytes()), uint64(bin.Len()), dst)
			require.NoError(t, err)
			assert.Equal(t, uint64(bin.Len()), rn)
			assert.Len(t, dst.RollDistances, count)
			assert.Equal(t, src, dst)

			// display
			str, err := Stringify(dst)
			require.NoError(t, err)
			assert.Contains(t, str, "EntryCount=")
			assert.Contains(t, str, "RollDistances=[")

			// re-write must reproduce the exact bytes
			again := bytes.NewBuffer(nil)
			_, err = Marshal(again, dst)
			require.NoError(t, err)
			assert.Equal(t, bin.Bytes(), again.Bytes())
		})
	}
}

// TestSgpdUnknownGroupingPreserved ensures an unrecognized but legal grouping
// type is kept verbatim, rather than rejected, and round-trips byte-for-byte.
func TestSgpdUnknownGroupingPreserved(t *testing.T) {
	entries := bytes.Repeat([]byte{0xab, 0xcd, 0xef, 0x01}, 256) // 256 entries
	payload := buildSgpdPayload(1, [4]byte{'z', 'z', 'z', 'z'}, 4, entries)

	dst := &Sgpd{}
	n, err := Unmarshal(bytes.NewReader(payload), uint64(len(payload)), dst)
	require.NoError(t, err)
	assert.Equal(t, uint64(len(payload)), n)
	assert.Equal(t, uint32(256), dst.EntryCount)
	assert.Nil(t, dst.RollDistances)
	assert.Equal(t, entries, dst.Unsupported)

	// write back identical bytes
	out := bytes.NewBuffer(nil)
	_, err = Marshal(out, dst)
	require.NoError(t, err)
	assert.Equal(t, payload, out.Bytes())
}

// TestSgpdVersion0RawRoundTrip covers the deprecated version 0, whose entries
// have no length field and are preserved raw.
func TestSgpdVersion0RawRoundTrip(t *testing.T) {
	entries := []byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66}
	payload := buildSgpdPayload(0, [4]byte{'r', 'o', 'l', 'l'}, 0, entries)
	putU32(payload[len(payload)-4-len(entries):], 2) // entry count = 2

	dst := &Sgpd{}
	n, err := Unmarshal(bytes.NewReader(payload), uint64(len(payload)), dst)
	require.NoError(t, err)
	assert.Equal(t, uint64(len(payload)), n)
	assert.Equal(t, uint32(2), dst.EntryCount)
	assert.Equal(t, entries, dst.Unsupported)

	out := bytes.NewBuffer(nil)
	_, err = Marshal(out, dst)
	require.NoError(t, err)
	assert.Equal(t, payload, out.Bytes())
}

// TestSgpdPerEntryLengthVariants verifies the explicit per-entry-length ("L")
// forms of "rap " and "tele" (used when default_length == 0) round-trip.
func TestSgpdPerEntryLengthVariants(t *testing.T) {
	t.Run("rap", func(t *testing.T) {
		src := &Sgpd{
			FullBox:       FullBox{Version: 1},
			GroupingType:  [4]byte{'r', 'a', 'p', ' '},
			DefaultLength: 0,
			EntryCount:    2,
			VisualRandomAccessEntriesL: []VisualRandomAccessEntryL{
				{DescriptionLength: 1, VisualRandomAccessEntry: VisualRandomAccessEntry{NumLeadingSamplesKnown: true, NumLeadingSamples: 0x27}},
				{DescriptionLength: 1, VisualRandomAccessEntry: VisualRandomAccessEntry{NumLeadingSamplesKnown: false, NumLeadingSamples: 0x1a}},
			},
		}
		bin := bytes.NewBuffer(nil)
		_, err := Marshal(bin, src)
		require.NoError(t, err)

		dst := &Sgpd{}
		_, err = Unmarshal(bytes.NewReader(bin.Bytes()), uint64(bin.Len()), dst)
		require.NoError(t, err)
		assert.Len(t, dst.VisualRandomAccessEntriesL, 2)
		assert.Equal(t, src, dst)

		out := bytes.NewBuffer(nil)
		_, err = Marshal(out, dst)
		require.NoError(t, err)
		assert.Equal(t, bin.Bytes(), out.Bytes())
	})

	t.Run("tele", func(t *testing.T) {
		src := &Sgpd{
			FullBox:       FullBox{Version: 1},
			GroupingType:  [4]byte{'t', 'e', 'l', 'e'},
			DefaultLength: 0,
			EntryCount:    2,
			TemporalLevelEntriesL: []TemporalLevelEntryL{
				{DescriptionLength: 1, TemporalLevelEntry: TemporalLevelEntry{LevelIndependentlyDecodable: true}},
				{DescriptionLength: 1, TemporalLevelEntry: TemporalLevelEntry{LevelIndependentlyDecodable: false}},
			},
		}
		bin := bytes.NewBuffer(nil)
		_, err := Marshal(bin, src)
		require.NoError(t, err)

		dst := &Sgpd{}
		_, err = Unmarshal(bytes.NewReader(bin.Bytes()), uint64(bin.Len()), dst)
		require.NoError(t, err)
		assert.Len(t, dst.TemporalLevelEntriesL, 2)
		assert.Equal(t, src, dst)

		out := bytes.NewBuffer(nil)
		_, err = Marshal(out, dst)
		require.NoError(t, err)
		assert.Equal(t, bin.Bytes(), out.Bytes())
	})
}

// TestSgpdTruncationReportsError makes truncated payloads fail loudly instead
// of silently yielding fewer entries.
func TestSgpdTruncationReportsError(t *testing.T) {
	src := &Sgpd{
		FullBox:       FullBox{Version: 1},
		GroupingType:  [4]byte{'r', 'o', 'l', 'l'},
		DefaultLength: 2,
		EntryCount:    4,
		RollDistances: []int16{1, 2, 3, 4},
	}
	full := bytes.NewBuffer(nil)
	_, err := Marshal(full, src)
	require.NoError(t, err)

	// Claiming the full payload size but providing fewer bytes must error.
	for _, cut := range []int{2, 4, 6} {
		truncated := full.Bytes()[:full.Len()-cut]
		dst := &Sgpd{}
		_, err = Unmarshal(bytes.NewReader(truncated), uint64(len(truncated)), dst)
		require.Error(t, err, "expected error when %d bytes are missing", cut)
		// The declared count is always retained for diagnostics.
		assert.Equal(t, uint32(4), dst.EntryCount)
	}

	// A truncated avcC-style length prefix (entry longer than the data left)
	// is covered by TestAVCTruncationReportsError below.
}

// TestAVCDecoderConfigurationNALUnits verifies that an avcC with two SPS and
// two PPS exposes four structured NAL units and round-trips exactly.
func TestAVCDecoderConfigurationNALUnits(t *testing.T) {
	src := &AVCDecoderConfiguration{
		AnyTypeBox:                 AnyTypeBox{Type: StrToBoxType("avcC")},
		ConfigurationVersion:       1,
		Profile:                    AVCMainProfile,
		Level:                      30,
		Reserved:                   0x3f,
		LengthSizeMinusOne:         3,
		Reserved2:                  0x7,
		NumOfSequenceParameterSets: 2,
		SequenceParameterSets: []AVCParameterSet{
			{Length: 3, NALUnit: []byte{0x67, 0x42, 0x00}},
			{Length: 4, NALUnit: []byte{0x67, 0x4d, 0x40, 0x1f}},
		},
		NumOfPictureParameterSets: 2,
		PictureParameterSets: []AVCParameterSet{
			{Length: 2, NALUnit: []byte{0x68, 0xce}},
			{Length: 5, NALUnit: []byte{0x68, 0xee, 0x3c, 0x80, 0x00}},
		},
	}

	// write
	bin := bytes.NewBuffer(nil)
	_, err := Marshal(bin, src)
	require.NoError(t, err)

	// read
	dst := &AVCDecoderConfiguration{AnyTypeBox: AnyTypeBox{Type: StrToBoxType("avcC")}}
	rn, err := Unmarshal(bytes.NewReader(bin.Bytes()), uint64(bin.Len()), dst)
	require.NoError(t, err)
	assert.Equal(t, uint64(bin.Len()), rn)
	assert.Len(t, dst.SequenceParameterSets, 2)
	assert.Len(t, dst.PictureParameterSets, 2)
	assert.Equal(t, 4, len(dst.SequenceParameterSets)+len(dst.PictureParameterSets))
	assert.False(t, dst.HighProfileFieldsEnabled)
	assert.Nil(t, dst.SequenceParameterSetsExt)
	assert.Equal(t, src, dst)

	// display contains both parameter set lists
	str, err := Stringify(dst)
	require.NoError(t, err)
	assert.Contains(t, str, "SequenceParameterSets=[")
	assert.Contains(t, str, "PictureParameterSets=[")
	assert.NotContains(t, str, "HighProfileFieldsEnabled")

	// byte round-trip
	again := bytes.NewBuffer(nil)
	_, err = Marshal(again, dst)
	require.NoError(t, err)
	assert.Equal(t, bin.Bytes(), again.Bytes())
}

// TestAVCDecoderConfigurationHighProfileRoundTrip checks the optional
// high-profile extension fields and ext parameter sets round-trip.
func TestAVCDecoderConfigurationHighProfileRoundTrip(t *testing.T) {
	src := &AVCDecoderConfiguration{
		AnyTypeBox:                 AnyTypeBox{Type: StrToBoxType("avcC")},
		ConfigurationVersion:       1,
		Profile:                    AVCHighProfile,
		Reserved:                   0x3f,
		LengthSizeMinusOne:         3,
		Reserved2:                  0x7,
		NumOfSequenceParameterSets: 1,
		SequenceParameterSets: []AVCParameterSet{
			{Length: 2, NALUnit: []byte{0x67, 0x64}},
		},
		NumOfPictureParameterSets: 1,
		PictureParameterSets: []AVCParameterSet{
			{Length: 2, NALUnit: []byte{0x68, 0xeb}},
		},
		HighProfileFieldsEnabled:     true,
		Reserved3:                    0x3f,
		ChromaFormat:                 1,
		Reserved4:                    0x1f,
		BitDepthLumaMinus8:           0,
		Reserved5:                    0x1f,
		BitDepthChromaMinus8:         0,
		NumOfSequenceParameterSetExt: 1,
		SequenceParameterSetsExt: []AVCParameterSet{
			{Length: 2, NALUnit: []byte{0x6d, 0x40}},
		},
	}

	bin := bytes.NewBuffer(nil)
	_, err := Marshal(bin, src)
	require.NoError(t, err)

	dst := &AVCDecoderConfiguration{AnyTypeBox: AnyTypeBox{Type: StrToBoxType("avcC")}}
	_, err = Unmarshal(bytes.NewReader(bin.Bytes()), uint64(bin.Len()), dst)
	require.NoError(t, err)
	assert.True(t, dst.HighProfileFieldsEnabled)
	assert.Len(t, dst.SequenceParameterSetsExt, 1)
	assert.Equal(t, src, dst)

	again := bytes.NewBuffer(nil)
	_, err = Marshal(again, dst)
	require.NoError(t, err)
	assert.Equal(t, bin.Bytes(), again.Bytes())
}

// TestAVCHighProfileOldSpecNoExtension confirms a high-profile avcC without
// the 4-byte extension suffix is parsed without the optional fields.
func TestAVCHighProfileOldSpecNoExtension(t *testing.T) {
	bin := []byte{
		0x01,       // configuration version
		0x64,       // high profile
		0x00,       // compatibility
		0x1e,       // level
		0xff,       // reserved + lengthSizeMinusOne
		0xe1,       // reserved + 1 SPS
		0x00, 0x02, // SPS length
		0x67, 0x64, // SPS
		0x01,       // 1 PPS
		0x00, 0x02, // PPS length
		0x68, 0xeb, // PPS
	}
	dst := &AVCDecoderConfiguration{AnyTypeBox: AnyTypeBox{Type: StrToBoxType("avcC")}}
	_, err := Unmarshal(bytes.NewReader(bin), uint64(len(bin)), dst)
	require.NoError(t, err)
	assert.False(t, dst.HighProfileFieldsEnabled)
	assert.Nil(t, dst.SequenceParameterSetsExt)
	assert.Len(t, dst.SequenceParameterSets, 1)
	assert.Len(t, dst.PictureParameterSets, 1)

	// The old-spec bytes must survive a round-trip unchanged.
	out := bytes.NewBuffer(nil)
	_, err = Marshal(out, dst)
	require.NoError(t, err)
	assert.Equal(t, bin, out.Bytes())
}

// TestAVCTruncationReportsError ensures a truncated avcC fails rather than
// returning a partial configuration.
func TestAVCTruncationReportsError(t *testing.T) {
	bin := []byte{
		0x01,       // version
		0x4d,       // main profile
		0x40, 0x1f, // compatibility, level
		0xff,       // reserved + lengthSizeMinusOne
		0xe1,       // reserved + 1 SPS
		0x00, 0x05, // SPS length claims 5 bytes...
		0x67, 0x4d, 0x40, // ...but only 3 are present
	}
	dst := &AVCDecoderConfiguration{AnyTypeBox: AnyTypeBox{Type: StrToBoxType("avcC")}}
	_, err := Unmarshal(bytes.NewReader(bin), uint64(len(bin)), dst)
	require.Error(t, err)
}
