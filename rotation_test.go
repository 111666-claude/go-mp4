package mp4

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"io/ioutil"
	"math"
	"os"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rotationMatrixBytes serializes a tkhd matrix the same way the tkhd box
// definition does.
func rotationMatrixBytes(m [9]int32) []byte {
	out := make([]byte, 36)
	for i, v := range m {
		binary.BigEndian.PutUint32(out[i*4:], uint32(v))
	}
	return out
}

// rotationFixtureBox assembles a box with a small header.
func rotationFixtureBox(typ BoxType, payload ...[]byte) []byte {
	body := bytes.Join(payload, nil)
	out := make([]byte, SmallHeaderSize, SmallHeaderSize+len(body))
	binary.BigEndian.PutUint32(out, uint32(SmallHeaderSize+len(body)))
	copy(out[4:], typ[:])
	return append(out, body...)
}

// rotationFixtureTkhd builds a version 0/1 tkhd payload.
func rotationFixtureTkhd(version uint8, trackID uint32, matrix [9]int32) []byte {
	matrixOffset := 40
	size := 84
	if version == 1 {
		matrixOffset = 52
		size = 96
	}
	payload := make([]byte, size)
	payload[0] = version
	payload[3] = 0x03                          // flags: track enabled, in movie
	binary.BigEndian.PutUint32(payload[4:], 0) // creation time
	if version == 0 {
		binary.BigEndian.PutUint32(payload[12:], trackID)
		binary.BigEndian.PutUint32(payload[20:], 1000) // duration
	} else {
		binary.BigEndian.PutUint64(payload[12:], 0)
		binary.BigEndian.PutUint32(payload[20:], trackID)
		binary.BigEndian.PutUint64(payload[28:], 1000)
	}
	copy(payload[matrixOffset:], rotationMatrixBytes(matrix))
	binary.BigEndian.PutUint32(payload[size-8:], 320<<16) // width 320.0
	binary.BigEndian.PutUint32(payload[size-4:], 180<<16) // height 180.0
	// volume is stored right after the 8 reserved bytes which follow duration.
	volumeOffset := 36
	if version == 1 {
		volumeOffset = 48
	}
	binary.BigEndian.PutUint16(payload[volumeOffset:], 0x0100)
	return payload
}

// rotationFixtureHdlr builds an hdlr payload for the given handler type.
func rotationFixtureHdlr(handlerType [4]byte) []byte {
	payload := make([]byte, 24)
	copy(payload[8:], handlerType[:])
	payload[8+4] = 0x01 // make the reserved bytes distinguishable
	return payload
}

// rotationFixtureMdhd builds a version 0 mdhd payload with the given
// timescale.
func rotationFixtureMdhd(timescale uint32) []byte {
	payload := make([]byte, 24)
	binary.BigEndian.PutUint32(payload[12:], timescale)
	binary.BigEndian.PutUint16(payload[20:], 0x55c4) // language "und"
	return payload
}

// rotationFixtureTrak builds a trak box containing tkhd, mdia/hdlr and an
// unknown box, plus the given extra children.
func rotationFixtureTrak(version uint8, trackID uint32, handlerType [4]byte, matrix [9]int32, extra ...[]byte) []byte {
	tkhdPayload := rotationFixtureTkhd(version, trackID, matrix)
	unknown := rotationFixtureBox(StrToBoxType("zzzz"), []byte{0xde, 0xad, 0xbe, 0xef, 0x01, 0x02, 0x03})
	timescale := uint32(10240)
	if handlerType == [4]byte{'s', 'o', 'u', 'n'} {
		timescale = 8000
	}
	mdia := rotationFixtureBox(BoxTypeMdia(),
		rotationFixtureBox(BoxTypeHdlr(), rotationFixtureHdlr(handlerType)),
		rotationFixtureBox(BoxTypeMinf()),
		rotationFixtureBox(BoxTypeMdhd(), rotationFixtureMdhd(timescale)),
	)
	children := [][]byte{
		rotationFixtureBox(BoxTypeTkhd(), tkhdPayload),
		rotationFixtureBox(BoxTypeEdts()),
		mdia,
		unknown,
	}
	return rotationFixtureBox(BoxTypeTrak(), append(children, extra...)...)
}

func rotationFixtureIdentity() [9]int32 {
	m, err := TrackDisplayMatrix(Rotation0)
	if err != nil {
		panic(err)
	}
	return m
}

func TestTrackDisplayMatrix(t *testing.T) {
	identity := [9]int32{
		0x00010000, 0, 0,
		0, 0x00010000, 0,
		0, 0, 0x40000000,
	}
	testCases := []struct {
		rotation int
		want     [9]int32
	}{
		{Rotation0, identity},
		{Rotation90, [9]int32{
			0, 0x00010000, 0,
			-0x00010000, 0, 0,
			0, 0, 0x40000000,
		}},
		{Rotation180, [9]int32{
			-0x00010000, 0, 0,
			0, -0x00010000, 0,
			0, 0, 0x40000000,
		}},
		{Rotation270, [9]int32{
			0, -0x00010000, 0,
			0x00010000, 0, 0,
			0, 0, 0x40000000,
		}},
	}
	for _, tc := range testCases {
		t.Run(strconv.Itoa(tc.rotation), func(t *testing.T) {
			got, err := TrackDisplayMatrix(tc.rotation)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)

			// The matrix must round-trip through the interpreter.
			back, err := TrackRotationFromMatrix(got)
			require.NoError(t, err)
			assert.Equal(t, tc.rotation, back)
		})
	}

	for _, rotation := range []int{-90, -1, 1, 45, 89, 91, 135, 360, 450, 1000} {
		_, err := TrackDisplayMatrix(rotation)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrInvalidRotation), "rotation=%d err=%v", rotation, err)
	}
}

