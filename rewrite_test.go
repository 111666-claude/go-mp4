package mp4

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// boxBuilder accumulates a serialized box tree in memory for test fixtures.
type boxBuilder struct {
	buf bytes.Buffer
}

// rawBox prepends an 8 byte small header around payload.
func rawBox(typ BoxType, payload []byte) []byte {
	size := SmallHeaderSize + len(payload)
	out := make([]byte, 0, size)
	hdr := make([]byte, SmallHeaderSize)
	binary.BigEndian.PutUint32(hdr, uint32(size))
	copy(hdr[4:], typ[:])
	out = append(out, hdr...)
	out = append(out, payload...)
	return out
}

// fullBoxPrefix builds the 4 byte version/flags prefix of a FullBox.
func fullBoxPrefix(version uint8, flags uint32) []byte {
	return []byte{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}
}

// stcoBox builds an stco box with the given chunk offsets.
func stcoBox(offsets []uint32) []byte {
	payload := fullBoxPrefix(0, 0)
	cnt := make([]byte, 4)
	binary.BigEndian.PutUint32(cnt, uint32(len(offsets)))
	payload = append(payload, cnt...)
	for _, o := range offsets {
		b := make([]byte, 4)
		binary.BigEndian.PutUint32(b, o)
		payload = append(payload, b...)
	}
	return rawBox(BoxTypeStco(), payload)
}

// co64Box builds a co64 box with the given chunk offsets.
func co64Box(offsets []uint64) []byte {
	payload := fullBoxPrefix(0, 0)
	cnt := make([]byte, 4)
	binary.BigEndian.PutUint32(cnt, uint32(len(offsets)))
	payload = append(payload, cnt...)
	for _, o := range offsets {
		b := make([]byte, 8)
		binary.BigEndian.PutUint64(b, o)
		payload = append(payload, b...)
	}
	return rawBox(BoxTypeCo64(), payload)
}

// buildSyntheticMP4 assembles an MP4 with a single track. When moovFirst is
// false the mdat is emitted before the moov. chunkOffsets are written into the
// stco verbatim, so callers pass the intended input offsets.
func buildSyntheticMP4(t *testing.T, moovFirst bool, stco []byte, extraMoovChildren ...[]byte) []byte {
	t.Helper()

	ftyp := rawBox(BoxTypeFtyp(), append([]byte{'i', 's', 'o', 'm', 0, 0, 0, 0}, []byte{'i', 's', 'o', 'm'}...))

	// moov > trak > mdia > minf > stbl > stco, plus any extra direct children.
	stbl := rawBox(BoxTypeStbl(), stco)
	minf := rawBox(BoxTypeMinf(), stbl)
	mdia := rawBox(BoxTypeMdia(), minf)
	trak := rawBox(BoxTypeTrak(), mdia)

	moovPayload := trak
	for _, child := range extraMoovChildren {
		moovPayload = append(moovPayload, child...)
	}
	moov := rawBox(BoxTypeMoov(), moovPayload)

	// A fixed media payload whose hash we can verify.
	mdatPayload := bytes.Repeat([]byte{0xAB, 0xCD, 0xEF, 0x01, 0x23}, 400) // 2000 bytes
	mdat := rawBox(BoxTypeMdat(), mdatPayload)

	if moovFirst {
		return append(append(append([]byte{}, ftyp...), moov...), mdat...)
	}
	return append(append(append([]byte{}, ftyp...), mdat...), moov...)
}

// parseTopLevel returns the top-level box infos of data.
func parseTopLevel(t *testing.T, data []byte) []*BoxInfo {
	t.Helper()
	var infos []*BoxInfo
	_, err := ReadBoxStructure(bytes.NewReader(data), func(h *ReadHandle) (interface{}, error) {
		cp := h.BoxInfo
		infos = append(infos, &cp)
		return nil, nil
	})
	require.NoError(t, err)
	return infos
}

func findBox(t *testing.T, data []byte, path ...BoxType) *BoxInfo {
	t.Helper()
	bis, err := ExtractBox(bytes.NewReader(data), nil, BoxPath(path))
	require.NoError(t, err)
	require.Len(t, bis, 1)
	return bis[0]
}

func mdatPayloadHash(data []byte, bi *BoxInfo) [32]byte {
	start := bi.Offset + bi.HeaderSize
	end := bi.Offset + bi.Size
	return sha256.Sum256(data[start:end])
}

func mdatOffsetAndSize(data []byte) (offset, size uint64) {
	bis, err := ExtractBox(bytes.NewReader(data), nil, BoxPath{BoxTypeMdat()})
	if err != nil || len(bis) != 1 {
		return 0, 0
	}
	return bis[0].Offset + bis[0].HeaderSize, bis[0].Size - bis[0].HeaderSize
}

// rewrite runs RewriteFile and returns the output bytes.
func rewrite(t *testing.T, input []byte, handler RewriteHandler, opts ...RewriteOption) []byte {
	t.Helper()
	var out bytes.Buffer
	// bytes.Buffer is not a WriteSeeker; RewriteFile only needs io.Writer for
	// output. Wrap via a writer that also supports Truncate to exercise that.
	w := &truncateableBuffer{Buffer: &out}
	err := RewriteFile(bytes.NewReader(input), w, handler, opts...)
	require.NoError(t, err)
	return out.Bytes()
}

