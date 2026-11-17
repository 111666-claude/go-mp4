package mp4

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/orcaman/writerseeker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Reference values computed from testdata/sample_chpl.mp4.
const (
	sampleChplMDATSHA256  = "52fa2f5f544c224f3ac1264c8d5f4f7d57a44c89f6ad3ce12dcf704400f0cb29"
	sampleChplMDATPayload = "e221c546ea47468cb06c9728a51361e2a73c8a6cedd216ac27140f990ebf353e"
	sampleChplBoxSHA256   = "92a595f8873497ee1ff832cc47a9789b26fa141e9d6abce64523e32a71c0bca2"
)

func openFixture(t *testing.T, name string) *os.File {
	t.Helper()
	f, err := os.Open("./testdata/" + name)
	require.NoError(t, err)
	t.Cleanup(func() { f.Close() })
	return f
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// memWriter is an in-memory io.WriteSeeker exposing its content.
type memWriter struct {
	writerseeker.WriterSeeker
}

func (m *memWriter) bytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	_, err := io.Copy(&buf, m.BytesReader())
	require.NoError(t, err)
	return buf.Bytes()
}

// mdatBytes extracts every mdat box including its header.
func mdatBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	r := bytes.NewReader(data)
	infos, err := ExtractBox(r, nil, BoxPath{BoxTypeMdat()})
	require.NoError(t, err)
	require.NotEmpty(t, infos)
	var out []byte
	for _, bi := range infos {
		buf := make([]byte, bi.Size)
		_, err := r.Seek(int64(bi.Offset), io.SeekStart)
		require.NoError(t, err)
		_, err = io.ReadFull(r, buf)
		require.NoError(t, err)
		out = append(out, buf...)
	}
	return out
}

func chplBoxBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	r := bytes.NewReader(data)
	infos, err := ExtractBox(r, nil, BoxPath{BoxTypeMoov(), BoxTypeUdta(), BoxTypeChpl()})
	require.NoError(t, err)
	require.Len(t, infos, 1)
	bi := infos[0]
	buf := make([]byte, bi.Size)
	_, err = r.Seek(int64(bi.Offset), io.SeekStart)
	require.NoError(t, err)
	_, err = io.ReadFull(r, buf)
	require.NoError(t, err)
	return buf
}

func TestReadChaptersFromFixture(t *testing.T) {
	f := openFixture(t, "sample_chpl.mp4")

	chapters, err := ReadChapters(f)
	require.NoError(t, err)
	require.Len(t, chapters, 2)
	assert.Equal(t, Chapter{StartTime: 0, Title: "Chapter One"}, chapters[0])
	assert.Equal(t, Chapter{StartTime: 2 * time.Second, Title: "Chapter Two"}, chapters[1])
}

func TestReadChaptersMissingBox(t *testing.T) {
	f := openFixture(t, "sample.mp4")

	chapters, err := ReadChapters(f)
	require.NoError(t, err)
	assert.Nil(t, chapters)
}

func TestChplBoxBytesMatchFixture(t *testing.T) {
	data, err := os.ReadFile("./testdata/sample_chpl.mp4")
	require.NoError(t, err)

	raw := chplBoxBytes(t, data)
	assert.Equal(t, sampleChplBoxSHA256, sha256Hex(raw))

	// re-marshalling the parsed box must reproduce the exact fixture payload
	f := openFixture(t, "sample_chpl.mp4")
	infos, err := ExtractBox(f, nil, BoxPath{BoxTypeMoov(), BoxTypeUdta(), BoxTypeChpl()})
	require.NoError(t, err)
	chpl, err := readChplPayload(f, infos[0])
	require.NoError(t, err)

	var buf bytes.Buffer
	_, err = Marshal(&buf, chpl, Context{})
	require.NoError(t, err)
	assert.Equal(t, raw[SmallHeaderSize:], buf.Bytes())
}