func TestTrackRotationFromMatrix(t *testing.T) {
	// Scaled versions of the cardinal rotations are accepted because only the
	// orientation of the upper-left 2x2 sub-matrix matters.
	scaled90, err := TrackDisplayMatrix(Rotation90)
	require.NoError(t, err)
	for i := 0; i < 6; i++ {
		scaled90[i] *= 2
	}
	got, err := TrackRotationFromMatrix(scaled90)
	require.NoError(t, err)
	assert.Equal(t, Rotation90, got)

	testCases := []struct {
		name   string
		matrix [9]int32
	}{
		{"zero", [9]int32{0, 0, 0, 0, 0, 0, 0, 0, 0x40000000}},
		{"flip horizontal", [9]int32{-0x10000, 0, 0, 0, 0x10000, 0, 0, 0, 0x40000000}},
		{"shear", [9]int32{0x10000, 0x10000, 0, 0, 0x10000, 0, 0, 0, 0x40000000}},
		{"45 degrees", [9]int32{0x0b505, 0x0b505, 0, -0x0b505, 0x0b505, 0, 0, 0, 0x40000000}},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := TrackRotationFromMatrix(tc.matrix)
			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrInvalidTrackMatrix), "err=%v", err)
		})
	}

	_, err = TkhdRotation(nil)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidTrackMatrix))

	tkhd := &Tkhd{}
	require.NoError(t, SetTkhdRotation(tkhd, Rotation270))
	assert.Equal(t, [9]int32{
		0, -0x10000, 0,
		0x10000, 0, 0,
		0, 0, 0x40000000,
	}, tkhd.Matrix)
	rotation, err := TkhdRotation(tkhd)
	require.NoError(t, err)
	assert.Equal(t, Rotation270, rotation)

	require.True(t, errors.Is(SetTkhdRotation(tkhd, 45), ErrInvalidRotation))
	require.True(t, errors.Is(SetTkhdRotation(nil, Rotation90), ErrInvalidTrackMatrix))

	// SetTkhdRotation must not disturb the other tkhd fields.
	orig := &Tkhd{
		TrackID:        7,
		CreationTimeV0: 11,
		DurationV0:     13,
		Width:          640 << 16,
		Height:         360 << 16,
		Volume:         0x0100,
	}
	require.NoError(t, SetTkhdRotation(orig, Rotation180))
	assert.Equal(t, uint32(7), orig.TrackID)
	assert.Equal(t, uint32(11), orig.CreationTimeV0)
	assert.Equal(t, uint32(13), orig.DurationV0)
	assert.Equal(t, uint32(640<<16), orig.Width)
	assert.Equal(t, uint32(360<<16), orig.Height)
	assert.Equal(t, int16(0x0100), orig.Volume)
}

// buildRotationFixture assembles a complete dual-track MP4 in memory:
//
//	ftyp
//	moov
//	  mvhd
//	  trak (video, trackID 1)
//	  trak (audio, trackID 2)
//	  free
//	unknown top-level box
//	mdat
func buildRotationFixture(t *testing.T, videoVersion uint8) []byte {
	t.Helper()
	ftyp := rotationFixtureBox(BoxTypeFtyp(), []byte{'i', 's', 'o', 'm', 0, 0, 0, 0, 'i', 's', 'o', 'm'})
	mvhd := rotationFixtureBox(BoxTypeMvhd(), make([]byte, 96))
	video := rotationFixtureTrak(videoVersion, 1, [4]byte{'v', 'i', 'd', 'e'}, rotationFixtureIdentity())
	audio := rotationFixtureTrak(0, 2, [4]byte{'s', 'o', 'u', 'n'}, rotationFixtureIdentity())
	free := rotationFixtureBox(BoxTypeFree(), []byte{1, 2, 3, 4, 5})
	moov := rotationFixtureBox(BoxTypeMoov(), mvhd, video, audio, free)
	// An unsupported box type which must be preserved verbatim.
	unkn := rotationFixtureBox(StrToBoxType("qqqq"), []byte{9, 8, 7, 6, 5, 4, 3, 2, 1})
	mdatPayload := bytes.Repeat([]byte{0xAB, 0xCD, 0xEF, 0x01, 0x23}, 400)
	mdat := rotationFixtureBox(BoxTypeMdat(), mdatPayload)
	return bytes.Join([][]byte{ftyp, moov, unkn, mdat}, nil)
}

// rotationTrackIDs returns the tkhd track IDs found in data, in file order.
func rotationTrackIDs(t *testing.T, data []byte) []uint32 {
	t.Helper()
	bips, err := ExtractBoxesWithPayload(bytes.NewReader(data), nil, []BoxPath{
		{BoxTypeMoov(), BoxTypeTrak(), BoxTypeTkhd()},
	})
	require.NoError(t, err)
	ids := make([]uint32, 0, len(bips))
	for _, bip := range bips {
		ids = append(ids, bip.Payload.(*Tkhd).TrackID)
	}
	return ids
}

// rotationTkhds returns the parsed tkhd boxes of data.
func rotationTkhds(t *testing.T, data []byte) []*Tkhd {
	t.Helper()
	bips, err := ExtractBoxesWithPayload(bytes.NewReader(data), nil, []BoxPath{
		{BoxTypeMoov(), BoxTypeTrak(), BoxTypeTkhd()},
	})
	require.NoError(t, err)
	out := make([]*Tkhd, 0, len(bips))
	for _, bip := range bips {
		out = append(out, bip.Payload.(*Tkhd))
	}
	return out
}

