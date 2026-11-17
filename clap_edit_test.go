package mp4

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/orcaman/writerseeker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mustMarshal(t *testing.T, w io.Writer, box IImmutableBox) {
	t.Helper()
	_, err := Marshal(w, box, Context{})
	require.NoError(t, err)
}

func startBox(t *testing.T, w *Writer, typ BoxType) {
	t.Helper()
	_, err := w.StartBox(&BoxInfo{Type: typ})
	require.NoError(t, err)
}

func endBox(t *testing.T, w *Writer) {
	t.Helper()
	_, err := w.EndBox()
	require.NoError(t, err)
}

// buildTestMP4 builds a small but valid non-fragmented MP4 in memory.
// Its video sample entry contains an avcC box and a pasp box as siblings,
// and its single mdat box carries deterministic payload bytes.
// Every given clap box is inserted into the avc1 entry (before avcC).
func buildTestMP4(t *testing.T, clapBoxes ...*CleanApertureBox) []byte {
	t.Helper()
	w := &writerseeker.WriterSeeker{}
	writer := NewWriter(w)

	// ftyp
	startBox(t, writer, BoxTypeFtyp())
	mustMarshal(t, writer, &Ftyp{
		MajorBrand:   [4]byte{'i', 's', 'o', 'm'},
		MinorVersion: 512,
		CompatibleBrands: []CompatibleBrandElem{
			{CompatibleBrand: [4]byte{'i', 's', 'o', 'm'}},
			{CompatibleBrand: [4]byte{'a', 'v', 'c', '1'}},
		},
	})
	endBox(t, writer)

	// mdat with deterministic payload
	mdatPayload := []byte{0xde, 0xad, 0xbe, 0xef, 0x00, 0x11, 0x22, 0x33,
		0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb}
	bi, err := writer.StartBox(&BoxInfo{Type: BoxTypeMdat(), Size: SmallHeaderSize + uint64(len(mdatPayload))})
	require.NoError(t, err)
	_, err = writer.Write(mdatPayload)
	require.NoError(t, err)
	mdatInfo, err := writer.EndBox()
	require.NoError(t, err)
	require.Equal(t, SmallHeaderSize+uint64(len(mdatPayload)), mdatInfo.Size)

	// moov
	startBox(t, writer, BoxTypeMoov())

	// mvhd (version 0, minimal)
	startBox(t, writer, BoxTypeMvhd())
	mustMarshal(t, writer, &Mvhd{
		Timescale:  1000,
		DurationV0: 1000,
		Rate:       0x00010000,
	})
	endBox(t, writer)

	// trak
	startBox(t, writer, BoxTypeTrak())

	// tkhd
	startBox(t, writer, BoxTypeTkhd())
	mustMarshal(t, writer, &Tkhd{
		TrackID:    1,
		DurationV0: 1000,
		Width:      120 << 16,
		Height:     80 << 16,
	})
	endBox(t, writer)

	// mdia
	startBox(t, writer, BoxTypeMdia())

	// mdhd
	startBox(t, writer, BoxTypeMdhd())
	mustMarshal(t, writer, &Mdhd{
		Timescale:  1000,
		DurationV0: 1000,
		Language:   [3]byte{'e' - 0x60, 'n' - 0x60, 'g' - 0x60},
	})
	endBox(t, writer)

	// hdlr
	startBox(t, writer, BoxTypeHdlr())
	mustMarshal(t, writer, &Hdlr{
		HandlerType: [4]byte{'v', 'i', 'd', 'e'},
		Name:        "VideoHandler",
	})
	endBox(t, writer)

	// minf
	startBox(t, writer, BoxTypeMinf())

	// vmhd
	startBox(t, writer, BoxTypeVmhd())
	mustMarshal(t, writer, &Vmhd{
		FullBox: FullBox{Flags: [3]byte{0x00, 0x00, 0x01}},
	})
	endBox(t, writer)

	// dinf/dref/url
	startBox(t, writer, BoxTypeDinf())
	startBox(t, writer, BoxTypeDref())
	mustMarshal(t, writer, &Dref{EntryCount: 1})
	startBox(t, writer, BoxTypeUrl())
	mustMarshal(t, writer, &Url{
		FullBox: FullBox{Flags: [3]byte{0x00, 0x00, 0x01}},
	})
	endBox(t, writer)
	endBox(t, writer)
	endBox(t, writer)

	// stbl
	startBox(t, writer, BoxTypeStbl())

	// stsd with one avc1 entry
	startBox(t, writer, BoxTypeStsd())
	mustMarshal(t, writer, &Stsd{EntryCount: 1})

	startBox(t, writer, BoxTypeAvc1())
	mustMarshal(t, writer, &VisualSampleEntry{
		SampleEntry: SampleEntry{
			AnyTypeBox:         AnyTypeBox{Type: BoxTypeAvc1()},
			DataReferenceIndex: 1,
		},
		PreDefined2:     [3]uint32{0x00480000, 0x00480000},
		Width:           120,
		Height:          80,
		Horizresolution: 0x00480000,
		Vertresolution:  0x00480000,
		FrameCount:      1,
		Depth:           24,
		PreDefined3:     -1,
	})

	for _, clapBox := range clapBoxes {
		if clapBox == nil {
			continue
		}
		_, err = writeCleanApertureBox(writer, clapBox, Context{})
		require.NoError(t, err)
	}

	// avcC child
	startBox(t, writer, BoxTypeAvcC())
	mustMarshal(t, writer, &AVCDecoderConfiguration{
		AnyTypeBox:                 AnyTypeBox{Type: BoxTypeAvcC()},
		ConfigurationVersion:       1,
		Profile:                    0x42,
		Level:                      0x1e,
		Reserved:                   0x3f,
		LengthSizeMinusOne:         0x07,
		Reserved2:                  0x07,
		NumOfSequenceParameterSets: 0,
		NumOfPictureParameterSets:  0,
	})
	endBox(t, writer)

	// pasp child
	startBox(t, writer, BoxTypePasp())
	mustMarshal(t, writer, &PixelAspectRatioBox{
		AnyTypeBox: AnyTypeBox{Type: StrToBoxType("pasp")},
		HSpacing:   1,
		VSpacing:   1,
	})
	endBox(t, writer)

	endBox(t, writer) // avc1
	endBox(t, writer) // stsd

	// stts
	startBox(t, writer, BoxTypeStts())
	mustMarshal(t, writer, &Stts{
		EntryCount: 1,
		Entries:    []SttsEntry{{SampleCount: 1, SampleDelta: 1000}},
	})
	endBox(t, writer)

	// stsc
	startBox(t, writer, BoxTypeStsc())
	mustMarshal(t, writer, &Stsc{
		EntryCount: 1,
		Entries:    []StscEntry{{FirstChunk: 1, SamplesPerChunk: 1, SampleDescriptionIndex: 1}},
	})
	endBox(t, writer)

	// stsz
	startBox(t, writer, BoxTypeStsz())
	mustMarshal(t, writer, &Stsz{
		SampleCount: 1,
		EntrySize:   []uint32{uint32(len(mdatPayload))},
	})
	endBox(t, writer)

	// stco pointing at the mdat payload
	mdatPayloadOffset := bi.Offset + bi.HeaderSize
	startBox(t, writer, BoxTypeStco())
	mustMarshal(t, writer, &Stco{
		EntryCount:  1,
		ChunkOffset: []uint32{uint32(mdatPayloadOffset)},
	})
	endBox(t, writer)

	endBox(t, writer) // stbl
	endBox(t, writer) // minf
	endBox(t, writer) // mdia
	endBox(t, writer) // trak
	endBox(t, writer) // moov

	out, err := io.ReadAll(w.Reader())
	require.NoError(t, err)
	return out
}

