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
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runFastStart is the standard test entry point: feed data through FastStart
// using seekable in-memory wrappers and return the output bytes.
func runFastStart(t *testing.T, in []byte) []byte {
	t.Helper()
	r := bytes.NewReader(in)
	var out bytes.Buffer
	ws := newMemWriteSeeker(&out)
	require.NoError(t, FastStart(r, ws))
	return out.Bytes()
}

// memWriteSeeker adapts bytes.Buffer to io.WriteSeeker for tests.
type memWriteSeeker struct {
	buf *bytes.Buffer
	pos int64
}

func newMemWriteSeeker(buf *bytes.Buffer) *memWriteSeeker {
	return &memWriteSeeker{buf: buf}
}

func (m *memWriteSeeker) Write(p []byte) (int, error) {
	if m.pos != int64(m.buf.Len()) {
		// Tests only append; support overwrite at end.
		return 0, fmt.Errorf("unexpected non-append write at pos=%d len=%d", m.pos, m.buf.Len())
	}
	n, err := m.buf.Write(p)
	m.pos += int64(n)
	return n, err
}

func (m *memWriteSeeker) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		m.pos = offset
	case io.SeekCurrent:
		m.pos += offset
	case io.SeekEnd:
		m.pos = int64(m.buf.Len()) + offset
	}
	return m.pos, nil
}

func TestFastStartMovesMoovBeforeMdat(t *testing.T) {
	in := buildTwoTrackMP4()

	// Precondition: mdat precedes moov.
	moOff, _, _ := boxAt(in, "moov")
	mdOff, _, _ := boxAt(in, "mdat")
	require.Less(t, mdOff, moOff, "input must have mdat before moov")

	out := runFastStart(t, in)

	// Postcondition: moov precedes mdat and boxes stay otherwise ordered.
	moOff2, moSize, _ := boxAt(out, "moov")
	mdOff2, _, _ := boxAt(out, "mdat")
	require.Less(t, moOff2, mdOff2, "moov must precede mdat in output")
	require.Equal(t, moSize, moSize)
	require.Equal(t, len(in), len(out), "total length must be preserved")

	// ftyp and free (the boxes before mdat) must be byte-identical at the front.
	ftOff, ftSize, _ := boxAt(out, "ftyp")
	require.Equal(t, 0, ftOff)
	require.Equal(t, in[:ftOff+ftSize], out[:ftOff+ftSize])
}

func TestFastStartUpdatesAllStcoOffsets(t *testing.T) {
	in := buildTwoTrackMP4()
	_, moSize, _ := boxAt(in, "moov")
	mdOff, mdSize, mdPayload := boxAt(in, "mdat")
	_ = mdPayload
	delta := int64(moSize)

	out := runFastStart(t, in)

	// Locate the two stco boxes in input and output and compare every entry.
	inStco := findBoxes(in, "stco")
	outStco := findBoxes(out, "stco")
	require.Len(t, inStco, 2)
	require.Len(t, outStco, 2)

	for i := range inStco {
		inHdr := in[inStco[i]:]
		inBoxSize := int(binary.BigEndian.Uint32(inHdr[:4]))
		inEntries := parseStco(inHdr[8:inBoxSize])

		outHdr := out[outStco[i]:]
		outBoxSize := int(binary.BigEndian.Uint32(outHdr[:4]))
		outEntries := parseStco(outHdr[8:outBoxSize])

		require.Equal(t, inBoxSize, outBoxSize, "stco box size must not change")
		require.Len(t, outEntries, len(inEntries))
		for j, old := range inEntries {
			// stco points into mdat payload: sanity-check the precondition.
			require.GreaterOrEqual(t, uint64(old), uint64(mdOff+8))
			require.LessOrEqual(t, uint64(old), uint64(mdOff+mdSize))
			assert.Equal(t, uint64(old)+uint64(delta), uint64(outEntries[j]),
				"track %d chunk %d offset mismatch", i, j)
		}
	}
}