type truncateableBuffer struct {
	*bytes.Buffer
}

func (b *truncateableBuffer) Truncate(n int64) error {
	if n != 0 {
		return errors.New("test only supports truncate to zero")
	}
	b.Buffer.Reset()
	return nil
}

// noopHandler preserves every box.
func noopHandler(_ *RewriteHandle) error { return nil }

func TestRewriteFile_NoOpIsByteIdentical(t *testing.T) {
	// Compute the real mdat payload offset for the stco.
	base := buildSyntheticMP4(t, true, stcoBox([]uint32{0}))
	mdatOff, _ := mdatOffsetAndSize(base)
	input := buildSyntheticMP4(t, true, stcoBox([]uint32{uint32(mdatOff)}))

	output := rewrite(t, input, noopHandler)
	assert.Equal(t, input, output)
}

func TestRewriteFile_ReTraversableAndMdatHashStable_MoovFirst(t *testing.T) {
	base := buildSyntheticMP4(t, true, stcoBox([]uint32{0}))
	mdatOff, mdatSize := mdatOffsetAndSize(base)

	// Replace an inserted udta with a larger one.
	udtaOld := rawBox(BoxTypeUdta(), []byte("old-metadata"))
	inputWithUdta := buildSyntheticMP4(t, true, stcoBox([]uint32{uint32(mdatOff)}), udtaOld)
	// Rebuild because adding udta shifts mdat.
	mdatOff2, _ := mdatOffsetAndSize(inputWithUdta)
	inputWithUdta = buildSyntheticMP4(t, true, stcoBox([]uint32{uint32(mdatOff2)}), udtaOld)
	mdatOff3, _ := mdatOffsetAndSize(inputWithUdta)
	require.Equal(t, mdatOff2, mdatOff3)

	udtaNew := rawBox(BoxTypeUdta(), []byte("brand-new-metadata-payload"))
	output := rewrite(t, inputWithUdta, func(h *RewriteHandle) error {
		if h.BoxInfo.Type == BoxTypeUdta() {
			_, err := h.Writer.Write(udtaNew)
			return err
		}
		return nil
	})

	// Output must re-parse cleanly.
	toplevel := parseTopLevel(t, output)
	require.NotEmpty(t, toplevel)

	// mdat payload hash unchanged.
	inMdat := findBox(t, inputWithUdta, BoxTypeMdat())
	outMdat := findBox(t, output, BoxTypeMdat())
	assert.Equal(t, mdatPayloadHash(inputWithUdta, inMdat), mdatPayloadHash(output, outMdat))
	assert.Equal(t, mdatSize, outMdat.Size-outMdat.HeaderSize)

	// mdat moved by exactly the udta size delta.
	delta := int64(len(udtaNew)) - int64(len(udtaOld))
	assert.Equal(t, int64(inMdat.Offset)+delta, int64(outMdat.Offset))

	// stco offset shifted by the same delta.
	inStco := findBox(t, inputWithUdta, BoxTypeMoov(), BoxTypeTrak(), BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeStco())
	outStco := findBox(t, output, BoxTypeMoov(), BoxTypeTrak(), BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeStco())
	assert.Equal(t, inStco.Offset+uint64(delta) > 0, true)
	// Read the entry directly from bytes.
	inEntry := binary.BigEndian.Uint32(inputWithUdta[inStco.Offset+inStco.HeaderSize+8:])
	outEntry := binary.BigEndian.Uint32(output[outStco.Offset+outStco.HeaderSize+8:])
	assert.Equal(t, uint32(int64(inEntry)+delta), outEntry)
	assert.Equal(t, uint32(outMdat.Offset+outMdat.HeaderSize), outEntry)

	// udta content updated.
	outUdta := findBox(t, output, BoxTypeMoov(), BoxTypeUdta())
	assert.Equal(t, uint64(len(udtaNew)), outUdta.Size)
}

func TestRewriteFile_MoovAfterMdat_OffsetsUntouchedAndBytesIdentical(t *testing.T) {
	base := buildSyntheticMP4(t, false, stcoBox([]uint32{0}))
	mdatOff, _ := mdatOffsetAndSize(base)
	input := buildSyntheticMP4(t, false, stcoBox([]uint32{uint32(mdatOff)}))

	// Grow a free box inside moov; mdat precedes moov so its position cannot
	// change and stco values must stay identical.
	freeOld := rawBox(BoxTypeFree(), []byte("xx"))
	input = buildSyntheticMP4(t, false, stcoBox([]uint32{uint32(mdatOff)}), freeOld)
	mdatOff2, _ := mdatOffsetAndSize(input)
	require.Equal(t, mdatOff, mdatOff2)

	freeNew := rawBox(BoxTypeFree(), []byte("yyyyyyyy"))
	output := rewrite(t, input, func(h *RewriteHandle) error {
		if h.BoxInfo.Type == BoxTypeFree() {
			_, err := h.Writer.Write(freeNew)
			return err
		}
		return nil
	})

	inMdat := findBox(t, input, BoxTypeMdat())
	outMdat := findBox(t, output, BoxTypeMdat())
	assert.Equal(t, inMdat.Offset, outMdat.Offset)
	assert.Equal(t, mdatPayloadHash(input, inMdat), mdatPayloadHash(output, outMdat))

	inStco := findBox(t, input, BoxTypeMoov(), BoxTypeTrak(), BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeStco())
	outStco := findBox(t, output, BoxTypeMoov(), BoxTypeTrak(), BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeStco())
	inEntry := binary.BigEndian.Uint32(input[inStco.Offset+inStco.HeaderSize+8:])
	outEntry := binary.BigEndian.Uint32(output[outStco.Offset+outStco.HeaderSize+8:])
	assert.Equal(t, inEntry, outEntry)
}