func TestReplaceChaptersReproducesFixtureBytes(t *testing.T) {
	in := openFixture(t, "sample_chpl.mp4")
	existing, err := ReadChapters(in)
	require.NoError(t, err)

	in2 := openFixture(t, "sample_chpl.mp4")
	out := &memWriter{}
	require.NoError(t, ReplaceChapters(in2, out, existing))

	orig, err := os.ReadFile("./testdata/sample_chpl.mp4")
	require.NoError(t, err)
	// writing the same chapters must yield the exact same chpl box bytes
	assert.Equal(t, chplBoxBytes(t, orig), chplBoxBytes(t, out.bytes(t)))
	// and the only file-size change must be zero (identical layout)
	assert.Len(t, out.bytes(t), len(orig))
}

func TestAddChapterPreservesMedia(t *testing.T) {
	in := openFixture(t, "sample_chpl.mp4")
	out := &memWriter{}

	require.NoError(t, AddChapter(in, out, 4*time.Second, "Chapter Three"))
	got := out.bytes(t)

	// media data must be byte-identical, header and payload alike
	assert.Equal(t, sampleChplMDATSHA256, sha256Hex(mdatBytes(t, got)))
	assert.Equal(t, sampleChplMDATPayload, sha256Hex(mdatBytes(t, got)[SmallHeaderSize:]))

	// the result must re-parse with three chapters
	chapters, err := ReadChapters(bytes.NewReader(got))
	require.NoError(t, err)
	assert.Equal(t, []Chapter{
		{StartTime: 0, Title: "Chapter One"},
		{StartTime: 2 * time.Second, Title: "Chapter Two"},
		{StartTime: 4 * time.Second, Title: "Chapter Three"},
	}, chapters)
}

func TestReplaceChapters(t *testing.T) {
	in := openFixture(t, "sample_chpl.mp4")
	out := &memWriter{}

	newChapters := []Chapter{
		{StartTime: 100 * time.Millisecond, Title: "First"},
		{StartTime: 2500 * time.Millisecond, Title: "Second"},
	}
	require.NoError(t, ReplaceChapters(in, out, newChapters))
	got := out.bytes(t)

	chapters, err := ReadChapters(bytes.NewReader(got))
	require.NoError(t, err)
	assert.Equal(t, newChapters, chapters)

	// media untouched and box well-formed
	assert.Equal(t, sampleChplMDATSHA256, sha256Hex(mdatBytes(t, got)))
	raw := chplBoxBytes(t, got)
	assert.Equal(t, "chpl", string(raw[4:8]))
}

func TestRemoveChapters(t *testing.T) {
	in := openFixture(t, "sample_chpl.mp4")
	out := &memWriter{}

	require.NoError(t, RemoveChapters(in, out))
	got := out.bytes(t)

	chapters, err := ReadChapters(bytes.NewReader(got))
	require.NoError(t, err)
	assert.Nil(t, chapters)

	infos, err := ExtractBox(bytes.NewReader(got), nil, BoxPath{BoxTypeMoov(), BoxTypeUdta(), BoxTypeChpl()})
	require.NoError(t, err)
	assert.Empty(t, infos)

	// media untouched and file shrunk
	assert.Equal(t, sampleChplMDATSHA256, sha256Hex(mdatBytes(t, got)))
	orig, err := os.ReadFile("./testdata/sample_chpl.mp4")
	require.NoError(t, err)
	assert.Less(t, len(got), len(orig))

	// the sibling meta box inside udta survives
	metas, err := ExtractBox(bytes.NewReader(got), nil, BoxPath{BoxTypeMoov(), BoxTypeUdta(), BoxTypeMeta()})
	require.NoError(t, err)
	assert.Len(t, metas, 1)
}

func TestAddChaptersToFileWithUdtaButNoChpl(t *testing.T) {
	// sample.mp4 has moov/udta/meta but no chpl box; mdat precedes moov.
	in := openFixture(t, "sample.mp4")
	out := &memWriter{}

	chapters := []Chapter{
		{StartTime: 0, Title: "Start"},
		{StartTime: 512 * time.Millisecond, Title: "Middle"},
	}
	require.NoError(t, ReplaceChapters(in, out, chapters))
	got := out.bytes(t)

	readBack, err := ReadChapters(bytes.NewReader(got))
	require.NoError(t, err)
	assert.Equal(t, chapters, readBack)

	// legal hierarchy moov/udta/chpl
	infos, err := ExtractBox(bytes.NewReader(got), nil, BoxPath{BoxTypeMoov(), BoxTypeUdta(), BoxTypeChpl()})
	require.NoError(t, err)
	require.Len(t, infos, 1)

	// existing udta child (meta) survives
	metas, err := ExtractBox(bytes.NewReader(got), nil, BoxPath{BoxTypeMoov(), BoxTypeUdta(), BoxTypeMeta()})
	require.NoError(t, err)
	assert.Len(t, metas, 1)

	// media untouched
	orig, err := os.ReadFile("./testdata/sample.mp4")
	require.NoError(t, err)
	assert.Equal(t, sha256Hex(mdatBytes(t, orig)), sha256Hex(mdatBytes(t, got)))
}