func sampleCleanAperture() *CleanApertureBox {
	return &CleanApertureBox{
		AnyTypeBox:           AnyTypeBox{Type: StrToBoxType("clap")},
		CleanApertureWidthN:  239,
		CleanApertureWidthD:  2, // 119.5
		CleanApertureHeightN: 79,
		CleanApertureHeightD: 1,
		HorizOffN:            1,
		HorizOffD:            4, // non-integer: 0.25
		VertOffN:             0,
		VertOffD:             1,
	}
}

// mdatChecksum hashes every top-level mdat payload without deep-parsing the
// rest of the file, so unsupported box types cannot disturb the check.
func mdatChecksum(t *testing.T, data []byte) [32]byte {
	t.Helper()
	hash := sha256.New()
	r := bytes.NewReader(data)
	for {
		bi, err := ReadBoxInfo(r)
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		if bi.Type == BoxTypeMdat() {
			_, err = io.CopyN(hash, r, int64(bi.Size-bi.HeaderSize))
			require.NoError(t, err)
		} else {
			_, err = r.Seek(int64(bi.Offset+bi.Size), io.SeekStart)
			require.NoError(t, err)
		}
	}
	var sum [32]byte
	copy(sum[:], hash.Sum(nil))
	return sum
}

func extractClapBoxes(t *testing.T, data []byte) []*CleanApertureBox {
	t.Helper()
	r := bytes.NewReader(data)
	bips, err := ExtractBoxesWithPayload(r, nil, []BoxPath{
		{BoxTypeMoov(), BoxTypeTrak(), BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeStsd(), BoxTypeAvc1(), BoxTypeClap()},
	})
	require.NoError(t, err)
	claps := make([]*CleanApertureBox, 0, len(bips))
	for _, bip := range bips {
		claps = append(claps, bip.Payload.(*CleanApertureBox))
	}
	return claps
}

