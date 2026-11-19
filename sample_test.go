package mp4

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"testing"

	"github.com/orcaman/writerseeker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type expectedSample struct {
	Size     uint32
	Offset   uint64
	DTS      uint64
	PTS      int64
	Duration uint32
	Sync     bool
	Hash     string
}

// The expectations below were cross-checked against ffprobe packets
// (pos, dts, pts, duration, keyframe and SHA-256 of the packet payload) of
// testdata/sample.mp4, shifted into media time by the edit list offsets
// (2048 for track 1 and 1024 for track 2).
var (
	expectedVideoSamples = []expectedSample{
		{3679, 48, 0, 2048, 1024, true, "e59688b17180fb4665546bb812a332d76a1d240b2ec40849273ba9335a1c13f1"},
		{86, 3727, 1024, 3072, 1024, false, "cb5e645dfce36ea4b0fe888c29225dba868dc5955dfb108a9a885a7c3c6e6d08"},
		{545, 3836, 2048, 7168, 1024, false, "aafde3ba2ab1d5c8db80788ca408b30f07ec11d96fb58183f57aa90c58f4ffbe"},
		{180, 4527, 3072, 5120, 1024, false, "d6d481cb3274c1d34fbb8ff6201bcfaa83dbb96cd1351d5b48e6ec2236f42feb"},
		{69, 4864, 4096, 4096, 1024, false, "b93646488fcdb577e068c379338c25f0886e47bd45bef54bcb6e470059f6acec"},
		{60, 5043, 5120, 6144, 1024, false, "defa4eaa696af83ae3731cafec7dea5f102f93d4e82072875eaadb0c20584170"},
		{182, 5227, 6144, 9216, 1024, false, "30ee8ead375e94973abd50574043388bcef450cc4ab4a38581bfa665da8deb8e"},
		{22, 5560, 7168, 8192, 1024, false, "95ed63793970bbec5ad16ed8c8b3beabcff181c5d97c153fb878353be3869c8d"},
		{204, 5702, 8192, 11264, 1024, false, "102c6da700e563daf4e6c0e108ea9aefb6a90da5fe6d169bbaeb42c93d693c3f"},
		{15, 6038, 9216, 10240, 1024, false, "52694ebf22c057119d4deed657727228aad8793e306002e92d8594164f3a9eff"},
	}
	expectedAudioSamples = []expectedSample{
		{23, 3813, 0, 0, 1024, true, "437433fce2b6c6fad24db3dddee87a5bb882e4c85a61cb58225fdc68c92f3475"},
		{39, 4381, 1024, 1024, 1505, true, "7a694fe27bd8e3ff2900f33932dd2984117dcbfaf7de5650f8ce32578bd5c86f"},
		{35, 4420, 2529, 2529, 1024, true, "8d53dcb8afe63bcf9cae1317ea4c2af173c91fd3a864c888107c53d6116db742"},
		{36, 4455, 3553, 3553, 1024, true, "c0fd77164d85bf470aefa27a203b1d4c6f917e5b446e565b998e1024b6195fa6"},
		{36, 4491, 4577, 4577, 1024, true, "58a1d2283712877a11f35b3389472442eaae7be61bd52a127bb7c83b1c7e7beb"},
		{35, 4707, 5601, 5601, 1024, true, "9e8251ac2e35fb2bd4baff90c0844e0b504a2ce9c2a129cdd2be761eeea36491"},
		{31, 4742, 6625, 6625, 1024, true, "3bc0fdd716ec4e9aa9581ea7a7d41203c96f5b6a0ac425511980becf961bb178"},
		{33, 4773, 7649, 7649, 1024, true, "96786a5fb891118540766eb4dd25dff332627982a2a8befaa5be4cc294c7d6e6"},
		{28, 4806, 8673, 8673, 1024, true, "946e5813dafe33b313b567ceddaf0dd89f1e083f4357aa10f0ac292dca1f23ce"},
		{30, 4834, 9697, 9697, 1024, true, "0f4c17fbe45f5ca395656e5cf4941b1eca422dbb6a3b5a29e4cc141982f8eb31"},
		{33, 4933, 10721, 10721, 1024, true, "de8c1dd60d99995f9a4b7f059c6367ca06940beecdf363b67eef4cc72da9dfa7"},
		{29, 4966, 11745, 11745, 1024, true, "cf3480e69b53149b1d6d10dc043d3e1f43df6deacc0d79d197e7471361ee0d56"},
		{23, 4995, 12769, 12769, 1024, true, "bc08447916de82c0ffc33dfed60e38f1246c2513ceded1523b55717ca36e3644"},
		{25, 5018, 13793, 13793, 1024, true, "5a1e8bd154bce2655ddf8689ab8fe1197b548385e014719d8dcb0858bf1e83c4"},
		{25, 5103, 14817, 14817, 1024, true, "869a2db433e3fa7312c1b97a7340a6c65467c3764864bb5ef5b2abe0280a225c"},
		{36, 5128, 15841, 15841, 1024, true, "f8ad6c24f3518df928f9348d4c257b79f50a8f700d7e7ccfbbe82beae36349bf"},
		{27, 5164, 16865, 16865, 1024, true, "6782425825021c4eedf1d5008798f2466bfb5d731dfce6f0ec3df315759f4758"},
		{36, 5191, 17889, 17889, 1024, true, "855bb09bb27a3db653ec58a285f1414bdd401317fc728463d472193b39c18e5d"},
		{35, 5409, 18913, 18913, 1024, true, "5fb4818f043ac5bb2b33644fef3a02bab9c67fb2b9b043e82ed4dc1370af6e79"},
		{25, 5444, 19937, 19937, 1024, true, "02a8d563b63712748a3c578d205553d19bd06b71f38586bd210eee9e0800938b"},
		{28, 5469, 20961, 20961, 1024, true, "906de604fa3bb9d838864a3b31d3d5398ae5c33e56f88a4672d68210bd5f917c"},
		{36, 5497, 21985, 21985, 1024, true, "a8d3a89cd8393eb03975d55f15d7231d2393743de9e6b91290b410e4b841ccef"},
		{27, 5533, 23009, 23009, 1024, true, "543baff028ecb4e649becb8cbec204e0a7be4365d0bee78a70bdd0fa4cfa2f2c"},
		{29, 5582, 24033, 24033, 1024, true, "0dea31104df4c3fdb4eb77fbefa363a3c237936e84a7d5ec3d0139cc628c0142"},
		{28, 5611, 25057, 25057, 1024, true, "45658c88ef8015b0bedad615a5070276387b67d49d83e4afc2439de37a82821f"},
		{29, 5639, 26081, 26081, 1024, true, "35fb16834215e908d6927d35e3aa8aa2138ec71c29438b989460a206b72dbe1c"},
		{34, 5668, 27105, 27105, 1024, true, "7015baf87a0a4f97db22aff13fb4baf4fd87e1811d34505014bb43e43ff4ce1d"},
		{36, 5906, 28129, 28129, 1024, true, "8605b06f46b5cb4eae403b442ac1d4220c280b58fe17f8d27ce5258aa09be108"},
		{30, 5942, 29153, 29153, 1024, true, "d0097dd9f588b7cce54831e165b3e6edc082b5164cc407773e75c6f0e5e2b946"},
		{32, 5972, 30177, 30177, 1024, true, "2393a411a39476e757cd98415ed3a0ed2fc25b0882f6469a769ec2755a822014"},
		{34, 6004, 31201, 31201, 1024, true, "3055606b9ed3bfc144ba3be13e27898b55dde92c16abaa34a24e5b183e1a32b7"},
		{29, 6053, 32225, 32225, 1024, true, "b18fedb9a0e2b977f0fce5db15d729d524576b4d743420b7c3c64eb7d7d83fea"},
		{24, 6082, 33249, 33249, 1024, true, "b8950c61bc6fc571f0fea3179eba12d04bb2a23d6472af740bec1ed4e564e38f"},
		{34, 6106, 34273, 34273, 1024, true, "e5b022546094bf34f33c4fc629055cc8901b991588004f7b469abba0463f07fc"},
		{35, 6140, 35297, 35297, 1024, true, "cfa8c5683867a2bbe17c316ead65ef71e920ea1d51ad41b1b1cda863c0b36efa"},
		{28, 6175, 36321, 36321, 1024, true, "178a807e73b9839c492de99d30d11d43e95ff7c2961aef0c81c58803df85b984"},
		{24, 6203, 37345, 37345, 1024, true, "eab3aa9c6b8c19e6f664354fef748ae3bea0e0877d748c6a40dcb97c432c3440"},
		{30, 6227, 38369, 38369, 1024, true, "31d0d0d04984c81f0c116ff5f11b91f24bce0ee3274ec6eec8b4b9c96166faa1"},
		{29, 6257, 39393, 39393, 1024, true, "df790a8832845eca171d69bfd2fc94dc0186c3faad3c026fef476d13b5c4833a"},
		{29, 6286, 40417, 40417, 1024, true, "c2ca85f2ca94dc733925a90db06dd3e3994cef2f8977d92d3706186cb7939670"},
		{27, 6315, 41441, 41441, 1024, true, "b2abbeef3ee2ea2685fe3af43bd753af1dbde1026ce2906bd3ab496b22834fea"},
		{31, 6342, 42465, 42465, 1024, true, "f0ffe4a9450c9d043c252e70cec834c5a938a9a0e66f124567ca4db65a9b191d"},
		{34, 6373, 43489, 43489, 1024, true, "17ad1eb4bc04d442c54190ede5475bea7a8188a2e58d26cab254b085f85cf637"},
		{35, 6407, 44513, 44513, 611, true, "916dc388e3fdb26753db12c9456539dc9a7e956886344b75b16453ceb4383a91"},
	}
)