func TestRewriteFile_StcoWideningDecision(t *testing.T) {
	// The widening predicate is purely arithmetic on the shifted value.
	assert.True(t, fitsUint32(math.MaxUint32))
	assert.False(t, fitsUint32(math.MaxUint32+1))
	assert.True(t, fitsUint32(math.MaxUint32-1))

	// nodeOutputSize must reflect the stco -> co64 growth: the entry width
	// doubles while the 8 byte fixed prefix is unchanged.
	stco := stcoBox([]uint32{10, 20, 30})
	sr := bytes.NewReader(stco)
	bi, err := ReadBoxInfo(sr)
	require.NoError(t, err)
	node := &rewriteNode{info: *bi, leaf: true, widen: true}
	size, header, err := nodeOutputSize(node)
	require.NoError(t, err)
	// original payload = 8 + 3*4 = 20; widened payload = 8 + 3*8 = 32.
	assert.Equal(t, bi.HeaderSize+32, size)
	assert.Equal(t, bi.HeaderSize, header)
	assert.Equal(t, BoxTypeCo64(), node.outputType())
}

func TestRewriteFile_StcoWidenedToCo64OnOverflow(t *testing.T) {
	// Exercise the full stco->co64 emission path directly on a small fixture:
	// build a normal moov-first file, mark the stco node for widening through
	// the real layout/emit machinery, and assert the output is a valid co64
	// whose entries equal the mdat payload position. The arithmetic overflow
	// predicate itself is covered by TestRewriteFile_StcoWideningDecision.
	base := buildSyntheticMP4(t, true, stcoBox([]uint32{0}))
	mdatOff, _ := mdatOffsetAndSize(base)
	input := buildSyntheticMP4(t, true, stcoBox([]uint32{uint32(mdatOff)}))

	r := bytes.NewReader(input)
	fileSize := int64(len(input))
	roots, err := scanRewriteNodes(r, 0, uint64(fileSize), true, nil, Context{}, noopHandler)
	require.NoError(t, err)

	// Locate and force the stco node to widen.
	var stcoNode *rewriteNode
	var find func([]*rewriteNode)
	find = func(ns []*rewriteNode) {
		for _, n := range ns {
			if n.info.Type == BoxTypeStco() {
				stcoNode = n
			}
			find(n.children)
		}
	}
	find(roots)
	require.NotNil(t, stcoNode)
	stcoNode.widen = true

	state := &rewriteState{r: r, root: roots}
	require.NoError(t, computeLayout(roots, 0))
	// Offset validation would reject a forced widen on a non-overflowing value
	// only in its decision; here widen is already set, so layout/emission run.
	var out bytes.Buffer
	require.NoError(t, state.emit(&out, 64))

	outBytes := out.Bytes()
	co := findBox(t, outBytes, BoxTypeMoov(), BoxTypeTrak(), BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeCo64())
	require.NotNil(t, co)
	outMdat := findBox(t, outBytes, BoxTypeMdat())
	entry := binary.BigEndian.Uint64(outBytes[co.Offset+co.HeaderSize+8:])
	assert.Equal(t, outMdat.Offset+outMdat.HeaderSize, entry)
	require.NotEmpty(t, parseTopLevel(t, outBytes))
}

func TestRewriteFile_Co64LargeOffsets(t *testing.T) {
	// co64 entries use 64 bit offsets; verify normal shifting works and the
	// output remains co64 with corrected values.
	base := buildSyntheticMP4(t, true, co64Box([]uint64{0}))
	mdatOff, _ := mdatOffsetAndSize(base)
	udtaOld := rawBox(BoxTypeUdta(), []byte("a"))
	input := buildSyntheticMP4(t, true, co64Box([]uint64{mdatOff}), udtaOld)
	mdatOff2, _ := mdatOffsetAndSize(input)
	input = buildSyntheticMP4(t, true, co64Box([]uint64{mdatOff2}), udtaOld)
	mdatOff3, _ := mdatOffsetAndSize(input)
	require.Equal(t, mdatOff2, mdatOff3)

	udtaNew := rawBox(BoxTypeUdta(), []byte("ccccccccc"))
	output := rewrite(t, input, func(h *RewriteHandle) error {
		if h.BoxInfo.Type == BoxTypeUdta() {
			_, err := h.Writer.Write(udtaNew)
			return err
		}
		return nil
	})

	outCo64 := findBox(t, output, BoxTypeMoov(), BoxTypeTrak(), BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeCo64())
	outMdat := findBox(t, output, BoxTypeMdat())
	entry := binary.BigEndian.Uint64(output[outCo64.Offset+outCo64.HeaderSize+8:])
	assert.Equal(t, outMdat.Offset+outMdat.HeaderSize, entry)
}