func TestAddChaptersToFileWithoutUdta(t *testing.T) {
	buf := &memWriter{}
	require.NoError(t, writeMinimalMoovFile(NewWriter(buf), []uint32{64}))
	minimal := buf.bytes(t)

	out := &memWriter{}
	require.NoError(t, ReplaceChapters(bytes.NewReader(minimal), out, []Chapter{
		{StartTime: 0, Title: "Only"},
	}))
	got := out.bytes(t)

	readBack, err := ReadChapters(bytes.NewReader(got))
	require.NoError(t, err)
	require.Len(t, readBack, 1)
	assert.Equal(t, "Only", readBack[0].Title)

	// legal hierarchy moov/udta/chpl
	infos, err := ExtractBox(bytes.NewReader(got), nil, BoxPath{BoxTypeMoov(), BoxTypeUdta(), BoxTypeChpl()})
	require.NoError(t, err)
	require.Len(t, infos, 1)

	// mdat preserved
	assert.Equal(t, sha256Hex(mdatBytes(t, minimal)), sha256Hex(mdatBytes(t, got)))
}

func TestRewriteIsIdempotentAndKeepsOffsetsWhenMoovAfterMdat(t *testing.T) {
	in := openFixture(t, "sample_chpl.mp4")
	first := &memWriter{}
	chapters := []Chapter{
		{StartTime: 0, Title: "A"},
		{StartTime: 100 * time.Millisecond, Title: "B"},
		{StartTime: 200 * time.Millisecond, Title: "C"},
	}
	require.NoError(t, ReplaceChapters(in, first, chapters))

	// second pass on the rewritten file must produce identical output
	second := &memWriter{}
	require.NoError(t, ReplaceChapters(bytes.NewReader(first.bytes(t)), second, chapters))
	assert.Equal(t, first.bytes(t), second.bytes(t))

	// mdat precedes moov in this fixture, so growing moov must not move mdat
	// and stco offsets must remain exactly the originals
	orig, err := os.ReadFile("./testdata/sample_chpl.mp4")
	require.NoError(t, err)
	assert.Equal(t, stcoOffsets(t, orig), stcoOffsets(t, first.bytes(t)))
	assert.Equal(t, sha256Hex(mdatBytes(t, orig)), sha256Hex(mdatBytes(t, first.bytes(t))))
}

func TestFaststartLayoutShiftsMdatAndPatchesStco(t *testing.T) {
	orig, err := os.ReadFile("./testdata/sample_chpl.mp4")
	require.NoError(t, err)
	fast := reorderMoovFirst(t, orig)

	// sanity: moov now precedes mdat
	moovInfos, err := ExtractBox(bytes.NewReader(fast), nil, BoxPath{BoxTypeMoov()})
	require.NoError(t, err)
	mdatInfos, err := ExtractBox(bytes.NewReader(fast), nil, BoxPath{BoxTypeMdat()})
	require.NoError(t, err)
	require.Less(t, moovInfos[0].Offset, mdatInfos[0].Offset)
	beforeOffsets := stcoOffsets(t, fast)
	// the reordered file itself must still point at the mdat
	assert.NotEmpty(t, beforeOffsets)

	out := &memWriter{}
	require.NoError(t, AddChapter(bytes.NewReader(fast), out, 4500*time.Millisecond, "Late"))
	got := out.bytes(t)

	// mdat shifted by a positive delta; every stco offset shifts equally
	newMdat, err := ExtractBox(bytes.NewReader(got), nil, BoxPath{BoxTypeMdat()})
	require.NoError(t, err)
	delta := int64(newMdat[0].Offset) - int64(mdatInfos[0].Offset)
	assert.Greater(t, delta, int64(0))

	afterOffsets := stcoOffsets(t, got)
	require.Len(t, afterOffsets, len(beforeOffsets))
	for i := range beforeOffsets {
		assert.Equal(t, int64(beforeOffsets[i])+delta, int64(afterOffsets[i]), "stco index %d", i)
	}

	// chapters and media payload still correct
	chapters, err := ReadChapters(bytes.NewReader(got))
	require.NoError(t, err)
	require.Len(t, chapters, 3)
	assert.Equal(t, sampleChplMDATPayload, sha256Hex(mdatBytes(t, got)[SmallHeaderSize:]))

	// every patched offset must still point inside the moved mdat box
	mdatStart := newMdat[0].Offset + SmallHeaderSize
	mdatEnd := newMdat[0].Offset + newMdat[0].Size
	for _, off := range afterOffsets {
		assert.GreaterOrEqual(t, uint64(off), mdatStart)
		assert.Less(t, uint64(off), mdatEnd)
	}
}