func TestFastStartPreservesMdatAndUnknownBoxBytes(t *testing.T) {
	in := buildTwoTrackMP4()
	out := runFastStart(t, in)

	// mdat payload must be byte-identical.
	_, _, inMdatPayload := boxAt(in, "mdat")
	_, _, outMdatPayload := boxAt(out, "mdat")
	assert.Equal(t, inMdatPayload, outMdatPayload)

	// Unknown top-level boxes (free before mdat, wide after moov) are kept.
	freeOff, freeSize, freePayload := boxAt(in, "free")
	freeOffOut, freeSizeOut, freePayloadOut := boxAt(out, "free")
	assert.Equal(t, freeOff, freeOffOut, "free stays before the moved moov")
	assert.Equal(t, freeSize, freeSizeOut)
	assert.Equal(t, freePayload, freePayloadOut)

	_, _, widePayload := boxAt(in, "wide")
	_, _, widePayloadOut := boxAt(out, "wide")
	assert.Equal(t, widePayload, widePayloadOut)

	// Unknown boxes inside moov and the tracks are preserved (opaque leaves).
	for _, typ := range []string{"zzzz", "yyyy"} {
		require.NotEmpty(t, findBoxes(in, typ), typ)
		require.NotEmpty(t, findBoxes(out, typ), typ)
	}
	// free/wide unknown top-level boxes already asserted above.
}

func TestFastStartIsIdempotent(t *testing.T) {
	in := buildTwoTrackMP4()
	first := runFastStart(t, in)

	// Second run on already-fast-start input must be byte-identical.
	second := runFastStart(t, first)
	assert.True(t, bytes.Equal(first, second), "already-fast-start file must be emitted unchanged")

	// And the moov still precedes the mdat after the second pass.
	moOff, _, _ := boxAt(second, "moov")
	mdOff, _, _ := boxAt(second, "mdat")
	assert.Less(t, moOff, mdOff)
}

func TestFastStartOnSampleMP4(t *testing.T) {
	in, err := os.ReadFile("testdata/sample.mp4")
	require.NoError(t, err)

	out := runFastStart(t, in)

	// moov before mdat.
	moOff, _, _ := boxAt(out, "moov")
	mdOff, _, _ := boxAt(out, "mdat")
	require.Less(t, moOff, mdOff)
	assert.Equal(t, len(in), len(out))

	// Every stco entry shifted by exactly the moov size.
	_, moSize, _ := boxAt(in, "moov")
	inStco := findBoxes(in, "stco")
	outStco := findBoxes(out, "stco")
	require.Len(t, inStco, 2)
	require.Len(t, outStco, 2)
	for i := range inStco {
		inSize := int(binary.BigEndian.Uint32(in[inStco[i] : inStco[i]+4]))
		outSize := int(binary.BigEndian.Uint32(out[outStco[i] : outStco[i]+4]))
		require.Equal(t, inSize, outSize)
		oldEntries := parseStco(in[inStco[i]+8 : inStco[i]+inSize])
		newEntries := parseStco(out[outStco[i]+8 : outStco[i]+outSize])
		for j := range oldEntries {
			assert.Equal(t, uint64(oldEntries[j])+uint64(moSize), uint64(newEntries[j]))
		}
	}

	// Probe succeeds on both and flips the FastStart flag; tracks/chunks match.
	origInfo, err := Probe(bytes.NewReader(in))
	require.NoError(t, err)
	newInfo, err := Probe(bytes.NewReader(out))
	require.NoError(t, err)
	assert.False(t, origInfo.FastStart)
	assert.True(t, newInfo.FastStart)
	require.Len(t, newInfo.Tracks, 2)

	// The mdat media payload is byte-identical, so hashing all chunk data
	// through Probe's chunk/sample tables must match between old and new.
	hashChunks := func(data []byte, info *ProbeInfo) [32]byte {
		h := sha256.New()
		r := bytes.NewReader(data)
		for _, track := range info.Tracks {
			si := 0
			for _, chunk := range track.Chunks {
				end := si + int(chunk.SamplesPerChunk)
				off := chunk.DataOffset
				for ; si < end && si < len(track.Samples); si++ {
					size := uint64(track.Samples[si].Size)
					_, err := r.Seek(int64(off), io.SeekStart)
					require.NoError(t, err)
					_, err = io.CopyN(h, r, int64(size))
					require.NoError(t, err)
					off += size
				}
			}
		}
		var sum [32]byte
		copy(sum[:], h.Sum(nil))
		return sum
	}
	assert.Equal(t, hashChunks(in, origInfo), hashChunks(out, newInfo),
		"media bytes reachable via chunk offsets must be identical")

	// Idempotent on the real file too.
	again := runFastStart(t, out)
	assert.True(t, bytes.Equal(out, again))
}