// extractStcoOffsets returns the chunk offsets of every stco box in file
// order. Files may contain several tracks and therefore several stco boxes.
func extractStcoOffsets(t *testing.T, data []byte) [][]uint32 {
	t.Helper()
	r := bytes.NewReader(data)
	bips, err := ExtractBoxesWithPayload(r, nil, []BoxPath{
		{BoxTypeMoov(), BoxTypeTrak(), BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeStco()},
	})
	require.NoError(t, err)
	out := make([][]uint32, 0, len(bips))
	for _, bip := range bips {
		out = append(out, bip.Payload.(*Stco).ChunkOffset)
	}
	return out
}

func mustReadAll(t *testing.T, w *writerseeker.WriterSeeker) []byte {
	t.Helper()
	b, err := io.ReadAll(w.Reader())
	require.NoError(t, err)
	return b
}

func TestSetCleanApertureInsertsBox(t *testing.T) {
	original := buildTestMP4(t, nil)
	want := sampleCleanAperture()

	out := &writerseeker.WriterSeeker{}
	err := SetCleanAperture(bytes.NewReader(original), out, BoxTypeAvc1(), want)
	require.NoError(t, err)
	edited := mustReadAll(t, out)

	// File grew by exactly one clap box
	assert.Equal(t, len(original)+CleanApertureBoxSize, len(edited))

	// The clap box exists and round-trips exactly
	claps := extractClapBoxes(t, edited)
	require.Len(t, claps, 1)
	assert.Equal(t, want, claps[0])

	// mdat payload is untouched
	assert.Equal(t, mdatChecksum(t, original), mdatChecksum(t, edited))

	// stco still points at the same payload (mdat precedes moov)
	assert.Equal(t, extractStcoOffsets(t, original), extractStcoOffsets(t, edited))

	// Sibling boxes survived
	r := bytes.NewReader(edited)
	bips, err := ExtractBoxes(r, nil, []BoxPath{
		{BoxTypeMoov(), BoxTypeTrak(), BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeStsd(), BoxTypeAvc1(), BoxTypeAvcC()},
		{BoxTypeMoov(), BoxTypeTrak(), BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeStsd(), BoxTypeAvc1(), BoxTypePasp()},
	})
	require.NoError(t, err)
	assert.Len(t, bips, 2)
}