func TestRewriteFile_UnknownBoxBytesPreserved(t *testing.T) {
	// An unknown leaf box inside a known container must survive verbatim.
	unknownType := BoxType{'x', 'y', 'z', 'w'}
	unknown := rawBox(unknownType, []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x00, 0x11})
	base := buildSyntheticMP4(t, true, stcoBox([]uint32{0}))
	mdatOff, _ := mdatOffsetAndSize(base)
	input := buildSyntheticMP4(t, true, stcoBox([]uint32{uint32(mdatOff)}), unknown)
	mdatOff2, _ := mdatOffsetAndSize(input)
	input = buildSyntheticMP4(t, true, stcoBox([]uint32{uint32(mdatOff2)}), unknown)

	output := rewrite(t, input, noopHandler)
	ub := findBox(t, output, BoxTypeMoov(), unknownType)
	payload := output[ub.Offset+ub.HeaderSize : ub.Offset+ub.Size]
	assert.Equal(t, []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x00, 0x11}, payload)
}

func TestRewriteFile_LargeSizeHeaderWidthPreserved(t *testing.T) {
	// A box using a 16byte largesize header must keep that header width, even
	// though its size fits in 32 bits; downgrading would corrupt the streaming
	// offsets of the surrounding boxes.
	unknownType := BoxType{'b', 'i', 'g', '1'}
	payload := []byte{1, 2, 3, 4, 5}
	large := make([]byte, 0, LargeHeaderSize+len(payload))
	sizeBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(sizeBuf, 1) // largesize marker
	large = append(large, sizeBuf...)
	large = append(large, unknownType[:]...)
	ext := make([]byte, 8)
	binary.BigEndian.PutUint64(ext, uint64(LargeHeaderSize+len(payload)))
	large = append(large, ext...)
	large = append(large, payload...)

	base := buildSyntheticMP4(t, true, stcoBox([]uint32{0}))
	mdatOff, _ := mdatOffsetAndSize(base)
	input := buildSyntheticMP4(t, true, stcoBox([]uint32{uint32(mdatOff)}), large)
	mdatOff2, _ := mdatOffsetAndSize(input)
	input = buildSyntheticMP4(t, true, stcoBox([]uint32{uint32(mdatOff2)}), large)

	output := rewrite(t, input, noopHandler)
	ub := findBox(t, output, BoxTypeMoov(), unknownType)
	assert.Equal(t, uint64(LargeHeaderSize), ub.HeaderSize)
	assert.Equal(t, uint64(LargeHeaderSize+len(payload)), ub.Size)
	assert.Equal(t, payload, output[ub.Offset+ub.HeaderSize:ub.Offset+ub.Size])
}

func TestRewriteFile_ReplaceNestedMetaFullBox(t *testing.T) {
	// meta is a FullBox container (4 byte version/flags prefix) holding child
	// boxes. Replacing it must keep the rest of the file byte-stable apart from
	// the meta size delta, and the mdat payload hash must be unchanged.
	hdlrType := BoxTypeHdlr()
	hdlrChild := rawBox(hdlrType, append(fullBoxPrefix(0, 0), make([]byte, 12)...))
	metaOld := rawBox(BoxTypeMeta(), append(fullBoxPrefix(0, 0), hdlrChild...))

	base := buildSyntheticMP4(t, true, stcoBox([]uint32{0}))
	mdatOff, _ := mdatOffsetAndSize(base)
	input := buildSyntheticMP4(t, true, stcoBox([]uint32{uint32(mdatOff)}), metaOld)
	mdatOff2, _ := mdatOffsetAndSize(input)
	input = buildSyntheticMP4(t, true, stcoBox([]uint32{uint32(mdatOff2)}), metaOld)

	newChild := rawBox(hdlrType, append(fullBoxPrefix(0, 0), make([]byte, 40)...))
	metaNew := rawBox(BoxTypeMeta(), append(fullBoxPrefix(0, 0), newChild...))

	output := rewrite(t, input, func(h *RewriteHandle) error {
		if h.BoxInfo.Type == BoxTypeMeta() {
			_, err := h.Writer.Write(metaNew)
			return err
		}
		return nil
	})

	require.NotEmpty(t, parseTopLevel(t, output))
	outMeta := findBox(t, output, BoxTypeMoov(), BoxTypeMeta())
	assert.Equal(t, uint64(len(metaNew)), outMeta.Size)
	// FullBox prefix preserved.
	assert.Equal(t, fullBoxPrefix(0, 0),
		output[outMeta.Offset+outMeta.HeaderSize:outMeta.Offset+outMeta.HeaderSize+4])

	inMdat := findBox(t, input, BoxTypeMdat())
	outMdat := findBox(t, output, BoxTypeMdat())
	assert.Equal(t, mdatPayloadHash(input, inMdat), mdatPayloadHash(output, outMdat))
	delta := int64(len(metaNew)) - int64(len(metaOld))
	assert.Equal(t, int64(inMdat.Offset)+delta, int64(outMdat.Offset))
}