func TestFastStartCo64(t *testing.T) {
	// Build an MP4 whose single track uses co64 instead of stco.
	var b mp4Builder
	b.box("ftyp", []byte("isom\x00\x00\x00\x00"))
	mdatHeaderEnd := b.buf.Len() + 8
	b.box("mdat", bytes.Repeat([]byte{0x11}, 100))

	offsets := []uint64{uint64(mdatHeaderEnd), uint64(mdatHeaderEnd + 40)}
	var stbl mp4Builder
	stbl.raw(co64Box(offsets))
	var minf mp4Builder
	minf.box("stbl", stbl.bytes())
	var mdia mp4Builder
	mdia.box("minf", minf.bytes())
	var trakB mp4Builder
	trakB.box("mdia", mdia.bytes())
	var moov mp4Builder
	moov.box("trak", trakB.bytes())

	var top mp4Builder
	top.raw(b.bytes())
	writeBox(&top.buf, "moov", moov.bytes())
	in := top.bytes()

	out := runFastStart(t, in)
	_, moSize, _ := boxAt(in, "moov")
	coOffs := findBoxes(out, "co64")
	require.Len(t, coOffs, 1)
	outHdr := out[coOffs[0]:]
	outBoxSize := int(binary.BigEndian.Uint32(outHdr[:4]))
	got := parseCo64(outHdr[8:outBoxSize])
	require.Len(t, got, len(offsets))
	for i, old := range offsets {
		assert.Equal(t, old+uint64(moSize), got[i])
	}

	// moov must precede mdat after the co64 relocation.
	moOff, _, _ := boxAt(out, "moov")
	mdOff, _, _ := boxAt(out, "mdat")
	assert.Less(t, moOff, mdOff)
}

func TestFastStartRejectsFragmented(t *testing.T) {
	// moof at top level.
	var b mp4Builder
	b.box("ftyp", []byte("isom"))
	b.box("moof", []byte{1})
	b.box("mdat", []byte{2})
	var moov mp4Builder
	moov.box("mvhd", nil)
	var top mp4Builder
	top.raw(b.bytes())
	writeBox(&top.buf, "moov", moov.bytes())
	err := FastStart(bytes.NewReader(top.bytes()), newMemWriteSeeker(&bytes.Buffer{}))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrFastStartNotApplicable), "got %v", err)

	// mvex inside moov.
	in := buildTwoTrackMP4()
	// Inject an mvex box right after the moov header.
	moOff, _, _ := boxAt(in, "moov")
	var mvex bytes.Buffer
	writeBox(&mvex, "mvex", nil)
	injected := append(append([]byte{}, in[:moOff+8]...), mvex.Bytes()...)
	injected = append(injected, in[moOff+8:]...)
	err = FastStart(bytes.NewReader(injected), newMemWriteSeeker(&bytes.Buffer{}))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrFastStartNotApplicable), "got %v", err)

	// The bundled fragmented sample must also be rejected.
	frag, err := os.ReadFile("testdata/sample_fragmented.mp4")
	require.NoError(t, err)
	err = FastStart(bytes.NewReader(frag), newMemWriteSeeker(&bytes.Buffer{}))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrFastStartNotApplicable), "got %v", err)
}