func TestSetCleanApertureReplacesExisting(t *testing.T) {
	first := sampleCleanAperture()
	original := buildTestMP4(t, first)

	replacement := &CleanApertureBox{
		AnyTypeBox:           AnyTypeBox{Type: StrToBoxType("clap")},
		CleanApertureWidthN:  100,
		CleanApertureWidthD:  1,
		CleanApertureHeightN: 60,
		CleanApertureHeightD: 1,
		HorizOffN:            0,
		HorizOffD:            1,
		VertOffN:             0,
		VertOffD:             1,
	}
	out := &writerseeker.WriterSeeker{}
	err := SetCleanAperture(bytes.NewReader(original), out, BoxTypeAvc1(), replacement)
	require.NoError(t, err)
	edited := mustReadAll(t, out)

	// Same size: replacement is same fixed length
	assert.Equal(t, len(original), len(edited))
	claps := extractClapBoxes(t, edited)
	require.Len(t, claps, 1)
	assert.Equal(t, replacement, claps[0])
	assert.NotEqual(t, first, claps[0])
	assert.Equal(t, mdatChecksum(t, original), mdatChecksum(t, edited))
}

func TestRemoveCleanAperture(t *testing.T) {
	original := buildTestMP4(t, sampleCleanAperture())

	out := &writerseeker.WriterSeeker{}
	err := RemoveCleanAperture(bytes.NewReader(original), out, BoxTypeAvc1())
	require.NoError(t, err)
	edited := mustReadAll(t, out)

	assert.Equal(t, len(original)-CleanApertureBoxSize, len(edited))
	assert.Empty(t, extractClapBoxes(t, edited))
	assert.Equal(t, mdatChecksum(t, original), mdatChecksum(t, edited))
	assert.Equal(t, extractStcoOffsets(t, original), extractStcoOffsets(t, edited))

	// Round trip: a file without clap parses cleanly and can be re-edited.
	out2 := &writerseeker.WriterSeeker{}
	require.NoError(t, SetCleanAperture(bytes.NewReader(edited), out2, BoxTypeAvc1(), sampleCleanAperture()))
	reAdded := mustReadAll(t, out2)
	assert.Equal(t, len(original), len(reAdded))
	assert.Len(t, extractClapBoxes(t, reAdded), 1)
}

func TestRemoveCleanApertureWithoutBox(t *testing.T) {
	original := buildTestMP4(t, nil)
	out := &writerseeker.WriterSeeker{}
	err := RemoveCleanAperture(bytes.NewReader(original), out, BoxTypeAvc1())
	assert.True(t, errors.Is(err, ErrCleanApertureNotFound))
}

func TestCleanApertureSampleEntryNotFound(t *testing.T) {
	original := buildTestMP4(t, nil)
	out := &writerseeker.WriterSeeker{}
	err := SetCleanAperture(bytes.NewReader(original), out, BoxTypeHev1(), sampleCleanAperture())
	assert.True(t, errors.Is(err, ErrSampleEntryNotFound))
}

func TestCleanApertureRejectsNonVisualType(t *testing.T) {
	original := buildTestMP4(t, nil)
	out := &writerseeker.WriterSeeker{}
	err := SetCleanAperture(bytes.NewReader(original), out, BoxTypeMp4a(), sampleCleanAperture())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a video sample entry")
}

func TestSetCleanApertureRejectsInvalidValues(t *testing.T) {
	original := buildTestMP4(t, nil)

	cases := []struct {
		name string
		clap *CleanApertureBox
	}{
		{
			name: "zero denominator",
			clap: &CleanApertureBox{
				AnyTypeBox:          AnyTypeBox{Type: StrToBoxType("clap")},
				CleanApertureWidthN: 100, CleanApertureWidthD: 0,
				CleanApertureHeightN: 80, CleanApertureHeightD: 1,
				HorizOffN: 0, HorizOffD: 1, VertOffN: 0, VertOffD: 1,
			},
		},
		{
			name: "aperture wider than image",
			clap: &CleanApertureBox{
				AnyTypeBox:          AnyTypeBox{Type: StrToBoxType("clap")},
				CleanApertureWidthN: 200, CleanApertureWidthD: 1,
				CleanApertureHeightN: 80, CleanApertureHeightD: 1,
				HorizOffN: 0, HorizOffD: 1, VertOffN: 0, VertOffD: 1,
			},
		},
		{
			name: "out of bounds horizontal offset",
			clap: &CleanApertureBox{
				AnyTypeBox:          AnyTypeBox{Type: StrToBoxType("clap")},
				CleanApertureWidthN: 100, CleanApertureWidthD: 1,
				CleanApertureHeightN: 80, CleanApertureHeightD: 1,
				HorizOffN: 20, HorizOffD: 1, // right = 60+20+50 = 130 > 120
				VertOffN: 0, VertOffD: 1,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := &writerseeker.WriterSeeker{}
			err := SetCleanAperture(bytes.NewReader(original), out, BoxTypeAvc1(), tc.clap)
			require.Error(t, err)
			// Nothing should have been written on a pre-validation failure path.
			got, rerr := io.ReadAll(out.Reader())
			require.NoError(t, rerr)
			assert.Empty(t, got)
		})
	}
}