func rotationMdatHash(t *testing.T, data []byte) [32]byte {
	t.Helper()
	bis, err := ExtractBox(bytes.NewReader(data), nil, BoxPath{BoxTypeMdat()})
	require.NoError(t, err)
	require.Len(t, bis, 1)
	bi := bis[0]
	return sha256.Sum256(data[bi.Offset+bi.HeaderSize : bi.Offset+bi.Size])
}

func TestRewriteTrackRotationDualTrack(t *testing.T) {
	orig := buildRotationFixture(t, 0)

	tkhds := rotationTkhds(t, orig)
	require.Len(t, tkhds, 2)
	assert.Equal(t, uint32(1), tkhds[0].TrackID)
	assert.Equal(t, uint32(320<<16), tkhds[0].Width)
	assert.Equal(t, uint32(180<<16), tkhds[0].Height)
	assert.Equal(t, uint32(2), tkhds[1].TrackID)
	assert.Equal(t, uint32(0x0100), uint32(tkhds[1].Volume))
	origAudioMatrix := tkhds[1].Matrix
	origMdatHash := rotationMdatHash(t, orig)
	origSize := len(orig)

	for _, rotation := range []int{Rotation90, Rotation180, Rotation270, Rotation0} {
		t.Run(strconv.Itoa(rotation), func(t *testing.T) {
			var out bytes.Buffer
			require.NoError(t, RewriteTrackRotation(bytes.NewReader(orig), &out, 1, rotation))

			rewritten := out.Bytes()
			// The matrix is a fixed-size field, so the file size must not change.
			require.Len(t, rewritten, origSize)

			// The output must be parseable again.
			ids := rotationTrackIDs(t, rewritten)
			assert.Equal(t, []uint32{1, 2}, ids)

			rewrittenTkhds := rotationTkhds(t, rewritten)
			require.Len(t, rewrittenTkhds, 2)
			wantMatrix, err := TrackDisplayMatrix(rotation)
			require.NoError(t, err)
			assert.Equal(t, wantMatrix, rewrittenTkhds[0].Matrix)
			assert.Equal(t, uint32(1), rewrittenTkhds[0].TrackID)
			assert.Equal(t, uint32(320<<16), rewrittenTkhds[0].Width)
			assert.Equal(t, uint32(180<<16), rewrittenTkhds[0].Height)
			assert.Equal(t, uint32(0x000003), rewrittenTkhds[0].GetFlags())
			assert.Equal(t, uint32(1000), rewrittenTkhds[0].DurationV0)

			// The other track must be untouched.
			assert.Equal(t, origAudioMatrix, rewrittenTkhds[1].Matrix)
			assert.Equal(t, uint32(2), rewrittenTkhds[1].TrackID)
			assert.Equal(t, uint32(0x0100), uint32(rewrittenTkhds[1].Volume))

			// Every byte except the 36 matrix bytes must be preserved.
			matrixOffset := rotationMatrixFileOffset(t, orig, 1)
			assert.Equal(t, orig[:matrixOffset], rewritten[:matrixOffset])
			assert.Equal(t, orig[matrixOffset+36:], rewritten[matrixOffset+36:])

			// The media payload is untouched.
			assert.Equal(t, origMdatHash, rotationMdatHash(t, rewritten))

			// The resulting rotation can be read back.
			got, err := ReadTrackRotation(bytes.NewReader(rewritten), 1)
			require.NoError(t, err)
			assert.Equal(t, rotation, got)
		})
	}
}

// rotationMatrixFileOffset locates the absolute matrix offset of the track.
func rotationMatrixFileOffset(t *testing.T, data []byte, trackID uint32) int {
	t.Helper()
	bips, err := ExtractBoxesWithPayload(bytes.NewReader(data), nil, []BoxPath{
		{BoxTypeMoov(), BoxTypeTrak(), BoxTypeTkhd()},
	})
	require.NoError(t, err)
	for _, bip := range bips {
		if bip.Payload.(*Tkhd).TrackID != trackID {
			continue
		}
		// Scan the payload for the serialized identity matrix. It only occurs
		// once inside a tkhd payload.
		needle := rotationMatrixBytes(rotationFixtureIdentity())
		start := int(bip.Info.Offset + bip.Info.HeaderSize)
		end := int(bip.Info.Offset + bip.Info.Size)
		idx := bytes.Index(data[start:end], needle)
		require.NotEqual(t, -1, idx)
		return start + idx
	}
	t.Fatalf("track %d not found", trackID)
	return 0
}

// TestRewriteTrackRotationChunkOffsets verifies that the absolute chunk
// offsets in stco still address the same media bytes after the rewrite, and
// that both tracks keep their sample data. The patch does not move any box,
// so the offsets must stay literally identical.
func TestRewriteTrackRotationChunkOffsets(t *testing.T) {
	orig, err := ioutil.ReadFile("./testdata/sample_dual.mp4")
	require.NoError(t, err)

	origStco := rotationStcoEntries(t, orig)
	require.GreaterOrEqual(t, len(origStco), 2)
	require.NotEqual(t, origStco[0], origStco[1])

	var out bytes.Buffer
	require.NoError(t, RewriteTrackRotation(bytes.NewReader(orig), &out, 1, Rotation270))
	written := out.Bytes()

	// stco offsets are unchanged.
	assert.Equal(t, origStco, rotationStcoEntries(t, written))

	// And they point at identical bytes in both files.
	for trackIdx, offsets := range origStco {
		for _, off := range offsets {
			require.Less(t, off+8, uint32(len(orig)))
			assert.Equal(t, orig[off:off+8], written[off:off+8], "track #%d chunk at %d", trackIdx, off)
		}
	}

	// The probe reports the same chunk layout for both tracks.
	info, err := Probe(bytes.NewReader(written))
	require.NoError(t, err)
	require.Len(t, info.Tracks, 2)
	for i := range info.Tracks {
		require.Len(t, info.Tracks[i].Chunks, len(origStco[i]))
		for j, chunk := range info.Tracks[i].Chunks {
			assert.Equal(t, uint64(origStco[i][j]), chunk.DataOffset)
		}
	}
}