func TestRewriteFile_RejectsOffsetOutsideMdat(t *testing.T) {
	// stco points at an offset that is not inside any mdat payload.
	input := buildSyntheticMP4(t, true, stcoBox([]uint32{123456}))
	var out bytes.Buffer
	err := RewriteFile(bytes.NewReader(input), &out, noopHandler)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrUnsafeOffset))
	assert.Equal(t, 0, out.Len(), "no output should be produced on validation failure")
}

func TestRewriteFile_RejectsMalformedContainer(t *testing.T) {
	// A box whose declared size extends past the end of its container/file is
	// a structural error and must be rejected before any output is written.
	ftyp := rawBox(BoxTypeFtyp(), []byte("isom0000isom"))
	badMoov := make([]byte, 8)
	binary.BigEndian.PutUint32(badMoov, 1000)
	moovType := BoxTypeMoov()
	copy(badMoov[4:], moovType[:])
	badMoov = append(badMoov, make([]byte, 10)...) // only 10 payload bytes, not 992
	input := append(ftyp, badMoov...)

	var out bytes.Buffer
	err := RewriteFile(bytes.NewReader(input), &out, noopHandler)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidStructure) || errors.Is(err, ErrUnsafeOffset),
		"expected a structural/unsafe error, got %v", err)
	assert.Equal(t, 0, out.Len())
}

func TestRewriteFile_RejectsNestedSizeZeroBox(t *testing.T) {
	// A size-zero child box (only legal for the final top-level mdat) is an
	// offset structure that cannot be rewritten safely.
	inner := make([]byte, 8)
	zt := BoxType{'z', 'e', 'r', 'o'}
	copy(inner[4:], zt[:])
	moovPayload := inner
	moov := rawBox(BoxTypeMoov(), moovPayload)
	ftyp := rawBox(BoxTypeFtyp(), []byte("isom0000isom"))
	input := append(ftyp, moov...)

	var out bytes.Buffer
	err := RewriteFile(bytes.NewReader(input), &out, noopHandler)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrUnsafeOffset))
	assert.Equal(t, 0, out.Len())
}

func TestRewriteFile_RejectsBadReplacement(t *testing.T) {
	base := buildSyntheticMP4(t, true, stcoBox([]uint32{0}))
	mdatOff, _ := mdatOffsetAndSize(base)
	input := buildSyntheticMP4(t, true, stcoBox([]uint32{uint32(mdatOff)}))

	var out bytes.Buffer
	err := RewriteFile(bytes.NewReader(input), &out, func(h *RewriteHandle) error {
		if h.BoxInfo.Type == BoxTypeMoov() {
			// Wrong type and truncated.
			_, e := h.Writer.Write(rawBox(BoxTypeFree(), []byte("x")))
			return e
		}
		return nil
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrReplacementInvalid))
	assert.Equal(t, 0, out.Len())
}

func TestRewriteFile_RejectsMdatReplacement(t *testing.T) {
	base := buildSyntheticMP4(t, true, stcoBox([]uint32{0}))
	mdatOff, _ := mdatOffsetAndSize(base)
	input := buildSyntheticMP4(t, true, stcoBox([]uint32{uint32(mdatOff)}))

	var out bytes.Buffer
	err := RewriteFile(bytes.NewReader(input), &out, func(h *RewriteHandle) error {
		if h.BoxInfo.Type == BoxTypeMdat() {
			_, e := h.Writer.Write(rawBox(BoxTypeMdat(), []byte("nope")))
			return e
		}
		return nil
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrReplacementInvalid))
}

func TestRewriteFile_BoundedMemoryStreaming(t *testing.T) {
	// A large mdat payload rewritten with a tiny buffer must still produce a
	// byte-identical media payload, proving streaming rather than buffering.
	base := buildSyntheticMP4(t, true, stcoBox([]uint32{0}))
	mdatOff, _ := mdatOffsetAndSize(base)
	input := buildSyntheticMP4(t, true, stcoBox([]uint32{uint32(mdatOff)}))

	var out bytes.Buffer
	err := RewriteFile(bytes.NewReader(input), &out, noopHandler, WithRewriteBufferSize(64))
	require.NoError(t, err)
	assert.Equal(t, input, out.Bytes())
}

// failingWriter accepts `failAfter` bytes, then returns an error on every
// subsequent write. It records whether RewriteFile attempted to truncate it.
type failingWriter struct {
	failAfter   int
	written     int
	truncated   bool
	truncateErr error
}

var errInjectedWrite = errors.New("injected write failure")

func (f *failingWriter) Write(p []byte) (int, error) {
	if f.written >= f.failAfter {
		return 0, errInjectedWrite
	}
	n := len(p)
	if f.written+n > f.failAfter {
		n = f.failAfter - f.written
		f.written = f.failAfter
		return n, errInjectedWrite
	}
	f.written += n
	return n, nil
}

func (f *failingWriter) Truncate(int64) error {
	f.truncated = true
	return f.truncateErr
}