func TestSetCleanApertureDuplicateClapRejected(t *testing.T) {
	// A structurally valid file that violates the at-most-one-clap rule.
	original := buildTestMP4(t, sampleCleanAperture(), sampleCleanAperture())

	out := &writerseeker.WriterSeeker{}
	err := SetCleanAperture(bytes.NewReader(original), out, BoxTypeAvc1(), sampleCleanAperture())
	assert.True(t, errors.Is(err, ErrDuplicateCleanAperture))
}

func TestSetCleanApertureCorruptExistingClapRejected(t *testing.T) {
	original := buildTestMP4(t, sampleCleanAperture())

	r := bytes.NewReader(original)
	clapBIs, err := ExtractBoxes(r, nil, []BoxPath{{
		BoxTypeMoov(), BoxTypeTrak(), BoxTypeMdia(), BoxTypeMinf(),
		BoxTypeStbl(), BoxTypeStsd(), BoxTypeAvc1(), BoxTypeClap(),
	}})
	require.NoError(t, err)
	require.Len(t, clapBIs, 1)
	clapBI := clapBIs[0]

	corrupt := make([]byte, len(original))
	copy(corrupt, original)
	// CleanApertureWidthD is the second uint32 of the clap payload.
	for i := clapBI.Offset + clapBI.HeaderSize + 4; i < clapBI.Offset+clapBI.HeaderSize+8; i++ {
		corrupt[i] = 0
	}

	out := &writerseeker.WriterSeeker{}
	err = SetCleanAperture(bytes.NewReader(corrupt), out, BoxTypeAvc1(), sampleCleanAperture())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "zero denominator")
}

func TestSetCleanApertureClapAtWrongHierarchy(t *testing.T) {
	// A valid file (it contains an avc1, so preflight passes) with an extra
	// clap placed directly under moov, which is an illegal hierarchy.
	original := buildTestMP4(t, nil)

	moovBIs, err := ExtractBoxes(bytes.NewReader(original), nil, []BoxPath{{BoxTypeMoov()}})
	require.NoError(t, err)
	require.Len(t, moovBIs, 1)
	moov := moovBIs[0]

	// Build the misplaced clap bytes via the Writer so header/sizes are valid.
	cw := &writerseeker.WriterSeeker{}
	cWriter := NewWriter(cw)
	_, err = writeCleanApertureBox(cWriter, sampleCleanAperture(), Context{})
	require.NoError(t, err)
	clapBytes, err := io.ReadAll(cw.Reader())
	require.NoError(t, err)

	// Splice it in as the first child of moov and fix the moov size header.
	edited := make([]byte, 0, len(original)+len(clapBytes))
	edited = append(edited, original[:moov.Offset+moov.HeaderSize]...)
	edited = append(edited, clapBytes...)
	edited = append(edited, original[moov.Offset+moov.HeaderSize:]...)
	binaryBigEndianPutUint32(edited[moov.Offset:], uint32(moov.Size)+uint32(len(clapBytes)))

	out := &writerseeker.WriterSeeker{}
	err = SetCleanAperture(bytes.NewReader(edited), out, BoxTypeAvc1(), sampleCleanAperture())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be a child of a video sample entry")
}

func binaryBigEndianPutUint32(b []byte, v uint32) {
	b[0] = byte(v >> 24)
	b[1] = byte(v >> 16)
	b[2] = byte(v >> 8)
	b[3] = byte(v)
}