func rotationStcoEntries(t *testing.T, data []byte) [][]uint32 {
	t.Helper()
	bips, err := ExtractBoxesWithPayload(bytes.NewReader(data), nil, []BoxPath{
		{BoxTypeMoov(), BoxTypeTrak(), BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeStco()},
	})
	require.NoError(t, err)
	out := make([][]uint32, 0, len(bips))
	for _, bip := range bips {
		offsets := bip.Payload.(*Stco).ChunkOffset
		out = append(out, append([]uint32(nil), offsets...))
	}
	return out
}

// TestRewriteTrackRotationTrackOrder builds a movie whose audio track comes
// first and whose video track is second, then edits the video track by ID.
func TestRewriteTrackRotationTrackOrder(t *testing.T) {
	ftyp := rotationFixtureBox(BoxTypeFtyp(), []byte{'i', 's', 'o', 'm', 0, 0, 0, 0, 'i', 's', 'o', 'm'})
	audio := rotationFixtureTrak(0, 1, [4]byte{'s', 'o', 'u', 'n'}, rotationFixtureIdentity())
	video := rotationFixtureTrak(0, 2, [4]byte{'v', 'i', 'd', 'e'}, rotationFixtureIdentity())
	mdat := rotationFixtureBox(BoxTypeMdat(), bytes.Repeat([]byte{0x42}, 64))
	orig := bytes.Join([][]byte{ftyp, rotationFixtureBox(BoxTypeMoov(), audio, video), mdat}, nil)

	assert.Equal(t, []uint32{1, 2}, rotationTrackIDs(t, orig))

	var out bytes.Buffer
	require.NoError(t, RewriteTrackRotation(bytes.NewReader(orig), &out, 2, Rotation180))
	written := out.Bytes()

	tkhds := rotationTkhds(t, written)
	require.Len(t, tkhds, 2)
	want, err := TrackDisplayMatrix(Rotation180)
	require.NoError(t, err)
	assert.Equal(t, want, tkhds[1].Matrix)
	assert.Equal(t, rotationFixtureIdentity(), tkhds[0].Matrix)

	rotation, err := ReadTrackRotation(bytes.NewReader(written), 2)
	require.NoError(t, err)
	assert.Equal(t, Rotation180, rotation)

	// Selecting the audio track by its ID must still be rejected.
	var rejected bytes.Buffer
	err = RewriteTrackRotation(bytes.NewReader(orig), &rejected, 1, Rotation90)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrTrackNotVideo), "err=%v", err)
	assert.Zero(t, rejected.Len())
}

func TestRewriteTrackRotationVersion1(t *testing.T) {
	orig := buildRotationFixture(t, 1)
	tkhds := rotationTkhds(t, orig)
	require.Len(t, tkhds, 2)
	assert.Equal(t, uint8(1), tkhds[0].GetVersion())

	var out bytes.Buffer
	require.NoError(t, RewriteTrackRotation(bytes.NewReader(orig), &out, 1, Rotation90))
	rewritten := out.Bytes()
	require.Len(t, rewritten, len(orig))

	rewrittenTkhds := rotationTkhds(t, rewritten)
	require.Len(t, rewrittenTkhds, 2)
	want, err := TrackDisplayMatrix(Rotation90)
	require.NoError(t, err)
	assert.Equal(t, want, rewrittenTkhds[0].Matrix)
	assert.Equal(t, uint8(1), rewrittenTkhds[0].GetVersion())
	assert.Equal(t, uint64(1000), rewrittenTkhds[0].DurationV1)
	assert.Equal(t, uint32(2), rewrittenTkhds[1].TrackID)
}

func TestSetTrackRotationInPlace(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/dual.mp4"
	orig := buildRotationFixture(t, 0)
	require.NoError(t, ioutil.WriteFile(path, orig, 0o600))

	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	require.NoError(t, err)
	require.NoError(t, SetTrackRotation(f, 1, Rotation180))
	require.NoError(t, f.Close())

	got, err := ioutil.ReadFile(path)
	require.NoError(t, err)
	require.Len(t, got, len(orig))

	rotation, err := ReadTrackRotation(bytes.NewReader(got), 1)
	require.NoError(t, err)
	assert.Equal(t, Rotation180, rotation)
	assert.Equal(t, rotationMdatHash(t, orig), rotationMdatHash(t, got))

	tkhds := rotationTkhds(t, got)
	assert.Equal(t, rotationFixtureIdentity(), tkhds[1].Matrix)
}

func TestReadTrackRotationDualTrack(t *testing.T) {
	orig := buildRotationFixture(t, 0)

	rotation, err := ReadTrackRotation(bytes.NewReader(orig), 1)
	require.NoError(t, err)
	assert.Equal(t, Rotation0, rotation)

	// A file pre-rotated by another tool must be read back.
	var rotated bytes.Buffer
	require.NoError(t, RewriteTrackRotation(bytes.NewReader(orig), &rotated, 1, Rotation270))
	rotation, err = ReadTrackRotation(bytes.NewReader(rotated.Bytes()), 1)
	require.NoError(t, err)
	assert.Equal(t, Rotation270, rotation)
}