func sampleHash(data []byte) string {
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum[:])
}

func requireSample(t *testing.T, sample *MediaSampleData, expected expectedSample) {
	t.Helper()
	assert.Equal(t, expected.Size, sample.Size)
	assert.Equal(t, expected.Offset, sample.Offset)
	assert.Equal(t, expected.DTS, sample.DTS)
	assert.Equal(t, expected.PTS, sample.PTS)
	assert.Equal(t, expected.Duration, sample.Duration)
	assert.Equal(t, expected.Sync, sample.Sync)
	assert.Len(t, sample.Data, int(expected.Size))
	assert.Equal(t, expected.Hash, sampleHash(sample.Data))
}

func TestSampleReaderDualTrack(t *testing.T) {
	f, err := os.Open("./testdata/sample.mp4")
	require.NoError(t, err)
	defer f.Close()

	sr, err := NewSampleReader(f)
	require.NoError(t, err)

	// Both tracks are present and keep the order of the trak boxes.
	assert.Equal(t, []uint32{1, 2}, sr.TrackIDs())

	for _, c := range []struct {
		trackID   uint32
		timescale uint32
		expected  []expectedSample
	}{
		{trackID: 1, timescale: 10240, expected: expectedVideoSamples},
		{trackID: 2, timescale: 44100, expected: expectedAudioSamples},
	} {
		timescale, err := sr.Timescale(c.trackID)
		require.NoError(t, err)
		assert.Equal(t, c.timescale, timescale)

		count, err := sr.SampleCount(c.trackID)
		require.NoError(t, err)
		require.Equal(t, uint32(len(c.expected)), count)

		samples, err := sr.Samples(c.trackID)
		require.NoError(t, err)
		require.Len(t, samples, len(c.expected))

		for i, sample := range samples {
			expected := c.expected[i]
			assert.Equal(t, c.trackID, sample.TrackID)
			assert.Equal(t, uint32(i), sample.Index)
			assert.Equal(t, expected.Size, sample.Size)
			assert.Equal(t, expected.Offset, sample.Offset)
			assert.Equal(t, expected.DTS, sample.DTS)
			assert.Equal(t, expected.PTS, sample.PTS)
			assert.Equal(t, expected.Duration, sample.Duration)
			assert.Equal(t, expected.Sync, sample.Sync)

			// ReadSample returns the same metadata along with the payload.
			data, err := sr.ReadSample(c.trackID, uint32(i))
			require.NoError(t, err)
			requireSample(t, data, expected)
			assert.Equal(t, sample, data.MediaSample)
		}
	}

	// The tracks are interleaved: the first audio chunk lies between the
	// first and the third video chunk in the file.
	assert.Less(t, expectedVideoSamples[0].Offset, expectedAudioSamples[0].Offset)
	assert.Less(t, expectedAudioSamples[0].Offset, expectedVideoSamples[2].Offset)
	assert.Equal(t, expectedVideoSamples[1].Offset+uint64(expectedVideoSamples[1].Size), expectedAudioSamples[0].Offset)
}

