package mp4

import (
	"bytes"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/orcaman/writerseeker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func chrom16(v uint16) Chromaticity16 {
	return Chromaticity16{Chromaticity: v}
}

func TestBoxTypesISO14496_15(t *testing.T) {
	testCases := []struct {
		name string
		src  IImmutableBox
		dst  IBox
		bin  []byte
		str  string
		ctx  Context
	}{
		{
			name: "mdcv",
			src: &Mdcv{
				DisplayPrimariesR:     [2]Chromaticity16{chrom16(35400), chrom16(14600)},
				DisplayPrimariesG:     [2]Chromaticity16{chrom16(8500), chrom16(39850)},
				DisplayPrimariesB:     [2]Chromaticity16{chrom16(6550), chrom16(2300)},
				WhitePoint:            [2]Chromaticity16{chrom16(15635), chrom16(16450)},
				MinMasteringLuminance: 50,       // 0.0050 cd/m^2
				MaxMasteringLuminance: 10000000, // 1000.0000 cd/m^2
			},
			dst: &Mdcv{},
			bin: []byte{
				0x8a, 0x48, // display primary R x = 35400
				0x39, 0x08, // display primary R y = 14600
				0x21, 0x34, // display primary G x = 8500
				0x9b, 0xaa, // display primary G y = 39850
				0x19, 0x96, // display primary B x = 6550
				0x08, 0xfc, // display primary B y = 2300
				0x3d, 0x13, // white point x = 15635
				0x40, 0x42, // white point y = 16450
				0x00, 0x00, 0x00, 0x32, // min mastering luminance = 50
				0x00, 0x98, 0x96, 0x80, // max mastering luminance = 10000000
			},
			str: `DisplayPrimariesR=[{Chromaticity=35400}, {Chromaticity=14600}] ` +
				`DisplayPrimariesG=[{Chromaticity=8500}, {Chromaticity=39850}] ` +
				`DisplayPrimariesB=[{Chromaticity=6550}, {Chromaticity=2300}] ` +
				`WhitePoint=[{Chromaticity=15635}, {Chromaticity=16450}] ` +
				`MinMasteringLuminance=50 MaxMasteringLuminance=10000000`,
		},
		{
			name: "clli",
			src: &Clli{
				MaxContentLightLevel:        1000,
				MaxPictureAverageLightLevel: 400,
			},
			dst: &Clli{},
			bin: []byte{
				0x03, 0xe8, // MaxCLL = 1000
				0x01, 0x90, // MaxFALL = 400
			},
			str: `MaxContentLightLevel=1000 MaxPictureAverageLightLevel=400`,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Marshal
			buf := bytes.NewBuffer(nil)
			n, err := Marshal(buf, tc.src, tc.ctx)
			require.NoError(t, err)
			assert.Equal(t, uint64(len(tc.bin)), n)
			assert.Equal(t, tc.bin, buf.Bytes())

			// Unmarshal
			r := bytes.NewReader(tc.bin)
			n, err = Unmarshal(r, uint64(len(tc.bin)), tc.dst, tc.ctx)
			require.NoError(t, err)
			assert.Equal(t, uint64(buf.Len()), n)
			assert.Equal(t, tc.src, tc.dst)
			s, err := r.Seek(0, io.SeekCurrent)
			require.NoError(t, err)
			assert.Equal(t, int64(buf.Len()), s)

			// UnmarshalAny
			dst, n, err := UnmarshalAny(bytes.NewReader(tc.bin), tc.src.GetType(), uint64(len(tc.bin)), tc.ctx)
			require.NoError(t, err)
			assert.Equal(t, uint64(buf.Len()), n)
			assert.Equal(t, tc.src, dst)

			// Stringify
			str, err := Stringify(tc.src, tc.ctx)
			require.NoError(t, err)
			assert.Equal(t, tc.str, str)
		})
	}
}