func TestRewriteTrackRotationErrors(t *testing.T) {
	orig := buildRotationFixture(t, 0)

	t.Run("unknown track", func(t *testing.T) {
		var out bytes.Buffer
		err := RewriteTrackRotation(bytes.NewReader(orig), &out, 3, Rotation90)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrTrackNotFound), "err=%v", err)
		assert.Zero(t, out.Len(), "nothing must be written on failure")
	})

	t.Run("non video track", func(t *testing.T) {
		var out bytes.Buffer
		err := RewriteTrackRotation(bytes.NewReader(orig), &out, 2, Rotation90)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrTrackNotVideo), "err=%v", err)
		assert.Zero(t, out.Len())
	})

	t.Run("invalid rotation", func(t *testing.T) {
		var out bytes.Buffer
		err := RewriteTrackRotation(bytes.NewReader(orig), &out, 1, 45)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrInvalidRotation), "err=%v", err)
		assert.Zero(t, out.Len())
	})

	t.Run("duplicate track ID", func(t *testing.T) {
		ftyp := rotationFixtureBox(BoxTypeFtyp(), []byte{'i', 's', 'o', 'm', 0, 0, 0, 0, 'i', 's', 'o', 'm'})
		moov := rotationFixtureBox(BoxTypeMoov(),
			rotationFixtureTrak(0, 1, [4]byte{'v', 'i', 'd', 'e'}, rotationFixtureIdentity()),
			rotationFixtureTrak(0, 1, [4]byte{'v', 'i', 'd', 'e'}, rotationFixtureIdentity()),
		)
		dup := bytes.Join([][]byte{ftyp, moov}, nil)
		var out bytes.Buffer
		err := RewriteTrackRotation(bytes.NewReader(dup), &out, 1, Rotation90)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrDuplicateTrackID), "err=%v", err)
		assert.Zero(t, out.Len())
	})

	t.Run("missing moov", func(t *testing.T) {
		data := rotationFixtureBox(BoxTypeFtyp(), []byte{'i', 's', 'o', 'm', 0, 0, 0, 0, 'i', 's', 'o', 'm'})
		var out bytes.Buffer
		err := RewriteTrackRotation(bytes.NewReader(data), &out, 1, Rotation90)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrInvalidBoxStructure), "err=%v", err)
		assert.Zero(t, out.Len())
	})

	t.Run("missing tkhd", func(t *testing.T) {
		ftyp := rotationFixtureBox(BoxTypeFtyp(), []byte{'i', 's', 'o', 'm', 0, 0, 0, 0, 'i', 's', 'o', 'm'})
		trak := rotationFixtureBox(BoxTypeTrak(),
			rotationFixtureBox(BoxTypeMdia(), rotationFixtureBox(BoxTypeHdlr(), rotationFixtureHdlr([4]byte{'v', 'i', 'd', 'e'}))),
		)
		data := bytes.Join([][]byte{ftyp, rotationFixtureBox(BoxTypeMoov(), trak)}, nil)
		var out bytes.Buffer
		err := RewriteTrackRotation(bytes.NewReader(data), &out, 1, Rotation90)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrInvalidBoxStructure), "err=%v", err)
	})

	t.Run("missing hdlr", func(t *testing.T) {
		ftyp := rotationFixtureBox(BoxTypeFtyp(), []byte{'i', 's', 'o', 'm', 0, 0, 0, 0, 'i', 's', 'o', 'm'})
		tkhdPayload := rotationFixtureTkhd(0, 1, rotationFixtureIdentity())
		trak := rotationFixtureBox(BoxTypeTrak(),
			rotationFixtureBox(BoxTypeTkhd(), tkhdPayload),
			rotationFixtureBox(BoxTypeMdia()),
		)
		data := bytes.Join([][]byte{ftyp, rotationFixtureBox(BoxTypeMoov(), trak)}, nil)
		var out bytes.Buffer
		err := RewriteTrackRotation(bytes.NewReader(data), &out, 1, Rotation90)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrInvalidBoxStructure), "err=%v", err)
	})
}