func TestSampleReaderReadsPayloadOnDemand(t *testing.T) {
	f, err := os.Open("./testdata/sample.mp4")
	require.NoError(t, err)
	defer f.Close()
	st, err := f.Stat()
	require.NoError(t, err)

	cr := &countingReadSeeker{r: f}
	sr, err := NewSampleReader(cr)
	require.NoError(t, err)

	// Constructing the index reads box headers and sample tables only, not
	// the media payload.
	tableBytes := cr.read
	assert.Less(t, tableBytes, st.Size()/2)
	assert.Less(t, tableBytes, int64(4096))

	// ReadSample reads exactly the payload of the requested sample.
	sample, err := sr.ReadSample(1, 0)
	require.NoError(t, err)
	assert.Equal(t, int64(3679), cr.read-tableBytes)
	assert.Len(t, sample.Data, 3679)

	// Reading the same sample again reads it again; nothing is cached.
	_, err = sr.ReadSample(1, 0)
	require.NoError(t, err)
	assert.Equal(t, int64(2*3679), cr.read-tableBytes)

	// ReadSampleDataTo streams the payload of another sample.
	samples, err := sr.Samples(1)
	require.NoError(t, err)
	before := cr.read
	var buf bytes.Buffer
	n, err := sr.ReadSampleDataTo(&samples[1], &buf)
	require.NoError(t, err)
	assert.Equal(t, int64(86), n)
	assert.Equal(t, int64(86), cr.read-before)
	assert.Equal(t, expectedVideoSamples[1].Hash, sampleHash(buf.Bytes()))

	// A zero sized sample does not issue any read at all.
	zero := samples[1]
	zero.Size = 0
	before = cr.read
	n, err = sr.ReadSampleDataTo(&zero, io.Discard)
	require.NoError(t, err)
	assert.Equal(t, int64(0), n)
	assert.Equal(t, int64(0), cr.read-before)
}