func TestMdcvSizeErrors(t *testing.T) {
	t.Run("too short", func(t *testing.T) {
		bin := make([]byte, MdcvPayloadSize-1)
		_, err := Unmarshal(bytes.NewReader(bin), uint64(len(bin)), &Mdcv{}, Context{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "mdcv box payload size must be 24 bytes")
	})
	t.Run("too long", func(t *testing.T) {
		bin := make([]byte, MdcvPayloadSize+1)
		_, err := Unmarshal(bytes.NewReader(bin), uint64(len(bin)), &Mdcv{}, Context{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "mdcv box payload size must be 24 bytes")
	})
}

func TestClliSizeErrors(t *testing.T) {
	t.Run("too short", func(t *testing.T) {
		bin := make([]byte, ClliPayloadSize-1)
		_, err := Unmarshal(bytes.NewReader(bin), uint64(len(bin)), &Clli{}, Context{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "clli box payload size must be 4 bytes")
	})
	t.Run("too long", func(t *testing.T) {
		bin := make([]byte, ClliPayloadSize+1)
		_, err := Unmarshal(bytes.NewReader(bin), uint64(len(bin)), &Clli{}, Context{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "clli box payload size must be 4 bytes")
	})
}

func TestMdcvChromaticityRange(t *testing.T) {
	newMdcv := func() *Mdcv {
		return &Mdcv{
			DisplayPrimariesR:     [2]Chromaticity16{chrom16(35400), chrom16(14600)},
			DisplayPrimariesG:     [2]Chromaticity16{chrom16(8500), chrom16(39850)},
			DisplayPrimariesB:     [2]Chromaticity16{chrom16(6550), chrom16(2300)},
			WhitePoint:            [2]Chromaticity16{chrom16(15635), chrom16(16450)},
			MinMasteringLuminance: 50,
			MaxMasteringLuminance: 10000000,
		}
	}

	t.Run("boundary values are accepted", func(t *testing.T) {
		mdcv := newMdcv()
		mdcv.WhitePoint[1].Chromaticity = MaxChromaticity
		buf := bytes.NewBuffer(nil)
		_, err := Marshal(buf, mdcv, Context{})
		require.NoError(t, err)

		var got Mdcv
		_, err = Unmarshal(bytes.NewReader(buf.Bytes()), uint64(buf.Len()), &got, Context{})
		require.NoError(t, err)
		assert.Equal(t, mdcv, &got)
	})

	t.Run("write rejects out of range", func(t *testing.T) {
		mdcv := newMdcv()
		mdcv.DisplayPrimariesB[0].Chromaticity = MaxChromaticity + 1
		_, err := Marshal(bytes.NewBuffer(nil), mdcv, Context{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "mdcv DisplayPrimariesB.x out of range")
	})

	t.Run("read rejects out of range", func(t *testing.T) {
		// valid box with the green primary y coordinate set to 50001
		bin := []byte{
			0x8a, 0x48, // R x
			0x39, 0x08, // R y
			0x21, 0x34, // G x
			0xc3, 0x51, // G y = 50001
			0x19, 0x96, // B x
			0x08, 0xfc, // B y
			0x3d, 0x13, // white x
			0x40, 0x42, // white y
			0x00, 0x00, 0x00, 0x32,
			0x00, 0x98, 0x96, 0x80,
		}
		_, err := Unmarshal(bytes.NewReader(bin), uint64(len(bin)), &Mdcv{}, Context{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "mdcv DisplayPrimariesG.y out of range: value=50001 max=50000")
	})

	t.Run("read rejects out of range white point", func(t *testing.T) {
		// The WhitePoint pair is the last coordinate field. This also proves
		// the range hook runs after the WhitePoint fields have been read.
		bin := []byte{
			0x8a, 0x48, // R x
			0x39, 0x08, // R y
			0x21, 0x34, // G x
			0x9b, 0xaa, // G y
			0x19, 0x96, // B x
			0x08, 0xfc, // B y
			0xc3, 0x51, // white x = 50001 (invalid)
			0x40, 0x42, // white y
			0x00, 0x00, 0x00, 0x32,
			0x00, 0x98, 0x96, 0x80,
		}
		_, err := Unmarshal(bytes.NewReader(bin), uint64(len(bin)), &Mdcv{}, Context{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "mdcv WhitePoint.x out of range: value=50001 max=50000")
	})

	t.Run("write rejects min luminance greater than max", func(t *testing.T) {
		mdcv := newMdcv()
		mdcv.MinMasteringLuminance = 10000001
		mdcv.MaxMasteringLuminance = 10000000
		_, err := Marshal(bytes.NewBuffer(nil), mdcv, Context{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "MinMasteringLuminance must not exceed MaxMasteringLuminance")
	})

	t.Run("read rejects min luminance greater than max", func(t *testing.T) {
		buf := bytes.NewBuffer(nil)
		// Bypass Marshal validation to construct an invalid on-disk box.
		_, err := buf.Write([]byte{
			0x8a, 0x48, 0x39, 0x08,
			0x21, 0x34, 0x9b, 0xaa,
			0x19, 0x96, 0x08, 0xfc,
			0x3d, 0x13, 0x40, 0x42,
			0x00, 0x98, 0x96, 0x81, // min = 10000001
			0x00, 0x98, 0x96, 0x80, // max = 10000000
		})
		require.NoError(t, err)
		_, err = Unmarshal(bytes.NewReader(buf.Bytes()), uint64(buf.Len()), &Mdcv{}, Context{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "MinMasteringLuminance must not exceed MaxMasteringLuminance")
	})
}

// buildHDRMP4 builds an in-memory MP4 containing an avc1 video sample
// entry with the given child boxes. It returns the serialized bytes.
func buildHDRMP4(t *testing.T, children ...IBox) []byte {
	t.Helper()
	mem := &writerseeker.WriterSeeker{}
	w := NewWriter(mem)

	// ftyp
	_, err := w.StartBox(&BoxInfo{Type: BoxTypeFtyp()})
	require.NoError(t, err)
	_, err = Marshal(w, &Ftyp{
		MajorBrand:       [4]byte{'i', 's', 'o', 'm'},
		MinorVersion:     512,
		CompatibleBrands: []CompatibleBrandElem{{CompatibleBrand: [4]byte{'i', 's', 'o', 'm'}}},
	}, Context{})
	require.NoError(t, err)
	_, err = w.EndBox()
	require.NoError(t, err)

	// moov/trak/mdia/minf/stbl/stsd/avc1 + children
	start := func(typ BoxType) {
		t.Helper()
		_, err := w.StartBox(&BoxInfo{Type: typ})
		require.NoError(t, err)
	}
	end := func() {
		t.Helper()
		_, err := w.EndBox()
		require.NoError(t, err)
	}

	start(BoxTypeMoov())
	start(BoxTypeTrak())
	start(BoxTypeMdia())
	start(BoxTypeMinf())
	start(BoxTypeStbl())
	start(BoxTypeStsd())
	_, err = Marshal(w, &Stsd{FullBox: FullBox{}, EntryCount: 1}, Context{})
	require.NoError(t, err)

	start(BoxTypeAvc1())
	_, err = Marshal(w, &VisualSampleEntry{
		SampleEntry: SampleEntry{
			AnyTypeBox:         AnyTypeBox{Type: BoxTypeAvc1()},
			DataReferenceIndex: 1,
		},
		Width:           1920,
		Height:          1080,
		Horizresolution: 0x00480000,
		Vertresolution:  0x00480000,
		FrameCount:      1,
		Depth:           0x0018,
		PreDefined3:     -1,
	}, Context{})
	require.NoError(t, err)
	for _, child := range children {
		start(child.GetType())
		_, err = Marshal(w, child, Context{})
		require.NoError(t, err)
		end()
	}
	end() // avc1
	end() // stsd
	end() // stbl
	end() // minf
	end() // mdia
	end() // trak
	end() // moov

	out, err := io.ReadAll(mem.Reader())
	require.NoError(t, err)
	return out
}

func sampleMdcv() *Mdcv {
	return &Mdcv{
		DisplayPrimariesR:     [2]Chromaticity16{chrom16(35400), chrom16(14600)},
		DisplayPrimariesG:     [2]Chromaticity16{chrom16(8500), chrom16(39850)},
		DisplayPrimariesB:     [2]Chromaticity16{chrom16(6550), chrom16(2300)},
		WhitePoint:            [2]Chromaticity16{chrom16(15635), chrom16(16450)},
		MinMasteringLuminance: 50,
		MaxMasteringLuminance: 10000000,
	}
}

func sampleClli() *Clli {
	return &Clli{
		MaxContentLightLevel:        1000,
		MaxPictureAverageLightLevel: 400,
	}
}

func TestHDRBoxesInVisualSampleEntry(t *testing.T) {
	bin := buildHDRMP4(t, sampleMdcv(), sampleClli())

	var gotMdcv *Mdcv
	var gotClli *Clli
	var hdrPath BoxPath
	_, err := ReadBoxStructure(bytes.NewReader(bin), func(h *ReadHandle) (interface{}, error) {
		switch h.BoxInfo.Type {
		case BoxTypeMdcv():
			box, _, err := h.ReadPayload()
			require.NoError(t, err)
			gotMdcv = box.(*Mdcv)
			hdrPath = append(BoxPath{}, h.Path...)
		case BoxTypeClli():
			box, _, err := h.ReadPayload()
			require.NoError(t, err)
			gotClli = box.(*Clli)
		}
		return h.Expand()
	})
	require.NoError(t, err)
	require.NotNil(t, gotMdcv)
	require.NotNil(t, gotClli)
	assert.Equal(t, sampleMdcv(), gotMdcv)
	assert.Equal(t, sampleClli(), gotClli)
	assert.Equal(t, BoxPath{BoxTypeMoov(), BoxTypeTrak(), BoxTypeMdia(), BoxTypeMinf(),
		BoxTypeStbl(), BoxTypeStsd(), BoxTypeAvc1(), BoxTypeMdcv()}, hdrPath)
}

func TestHDRBoxRoundTripPreservesBytesAndPayload(t *testing.T) {
	original := buildHDRMP4(t, sampleMdcv(), sampleClli())

	// Rewrite the whole file through the canonical StartBox/EndBox path.
	// ReadPayload is called for every box: it determines the boundary
	// between the payload and the child boxes (childrenOffset), so Expand
	// visits only the children. The mdcv box is re-marshalled from the
	// parsed struct; every other box payload is copied verbatim. No box is
	// moved and no media payload is altered.
	mem := &writerseeker.WriterSeeker{}
	w := NewWriter(mem)
	r := bytes.NewReader(original)
	_, err := ReadBoxStructure(r, func(h *ReadHandle) (interface{}, error) {
		_, err := w.StartBox(&h.BoxInfo)
		require.NoError(t, err)

		// Establish the payload/children boundary by reading the payload.
		payload, payloadN, err := h.ReadPayload()
		require.NoError(t, err)

		if h.BoxInfo.Type == BoxTypeMdcv() {
			_, err = Marshal(w, payload, h.BoxInfo.Context)
			require.NoError(t, err)
		} else if payloadN > 0 {
			// Re-seek to the payload start and copy the exact payload bytes.
			_, err = h.BoxInfo.SeekToPayload(r)
			require.NoError(t, err)
			_, err = io.CopyN(w, r, int64(payloadN))
			require.NoError(t, err)
		}

		vals, err := h.Expand()
		require.NoError(t, err)

		_, err = w.EndBox()
		require.NoError(t, err)
		return vals, nil
	})
	require.NoError(t, err)

	out, err := io.ReadAll(mem.Reader())
	require.NoError(t, err)

	// Exact byte-for-byte round trip: no box moved and no payload changed.
	assert.Equal(t, original, out)
}

func TestDuplicateHDRBoxRejected(t *testing.T) {
	bin := buildHDRMP4(t, sampleMdcv(), sampleMdcv())
	_, err := ReadBoxStructure(bytes.NewReader(bin), func(h *ReadHandle) (interface{}, error) {
		return h.Expand()
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrDuplicateHDRBox), "unexpected error: %v", err)
	assert.Contains(t, err.Error(), "mdcv")
}

func TestHDRBoxesInSeparateSampleEntriesAreIndependent(t *testing.T) {
	// One stsd with two avc1 entries, each carrying its own mdcv. The two
	// mdcv boxes are not duplicates because they belong to different sample
	// entries identified by their file offsets.
	mem := &writerseeker.WriterSeeker{}
	w := NewWriter(mem)

	start := func(typ BoxType) {
		t.Helper()
		_, err := w.StartBox(&BoxInfo{Type: typ})
		require.NoError(t, err)
	}
	end := func() {
		t.Helper()
		_, err := w.EndBox()
		require.NoError(t, err)
	}
	writeVSE := func() {
		t.Helper()
		start(BoxTypeAvc1())
		_, err := Marshal(w, &VisualSampleEntry{
			SampleEntry: SampleEntry{
				AnyTypeBox:         AnyTypeBox{Type: BoxTypeAvc1()},
				DataReferenceIndex: 1,
			},
			Width: 1920, Height: 1080,
			Horizresolution: 0x00480000,
			Vertresolution:  0x00480000,
			FrameCount:      1,
			Depth:           0x0018,
			PreDefined3:     -1,
		}, Context{})
		require.NoError(t, err)
		start(BoxTypeMdcv())
		_, err = Marshal(w, sampleMdcv(), Context{})
		require.NoError(t, err)
		end() // mdcv
		end() // avc1
	}

	start(BoxTypeMoov())
	start(BoxTypeTrak())
	start(BoxTypeMdia())
	start(BoxTypeMinf())
	start(BoxTypeStbl())
	start(BoxTypeStsd())
	_, err := Marshal(w, &Stsd{FullBox: FullBox{}, EntryCount: 2}, Context{})
	require.NoError(t, err)
	writeVSE()
	writeVSE()
	end() // stsd
	end() // stbl
	end() // minf
	end() // mdia
	end() // trak
	end() // moov

	var mdcvCount int
	_, err = ReadBoxStructure(bytes.NewReader(mustReadAll(t, mem)), func(h *ReadHandle) (interface{}, error) {
		if h.BoxInfo.Type == BoxTypeMdcv() {
			mdcvCount++
		}
		return h.Expand()
	})
	require.NoError(t, err)
	assert.Equal(t, 2, mdcvCount)
}

func mustReadAll(t *testing.T, mem *writerseeker.WriterSeeker) []byte {
	t.Helper()
	b, err := io.ReadAll(mem.Reader())
	require.NoError(t, err)
	return b
}

func TestHDRBoxWrongHierarchyRejected(t *testing.T) {
	// Place mdcv directly under stsd (sibling of the avc1 sample entry)
	// instead of inside the video sample entry.
	mem := &writerseeker.WriterSeeker{}
	w := NewWriter(mem)

	start := func(typ BoxType) {
		t.Helper()
		_, err := w.StartBox(&BoxInfo{Type: typ})
		require.NoError(t, err)
	}
	end := func() {
		t.Helper()
		_, err := w.EndBox()
		require.NoError(t, err)
	}

	start(BoxTypeFtyp())
	_, err := Marshal(w, &Ftyp{
		MajorBrand:       [4]byte{'i', 's', 'o', 'm'},
		MinorVersion:     512,
		CompatibleBrands: []CompatibleBrandElem{{CompatibleBrand: [4]byte{'i', 's', 'o', 'm'}}},
	}, Context{})
	require.NoError(t, err)
	end()

	start(BoxTypeMoov())
	start(BoxTypeTrak())
	start(BoxTypeMdia())
	start(BoxTypeMinf())
	start(BoxTypeStbl())
	start(BoxTypeStsd())
	_, err = Marshal(w, &Stsd{FullBox: FullBox{}, EntryCount: 1}, Context{})
	require.NoError(t, err)

	start(BoxTypeMdcv())
	_, err = Marshal(w, sampleMdcv(), Context{})
	require.NoError(t, err)
	end() // mdcv

	end() // stsd
	end() // stbl
	end() // minf
	end() // mdia
	end() // trak
	end() // moov

	wrongHierarchy, err := io.ReadAll(mem.Reader())
	require.NoError(t, err)
	_, err = ReadBoxStructure(bytes.NewReader(wrongHierarchy), func(h *ReadHandle) (interface{}, error) {
		return h.Expand()
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidHDRBoxPlacement), "unexpected error: %v", err)
	assert.Contains(t, err.Error(), "mdcv")
}

func TestNonHDRMP4Unaffected(t *testing.T) {
	// The existing sample MP4 contains neither mdcv nor clli (and includes an
	// unsupported box type). Copying it box-by-box must reproduce identical
	// bytes and never return an HDR validation error.
	original, err := os.ReadFile("./testdata/sample.mp4")
	require.NoError(t, err)

	mem := &writerseeker.WriterSeeker{}
	w := NewWriter(mem)
	r := bytes.NewReader(original)
	_, err = ReadBoxStructure(r, func(h *ReadHandle) (interface{}, error) {
		// CopyBox copies the complete box (header and payload) and expands
		// nothing, so unsupported boxes and media payloads stay untouched.
		require.NoError(t, w.CopyBox(r, &h.BoxInfo))
		return nil, nil
	})
	require.NoError(t, err)
	out, err := io.ReadAll(mem.Reader())
	require.NoError(t, err)
	assert.Equal(t, original, out)
}

func TestBoxPathString(t *testing.T) {
	assert.Equal(t, "", BoxPath(nil).String())
	assert.Equal(t, "moov/trak", BoxPath{BoxTypeMoov(), BoxTypeTrak()}.String())
}