func TestRewriteFile_OutputFailureTruncatesDestination(t *testing.T) {
	// When the destination fails mid-stream, RewriteFile must surface the I/O
	// error and truncate a Truncate-capable destination so no half-written,
	// unusable file remains.
	base := buildSyntheticMP4(t, true, stcoBox([]uint32{0}))
	mdatOff, _ := mdatOffsetAndSize(base)
	input := buildSyntheticMP4(t, true, stcoBox([]uint32{uint32(mdatOff)}))

	fw := &failingWriter{failAfter: 30}
	err := RewriteFile(bytes.NewReader(input), fw, noopHandler, WithRewriteBufferSize(16))
	require.Error(t, err)
	assert.True(t, errors.Is(err, errInjectedWrite))
	assert.True(t, fw.truncated, "expected the failed destination to be truncated")
}

func TestRewriteFile_ShrinkMetadataShiftsOffsetsBack(t *testing.T) {
	base := buildSyntheticMP4(t, true, stcoBox([]uint32{0}))
	mdatOff, _ := mdatOffsetAndSize(base)
	udtaOld := rawBox(BoxTypeUdta(), []byte("a-much-longer-original-value"))
	input := buildSyntheticMP4(t, true, stcoBox([]uint32{uint32(mdatOff)}), udtaOld)
	mdatOff2, _ := mdatOffsetAndSize(input)
	input = buildSyntheticMP4(t, true, stcoBox([]uint32{uint32(mdatOff2)}), udtaOld)

	udtaNew := rawBox(BoxTypeUdta(), []byte("z"))
	output := rewrite(t, input, func(h *RewriteHandle) error {
		if h.BoxInfo.Type == BoxTypeUdta() {
			_, err := h.Writer.Write(udtaNew)
			return err
		}
		return nil
	})

	inMdat := findBox(t, input, BoxTypeMdat())
	outMdat := findBox(t, output, BoxTypeMdat())
	delta := int64(len(udtaNew)) - int64(len(udtaOld)) // negative
	assert.Equal(t, int64(inMdat.Offset)+delta, int64(outMdat.Offset))

	outStco := findBox(t, output, BoxTypeMoov(), BoxTypeTrak(), BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeStco())
	entry := binary.BigEndian.Uint32(output[outStco.Offset+outStco.HeaderSize+8:])
	assert.Equal(t, uint32(outMdat.Offset+outMdat.HeaderSize), entry)
	assert.Equal(t, mdatPayloadHash(input, inMdat), mdatPayloadHash(output, outMdat))
}

func TestRewriteFile_RealSample(t *testing.T) {
	// End-to-end on the shipped sample: no-op must be byte identical and the
	// result must re-traverse.
	for _, name := range []string{"testdata/sample.mp4", "testdata/sample_fragmented.mp4", "testdata/sample_qt.mp4"} {
		t.Run(name, func(t *testing.T) {
			input := readFile(t, name)
			var out bytes.Buffer
			err := RewriteFile(bytes.NewReader(input), &out, noopHandler)
			require.NoError(t, err)
			assert.Equal(t, input, out.Bytes())
			require.NotEmpty(t, parseTopLevel(t, out.Bytes()))
		})
	}
}

func TestRewriteFile_RealSampleReplaceUdta(t *testing.T) {
	// Replace the existing udta on a real file. Because sample.mp4 places the
	// mdat before the moov, the media payload must not move and its hash must
	// be unchanged; only the moov subtree grows.
	input := readFile(t, "testdata/sample.mp4")
	inMdat := findBox(t, input, BoxTypeMdat())
	inHash := mdatPayloadHash(input, inMdat)

	newUdta := rawBox(BoxTypeUdta(), []byte("replaced-user-data-payload-0123456789"))
	var out bytes.Buffer
	err := RewriteFile(bytes.NewReader(input), &out, func(h *RewriteHandle) error {
		if h.BoxInfo.Type == BoxTypeUdta() {
			_, e := h.Writer.Write(newUdta)
			return e
		}
		return nil
	})
	require.NoError(t, err)

	output := out.Bytes()
	require.NotEmpty(t, parseTopLevel(t, output))

	// mdat payload untouched and at the same absolute position.
	outMdat := findBox(t, output, BoxTypeMdat())
	assert.Equal(t, inMdat.Offset, outMdat.Offset)
	assert.Equal(t, inHash, mdatPayloadHash(output, outMdat))

	// moov grew by exactly the udta size delta.
	inMoov := findBox(t, input, BoxTypeMoov())
	outMoov := findBox(t, output, BoxTypeMoov())
	inUdta := findBox(t, input, BoxTypeMoov(), BoxTypeUdta())
	outUdta := findBox(t, output, BoxTypeMoov(), BoxTypeUdta())
	assert.Equal(t, uint64(len(newUdta)), outUdta.Size)
	assert.Equal(t,
		inMoov.Size+(outUdta.Size-inUdta.Size),
		outMoov.Size)

	// Every stco entry still points inside the unchanged mdat.
	stcos, err := ExtractBoxes(bytes.NewReader(output), nil,
		[]BoxPath{{BoxTypeMoov(), BoxTypeTrak(), BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeStco()}})
	require.NoError(t, err)
	require.NotEmpty(t, stcos)
	for _, bi := range stcos {
		count := binary.BigEndian.Uint32(output[bi.Offset+bi.HeaderSize+4:])
		for i := uint32(0); i < count; i++ {
			off := binary.BigEndian.Uint32(output[bi.Offset+bi.HeaderSize+8+uint64(i)*4:])
			assert.GreaterOrEqual(t, uint64(off), outMdat.Offset+outMdat.HeaderSize)
			assert.Less(t, uint64(off), outMdat.Offset+outMdat.Size)
		}
	}
}