func TestSampleReaderErrors(t *testing.T) {
	t.Run("unknown track", func(t *testing.T) {
		sr := newTestSampleReader(t, buildTestFile(t, &testFileOptions{
			tracks: []*testTrackSpec{smallTrack(1)},
		}))
		_, err := sr.Samples(9)
		requireErrorIs(t, err, ErrTrackNotFound)
		_, err = sr.ReadSample(9, 0)
		requireErrorIs(t, err, ErrTrackNotFound)
	})

	t.Run("index out of range", func(t *testing.T) {
		sr := newTestSampleReader(t, buildTestFile(t, &testFileOptions{
			tracks: []*testTrackSpec{smallTrack(1)},
		}))
		_, err := sr.ReadSample(1, 3)
		requireErrorIs(t, err, ErrSampleIndexOutOfRange)
	})

	t.Run("nil reader", func(t *testing.T) {
		_, err := NewSampleReader(nil)
		assert.Error(t, err)
	})

	t.Run("empty track", func(t *testing.T) {
		file := buildTestFile(t, &testFileOptions{
			tracks: []*testTrackSpec{{trackID: 7, timescale: 90000}},
		})
		sr := newTestSampleReader(t, file)
		assert.Equal(t, []uint32{7}, sr.TrackIDs())
		count, err := sr.SampleCount(7)
		require.NoError(t, err)
		assert.Equal(t, uint32(0), count)
		_, err = sr.Samples(7)
		requireErrorIs(t, err, ErrEmptyTrack)
		_, err = sr.ReadSample(7, 0)
		requireErrorIs(t, err, ErrEmptyTrack)
	})

	t.Run("empty track with samples in stts", func(t *testing.T) {
		file := buildTestFile(t, &testFileOptions{
			tracks: []*testTrackSpec{{trackID: 7, timescale: 90000}},
			mutate: func(tables map[uint32]*testTables) {
				tables[7].stts.Entries = []SttsEntry{{SampleCount: 1, SampleDelta: 1000}}
			},
		})
		_, err := NewSampleReader(bytes.NewReader(file))
		requireErrorIs(t, err, ErrInvalidSampleTable)
	})

	t.Run("stts sample count mismatch", func(t *testing.T) {
		file := buildTestFile(t, &testFileOptions{
			tracks: []*testTrackSpec{smallTrack(1)},
			mutate: func(tables map[uint32]*testTables) {
				tables[1].stts.Entries = []SttsEntry{{SampleCount: 2, SampleDelta: 1000}}
			},
		})
		_, err := NewSampleReader(bytes.NewReader(file))
		requireErrorIs(t, err, ErrInvalidSampleTable)
	})

	t.Run("ctts sample count mismatch", func(t *testing.T) {
		file := buildTestFile(t, &testFileOptions{
			tracks: []*testTrackSpec{smallTrack(1)},
			mutate: func(tables map[uint32]*testTables) {
				tables[1].ctts = &Ctts{Entries: []CttsEntry{{SampleCount: 1, SampleOffsetV0: 10}}}
			},
		})
		_, err := NewSampleReader(bytes.NewReader(file))
		requireErrorIs(t, err, ErrInvalidSampleTable)
	})

	t.Run("stsz sample count mismatch", func(t *testing.T) {
		file := buildTestFile(t, &testFileOptions{
			tracks: []*testTrackSpec{smallTrack(1)},
			mutate: func(tables map[uint32]*testTables) {
				tables[1].stsz.SampleSize = 4
				tables[1].stsz.SampleCount = 4
				tables[1].stsz.EntrySize = nil
			},
		})
		_, err := NewSampleReader(bytes.NewReader(file))
		requireErrorIs(t, err, ErrInvalidSampleTable)
	})

	t.Run("stsc maps too many samples", func(t *testing.T) {
		file := buildTestFile(t, &testFileOptions{
			tracks: []*testTrackSpec{smallTrack(1)},
			mutate: func(tables map[uint32]*testTables) {
				tables[1].stsc.Entries[0].SamplesPerChunk++
			},
		})
		_, err := NewSampleReader(bytes.NewReader(file))
		requireErrorIs(t, err, ErrInvalidSampleTable)
	})

	t.Run("stsc first chunk is not one", func(t *testing.T) {
		file := buildTestFile(t, &testFileOptions{
			tracks: []*testTrackSpec{smallTrack(1)},
			mutate: func(tables map[uint32]*testTables) {
				tables[1].stsc.Entries[0].FirstChunk = 2
			},
		})
		_, err := NewSampleReader(bytes.NewReader(file))
		requireErrorIs(t, err, ErrInvalidSampleTable)
	})

	t.Run("stss sample number out of range", func(t *testing.T) {
		file := buildTestFile(t, &testFileOptions{
			tracks: []*testTrackSpec{smallTrack(1)},
			mutate: func(tables map[uint32]*testTables) {
				tables[1].stss = &Stss{SampleNumber: []uint32{4}}
			},
		})
		_, err := NewSampleReader(bytes.NewReader(file))
		requireErrorIs(t, err, ErrInvalidSampleTable)
	})

	t.Run("chunk offset outside mdat", func(t *testing.T) {
		file := buildTestFile(t, &testFileOptions{
			tracks: []*testTrackSpec{smallTrack(1)},
			mutate: func(tables map[uint32]*testTables) {
				tables[1].stco.ChunkOffset[0] = 0
			},
		})
		_, err := NewSampleReader(bytes.NewReader(file))
		requireErrorIs(t, err, ErrSampleDataOutOfBounds)
	})

	t.Run("chunk offset beyond file", func(t *testing.T) {
		file := buildTestFile(t, &testFileOptions{
			tracks: []*testTrackSpec{smallTrack(1)},
			mutate: func(tables map[uint32]*testTables) {
				tables[1].stco.ChunkOffset[0] = 0xfffffff0
			},
		})
		_, err := NewSampleReader(bytes.NewReader(file))
		requireErrorIs(t, err, ErrSampleDataOutOfBounds)
	})

	t.Run("sample duration overflow", func(t *testing.T) {
		file := buildTestFile(t, &testFileOptions{
			tracks: []*testTrackSpec{smallTrack(1)},
			mutate: func(tables map[uint32]*testTables) {
				tables[1].stsz.SampleSize = 1
				tables[1].stsz.SampleCount = math.MaxUint32
				tables[1].stsz.EntrySize = nil
				tables[1].stts.Entries = []SttsEntry{{SampleCount: math.MaxUint32, SampleDelta: math.MaxUint32}}
				tables[1].stsc.Entries = []StscEntry{{FirstChunk: 1, SamplesPerChunk: math.MaxUint32, SampleDescriptionIndex: 1}}
			},
		})
		_, err := NewSampleReader(bytes.NewReader(file))
		requireErrorIs(t, err, ErrSampleTimeOverflow)
	})

	t.Run("truncated mdat", func(t *testing.T) {
		file := buildTestFile(t, &testFileOptions{
			tracks: []*testTrackSpec{smallTrack(1)},
		})
		// The mdat box is the first box; enlarge its declared size.
		binary.BigEndian.PutUint32(file[0:4], uint32(len(file)+4096))
		_, err := NewSampleReader(bytes.NewReader(file))
		requireErrorIs(t, err, ErrTruncatedMdat)
	})

	t.Run("no moov", func(t *testing.T) {
		buf := &writerseeker.WriterSeeker{}
		w := NewWriter(buf)
		writeTestMdat(t, w, []byte("0123456789"))
		_, err := NewSampleReader(buf.BytesReader())
		requireErrorIs(t, err, ErrUnsupportedSampleLayout)
	})

	t.Run("fragmented file", func(t *testing.T) {
		f, err := os.Open("./testdata/sample_fragmented.mp4")
		require.NoError(t, err)
		defer f.Close()
		_, err = NewSampleReader(f)
		requireErrorIs(t, err, ErrUnsupportedSampleLayout)
	})

	t.Run("initialization segment", func(t *testing.T) {
		f, err := os.Open("./testdata/sample_init.encv.mp4")
		require.NoError(t, err)
		defer f.Close()
		_, err = NewSampleReader(f)
		requireErrorIs(t, err, ErrUnsupportedSampleLayout)
	})

	t.Run("stz2 sample size box", func(t *testing.T) {
		// stz2 has no box definition in this package, so build it by hand:
		// FullBox(version 0, flags 0) + reserved(24) + field_size(8) + sample_count(32).
		stz2 := make([]byte, 0, 20)
		stz2 = append(stz2, EncodeBoxInfo(&BoxInfo{
			Size:       20,
			HeaderSize: SmallHeaderSize,
			Type:       StrToBoxType("stz2"),
		})...)
		stz2 = append(stz2, 0, 0, 0, 0, 0, 0, 0, 8, 0, 0, 0, 1)
		file := buildTestFile(t, &testFileOptions{
			tracks:         []*testTrackSpec{smallTrack(1)},
			extraStblBoxes: [][]byte{stz2},
		})
		_, err := NewSampleReader(bytes.NewReader(file))
		requireErrorIs(t, err, ErrUnsupportedSampleLayout)
	})
}