func TestChapterValidationErrors(t *testing.T) {
	cases := []struct {
		name     string
		chapters []Chapter
		want     error
	}{
		{"empty title", []Chapter{{StartTime: 0, Title: ""}}, ErrEmptyChapterTitle},
		{"duplicate time", []Chapter{{StartTime: 0, Title: "A"}, {StartTime: 0, Title: "B"}}, ErrChapterTimeOrder},
		{"backward time", []Chapter{{StartTime: 10 * time.Second, Title: "A"}, {StartTime: 9 * time.Second, Title: "B"}}, ErrChapterTimeOrder},
		{"negative time", []Chapter{{StartTime: -time.Second, Title: "A"}}, ErrChapterTimeOrder},
		{"title too long", []Chapter{{StartTime: 0, Title: string(make([]byte, MaxChapterTitleLen+1))}}, ErrChapterTitleTooLong},
		{"invalid utf8", []Chapter{{StartTime: 0, Title: string([]byte{0xff, 0xfe})}}, ErrInvalidChpl},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := openFixture(t, "sample.mp4")
			out := &memWriter{}
			err := ReplaceChapters(in, out, tc.chapters)
			assert.True(t, errors.Is(err, tc.want))
			assert.Empty(t, out.bytes(t), "no output should be written on validation failure")
		})
	}
}

func TestUnsupportedLayouts(t *testing.T) {
	t.Run("no moov", func(t *testing.T) {
		data := []byte{
			0x00, 0x00, 0x00, 0x10, 'f', 't', 'y', 'p',
			'i', 's', 'o', 'm', 0x00, 0x00, 0x00, 0x00,
		}
		out := &memWriter{}
		err := ReplaceChapters(bytes.NewReader(data), out, []Chapter{{StartTime: 0, Title: "X"}})
		assert.True(t, errors.Is(err, ErrNoMoov))
	})

	t.Run("fragmented", func(t *testing.T) {
		in := openFixture(t, "sample_fragmented.mp4")
		out := &memWriter{}
		err := AddChapter(in, out, 0, "X") // zero Duration is valid
		assert.True(t, errors.Is(err, ErrUnsupportedLayout))
	})

	t.Run("box extends to EOF", func(t *testing.T) {
		data := append(
			[]byte{0x00, 0x00, 0x00, 0x00, 'm', 'o', 'o', 'v'}, // size 0: extends to EOF
			make([]byte, 16)...,
		)
		out := &memWriter{}
		err := ReplaceChapters(bytes.NewReader(data), out, []Chapter{{StartTime: 0, Title: "X"}})
		assert.True(t, errors.Is(err, ErrUnsupportedLayout))
	})
}

func TestCorruptedChpl(t *testing.T) {
	orig, err := os.ReadFile("./testdata/sample_chpl.mp4")
	require.NoError(t, err)

	// locate chpl and corrupt its chapter count (claim 255 chapters)
	idx := bytes.LastIndex(orig, []byte("chpl"))
	require.True(t, idx > 0)
	corrupt := make([]byte, len(orig))
	copy(corrupt, orig)
	// header(8) + version(1) + flags(3) + unknown(4) then the count byte
	corrupt[idx+4+8] = 0xff

	_, err = ReadChapters(bytes.NewReader(corrupt))
	assert.Error(t, err)

	out := &memWriter{}
	err = ReplaceChapters(bytes.NewReader(corrupt), out, []Chapter{{StartTime: 0, Title: "X"}})
	assert.True(t, errors.Is(err, ErrInvalidChpl))
	assert.Empty(t, out.bytes(t))
}