func TestRewriteTrackRotationCorruptBoxes(t *testing.T) {
	orig := buildRotationFixture(t, 0)

	t.Run("truncated file", func(t *testing.T) {
		var out bytes.Buffer
		err := RewriteTrackRotation(bytes.NewReader(orig[:len(orig)-7]), &out, 1, Rotation90)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrInvalidBoxStructure), "err=%v", err)
		assert.Zero(t, out.Len())
	})

	t.Run("truncated inside moov", func(t *testing.T) {
		// ftyp + a moov box whose declared size exceeds the file.
		ftyp := rotationFixtureBox(BoxTypeFtyp(), []byte{'i', 's', 'o', 'm', 0, 0, 0, 0, 'i', 's', 'o', 'm'})
		moov := rotationFixtureBox(BoxTypeMoov(), make([]byte, 16))
		data := append(append([]byte{}, ftyp...), moov...)
		binary.BigEndian.PutUint32(data[len(ftyp):], uint32(len(moov)+100))
		var out bytes.Buffer
		err := RewriteTrackRotation(bytes.NewReader(data), &out, 1, Rotation90)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrInvalidBoxStructure), "err=%v", err)
	})

	t.Run("box size smaller than header", func(t *testing.T) {
		data := append([]byte{}, orig...)
		// Corrupt the first top-level box size to 4.
		binary.BigEndian.PutUint32(data, 4)
		var out bytes.Buffer
		err := RewriteTrackRotation(bytes.NewReader(data), &out, 1, Rotation90)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrInvalidBoxStructure), "err=%v", err)
	})

	t.Run("declared child larger than parent", func(t *testing.T) {
		// A trak whose declared size extends past the end of its moov parent.
		ftyp := rotationFixtureBox(BoxTypeFtyp(), []byte{'i', 's', 'o', 'm', 0, 0, 0, 0, 'i', 's', 'o', 'm'})
		trak := rotationFixtureBox(BoxTypeTrak(), make([]byte, 8))
		moov := rotationFixtureBox(BoxTypeMoov(), trak)
		binary.BigEndian.PutUint32(moov[SmallHeaderSize:], uint32(len(trak)+16))
		data := bytes.Join([][]byte{ftyp, moov}, nil)
		var out bytes.Buffer
		err := RewriteTrackRotation(bytes.NewReader(data), &out, 1, Rotation90)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrInvalidBoxStructure), "err=%v", err)
	})

	t.Run("trailing garbage", func(t *testing.T) {
		data := append(append([]byte{}, orig...), 0x01, 0x02, 0x03)
		var out bytes.Buffer
		err := RewriteTrackRotation(bytes.NewReader(data), &out, 1, Rotation90)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrInvalidBoxStructure), "err=%v", err)
	})

	t.Run("truncated tkhd payload", func(t *testing.T) {
		// A version 0 tkhd with only 60 payload bytes.
		ftyp := rotationFixtureBox(BoxTypeFtyp(), []byte{'i', 's', 'o', 'm', 0, 0, 0, 0, 'i', 's', 'o', 'm'})
		shortTkhd := rotationFixtureBox(BoxTypeTkhd(), make([]byte, 60))
		trak := rotationFixtureBox(BoxTypeTrak(),
			shortTkhd,
			rotationFixtureBox(BoxTypeMdia(), rotationFixtureBox(BoxTypeHdlr(), rotationFixtureHdlr([4]byte{'v', 'i', 'd', 'e'}))),
		)
		data := bytes.Join([][]byte{ftyp, rotationFixtureBox(BoxTypeMoov(), trak)}, nil)
		var out bytes.Buffer
		err := RewriteTrackRotation(bytes.NewReader(data), &out, 1, Rotation90)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrInvalidBoxStructure), "err=%v", err)
	})

	t.Run("unsupported tkhd version", func(t *testing.T) {
		ftyp := rotationFixtureBox(BoxTypeFtyp(), []byte{'i', 's', 'o', 'm', 0, 0, 0, 0, 'i', 's', 'o', 'm'})
		payload := make([]byte, 100)
		payload[0] = 2
		trak := rotationFixtureBox(BoxTypeTrak(),
			rotationFixtureBox(BoxTypeTkhd(), payload),
			rotationFixtureBox(BoxTypeMdia(), rotationFixtureBox(BoxTypeHdlr(), rotationFixtureHdlr([4]byte{'v', 'i', 'd', 'e'}))),
		)
		data := bytes.Join([][]byte{ftyp, rotationFixtureBox(BoxTypeMoov(), trak)}, nil)
		var out bytes.Buffer
		err := RewriteTrackRotation(bytes.NewReader(data), &out, 1, Rotation90)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrInvalidBoxStructure), "err=%v", err)
	})

	t.Run("extend to EOF box inside container", func(t *testing.T) {
		// A size 0 box is only valid at the top level. A zero-size child of
		// moov must be rejected instead of swallowing the rest of the file.
		ftyp := rotationFixtureBox(BoxTypeFtyp(), []byte{'i', 's', 'o', 'm', 0, 0, 0, 0, 'i', 's', 'o', 'm'})
		zeroChild := make([]byte, SmallHeaderSize+8)
		freeType := BoxTypeFree()
		copy(zeroChild[4:], freeType[:])
		moov := rotationFixtureBox(BoxTypeMoov(), zeroChild)
		data := bytes.Join([][]byte{ftyp, moov, rotationFixtureBox(BoxTypeMdat(), []byte{1, 2, 3})}, nil)
		var out bytes.Buffer
		err := RewriteTrackRotation(bytes.NewReader(data), &out, 1, Rotation90)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrInvalidBoxStructure), "err=%v", err)
	})
}

func TestRewriteTrackRotationLargeHeader(t *testing.T) {
	// The rewriter must handle boxes with a 16-byte header and keep the bytes
	// outside of the matrix untouched.
	orig := buildRotationFixture(t, 0)

	// Rebuild mdat with a large header (size==1 + 64-bit size).
	mdatStart := bytes.LastIndex(orig, []byte("mdat")) - 4
	require.Greater(t, mdatStart, 0)
	mdatSize := binary.BigEndian.Uint32(orig[mdatStart : mdatStart+4])
	payload := orig[mdatStart+8 : int(mdatStart)+int(mdatSize)]
	large := make([]byte, LargeHeaderSize)
	binary.BigEndian.PutUint32(large, 1)
	copy(large[4:], "mdat")
	binary.BigEndian.PutUint64(large[8:], uint64(LargeHeaderSize+len(payload)))
	withLarge := append(append([]byte{}, orig[:mdatStart]...), large...)
	withLarge = append(withLarge, payload...)

	// With the identity rotation, the rewrite must be a no-op.
	var identity bytes.Buffer
	require.NoError(t, RewriteTrackRotation(bytes.NewReader(withLarge), &identity, 1, Rotation0))
	assert.Equal(t, withLarge, identity.Bytes())

	// With a 90 degree rotation, only the 36 matrix bytes may change; the large
	// mdat header and payload must be preserved.
	var out bytes.Buffer
	require.NoError(t, RewriteTrackRotation(bytes.NewReader(withLarge), &out, 1, Rotation90))
	written := out.Bytes()
	require.Len(t, written, len(withLarge))
	matrixOffset := rotationMatrixFileOffset(t, withLarge, 1)
	want, err := TrackDisplayMatrix(Rotation90)
	require.NoError(t, err)
	assert.Equal(t, rotationMatrixBytes(want), written[matrixOffset:matrixOffset+36])
	assert.Equal(t, withLarge[:matrixOffset], written[:matrixOffset])
	assert.Equal(t, withLarge[matrixOffset+36:], written[matrixOffset+36:])
}