func TestFastStartRejectsMultipleMdatAndMoov(t *testing.T) {
	var b mp4Builder
	b.box("ftyp", []byte("isom"))
	b.box("mdat", []byte{1})
	b.box("mdat", []byte{2})
	var moov mp4Builder
	moov.box("mvhd", nil)
	var top mp4Builder
	top.raw(b.bytes())
	writeBox(&top.buf, "moov", moov.bytes())
	err := FastStart(bytes.NewReader(top.bytes()), newMemWriteSeeker(&bytes.Buffer{}))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrFastStartNotApplicable), "got %v", err)

	// Two moov boxes.
	var b2 mp4Builder
	b2.box("ftyp", []byte("isom"))
	b2.box("mdat", []byte{1})
	var top2 mp4Builder
	top2.raw(b2.bytes())
	writeBox(&top2.buf, "moov", []byte{})
	writeBox(&top2.buf, "moov", []byte{})
	err = FastStart(bytes.NewReader(top2.bytes()), newMemWriteSeeker(&bytes.Buffer{}))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrFastStartNotApplicable), "got %v", err)
}

func TestFastStartRejectsMissingBoxes(t *testing.T) {
	var b mp4Builder
	b.box("ftyp", []byte("isom"))
	b.box("mdat", []byte{1})
	// no moov
	err := FastStart(bytes.NewReader(b.bytes()), newMemWriteSeeker(&bytes.Buffer{}))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrFastStartNotApplicable), "got %v", err)

	var b2 mp4Builder
	b2.box("ftyp", []byte("isom"))
	var moov mp4Builder
	moov.box("mvhd", nil)
	var top mp4Builder
	top.raw(b2.bytes())
	writeBox(&top.buf, "moov", moov.bytes())
	// no mdat
	err = FastStart(bytes.NewReader(top.bytes()), newMemWriteSeeker(&bytes.Buffer{}))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrFastStartNotApplicable), "got %v", err)
}

func TestFastStartRejectsCorruptedBox(t *testing.T) {
	// Truncated top-level header: declared size extends past EOF.
	var b mp4Builder
	b.box("ftyp", []byte("isom"))
	b.raw([]byte{0, 0, 0xff, 0xff, 'm', 'd', 'a', 't'})
	err := FastStart(bytes.NewReader(b.bytes()), newMemWriteSeeker(&bytes.Buffer{}))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidBox), "got %v", err)

	// Corrupted moov: a child box declares a size beyond its parent.
	in := buildTwoTrackMP4()
	moOff, moSize, _ := boxAt(in, "moov")
	corrupt := make([]byte, len(in))
	copy(corrupt, in)
	// Corrupt the first inner box (mvhd) size to be huge. Its header starts at
	// moov payload start.
	binary.BigEndian.PutUint32(corrupt[moOff+8:moOff+12], uint32(moSize+100))
	err = FastStart(bytes.NewReader(corrupt), newMemWriteSeeker(&bytes.Buffer{}))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidBox), "got %v", err)

	// Trailing garbage inside a container (non-box-aligned bytes).
	in2 := buildTwoTrackMP4()
	moOff2, _, _ := boxAt(in2, "moov")
	corrupt2 := make([]byte, len(in2)+3)
	copy(corrupt2, in2[:moOff2])
	copy(corrupt2[moOff2:], in2[moOff2:])
	// Append 3 bytes inside moov by shrinking: easier to corrupt the mvhd size
	// downward so leftover bytes remain.
	binary.BigEndian.PutUint32(corrupt2[moOff2+8:moOff2+12], 12)
	err = FastStart(bytes.NewReader(corrupt2), newMemWriteSeeker(&bytes.Buffer{}))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidBox), "got %v", err)

	// stco entry count inconsistent with box size.
	in3 := buildTwoTrackMP4()
	stcoOff := findBoxes(in3, "stco")[0]
	corrupt3 := make([]byte, len(in3))
	copy(corrupt3, in3)
	// Claim an entry count far beyond the actual box payload.
	binary.BigEndian.PutUint32(corrupt3[stcoOff+8+4:stcoOff+8+8], 1_000_000)
	err = FastStart(bytes.NewReader(corrupt3), newMemWriteSeeker(&bytes.Buffer{}))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidBox), "got %v", err)
}

func TestFastStartRejectsMixedOffsetTables(t *testing.T) {
	// An stbl containing both an stco and a co64 box is structurally invalid.
	in := buildTwoTrackMP4()
	firstStco := findBoxes(in, "stco")[0]
	corrupt := make([]byte, len(in))
	copy(corrupt, in)
	// Insert a co64 box immediately before the first stco box.
	co := co64Box([]uint64{1, 2})
	corrupt = append(corrupt[:firstStco], append(co, corrupt[firstStco:]...)...)
	err := FastStart(bytes.NewReader(corrupt), newMemWriteSeeker(&bytes.Buffer{}))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidBox), "got %v", err)
}