func TestComposePTS(t *testing.T) {
	testCases := []struct {
		name     string
		dts      uint64
		offset   int64
		expected int64
		hasError bool
	}{
		{name: "zero", dts: 0, offset: 0, expected: 0},
		{name: "positive offset", dts: 1024, offset: 2048, expected: 3072},
		{name: "negative result", dts: 0, offset: -2048, expected: -2048},
		{name: "negative offset", dts: 4096, offset: -2048, expected: 2048},
		{name: "min int32 offset", dts: 0, offset: math.MinInt32, expected: math.MinInt32},
		{name: "max int32 offset", dts: 0, offset: math.MaxInt32, expected: math.MaxInt32},
		{name: "max int64", dts: math.MaxInt64 - 1, offset: 1, expected: math.MaxInt64},
		{name: "positive overflow", dts: math.MaxInt64, offset: 1, hasError: true},
		{name: "uint64 overflow with positive offset", dts: math.MaxUint64, offset: 1, hasError: true},
		{name: "min int64", dts: 0, offset: math.MinInt64, expected: math.MinInt64},
		{name: "large dts with min int32 offset overflows", dts: math.MaxUint64, offset: math.MinInt32, hasError: true},
	}
	for _, c := range testCases {
		t.Run(c.name, func(t *testing.T) {
			pts, err := composePTS(c.dts, c.offset)
			if c.hasError {
				requireErrorIs(t, err, ErrSampleTimeOverflow)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, c.expected, pts)
		})
	}
}