func TestSetTrackRotationErrors(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/dual.mp4"
	require.NoError(t, ioutil.WriteFile(path, buildRotationFixture(t, 0), 0o600))

	t.Run("read only stream", func(t *testing.T) {
		f, err := os.Open(path)
		require.NoError(t, err)
		defer f.Close()
		// An os.File opened read-only still implements ReadWriteSeeker, the
		// write itself fails; the returned error must be explicit.
		err = SetTrackRotation(f, 1, Rotation90)
		require.Error(t, err)
	})

	t.Run("invalid rotation", func(t *testing.T) {
		f, err := os.OpenFile(path, os.O_RDWR, 0o600)
		require.NoError(t, err)
		defer f.Close()
		err = SetTrackRotation(f, 1, 360)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrInvalidRotation), "err=%v", err)
	})

	t.Run("unknown track", func(t *testing.T) {
		f, err := os.OpenFile(path, os.O_RDWR, 0o600)
		require.NoError(t, err)
		defer f.Close()
		err = SetTrackRotation(f, 9, Rotation90)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrTrackNotFound), "err=%v", err)
	})
}

// TestRotationTestdataAllSamples rewrites the matrix of every video sample in
// testdata and verifies that the result is one patch away from the original
// and parses again.
func TestRotationTestdataAllSamples(t *testing.T) {
	files := []struct {
		name    string
		trackID uint32
	}{
		{"sample.mp4", 1},
		{"sample_qt.mp4", 1},
		{"sample_fragmented.mp4", 1},
		{"sample_fragmented_muxed.mp4", 1},
		{"sample_init.encv.mp4", 1},
	}
	for _, tc := range files {
		t.Run(tc.name, func(t *testing.T) {
			orig, err := os.ReadFile("./testdata/" + tc.name)
			require.NoError(t, err)

			var out bytes.Buffer
			require.NoError(t, RewriteTrackRotation(bytes.NewReader(orig), &out, tc.trackID, Rotation90))
			written := out.Bytes()
			require.Len(t, written, len(orig))

			rotation, err := ReadTrackRotation(bytes.NewReader(written), tc.trackID)
			require.NoError(t, err)
			assert.Equal(t, Rotation90, rotation)

			// The output must parse with the regular box reader.
			_, err = ReadBoxStructure(bytes.NewReader(written), func(h *ReadHandle) (interface{}, error) {
				return nil, nil
			})
			require.NoError(t, err)

			// Exactly the 36 matrix bytes differ.
			matrixOffset := rotationMatrixFileOffsetFromData(t, orig, tc.trackID)
			assert.Equal(t, orig[:matrixOffset], written[:matrixOffset])
			assert.Equal(t, orig[matrixOffset+36:], written[matrixOffset+36:])
		})
	}
}

func rotationMatrixFileOffsetFromData(t *testing.T, data []byte, trackID uint32) int {
	t.Helper()
	bips, err := ExtractBoxesWithPayload(bytes.NewReader(data), nil, []BoxPath{
		{BoxTypeMoov(), BoxTypeTrak(), BoxTypeTkhd()},
	})
	require.NoError(t, err)
	for _, bip := range bips {
		tkhd := bip.Payload.(*Tkhd)
		if tkhd.TrackID != trackID {
			continue
		}
		needle := rotationMatrixBytes(tkhd.Matrix)
		start := int(bip.Info.Offset + bip.Info.HeaderSize)
		end := int(bip.Info.Offset + bip.Info.Size)
		idx := bytes.Index(data[start:end], needle)
		require.NotEqual(t, -1, idx)
		return start + idx
	}
	t.Fatalf("track %d not found", trackID)
	return 0
}

func TestRotationTestdataDualTrack(t *testing.T) {
	f, err := os.Open("./testdata/sample_dual.mp4")
	require.NoError(t, err)
	defer f.Close()

	orig, err := io.ReadAll(f)
	require.NoError(t, err)

	// The sample has a video track (ID 1) and an audio track (ID 2), both with
	// an identity display matrix.
	ids := rotationTrackIDs(t, orig)
	assert.Equal(t, []uint32{1, 2}, ids)

	origHash := rotationMdatHash(t, orig)
	origTkhds := rotationTkhds(t, orig)
	require.Len(t, origTkhds, 2)

	for _, rotation := range []int{Rotation0, Rotation90, Rotation180, Rotation270} {
		var out bytes.Buffer
		require.NoError(t, RewriteTrackRotation(bytes.NewReader(orig), &out, 1, rotation))
		written := out.Bytes()
		require.Len(t, written, len(orig))

		// The rewritten file must be parsed by the regular reader.
		info, err := Probe(bytes.NewReader(written))
		require.NoError(t, err)
		require.Len(t, info.Tracks, 2)
		assert.Equal(t, uint32(1), info.Tracks[0].TrackID)
		assert.Equal(t, uint32(2), info.Tracks[1].TrackID)
		assert.Equal(t, uint32(10240), info.Tracks[0].Timescale)
		assert.Equal(t, uint32(8000), info.Tracks[1].Timescale)

		tkhds := rotationTkhds(t, written)
		require.Len(t, tkhds, 2)
		want, err := TrackDisplayMatrix(rotation)
		require.NoError(t, err)
		assert.Equal(t, want, tkhds[0].Matrix)
		// Width and height are preserved.
		assert.Equal(t, origTkhds[0].Width, tkhds[0].Width)
		assert.Equal(t, origTkhds[0].Height, tkhds[0].Height)
		assert.Equal(t, uint32(160<<16), tkhds[0].Width)
		assert.Equal(t, uint32(90<<16), tkhds[0].Height)
		// The audio track is untouched.
		assert.Equal(t, origTkhds[1].Matrix, tkhds[1].Matrix)
		// The mdat bytes are identical.
		assert.Equal(t, origHash, rotationMdatHash(t, written))

		got, err := ReadTrackRotation(bytes.NewReader(written), 1)
		require.NoError(t, err)
		assert.Equal(t, rotation, got)
	}

	// Audio tracks must be rejected.
	_, err = ReadTrackRotation(bytes.NewReader(orig), 2)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrTrackNotVideo), "err=%v", err)

	var out bytes.Buffer
	err = RewriteTrackRotation(bytes.NewReader(orig), &out, 2, Rotation90)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrTrackNotVideo), "err=%v", err)
	assert.Zero(t, out.Len())

	// A non-existent track must be rejected.
	_, err = ReadTrackRotation(bytes.NewReader(orig), 7)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrTrackNotFound), "err=%v", err)
}