func TestStcoOverflowRejected(t *testing.T) {
	// faststart-style file whose stco entry would overflow uint32 when the
	// growing moov box shifts the mdat box forward.
	nearMax := uint32(0xffffffff) - 4
	buf := &memWriter{}
	require.NoError(t, writeMinimalMoovFile(NewWriter(buf), []uint32{nearMax}))
	data := buf.bytes(t)
	// move the moov before mdat so its growth shifts mdat forward
	fast := reorderMoovFirst(t, data)

	out := &memWriter{}
	err := AddChapter(bytes.NewReader(fast), out, time.Second, "X")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrUnsupportedLayout))
}

func TestRewriteOffsetBox(t *testing.T) {
	// build an stco box handle and exercise the shift directly
	stco := &Stco{
		FullBox:     FullBox{Version: 0},
		EntryCount:  2,
		ChunkOffset: []uint32{100, 200},
	}
	var stcoBuf bytes.Buffer
	_, err := Marshal(&stcoBuf, stco, Context{})
	require.NoError(t, err)
	stcoInfo := &BoxInfo{Type: BoxTypeStco(), Size: SmallHeaderSize + uint64(stcoBuf.Len()), HeaderSize: SmallHeaderSize}
	raw := append(EncodeBoxInfo(stcoInfo), stcoBuf.Bytes()...)

	r := bytes.NewReader(raw)
	bi, err := ReadBoxInfo(r)
	require.NoError(t, err)
	_, err = ReadBoxStructureFromInternal(r, bi, func(h *ReadHandle) (interface{}, error) {
		out := &memWriter{}
		w := NewWriter(out)
		require.NoError(t, rewriteOffsetBox(r, w, h, 7))

		// re-parse and confirm every offset shifted by 7
		boxes, err := ExtractBoxesWithPayload(bytes.NewReader(out.bytes(t)), nil,
			[]BoxPath{{BoxTypeStco()}})
		require.NoError(t, err)
		require.Len(t, boxes, 1)
		assert.Equal(t, []uint32{107, 207}, boxes[0].Payload.(*Stco).ChunkOffset)
		return nil, nil
	})
	require.NoError(t, err)

	// co64 underflow must be reported
	co64 := &Co64{
		FullBox:     FullBox{Version: 0},
		EntryCount:  1,
		ChunkOffset: []uint64{5},
	}
	var co64Buf bytes.Buffer
	_, err = Marshal(&co64Buf, co64, Context{})
	require.NoError(t, err)
	co64Info := &BoxInfo{Type: BoxTypeCo64(), Size: SmallHeaderSize + uint64(co64Buf.Len()), HeaderSize: SmallHeaderSize}
	raw64 := append(EncodeBoxInfo(co64Info), co64Buf.Bytes()...)
	r64 := bytes.NewReader(raw64)
	bi64, err := ReadBoxInfo(r64)
	require.NoError(t, err)
	_, err = ReadBoxStructureFromInternal(r64, bi64, func(h *ReadHandle) (interface{}, error) {
		return nil, rewriteOffsetBox(r64, NewWriter(&memWriter{}), h, -10)
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrUnsupportedLayout))
}

// --- synthetic file builder ---