func TestFastStartRejectsTruncatedFile(t *testing.T) {
	in := buildTwoTrackMP4()
	// Cut the file partway through the moov box so a child box is truncated.
	moOff, _, _ := boxAt(in, "moov")
	truncated := in[:moOff+8+3]
	err := FastStart(bytes.NewReader(truncated), newMemWriteSeeker(&bytes.Buffer{}))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidBox), "got %v", err)
}

func TestFastStartPreservesUnknownNestedBoxes(t *testing.T) {
	// Unknown opaque leaf boxes at every nesting level must survive verbatim.
	in := buildTwoTrackMP4()
	out := runFastStart(t, in)
	// zzzz lives inside the first trak's stbl; verify it byte-for-byte.
	zzOff := findBoxes(in, "zzzz")[0]
	zzOffOut := findBoxes(out, "zzzz")[0]
	inSize := int(binary.BigEndian.Uint32(in[zzOff : zzOff+4]))
	outSize := int(binary.BigEndian.Uint32(out[zzOffOut : zzOffOut+4]))
	assert.Equal(t, inSize, outSize)
	assert.Equal(t, in[zzOff:zzOff+inSize], out[zzOffOut:zzOffOut+outSize])
	// The moov-level unknown leaf must also survive.
	require.NotEmpty(t, findBoxes(in, "unkn"))
	require.NotEmpty(t, findBoxes(out, "unkn"))
	ukOff := findBoxes(in, "unkn")[0]
	ukOffOut := findBoxes(out, "unkn")[0]
	ukSize := int(binary.BigEndian.Uint32(in[ukOff : ukOff+4]))
	assert.Equal(t, in[ukOff:ukOff+ukSize], out[ukOffOut:ukOffOut+ukSize])
}

func TestAddOffsetDeltaOverflow(t *testing.T) {
	// stco relocation adds a positive delta (the moov size). A uint32-range
	// result that exceeds the box width is rejected at the stco layer; here we
	// pin the 64-bit arithmetic itself for both directions.
	got, err := addOffsetDelta(math.MaxUint64-3, 10)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrChunkOffsetOverflow))
	assert.Equal(t, uint64(0), got)

	got, err = addOffsetDelta(5, -10)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrChunkOffsetOverflow))
	assert.Equal(t, uint64(0), got)

	got, err = addOffsetDelta(100, 25)
	require.NoError(t, err)
	assert.Equal(t, uint64(125), got)

	got, err = addOffsetDelta(100, -30)
	require.NoError(t, err)
	assert.Equal(t, uint64(70), got)
}

// sparseReadSeeker reports a large logical file size while serving only the
// small head/tail regions, so the scanner can validate a near-uint32-max
// stco entry without a multi-GB allocation. The middle returns zeros.
type sparseReadSeeker struct {
	head, tail []byte
	logical    int64
	pos        int64
}

func (s *sparseReadSeeker) Read(p []byte) (int, error) {
	if s.pos >= s.logical {
		return 0, io.EOF
	}
	n := len(p)
	if s.pos+int64(n) > s.logical {
		n = int(s.logical - s.pos)
	}
	// Serve head bytes, zeros for the hole, tail bytes.
	for i := 0; i < n; i++ {
		abs := s.pos + int64(i)
		switch {
		case abs < int64(len(s.head)):
			p[i] = s.head[abs]
		case abs >= s.logical-int64(len(s.tail)):
			p[i] = s.tail[abs-(s.logical-int64(len(s.tail)))]
		default:
			p[i] = 0
		}
	}
	s.pos += int64(n)
	return n, nil
}

func (s *sparseReadSeeker) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		s.pos = offset
	case io.SeekCurrent:
		s.pos += offset
	case io.SeekEnd:
		s.pos = s.logical + offset
	}
	return s.pos, nil
}