func TestRewriteFile_RealFragmentedSampleReTraversable(t *testing.T) {
	// A no-op rewrite of a fragmented file containing moof/mdat pairs and a
	// tfra table must stay byte identical and remain traversable.
	input := readFile(t, "testdata/sample_fragmented.mp4")
	var out bytes.Buffer
	require.NoError(t, RewriteFile(bytes.NewReader(input), &out, noopHandler))
	assert.Equal(t, input, out.Bytes())
	require.NotEmpty(t, parseTopLevel(t, out.Bytes()))
}

// tfraBox builds a version-0 tfra FullBox with one moof-offset entry.
func tfraBox(t *testing.T, moofOffset uint32) []byte {
	t.Helper()
	tfra := &Tfra{
		NumberOfEntry: 1,
		Entries: []TfraEntry{{
			TimeV0:       0,
			MoofOffsetV0: moofOffset,
			TrafNumber:   1,
			TrunNumber:   1,
			SampleNumber: 1,
		}},
	}
	var buf bytes.Buffer
	_, err := Marshal(&buf, tfra, Context{})
	require.NoError(t, err)
	return rawBox(BoxTypeTfra(), buf.Bytes())
}

// buildFragmentedMP4 assembles ftyp + free + (moof+mdat) + mfra(tfra) with the
// tfra entry pointing at the single moof.
func buildFragmentedMP4(t *testing.T, freePayloadLen int) []byte {
	t.Helper()
	ftyp := rawBox(BoxTypeFtyp(), []byte("isom0000isom"))
	free := rawBox(BoxTypeFree(), make([]byte, freePayloadLen))
	var mfhdBuf bytes.Buffer
	_, err := Marshal(&mfhdBuf, &Mfhd{SequenceNumber: 1}, Context{})
	require.NoError(t, err)
	moof := rawBox(BoxTypeMoof(), rawBox(BoxTypeMfhd(), mfhdBuf.Bytes()))
	mdat := rawBox(BoxTypeMdat(), []byte("fragment-media-data"))

	moofOffset := uint32(len(ftyp) + len(free))
	mfra := rawBox(BoxTypeMfra(), tfraBox(t, moofOffset))
	out := append([]byte{}, ftyp...)
	out = append(out, free...)
	out = append(out, moof...)
	out = append(out, mdat...)
	out = append(out, mfra...)
	return out
}

func TestRewriteFile_FragmentedTfraOffsetsShifted(t *testing.T) {
	// Grow the top-level free box in a fragmented file: the following moof/mdat
	// move, so the absolute moof offset stored in the tfra box must be shifted
	// by the same amount and remain internally consistent.
	input := buildFragmentedMP4(t, 4)

	inMoofs, err := ExtractBoxes(bytes.NewReader(input), nil, []BoxPath{{BoxTypeMoof()}})
	require.NoError(t, err)
	require.Len(t, inMoofs, 1)
	inTfras, err := ExtractBoxesWithPayload(bytes.NewReader(input), nil,
		[]BoxPath{{BoxTypeMfra(), BoxTypeTfra()}})
	require.NoError(t, err)
	require.Len(t, inTfras, 1)

	const grow = 37
	var out bytes.Buffer
	require.NoError(t, RewriteFile(bytes.NewReader(input), &out, func(h *RewriteHandle) error {
		if h.BoxInfo.Type == BoxTypeFree() {
			orig := h.BoxInfo.Size - h.BoxInfo.HeaderSize
			_, e := h.Writer.Write(rawBox(BoxTypeFree(), make([]byte, orig+grow)))
			return e
		}
		return nil
	}))

	output := out.Bytes()
	require.NotEmpty(t, parseTopLevel(t, output))

	// The moof moved forward by exactly `grow`.
	outMoofs, err := ExtractBoxes(bytes.NewReader(output), nil, []BoxPath{{BoxTypeMoof()}})
	require.NoError(t, err)
	require.Len(t, outMoofs, 1)
	assert.Equal(t, inMoofs[0].Offset+grow, outMoofs[0].Offset)

	// The tfra entry now points at the shifted moof position.
	outTfras, err := ExtractBoxesWithPayload(bytes.NewReader(output), nil,
		[]BoxPath{{BoxTypeMfra(), BoxTypeTfra()}})
	require.NoError(t, err)
	require.Len(t, outTfras, 1)
	inT := inTfras[0].Payload.(*Tfra)
	outT := outTfras[0].Payload.(*Tfra)
	require.Len(t, outT.Entries, 1)
	assert.Equal(t, inT.Entries[0].MoofOffsetV0+uint32(grow), outT.Entries[0].MoofOffsetV0)
	assert.Equal(t, uint32(outMoofs[0].Offset), outT.Entries[0].MoofOffsetV0)
}