func TestSampleReaderSyntheticTables(t *testing.T) {
	// Two interleaved tracks with variable and constant sample sizes, multi
	// entry stts, ctts version 0 and 1 and explicit sync samples.
	file := buildTestFile(t, &testFileOptions{
		tracks: []*testTrackSpec{
			{
				trackID:   11,
				timescale: 1000,
				samples: []testSampleSpec{
					{size: 4, delta: 100, ctts: 50, hasCtts: true, sync: true},
					{size: 5, delta: 100, ctts: 0, hasCtts: true},
					{size: 6, delta: 100, ctts: -20, hasCtts: true},
				},
				chunks: [][]int{{0}, {1, 2}},
			},
			{
				trackID:   22,
				timescale: 48000,
				samples: []testSampleSpec{
					{size: 7, delta: 1024, sync: true},
					{size: 7, delta: 1024},
					{size: 7, delta: 960, sync: true},
					{size: 7, delta: 1024},
				},
				chunks: [][]int{{0, 1}, {2}, {3}},
			},
		},
		mutate: func(tables map[uint32]*testTables) {
			tables[22].stsz.SampleSize = 7
			tables[22].stsz.SampleCount = 4
			tables[22].stsz.EntrySize = nil
			tables[22].stss = &Stss{SampleNumber: []uint32{1, 3}}
		},
	})

	sr := newTestSampleReader(t, file)
	assert.Equal(t, []uint32{11, 22}, sr.TrackIDs())

	samples, err := sr.Samples(11)
	require.NoError(t, err)
	require.Len(t, samples, 3)
	assert.Equal(t, uint64(0), samples[0].DTS)
	assert.Equal(t, int64(50), samples[0].PTS)
	assert.True(t, samples[0].Sync)
	assert.Equal(t, int64(100), samples[1].PTS)
	assert.False(t, samples[1].Sync)
	assert.Equal(t, int64(180), samples[2].PTS)
	assert.Equal(t, uint32(100), samples[2].Duration)
	for i := range samples {
		data, err := sr.ReadSample(11, uint32(i))
		require.NoError(t, err)
		assert.Equal(t, samples[i].Size, data.Size)
		assert.Equal(t, testPayload(11, uint32(i), samples[i].Size), data.Data)
	}

	audio, err := sr.Samples(22)
	require.NoError(t, err)
	require.Len(t, audio, 4)
	assert.Equal(t, []uint64{0, 1024, 2048, 3008}, []uint64{audio[0].DTS, audio[1].DTS, audio[2].DTS, audio[3].DTS})
	assert.Equal(t, []int64{0, 1024, 2048, 3008}, []int64{audio[0].PTS, audio[1].PTS, audio[2].PTS, audio[3].PTS})
	assert.Equal(t, []uint32{1024, 1024, 960, 1024}, []uint32{audio[0].Duration, audio[1].Duration, audio[2].Duration, audio[3].Duration})
	assert.Equal(t, []bool{true, false, true, false}, []bool{audio[0].Sync, audio[1].Sync, audio[2].Sync, audio[3].Sync})
	for i := range audio {
		data, err := sr.ReadSample(22, uint32(i))
		require.NoError(t, err)
		assert.Equal(t, testPayload(22, uint32(i), audio[i].Size), data.Data)
	}
}

//
// Test file builders
//

type testSampleSpec struct {
	size    uint32
	delta   uint32
	ctts    int32
	hasCtts bool
	sync    bool
}

type testTrackSpec struct {
	trackID   uint32
	timescale uint32
	samples   []testSampleSpec
	chunks    [][]int // chunk -> sample indexes; nil means one sample per chunk
}

type testTables struct {
	stsc *Stsc
	stco *Stco
	stsz *Stsz
	stts *Stts
	ctts *Ctts
	stss *Stss
}

type testFileOptions struct {
	tracks         []*testTrackSpec
	mutate         func(map[uint32]*testTables)
	extraStblBoxes [][]byte
}

func smallTrack(trackID uint32) *testTrackSpec {
	return &testTrackSpec{
		trackID:   trackID,
		timescale: 1000,
		samples: []testSampleSpec{
			{size: 4, delta: 100, sync: true},
			{size: 5, delta: 100},
			{size: 6, delta: 100},
		},
		chunks: [][]int{{0, 1, 2}},
	}
}