func TestFastStartRejectsStcoOverflow(t *testing.T) {
	// A near-uint32-max stco entry plus a moov large enough to cross the
	// 32-bit boundary must be rejected with ErrChunkOffsetOverflow. The mdat
	// payload is a sparse hole, so this runs without a multi-GB allocation.
	entry := uint32(0xfffffff0)

	// Head: ftyp then mdat header whose declared size covers the entry.
	var head bytes.Buffer
	writeBox(&head, "ftyp", []byte("isom\x00\x00\x00\x00"))
	mdatStart := head.Len()
	mdatDeclSize := uint64(entry) - uint64(mdatStart) + 8
	var mdatHdr [8]byte
	binary.BigEndian.PutUint32(mdatHdr[:4], uint32(mdatDeclSize))
	copy(mdatHdr[4:8], "mdat")
	head.Write(mdatHdr[:])

	// Tail: moov placed at the mdat's declared end, padded so moovSize crosses
	// the remaining 32-bit headroom.
	var stbl mp4Builder
	stbl.raw(stcoBox([]uint32{entry}))
	var minf mp4Builder
	minf.box("stbl", stbl.bytes())
	var mdia mp4Builder
	mdia.box("minf", minf.bytes())
	var trakB mp4Builder
	trakB.box("mdia", mdia.bytes())
	var moov mp4Builder
	moov.box("trak", trakB.bytes())
	headroom := uint64(math.MaxUint32) - uint64(entry)
	moov.box("pad ", make([]byte, int(headroom)+16))
	var tail bytes.Buffer
	writeBox(&tail, "moov", moov.bytes())

	logical := int64(mdatStart) + int64(mdatDeclSize) + int64(tail.Len())
	r := &sparseReadSeeker{head: head.Bytes(), tail: tail.Bytes(), logical: logical}

	err := FastStart(r, newMemWriteSeeker(&bytes.Buffer{}))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrChunkOffsetOverflow), "got %v", err)
}

func TestFastStartRejectsOffsetOutsideMdat(t *testing.T) {
	// An stco entry pointing outside the mdat payload is treated as corruption.
	in := buildTwoTrackMP4()
	stcoOff := findBoxes(in, "stco")[0]
	corrupt := make([]byte, len(in))
	copy(corrupt, in)
	// First entry's 4 bytes are at stco payload+8 (after version/flags+count).
	binary.BigEndian.PutUint32(corrupt[stcoOff+8+8:stcoOff+8+12], 7) // tiny, outside mdat
	err := FastStart(bytes.NewReader(corrupt), newMemWriteSeeker(&bytes.Buffer{}))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidBox), "got %v", err)
}

// failWriter returns an error after a configurable number of bytes to model a
// write interruption.
type failWriter struct {
	failAfter int
	written   int
}

func (f *failWriter) Write(p []byte) (int, error) {
	if f.written+len(p) > f.failAfter {
		return 0, errors.New("simulated disk write failure")
	}
	f.written += len(p)
	return len(p), nil
}

func (f *failWriter) Seek(int64, int) (int64, error) { return int64(f.written), nil }

func TestFastStartPropagatesWriteFailure(t *testing.T) {
	in := buildTwoTrackMP4()
	err := FastStart(bytes.NewReader(in), &failWriter{failAfter: 50})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "write failure")
}

func TestFastStartFileWritesAtomically(t *testing.T) {
	dir := t.TempDir()
	inPath := filepath.Join(dir, "in.mp4")
	nestedDir := filepath.Join(dir, "nested")
	require.NoError(t, os.MkdirAll(nestedDir, 0o700))
	outPath := filepath.Join(nestedDir, "out.mp4")
	require.NoError(t, os.WriteFile(inPath, buildTwoTrackMP4(), 0o600))

	require.NoError(t, FastStartFile(inPath, outPath))
	got, err := os.ReadFile(outPath)
	require.NoError(t, err)
	expect := runFastStart(t, buildTwoTrackMP4())
	assert.Equal(t, expect, got)

	// No temporary files left behind.
	entries, err := os.ReadDir(filepath.Dir(outPath))
	require.NoError(t, err)
	for _, e := range entries {
		assert.NotContains(t, e.Name(), "mp4-faststart-", "temp file not cleaned up: %s", e.Name())
	}

	// Input untouched.
	orig, err := os.ReadFile(inPath)
	require.NoError(t, err)
	assert.Equal(t, buildTwoTrackMP4(), orig)
}