func TestRewriteFile_RejectsAbsoluteSaioOutsideMoof(t *testing.T) {
	// An saio carrying absolute offsets directly under moov cannot be kept
	// consistent after a metadata edit and must be rejected explicitly.
	saio := &Saio{EntryCount: 1, OffsetV0: []uint32{4242}}
	var sb bytes.Buffer
	_, err := Marshal(&sb, saio, Context{})
	require.NoError(t, err)
	moov := rawBox(BoxTypeMoov(), rawBox(BoxTypeSaio(), sb.Bytes()))
	ftyp := rawBox(BoxTypeFtyp(), []byte("isom0000isom"))
	input := append(ftyp, moov...)

	var out bytes.Buffer
	err = RewriteFile(bytes.NewReader(input), &out, noopHandler)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrUnsafeOffset))
	assert.Equal(t, 0, out.Len())
}

func TestRewriteFile_AcceptsSaioInsideMoof(t *testing.T) {
	// An saio inside moof uses moof-relative offsets; since each moof subtree
	// keeps its internal layout, the no-op rewrite is byte identical.
	saio := &Saio{EntryCount: 1, OffsetV0: []uint32{16}}
	var sb bytes.Buffer
	_, err := Marshal(&sb, saio, Context{})
	require.NoError(t, err)
	traf := rawBox(BoxTypeTraf(), rawBox(BoxTypeSaio(), sb.Bytes()))
	moof := rawBox(BoxTypeMoof(), traf)
	mdat := rawBox(BoxTypeMdat(), []byte("media"))
	ftyp := rawBox(BoxTypeFtyp(), []byte("isom0000isom"))
	input := append(append(ftyp, moof...), mdat...)

	var out bytes.Buffer
	require.NoError(t, RewriteFile(bytes.NewReader(input), &out, noopHandler))
	assert.Equal(t, input, out.Bytes())
}

func readFile(t *testing.T, name string) []byte {
	t.Helper()
	r, err := os.Open(name)
	require.NoError(t, err)
	defer r.Close()
	b, err := io.ReadAll(r)
	require.NoError(t, err)
	return b
}

// tfhdBox builds a tfhd FullBox carrying an explicit BaseDataOffset that points
// at the given absolute box offset.
func tfhdBox(t *testing.T, baseDataOffset uint64) []byte {
	t.Helper()
	tfhd := &Tfhd{
		TrackID:        1,
		BaseDataOffset: baseDataOffset,
	}
	tfhd.SetFlags(TfhdBaseDataOffsetPresent)
	var buf bytes.Buffer
	_, err := Marshal(&buf, tfhd, Context{})
	require.NoError(t, err)
	return rawBox(BoxTypeTfhd(), buf.Bytes())
}

func TestRewriteFile_TfhdBaseDataOffsetShifted(t *testing.T) {
	// A tfhd with BaseDataOffset pointing at the following mdat box. Growing a
	// preceding free box moves the mdat, so the 64bit base offset must be
	// shifted by the same amount.
	ftyp := rawBox(BoxTypeFtyp(), []byte("isom0000isom"))
	free := rawBox(BoxTypeFree(), []byte("abcd"))

	// Layout up to the moof header is needed before the offset is known, so
	// build the rest first with a placeholder and then patch.
	placeholder := tfhdBox(t, 0)
	moofInner := rawBox(BoxTypeTraf(), placeholder)
	moof := rawBox(BoxTypeMoof(), moofInner)
	mdat := rawBox(BoxTypeMdat(), []byte("tfhd-media"))

	mdatBoxOffset := uint64(len(ftyp) + len(free) + len(moof))
	tfhd := tfhdBox(t, mdatBoxOffset)
	moofInner = rawBox(BoxTypeTraf(), tfhd)
	moof = rawBox(BoxTypeMoof(), moofInner)

	input := append([]byte{}, ftyp...)
	input = append(input, free...)
	input = append(input, moof...)
	input = append(input, mdat...)

	const grow = 21
	var out bytes.Buffer
	require.NoError(t, RewriteFile(bytes.NewReader(input), &out, func(h *RewriteHandle) error {
		if h.BoxInfo.Type == BoxTypeFree() {
			orig := h.BoxInfo.Size - h.BoxInfo.HeaderSize
			_, e := h.Writer.Write(rawBox(BoxTypeFree(), make([]byte, orig+grow)))
			return e
		}
		return nil
	}))

	output := out.Bytes()
	require.NotEmpty(t, parseTopLevel(t, output))

	tfhdInfos, err := ExtractBoxesWithPayload(bytes.NewReader(output), nil,
		[]BoxPath{{BoxTypeMoof(), BoxTypeTraf(), BoxTypeTfhd()}})
	require.NoError(t, err)
	require.Len(t, tfhdInfos, 1)
	got := tfhdInfos[0].Payload.(*Tfhd)
	assert.True(t, got.CheckFlag(TfhdBaseDataOffsetPresent))

	outMdat := findBox(t, output, BoxTypeMdat())
	assert.Equal(t, outMdat.Offset, got.BaseDataOffset)
	assert.Equal(t, mdatBoxOffset+grow, got.BaseDataOffset)
}