func testPayload(trackID, index, size uint32) []byte {
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(trackID*31 + index*7 + uint32(i))
	}
	return data
}

// buildTestFile writes an mdat box followed by a moov box containing the
// requested tracks. Chunks of different tracks are interleaved in the mdat.
func buildTestFile(t *testing.T, opts *testFileOptions) []byte {
	t.Helper()

	tables := make(map[uint32]*testTables, len(opts.tracks))
	for _, track := range opts.tracks {
		tables[track.trackID] = newTestTables(track)
	}

	// Interleave one chunk per track at a time and compute chunk offsets.
	type pendingChunk struct {
		track *testTrackSpec
		idxs  []int
	}
	var order []pendingChunk
	maxChunks := 0
	for _, track := range opts.tracks {
		if n := len(trackChunks(track)); n > maxChunks {
			maxChunks = n
		}
	}
	for ci := 0; ci < maxChunks; ci++ {
		for _, track := range opts.tracks {
			chunks := trackChunks(track)
			if ci < len(chunks) {
				order = append(order, pendingChunk{track: track, idxs: chunks[ci]})
			}
		}
	}

	var mdatPayload bytes.Buffer
	for _, chunk := range order {
		var chunkOffset = mdatPayload.Len() + SmallHeaderSize
		tables[chunk.track.trackID].stco.ChunkOffset = append(tables[chunk.track.trackID].stco.ChunkOffset, uint32(chunkOffset))
		for _, si := range chunk.idxs {
			sample := chunk.track.samples[si]
			mdatPayload.Write(testPayload(chunk.track.trackID, uint32(si), sample.size))
		}
	}

	if opts.mutate != nil {
		opts.mutate(tables)
	}
	for _, t := range tables {
		normalizeTestTables(t)
	}

	buf := &writerseeker.WriterSeeker{}
	w := NewWriter(buf)

	writeTestMdat(t, w, mdatPayload.Bytes())

	// moov > trak*
	moovBI, err := w.StartBox(&BoxInfo{Type: BoxTypeMoov()})
	require.NoError(t, err)
	for _, track := range opts.tracks {
		writeTestTrak(t, w, track, tables[track.trackID], opts.extraStblBoxes)
	}
	_, err = w.EndBox()
	require.NoError(t, err)
	require.True(t, moovBI.Size > 8)

	r := buf.BytesReader()
	file, err := io.ReadAll(r)
	require.NoError(t, err)
	return file
}

func trackChunks(track *testTrackSpec) [][]int {
	if track.chunks != nil {
		return track.chunks
	}
	chunks := make([][]int, len(track.samples))
	for i := range track.samples {
		chunks[i] = []int{i}
	}
	return chunks
}

func newTestTables(track *testTrackSpec) *testTables {
	tables := &testTables{
		stsc: &Stsc{},
		stco: &Stco{},
		stsz: &Stsz{SampleCount: uint32(len(track.samples))},
		stts: &Stts{},
	}
	hasCtts := false
	hasStss := false
	for _, sample := range track.samples {
		tables.stsz.EntrySize = append(tables.stsz.EntrySize, sample.size)
		if sample.hasCtts {
			hasCtts = true
		}
		if sample.sync {
			hasStss = true
		}
	}
	if hasCtts {
		tables.ctts = &Ctts{}
	}
	if hasStss {
		tables.stss = &Stss{}
	}

	chunks := trackChunks(track)
	for ci, idxs := range chunks {
		if ci == 0 || chunks[ci-1] == nil || len(chunks[ci-1]) != len(idxs) {
			tables.stsc.Entries = append(tables.stsc.Entries, StscEntry{
				FirstChunk:             uint32(ci + 1),
				SamplesPerChunk:        uint32(len(idxs)),
				SampleDescriptionIndex: 1,
			})
		}
	}

	for i, sample := range track.samples {
		appendSttsEntry(tables.stts, sample.delta)
		if tables.ctts != nil {
			offset := int32(0)
			if sample.hasCtts {
				offset = sample.ctts
			}
			appendCttsEntry(tables.ctts, offset, hasCtts)
		}
		if sample.sync {
			tables.stss.SampleNumber = append(tables.stss.SampleNumber, uint32(i+1))
		}
	}
	return tables
}

func appendSttsEntry(stts *Stts, delta uint32) {
	if n := len(stts.Entries); n > 0 && stts.Entries[n-1].SampleDelta == delta {
		stts.Entries[n-1].SampleCount++
		return
	}
	stts.Entries = append(stts.Entries, SttsEntry{SampleCount: 1, SampleDelta: delta})
}