// rotationUnknownBoxOffsets records the file offset of every unsupported box
// so tests can prove they are preserved byte for byte.
func rotationUnknownBoxOffsets(t *testing.T, data []byte) map[string]BoxInfo {
	t.Helper()
	boxes := make(map[string]BoxInfo)
	_, err := ReadBoxStructure(bytes.NewReader(data), func(h *ReadHandle) (interface{}, error) {
		if !h.BoxInfo.IsSupportedType() && h.BoxInfo.Type != BoxTypeMdat() {
			boxes[h.BoxInfo.Type.String()] = h.BoxInfo
			return nil, nil
		}
		if h.BoxInfo.Type == BoxTypeMoov() || h.BoxInfo.Type == BoxTypeTrak() ||
			h.BoxInfo.Type == BoxTypeMdia() || h.BoxInfo.Type == BoxTypeMinf() {
			return h.Expand()
		}
		return nil, nil
	})
	require.NoError(t, err)
	return boxes
}

func TestRewriteTrackRotationPreservesUnknownBoxes(t *testing.T) {
	orig := buildRotationFixture(t, 0)
	unknowns := rotationUnknownBoxOffsets(t, orig)
	require.Contains(t, unknowns, "qqqq")
	require.Contains(t, unknowns, "zzzz")

	var out bytes.Buffer
	require.NoError(t, RewriteTrackRotation(bytes.NewReader(orig), &out, 1, Rotation90))
	written := out.Bytes()

	after := rotationUnknownBoxOffsets(t, written)
	require.Contains(t, after, "qqqq")
	require.Contains(t, after, "zzzz")
	for typ, before := range unknowns {
		now, ok := after[typ]
		require.True(t, ok, "box %s disappeared", typ)
		assert.Equal(t, before.Offset, now.Offset, "box %s moved", typ)
		assert.Equal(t, before.Size, now.Size, "box %s changed size", typ)
		assert.Equal(t, orig[before.Offset:before.Offset+before.Size], written[now.Offset:now.Offset+now.Size])
	}

	// All box offsets and sizes are unchanged because the matrix is fixed-size.
	origTop := parseRotationTopLevel(t, orig)
	newTop := parseRotationTopLevel(t, written)
	require.Len(t, newTop, len(origTop))
	for i := range origTop {
		assert.Equal(t, origTop[i].Offset, newTop[i].Offset)
		assert.Equal(t, origTop[i].Size, newTop[i].Size)
		assert.Equal(t, origTop[i].Type, newTop[i].Type)
	}
}

func parseRotationTopLevel(t *testing.T, data []byte) []*BoxInfo {
	t.Helper()
	var infos []*BoxInfo
	_, err := ReadBoxStructure(bytes.NewReader(data), func(h *ReadHandle) (interface{}, error) {
		if len(h.Path) == 1 {
			bi := h.BoxInfo
			infos = append(infos, &bi)
		}
		return nil, nil
	})
	require.NoError(t, err)
	return infos
}

func TestRewriteTrackRotationNonSeekableWriter(t *testing.T) {
	// The scan relies on absolute offsets and therefore needs a seekable input,
	// but the output is written sequentially and only has to implement io.Writer.
	orig := buildRotationFixture(t, 0)
	var out bytes.Buffer
	require.NoError(t, RewriteTrackRotation(bytes.NewReader(orig), &out, 1, Rotation90))

	var second bytes.Buffer
	require.NoError(t, RewriteTrackRotation(bytes.NewReader(orig), &second, 1, Rotation90))
	assert.Equal(t, out.Bytes(), second.Bytes())
}

func TestTrackRotationFromMatrixRounding(t *testing.T) {
	// Values around the cardinal rotations within tolerance must be accepted,
	// values clearly in between must be rejected.
	near90 := [9]int32{
		0, 0x00010000, 0,
		-0x00010000, 0, 0,
		0, 0, 0x40000000,
	}
	near90[0] = 3 // tiny skew
	got, err := TrackRotationFromMatrix(near90)
	require.NoError(t, err)
	assert.Equal(t, Rotation90, got)

	// A matrix scaled by a negative factor is a rotation plus flip and must be
	// rejected.
	flip := [9]int32{
		-0x00010000, 0, 0,
		0, 0x00010000, 0,
		0, 0, 0x40000000,
	}
	_, err = TrackRotationFromMatrix(flip)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidTrackMatrix))

	// Huge values must not overflow into a false positive.
	huge := [9]int32{
		math.MaxInt32, 0, 0,
		0, math.MaxInt32, 0,
		0, 0, 0x40000000,
	}
	got, err = TrackRotationFromMatrix(huge)
	require.NoError(t, err)
	assert.Equal(t, Rotation0, got)
}