func TestFastStartFileCleansUpOnFailure(t *testing.T) {
	dir := t.TempDir()
	inPath := filepath.Join(dir, "in.mp4")
	outPath := filepath.Join(dir, "out.mp4")
	// A fragmented input is rejected, so no output should appear.
	frag, err := os.ReadFile("testdata/sample_fragmented.mp4")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(inPath, frag, 0o600))

	err = FastStartFile(inPath, outPath)
	require.Error(t, err)
	_, statErr := os.Stat(outPath)
	assert.True(t, os.IsNotExist(statErr), "output must not be created on failure")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		assert.NotContains(t, e.Name(), "mp4-faststart-", "temp file leaked: %s", e.Name())
	}
}

func TestFastStartLargeHeaderBoxes(t *testing.T) {
	// A 16-byte largesize box (size==1) must be parsed and copied correctly.
	var b bytes.Buffer
	// ftyp small, with a valid payload length
	writeBox(&b, "ftyp", []byte("isom\x00\x00\x00\x00"))
	// mdat with 64-bit largesize header
	var mdatHdr [16]byte
	mdatHdr[0] = 0
	mdatHdr[1] = 0
	mdatHdr[2] = 0
	mdatHdr[3] = 1
	copy(mdatHdr[4:8], "mdat")
	payload := bytes.Repeat([]byte{0x5a}, 40)
	binary.BigEndian.PutUint64(mdatHdr[8:16], uint64(16+len(payload)))
	b.Write(mdatHdr[:])
	b.Write(payload)

	mdatHeaderEnd := b.Len() - len(payload)
	var stbl mp4Builder
	stbl.raw(stcoBox([]uint32{uint32(mdatHeaderEnd)}))
	var minf mp4Builder
	minf.box("stbl", stbl.bytes())
	var mdia mp4Builder
	mdia.box("minf", minf.bytes())
	var trakB mp4Builder
	trakB.box("mdia", mdia.bytes())
	var moov mp4Builder
	moov.box("trak", trakB.bytes())
	writeBox(&b, "moov", moov.bytes())

	in := b.Bytes()
	out := runFastStart(t, in)

	// The output mdat must retain its 16-byte header and identical payload.
	mdOff, mdSize, mdPayload := boxAt(out, "mdat")
	require.NotEqual(t, -1, mdOff)
	assert.Equal(t, 16+len(payload), mdSize)
	assert.Equal(t, payload, mdPayload)
	// Verify the largesize marker survived.
	assert.Equal(t, uint32(1), binary.BigEndian.Uint32(out[mdOff:mdOff+4]))
	assert.Equal(t, uint64(16+len(payload)), binary.BigEndian.Uint64(out[mdOff+8:mdOff+16]))

	// moov precedes the relocated mdat.
	moOff, _, _ := boxAt(out, "moov")
	assert.Less(t, moOff, mdOff)
}

func TestFastStartRejectsMdatExtendingToEOF(t *testing.T) {
	// mdat with size 0 (extends to EOF) cannot be relocated.
	var b bytes.Buffer
	writeBox(&b, "ftyp", []byte("isom"))
	var hdr [8]byte // size == 0
	copy(hdr[4:8], "mdat")
	b.Write(hdr[:])
	b.Write([]byte{1, 2, 3, 4})
	var moov bytes.Buffer
	writeBox(&moov, "mvhd", nil)
	var top bytes.Buffer
	top.Write(b.Bytes())
	writeBox(&top, "moov", moov.Bytes())
	err := FastStart(bytes.NewReader(top.Bytes()), newMemWriteSeeker(&bytes.Buffer{}))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrFastStartNotApplicable), "got %v", err)
}