func TestSetCleanApertureOnSampleFixture(t *testing.T) {
	// The committed fixture has no clap; editing it must stay parseable, keep
	// the mdat checksum stable and preserve the existing pasp/avcC boxes.
	original, err := os.ReadFile("./testdata/sample.mp4")
	require.NoError(t, err)

	out := &writerseeker.WriterSeeker{}
	err = SetCleanAperture(bytes.NewReader(original), out, BoxTypeAvc1(), &CleanApertureBox{
		AnyTypeBox:           AnyTypeBox{Type: StrToBoxType("clap")},
		CleanApertureWidthN:  319,
		CleanApertureWidthD:  2, // 159.5, leaves room for the 0.5 offset
		CleanApertureHeightN: 180,
		CleanApertureHeightD: 1,
		HorizOffN:            1,
		HorizOffD:            2, // 0.5, non-integer
		VertOffN:             0,
		VertOffD:             1,
	})
	require.NoError(t, err)
	edited := mustReadAll(t, out)

	require.Len(t, extractClapBoxes(t, edited), 1)
	assert.Equal(t, mdatChecksum(t, original), mdatChecksum(t, edited))
	assert.Equal(t, len(original)+CleanApertureBoxSize, len(edited))
	// Fast-start file: mdat precedes moov, so stco offsets must be unchanged.
	assert.Equal(t, extractStcoOffsets(t, original), extractStcoOffsets(t, edited))

	// The whole edited file still parses from the root. Unsupported boxes
	// (the fixture contains a "loci" box) are skipped rather than expanded.
	_, err = ReadBoxStructure(bytes.NewReader(edited), func(h *ReadHandle) (interface{}, error) {
		if h.BoxInfo.IsSupportedType() {
			return h.Expand()
		}
		return nil, nil
	})
	require.NoError(t, err)
}

// topLevelBoxes splits a file into its top-level boxes.
func topLevelBoxes(t *testing.T, data []byte) []*BoxInfo {
	t.Helper()
	r := bytes.NewReader(data)
	var out []*BoxInfo
	for {
		bi, err := ReadBoxInfo(r)
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		out = append(out, bi)
		_, err = r.Seek(int64(bi.Offset+bi.Size), io.SeekStart)
		require.NoError(t, err)
	}
	return out
}

// toMoovFirst rewrites a fast-start (mdat before moov) file into a moov-first
// file and patches the stco chunk offset to the new mdat payload position.
func toMoovFirst(t *testing.T, data []byte) []byte {
	t.Helper()
	boxes := topLevelBoxes(t, data)
	var ftyp, moov, mdat *BoxInfo
	for _, b := range boxes {
		switch b.Type {
		case BoxTypeFtyp():
			ftyp = b
		case BoxTypeMoov():
			moov = b
		case BoxTypeMdat():
			mdat = b
		}
	}
	require.NotNil(t, ftyp)
	require.NotNil(t, moov)
	require.NotNil(t, mdat)

	// New mdat payload sits after ftyp + moov + mdat header.
	moovBytes := data[moov.Offset : moov.Offset+moov.Size]
	newMdatPayloadOffset := ftyp.Size + moov.Size + mdat.HeaderSize

	// Patch stco inside the copied moov: find "stco", then skip
	// type(4)+version/flags(4)+entryCount(4) to reach the offset field.
	patched := append([]byte(nil), moovBytes...)
	idx := bytes.Index(patched, []byte("stco"))
	require.NotEqual(t, -1, idx)
	valuePos := idx + 4 + 4 + 4
	binaryBigEndianPutUint32(patched[valuePos:], uint32(newMdatPayloadOffset))

	out := make([]byte, 0, len(data))
	out = append(out, data[ftyp.Offset:ftyp.Offset+ftyp.Size]...)
	out = append(out, patched...)
	out = append(out, data[mdat.Offset:mdat.Offset+mdat.Size]...)
	return out
}