// writeMinimalMoovFile writes ftyp + moov(mvhd, trak/mdia/minf/stbl/stco) + mdat.
// The single stco entry is patched by the caller-controlled offsets.
func writeMinimalMoovFile(w *Writer, offsets []uint32) error {
	startBox := func(t BoxType) error {
		_, err := w.StartBox(&BoxInfo{Type: t})
		return err
	}
	endBox := func() error {
		_, err := w.EndBox()
		return err
	}

	if err := startBox(BoxTypeFtyp()); err != nil {
		return err
	}
	if _, err := Marshal(w, &Ftyp{
		MajorBrand:       [4]byte{'i', 's', 'o', 'm'},
		CompatibleBrands: []CompatibleBrandElem{{CompatibleBrand: [4]byte{'i', 's', 'o', 'm'}}},
	}, Context{}); err != nil {
		return err
	}
	if err := endBox(); err != nil {
		return err
	}

	for _, t := range []BoxType{BoxTypeMoov()} {
		if err := startBox(t); err != nil {
			return err
		}
	}
	if err := startBox(BoxTypeMvhd()); err != nil {
		return err
	}
	if _, err := Marshal(w, &Mvhd{
		FullBox:     FullBox{Version: 0},
		Timescale:   1000,
		DurationV0:  1000,
		NextTrackID: 2,
	}, Context{}); err != nil {
		return err
	}
	if err := endBox(); err != nil { // mvhd
		return err
	}

	for _, t := range []BoxType{BoxTypeTrak(), BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl()} {
		if err := startBox(t); err != nil {
			return err
		}
	}
	if err := startBox(BoxTypeStco()); err != nil {
		return err
	}
	if _, err := Marshal(w, &Stco{
		FullBox:     FullBox{Version: 0},
		EntryCount:  uint32(len(offsets)),
		ChunkOffset: offsets,
	}, Context{}); err != nil {
		return err
	}
	if err := endBox(); err != nil { // stco
		return err
	}
	for range []BoxType{BoxTypeStbl(), BoxTypeMinf(), BoxTypeMdia(), BoxTypeTrak(), BoxTypeMoov()} {
		if err := endBox(); err != nil {
			return err
		}
	}

	if err := startBox(BoxTypeMdat()); err != nil {
		return err
	}
	if _, err := w.Write([]byte("media-payload-media-payload")); err != nil {
		return err
	}
	return endBox()
}

func stcoOffsets(t *testing.T, data []byte) []uint32 {
	t.Helper()
	r := bytes.NewReader(data)
	boxes, err := ExtractBoxesWithPayload(r, nil, []BoxPath{
		{BoxTypeMoov(), BoxTypeTrak(), BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeStco()},
	})
	require.NoError(t, err)
	var out []uint32
	for _, b := range boxes {
		out = append(out, b.Payload.(*Stco).ChunkOffset...)
	}
	return out
}

// reorderMoovFirst produces a faststart-style file: ftyp/free then moov then
// mdat, patching every stco/co64 offset so it still points at the same media.
func reorderMoovFirst(t *testing.T, data []byte) []byte {
	t.Helper()
	r := bytes.NewReader(data)

	top, err := ExtractBoxes(r, nil, []BoxPath{{BoxTypeAny()}})
	require.NoError(t, err)

	var ftyp, free, moov, mdat *BoxInfo
	for _, bi := range top {
		switch bi.Type {
		case BoxTypeFtyp():
			ftyp = bi
		case BoxTypeFree():
			free = bi
		case BoxTypeMoov():
			moov = bi
		case BoxTypeMdat():
			mdat = bi
		}
	}
	require.NotNil(t, ftyp)
	require.NotNil(t, moov)
	require.NotNil(t, mdat)

	final := &memWriter{}
	fw := NewWriter(final)

	require.NoError(t, fw.CopyBox(r, ftyp))
	if free != nil {
		require.NoError(t, fw.CopyBox(r, free))
	}
	moovStart, err := final.Seek(0, io.SeekCurrent)
	require.NoError(t, err)
	newMdatOffset := uint64(moovStart) + moov.Size
	delta := int64(newMdatOffset) - int64(mdat.Offset)

	_, err = ReadBoxStructureFromInternal(r, moov, func(h *ReadHandle) (interface{}, error) {
		switch h.BoxInfo.Type {
		case BoxTypeStco(), BoxTypeCo64():
			return nil, rewriteOffsetBox(r, fw, h, delta)
		}
		if h.BoxInfo.IsSupportedType() && hasChildBoxes(h.BoxInfo.Type) {
			if err := startContainerAndCopyFixedPayload(r, fw, h); err != nil {
				return nil, err
			}
			if _, err := h.Expand(); err != nil {
				return nil, err
			}
			_, err := fw.EndBox()
			return nil, err
		}
		return nil, fw.CopyBox(r, &h.BoxInfo)
	})
	require.NoError(t, err)
	require.NoError(t, fw.CopyBox(r, mdat))

	return final.bytes(t)
}