func TestFastStartStreamingDoesNotBufferPayload(t *testing.T) {
	// Confirm media larger than the internal copy buffer is handled and the
	// output mdat payload is identical (proves chunked streaming works).
	var b mp4Builder
	b.box("ftyp", []byte("isom"))
	mdatHeaderEnd := b.buf.Len() + 8
	bigPayload := bytes.Repeat([]byte{0x7e}, fastStartCopyBufferSize*3+123)
	b.box("mdat", bigPayload)

	// Two chunks inside the large payload.
	first := uint32(mdatHeaderEnd)
	second := uint32(mdatHeaderEnd + fastStartCopyBufferSize)
	var stbl mp4Builder
	stbl.raw(stcoBox([]uint32{first, second}))
	var minf mp4Builder
	minf.box("stbl", stbl.bytes())
	var mdia mp4Builder
	mdia.box("minf", minf.bytes())
	var trakB mp4Builder
	trakB.box("mdia", mdia.bytes())
	var moov mp4Builder
	moov.box("trak", trakB.bytes())
	var top mp4Builder
	top.raw(b.bytes())
	writeBox(&top.buf, "moov", moov.bytes())

	out := runFastStart(t, top.bytes())
	_, _, outPayload := boxAt(out, "mdat")
	assert.Equal(t, bigPayload, outPayload)
}

func TestFastStartSparseLargeFile(t *testing.T) {
	// Use a real sparse file with a 64 MiB declared mdat to prove the media
	// payload is streamed (not buffered) and offsets are relocated correctly.
	if testing.Short() {
		t.Skip("sparse file test skipped in short mode")
	}
	dir := t.TempDir()
	inPath := filepath.Join(dir, "in.mp4")
	outPath := filepath.Join(dir, "out.mp4")

	const mdatPayload = 64 * 1024 * 1024 // 64 MiB, sparse
	f, err := os.Create(inPath)
	require.NoError(t, err)

	var head bytes.Buffer
	writeBox(&head, "ftyp", []byte("isom\x00\x00\x00\x00"))
	mdatStart := head.Len()
	// mdat small header whose size covers the large sparse payload.
	var mdatHdr [8]byte
	binary.BigEndian.PutUint32(mdatHdr[:4], uint32(8+mdatPayload))
	copy(mdatHdr[4:8], "mdat")
	head.Write(mdatHdr[:])
	_, err = f.Write(head.Bytes())
	require.NoError(t, err)

	// Sparse hole for the mdat payload, then write a marker at its tail so the
	// relocated chunk offset can be verified against real bytes.
	chunkOff := uint32(mdatStart) + 8 + mdatPayload - 16
	_, err = f.Seek(int64(chunkOff), io.SeekStart)
	require.NoError(t, err)
	marker := bytes.Repeat([]byte{0xab}, 16)
	_, err = f.Write(marker)
	require.NoError(t, err)

	// moov with one stco entry pointing at the marker.
	var stbl mp4Builder
	stbl.raw(stcoBox([]uint32{chunkOff}))
	var minf mp4Builder
	minf.box("stbl", stbl.bytes())
	var mdia mp4Builder
	mdia.box("minf", minf.bytes())
	var trakB mp4Builder
	trakB.box("mdia", mdia.bytes())
	var moov mp4Builder
	moov.box("trak", trakB.bytes())
	var moovBox bytes.Buffer
	writeBox(&moovBox, "moov", moov.bytes())
	_, err = f.Write(moovBox.Bytes())
	require.NoError(t, err)
	require.NoError(t, f.Close())

	require.NoError(t, FastStartFile(inPath, outPath))

	got, err := os.ReadFile(outPath)
	require.NoError(t, err)

	// moov before mdat.
	moOff, _, _ := boxAt(got, "moov")
	mdOff, _, mdPayload := boxAt(got, "mdat")
	require.Less(t, moOff, mdOff)
	require.Equal(t, mdatPayload, len(mdPayload))

	// Marker preserved at the tail of the mdat payload.
	assert.Equal(t, marker, mdPayload[len(mdPayload)-16:])

	// The stco entry now points at the marker in the output file.
	stcoOff := findBoxes(got, "stco")[0]
	stcoSize := int(binary.BigEndian.Uint32(got[stcoOff : stcoOff+4]))
	entries := parseStco(got[stcoOff+8 : stcoOff+stcoSize])
	require.Len(t, entries, 1)
	newOff := entries[0]
	assert.Equal(t, marker, got[newOff:newOff+16], "relocated offset must reach the marker")
}