func TestSetCleanApertureOnWireBytes(t *testing.T) {
	original := buildTestMP4(t, nil)
	want := sampleCleanAperture()

	out := &writerseeker.WriterSeeker{}
	require.NoError(t, SetCleanAperture(bytes.NewReader(original), out, BoxTypeAvc1(), want))
	edited := mustReadAll(t, out)

	// Locate the clap type tag and inspect the raw 40-byte box.
	idx := bytes.Index(edited, []byte("clap"))
	require.NotEqual(t, -1, idx)
	size := binaryBigEndianUint32(edited[idx-4:])
	assert.Equal(t, uint32(CleanApertureBoxSize), size)

	payload := edited[idx+4 : idx+4+32]
	got := make([]uint32, 8)
	for i := range got {
		got[i] = binaryBigEndianUint32(payload[i*4:])
	}
	// widthN,widthD,heightN,heightD,horizN,horizD,vertN,vertD
	assert.Equal(t, []uint32{239, 2, 79, 1, 1, 4, 0, 1}, got)
}

func binaryBigEndianUint32(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

func TestSetCleanApertureTruncatedFileRejected(t *testing.T) {
	original := buildTestMP4(t, nil)
	// Cutting the file mid-moov makes box boundaries inconsistent.
	truncated := original[:len(original)-40]

	out := &writerseeker.WriterSeeker{}
	err := SetCleanAperture(bytes.NewReader(truncated), out, BoxTypeAvc1(), sampleCleanAperture())
	require.Error(t, err)
	got, rerr := io.ReadAll(out.Reader())
	require.NoError(t, rerr)
	assert.Empty(t, got)
}

func TestRemoveCleanApertureTruncatedFileRejected(t *testing.T) {
	original := buildTestMP4(t, sampleCleanAperture())
	truncated := original[:len(original)-40]

	out := &writerseeker.WriterSeeker{}
	err := RemoveCleanAperture(bytes.NewReader(truncated), out, BoxTypeAvc1())
	require.Error(t, err)
}

func TestSetCleanApertureMoovFirstUpdatesStco(t *testing.T) {
	// Start from a valid fast-start file, then move moov ahead of mdat.
	fastStart := buildTestMP4(t, nil)
	original := toMoovFirst(t, fastStart)

	before := extractStcoOffsets(t, original)
	require.Len(t, before, 1)
	require.Len(t, before[0], 1)

	out := &writerseeker.WriterSeeker{}
	err := SetCleanAperture(bytes.NewReader(original), out, BoxTypeAvc1(), sampleCleanAperture())
	require.NoError(t, err)
	edited := mustReadAll(t, out)

	assert.Equal(t, len(original)+CleanApertureBoxSize, len(edited))
	require.Len(t, extractClapBoxes(t, edited), 1)

	// mdat moved later by exactly one clap box, so its offset must increase.
	after := extractStcoOffsets(t, edited)
	require.Len(t, after, 1)
	require.Len(t, after[0], 1)
	assert.Equal(t, before[0][0]+uint32(CleanApertureBoxSize), after[0][0])

	// The patched offset really points at the unchanged payload.
	mdatBIs, err := ExtractBoxes(bytes.NewReader(edited), nil, []BoxPath{{BoxTypeMdat()}})
	require.NoError(t, err)
	require.Len(t, mdatBIs, 1)
	mdatBI := mdatBIs[0]
	assert.Equal(t, after[0][0], uint32(mdatBI.Offset+mdatBI.HeaderSize))
	assert.Equal(t, mdatChecksum(t, original), mdatChecksum(t, edited))
}

// toCo64 rewrites a file's stco box into a co64 box (same entry count, 64-bit
// offsets) and patches the enclosing sizes.
func toCo64(t *testing.T, data []byte) []byte {
	t.Helper()
	r := bytes.NewReader(data)
	stcoBIs, err := ExtractBoxesWithPayload(r, nil, []BoxPath{
		{BoxTypeMoov(), BoxTypeTrak(), BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeStco()},
	})
	require.NoError(t, err)
	require.Len(t, stcoBIs, 1)
	stcoBI := stcoBIs[0].Info
	stco := stcoBIs[0].Payload.(*Stco)

	// Build the replacement co64 box.
	cw := &writerseeker.WriterSeeker{}
	cWriter := NewWriter(cw)
	startBox(t, cWriter, BoxTypeCo64())
	offsets := make([]uint64, len(stco.ChunkOffset))
	for i, o := range stco.ChunkOffset {
		offsets[i] = uint64(o)
	}
	mustMarshal(t, cWriter, &Co64{EntryCount: stco.EntryCount, ChunkOffset: offsets})
	endBox(t, cWriter)
	co64Bytes, err := io.ReadAll(cw.Reader())
	require.NoError(t, err)

	// Splice, growing every ancestor between stco and the file root.
	out := append([]byte(nil), data[:stcoBI.Offset]...)
	out = append(out, co64Bytes...)
	out = append(out, data[stcoBI.Offset+stcoBI.Size:]...)
	growth := len(co64Bytes) - int(stcoBI.Size)

	// Fix sizes of stbl, minf, mdia, trak, moov (all ancestors of stco).
	rr := bytes.NewReader(data)
	ancestors, err := ExtractBoxes(rr, nil, []BoxPath{
		{BoxTypeMoov()},
		{BoxTypeMoov(), BoxTypeTrak()},
		{BoxTypeMoov(), BoxTypeTrak(), BoxTypeMdia()},
		{BoxTypeMoov(), BoxTypeTrak(), BoxTypeMdia(), BoxTypeMinf()},
		{BoxTypeMoov(), BoxTypeTrak(), BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl()},
	})
	require.NoError(t, err)
	// Patch from the outermost box first; offsets in the new slice are stable
	// because every ancestor starts before the splice point.
	for _, a := range ancestors {
		binaryBigEndianPutUint32(out[a.Offset:], uint32(a.Size)+uint32(growth))
	}
	return out
}

func TestSetCleanApertureWithCo64(t *testing.T) {
	original := toCo64(t, buildTestMP4(t, nil))

	// Sanity: the converted file has a co64 and no stco.
	co64s, err := ExtractBoxesWithPayload(bytes.NewReader(original), nil, []BoxPath{
		{BoxTypeMoov(), BoxTypeTrak(), BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeCo64()},
	})
	require.NoError(t, err)
	require.Len(t, co64s, 1)
	before := co64s[0].Payload.(*Co64).ChunkOffset
	require.Len(t, before, 1)

	out := &writerseeker.WriterSeeker{}
	require.NoError(t, SetCleanAperture(bytes.NewReader(original), out, BoxTypeAvc1(), sampleCleanAperture()))
	edited := mustReadAll(t, out)

	assert.Equal(t, len(original)+CleanApertureBoxSize, len(edited))
	require.Len(t, extractClapBoxes(t, edited), 1)
	assert.Equal(t, mdatChecksum(t, original), mdatChecksum(t, edited))

	// Fast-start layout: mdat precedes moov, so co64 offsets stay unchanged.
	co64s2, err := ExtractBoxesWithPayload(bytes.NewReader(edited), nil, []BoxPath{
		{BoxTypeMoov(), BoxTypeTrak(), BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeCo64()},
	})
	require.NoError(t, err)
	require.Len(t, co64s2, 1)
	assert.Equal(t, before, co64s2[0].Payload.(*Co64).ChunkOffset)
}

func TestRemoveCleanApertureMoovFirstUpdatesStco(t *testing.T) {
	// A moov-first file that already contains a clap: removing it shrinks moov
	// and must pull the stco offset back.
	original := toMoovFirst(t, buildTestMP4(t, sampleCleanAperture()))
	before := extractStcoOffsets(t, original)
	require.Len(t, before, 1)

	out := &writerseeker.WriterSeeker{}
	require.NoError(t, RemoveCleanAperture(bytes.NewReader(original), out, BoxTypeAvc1()))
	edited := mustReadAll(t, out)

	assert.Equal(t, len(original)-CleanApertureBoxSize, len(edited))
	assert.Empty(t, extractClapBoxes(t, edited))
	after := extractStcoOffsets(t, edited)
	require.Len(t, after, 1)
	assert.Equal(t, before[0][0]-uint32(CleanApertureBoxSize), after[0][0])
	assert.Equal(t, mdatChecksum(t, original), mdatChecksum(t, edited))
}