func appendCttsEntry(ctts *Ctts, offset int32, version1 bool) {
	if version1 {
		if n := len(ctts.Entries); n > 0 && int32(ctts.Entries[n-1].SampleOffsetV1) == offset {
			ctts.Entries[n-1].SampleCount++
			return
		}
		ctts.Version = 1
		ctts.Entries = append(ctts.Entries, CttsEntry{SampleCount: 1, SampleOffsetV1: offset})
		return
	}
	if n := len(ctts.Entries); n > 0 && int32(ctts.Entries[n-1].SampleOffsetV0) == offset {
		ctts.Entries[n-1].SampleCount++
		return
	}
	ctts.Entries = append(ctts.Entries, CttsEntry{SampleCount: 1, SampleOffsetV0: uint32(offset)})
}

// normalizeTestTables makes the entry counts consistent with the slices after
// the test mutation, so that mutated tables can still be serialized.
func normalizeTestTables(tables *testTables) {
	tables.stsc.EntryCount = uint32(len(tables.stsc.Entries))
	tables.stco.EntryCount = uint32(len(tables.stco.ChunkOffset))
	tables.stts.EntryCount = uint32(len(tables.stts.Entries))
	if tables.ctts != nil {
		tables.ctts.EntryCount = uint32(len(tables.ctts.Entries))
	}
	if tables.stss != nil {
		tables.stss.EntryCount = uint32(len(tables.stss.SampleNumber))
	}
}

func writeTestMdat(t *testing.T, w *Writer, payload []byte) {
	t.Helper()
	_, err := w.StartBox(&BoxInfo{Type: BoxTypeMdat()})
	require.NoError(t, err)
	_, err = w.Write(payload)
	require.NoError(t, err)
	_, err = w.EndBox()
	require.NoError(t, err)
}

func writeTestTrak(t *testing.T, w *Writer, track *testTrackSpec, tables *testTables, extraStblBoxes [][]byte) {
	t.Helper()

	_, err := w.StartBox(&BoxInfo{Type: BoxTypeTrak()})
	require.NoError(t, err)

	_, err = w.StartBox(&BoxInfo{Type: BoxTypeTkhd()})
	require.NoError(t, err)
	_, err = Marshal(w, &Tkhd{TrackID: track.trackID}, Context{})
	require.NoError(t, err)
	_, err = w.EndBox()
	require.NoError(t, err)

	_, err = w.StartBox(&BoxInfo{Type: BoxTypeMdia()})
	require.NoError(t, err)

	_, err = w.StartBox(&BoxInfo{Type: BoxTypeMdhd()})
	require.NoError(t, err)
	_, err = Marshal(w, &Mdhd{Timescale: track.timescale}, Context{})
	require.NoError(t, err)
	_, err = w.EndBox()
	require.NoError(t, err)

	_, err = w.StartBox(&BoxInfo{Type: BoxTypeMinf()})
	require.NoError(t, err)
	_, err = w.StartBox(&BoxInfo{Type: BoxTypeStbl()})
	require.NoError(t, err)

	for _, box := range []IImmutableBox{tables.stsc, tables.stco, tables.stsz, tables.stts} {
		_, err = w.StartBox(&BoxInfo{Type: box.GetType()})
		require.NoError(t, err)
		_, err = Marshal(w, box, Context{})
		require.NoError(t, err)
		_, err = w.EndBox()
		require.NoError(t, err)
	}
	if tables.ctts != nil {
		_, err = w.StartBox(&BoxInfo{Type: BoxTypeCtts()})
		require.NoError(t, err)
		_, err = Marshal(w, tables.ctts, Context{})
		require.NoError(t, err)
		_, err = w.EndBox()
		require.NoError(t, err)
	}
	if tables.stss != nil {
		_, err = w.StartBox(&BoxInfo{Type: BoxTypeStss()})
		require.NoError(t, err)
		_, err = Marshal(w, tables.stss, Context{})
		require.NoError(t, err)
		_, err = w.EndBox()
		require.NoError(t, err)
	}
	for _, raw := range extraStblBoxes {
		_, err = w.Write(raw)
		require.NoError(t, err)
	}

	_, err = w.EndBox() // stbl
	require.NoError(t, err)
	_, err = w.EndBox() // minf
	require.NoError(t, err)
	_, err = w.EndBox() // mdia
	require.NoError(t, err)
	_, err = w.EndBox() // trak
	require.NoError(t, err)
}

func newTestSampleReader(t *testing.T, file []byte) *SampleReader {
	t.Helper()
	sr, err := NewSampleReader(bytes.NewReader(file))
	require.NoError(t, err)
	return sr
}

//
// Helpers
//

type countingReadSeeker struct {
	r    io.ReadSeeker
	read int64
}

func (c *countingReadSeeker) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read += int64(n)
	return n, err
}

func (c *countingReadSeeker) Seek(offset int64, whence int) (int64, error) {
	return c.r.Seek(offset, whence)
}

func requireErrorIs(t *testing.T, err error, target error) {
	t.Helper()
	require.Error(t, err)
	assert.Truef(t, errors.Is(err, target), "expected %v to wrap %v", err, target)
}
