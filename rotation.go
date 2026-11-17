package mp4

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

// Supported display rotations, in degrees.
//
// A rotation is applied clockwise while the track is presented. This is the
// convention used by the camera orientation hints of iOS and Android: a
// portrait recording made with a landscape sensor carries rotation 90, i.e.
// the stored frame has to be rotated 90 degrees clockwise for display.
//
// Note that FFmpeg reports the counter-clockwise angle in the display matrix
// side data, so a track with rotation 90 here is reported by ffprobe as
// rotation=-90.
const (
	Rotation0   = 0
	Rotation90  = 90
	Rotation180 = 180
	Rotation270 = 270
)

// Fixed-point representations used by the tkhd matrix.
const (
	rotationMatrixOne16_16 int32 = 0x00010000 // 1.0 in 16.16 fixed point
	rotationMatrixOne2_30  int32 = 0x40000000 // 1.0 in 2.30 fixed point
)

// Errors returned by the track rotation helpers. They are wrapped with
// contextual messages, so callers should use errors.Is instead of comparing
// them directly.
var (
	// ErrInvalidRotation is returned when a rotation other than 0, 90, 180
	// or 270 degrees is requested.
	ErrInvalidRotation = errors.New("go-mp4: invalid rotation")

	// ErrTrackNotFound is returned when no track with the requested track ID
	// exists in the moov box.
	ErrTrackNotFound = errors.New("go-mp4: track not found")

	// ErrTrackNotVideo is returned when the requested track is not a video
	// track (its hdlr handler_type is not "vide").
	ErrTrackNotVideo = errors.New("go-mp4: track is not a video track")

	// ErrDuplicateTrackID is returned when two or more trak boxes share the
	// same track ID, which makes the selection by track ID ambiguous.
	ErrDuplicateTrackID = errors.New("go-mp4: duplicate track ID")

	// ErrInvalidTrackMatrix is returned when a display matrix is not a pure
	// 0/90/180/270 degree rotation (for example a shear, a flip or a
	// non-cardinal angle), or when a tkhd box is not usable.
	ErrInvalidTrackMatrix = errors.New("go-mp4: invalid track display matrix")

	// ErrInvalidBoxStructure is returned when the file contains a broken box
	// layout: an impossible box size, a box exceeding its parent, a truncated
	// box, a missing moov/tkhd/mdia/hdlr box, or trailing bytes.
	ErrInvalidBoxStructure = errors.New("go-mp4: invalid box structure")
)

// TrackDisplayMatrix returns the tkhd display matrix for the given clockwise
// display rotation in degrees. Only 0, 90, 180 and 270 are accepted.
//
// The width and height of the tkhd box are not part of the matrix and are
// intentionally left to the caller.
func TrackDisplayMatrix(rotation int) ([9]int32, error) {
	switch rotation {
	case Rotation0:
		return [9]int32{
			rotationMatrixOne16_16, 0, 0,
			0, rotationMatrixOne16_16, 0,
			0, 0, rotationMatrixOne2_30,
		}, nil
	case Rotation90:
		return [9]int32{
			0, rotationMatrixOne16_16, 0,
			-rotationMatrixOne16_16, 0, 0,
			0, 0, rotationMatrixOne2_30,
		}, nil
	case Rotation180:
		return [9]int32{
			-rotationMatrixOne16_16, 0, 0,
			0, -rotationMatrixOne16_16, 0,
			0, 0, rotationMatrixOne2_30,
		}, nil
	case Rotation270:
		return [9]int32{
			0, -rotationMatrixOne16_16, 0,
			rotationMatrixOne16_16, 0, 0,
			0, 0, rotationMatrixOne2_30,
		}, nil
	}
	return [9]int32{}, fmt.Errorf("%w: %d degrees (supported values are 0, 90, 180 and 270)", ErrInvalidRotation, rotation)
}

// TrackRotationFromMatrix interprets a tkhd display matrix and returns the
// clockwise display rotation in degrees (0, 90, 180 or 270).
//
// The upper-left 2x2 sub-matrix is normalized, so matrices scaled by an
// arbitrary factor are accepted. Matrices containing a flip, a shear or a
// rotation that is not a multiple of 90 degrees are rejected with
// ErrInvalidTrackMatrix.
func TrackRotationFromMatrix(matrix [9]int32) (int, error) {
	a := float64(matrix[0])
	b := float64(matrix[1])
	c := float64(matrix[3])
	d := float64(matrix[4])

	scale := math.Hypot(a, b)
	if scale == 0 {
		return 0, fmt.Errorf("%w: zero scale", ErrInvalidTrackMatrix)
	}
	a /= scale
	b /= scale
	c /= scale
	d /= scale

	// A pure rotation of the form [a b; -b a].
	const epsilon = 1e-4
	if math.Abs(a-d) > epsilon || math.Abs(b+c) > epsilon {
		return 0, fmt.Errorf("%w: matrix is not a pure rotation", ErrInvalidTrackMatrix)
	}

	angle := math.Atan2(b, a) * 180 / math.Pi
	if angle < -1e-9 {
		angle += 360
	}
	for _, candidate := range []int{Rotation0, Rotation90, Rotation180, Rotation270} {
		diff := math.Abs(angle - float64(candidate))
		if diff > 180 {
			diff = 360 - diff
		}
		if diff < 1e-2 {
			return candidate, nil
		}
	}
	return 0, fmt.Errorf("%w: rotation is %.3f degrees", ErrInvalidTrackMatrix, angle)
}

// TkhdRotation returns the clockwise display rotation of a parsed tkhd box.
func TkhdRotation(tkhd *Tkhd) (int, error) {
	if tkhd == nil {
		return 0, fmt.Errorf("%w: nil tkhd", ErrInvalidTrackMatrix)
	}
	return TrackRotationFromMatrix(tkhd.Matrix)
}

// SetTkhdRotation replaces the display matrix of a parsed tkhd box with the
// matrix for the given clockwise rotation. The track ID, duration, width,
// height and all other tkhd fields are left untouched.
func SetTkhdRotation(tkhd *Tkhd, rotation int) error {
	if tkhd == nil {
		return fmt.Errorf("%w: nil tkhd", ErrInvalidTrackMatrix)
	}
	matrix, err := TrackDisplayMatrix(rotation)
	if err != nil {
		return err
	}
	tkhd.Matrix = matrix
	return nil
}

// ReadTrackRotation returns the clockwise display rotation of the video track
// identified by trackID.
//
// Audio and other non-video tracks are rejected with ErrTrackNotVideo, a
// missing track with ErrTrackNotFound and duplicate track IDs with
// ErrDuplicateTrackID. A matrix which is not a pure 0/90/180/270 degree
// rotation is reported as ErrInvalidTrackMatrix.
func ReadTrackRotation(r io.ReadSeeker, trackID uint32) (int, error) {
	pos, seekErr := r.Seek(0, io.SeekCurrent)
	if seekErr == nil {
		defer func() {
			_, _ = r.Seek(pos, io.SeekStart)
		}()
	}

	tracks, _, err := scanRotationTracks(r)
	if err != nil {
		return 0, err
	}
	idx, err := selectVideoTrack(tracks, trackID)
	if err != nil {
		return 0, err
	}
	return TrackRotationFromMatrix(tracks[idx].matrix)
}

// RewriteTrackRotation copies the MP4 file from r to w and replaces the tkhd
// display matrix of the video track identified by trackID with the matrix for
// the given clockwise display rotation.
//
// The rewrite does not re-encode or even re-serialize the media: every box is
// copied byte-for-byte and only the 36 matrix bytes inside the selected tkhd
// box are replaced. Unknown boxes, the mdat payload, the sample tables and all
// other track properties (including the tkhd width and height) are preserved
// verbatim.
//
// Because the matrix is a fixed-size field, the rewrite never changes a box
// size. Every box keeps its original file offset, so absolute offsets such as
// stco/co64 chunk offsets, tfhd base data offsets and tfra moof offsets remain
// correct without adjustment.
//
// The input must be structurally valid. Corrupted box layouts are reported
// before anything is written where possible; ErrInvalidBoxStructure is
// returned for truncated boxes, impossible sizes, boxes exceeding their
// parent and trailing garbage.
//
// r and w must be different streams.
func RewriteTrackRotation(r io.ReadSeeker, w io.Writer, trackID uint32, rotation int) error {
	patch, err := encodeRotationMatrix(rotation)
	if err != nil {
		return err
	}

	tracks, fileSize, err := scanRotationTracks(r)
	if err != nil {
		return err
	}
	idx, err := selectVideoTrack(tracks, trackID)
	if err != nil {
		return err
	}

	start := tracks[idx].matrixOffset
	if start > fileSize || fileSize-start < uint64(len(patch)) {
		return fmt.Errorf("%w: tkhd matrix at offset %d exceeds file size %d", ErrInvalidBoxStructure, start, fileSize)
	}

	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("go-mp4: failed to seek input: %w", err)
	}
	if err := copyRotationBytes(w, r, start); err != nil {
		return fmt.Errorf("go-mp4: failed to copy data before tkhd: %w", err)
	}
	if n, err := w.Write(patch[:]); err != nil {
		return fmt.Errorf("go-mp4: failed to write tkhd matrix: %w", err)
	} else if n != len(patch) {
		return fmt.Errorf("go-mp4: failed to write tkhd matrix: %w", io.ErrShortWrite)
	}
	// Skip the original matrix bytes in the input; they were replaced above.
	if err := rotationSeek(r, start+uint64(len(patch))); err != nil {
		return err
	}
	rest := fileSize - start - uint64(len(patch))
	if err := copyRotationBytes(w, r, rest); err != nil {
		return fmt.Errorf("go-mp4: failed to copy data after tkhd: %w", err)
	}
	return nil
}

func copyRotationBytes(w io.Writer, r io.Reader, n uint64) error {
	if n == 0 {
		return nil
	}
	buf := make([]byte, 128*1024)
	written, err := io.CopyBuffer(w, io.LimitReader(r, int64(n)), buf)
	if err != nil {
		return err
	}
	if written != int64(n) {
		return fmt.Errorf("copied %d bytes, expected %d: %w", written, n, io.ErrUnexpectedEOF)
	}
	return nil
}

// SetTrackRotation edits the display matrix of the video track identified by
// trackID in place. Only the 36 matrix bytes of the tkhd box are written, so
// no box size or file offset changes and the media payload is never touched.
//
// The stream must be opened for reading and writing.
func SetTrackRotation(rw io.ReadWriteSeeker, trackID uint32, rotation int) error {
	patch, err := encodeRotationMatrix(rotation)
	if err != nil {
		return err
	}

	tracks, fileSize, err := scanRotationTracks(rw)
	if err != nil {
		return err
	}
	idx, err := selectVideoTrack(tracks, trackID)
	if err != nil {
		return err
	}

	start := tracks[idx].matrixOffset
	if start > fileSize || fileSize-start < uint64(len(patch)) {
		return fmt.Errorf("%w: tkhd matrix at offset %d exceeds file size %d", ErrInvalidBoxStructure, start, fileSize)
	}
	if _, err := rw.Seek(int64(start), io.SeekStart); err != nil {
		return fmt.Errorf("go-mp4: failed to seek tkhd matrix: %w", err)
	}
	if n, err := rw.Write(patch[:]); err != nil {
		return fmt.Errorf("go-mp4: failed to write tkhd matrix: %w", err)
	} else if n != len(patch) {
		return fmt.Errorf("go-mp4: failed to write tkhd matrix: %w", io.ErrShortWrite)
	}
	return nil
}

func encodeRotationMatrix(rotation int) ([36]byte, error) {
	var data [36]byte
	matrix, err := TrackDisplayMatrix(rotation)
	if err != nil {
		return data, err
	}
	for i, v := range matrix {
		binary.BigEndian.PutUint32(data[i*4:], uint32(v))
	}
	return data, nil
}

// rotationBoxInfo describes one physical box found by scanRotationBoxes.
type rotationBoxInfo struct {
	boxType    BoxType
	offset     uint64
	size       uint64
	headerSize uint64
}

func (b rotationBoxInfo) payloadOffset() uint64 {
	return b.offset + b.headerSize
}

func (b rotationBoxInfo) end() uint64 {
	return b.offset + b.size
}

// rotationTrackInfo is the subset of a trak box needed to locate and rewrite
// the display matrix.
type rotationTrackInfo struct {
	trackID      uint32
	handlerType  [4]byte
	matrixOffset uint64 // absolute file offset of the 36 matrix bytes
	matrix       [9]int32
}

// scanRotationTracks validates the top-level structure, the moov/trak/mdia
// hierarchy and returns every track of the movie.
func scanRotationTracks(r io.ReadSeeker) ([]rotationTrackInfo, uint64, error) {
	fileSize, err := rotationFileSize(r)
	if err != nil {
		return nil, 0, err
	}
	if fileSize == 0 {
		return nil, 0, fmt.Errorf("%w: empty file", ErrInvalidBoxStructure)
	}

	topLevel, err := scanRotationBoxes(r, 0, fileSize, true)
	if err != nil {
		return nil, 0, err
	}
	var moov *rotationBoxInfo
	for i := range topLevel {
		if topLevel[i].boxType != BoxTypeMoov() {
			continue
		}
		if moov != nil {
			return nil, 0, fmt.Errorf("%w: multiple moov boxes", ErrInvalidBoxStructure)
		}
		moov = &topLevel[i]
	}
	if moov == nil {
		return nil, 0, fmt.Errorf("%w: moov box not found", ErrInvalidBoxStructure)
	}

	moovChildren, err := scanRotationBoxes(r, moov.payloadOffset(), moov.end(), false)
	if err != nil {
		return nil, 0, err
	}
	var tracks []rotationTrackInfo
	for i := range moovChildren {
		if moovChildren[i].boxType != BoxTypeTrak() {
			continue
		}
		track, err := scanRotationTrak(r, moovChildren[i])
		if err != nil {
			return nil, 0, err
		}
		tracks = append(tracks, track)
	}

	seen := make(map[uint32]int, len(tracks))
	for i := range tracks {
		if prev, ok := seen[tracks[i].trackID]; ok {
			return nil, 0, fmt.Errorf("%w: track ID %d is used by trak #%d and #%d", ErrDuplicateTrackID, tracks[i].trackID, prev, i)
		}
		seen[tracks[i].trackID] = i
	}

	return tracks, fileSize, nil
}

func scanRotationTrak(r io.ReadSeeker, trak rotationBoxInfo) (rotationTrackInfo, error) {
	children, err := scanRotationBoxes(r, trak.payloadOffset(), trak.end(), false)
	if err != nil {
		return rotationTrackInfo{}, err
	}

	var tkhd *rotationBoxInfo
	var mdia *rotationBoxInfo
	for i := range children {
		switch children[i].boxType {
		case BoxTypeTkhd():
			if tkhd != nil {
				return rotationTrackInfo{}, fmt.Errorf("%w: multiple tkhd boxes in trak at offset %d", ErrInvalidBoxStructure, trak.offset)
			}
			tkhd = &children[i]
		case BoxTypeMdia():
			if mdia != nil {
				return rotationTrackInfo{}, fmt.Errorf("%w: multiple mdia boxes in trak at offset %d", ErrInvalidBoxStructure, trak.offset)
			}
			mdia = &children[i]
		}
	}
	if tkhd == nil {
		return rotationTrackInfo{}, fmt.Errorf("%w: trak at offset %d has no tkhd box", ErrInvalidBoxStructure, trak.offset)
	}
	if mdia == nil {
		return rotationTrackInfo{}, fmt.Errorf("%w: trak at offset %d has no mdia box", ErrInvalidBoxStructure, trak.offset)
	}

	mdiaChildren, err := scanRotationBoxes(r, mdia.payloadOffset(), mdia.end(), false)
	if err != nil {
		return rotationTrackInfo{}, err
	}
	var hdlr *rotationBoxInfo
	for i := range mdiaChildren {
		if mdiaChildren[i].boxType != BoxTypeHdlr() {
			continue
		}
		if hdlr != nil {
			return rotationTrackInfo{}, fmt.Errorf("%w: multiple hdlr boxes in mdia at offset %d", ErrInvalidBoxStructure, mdia.offset)
		}
		hdlr = &mdiaChildren[i]
	}
	if hdlr == nil {
		return rotationTrackInfo{}, fmt.Errorf("%w: trak at offset %d has no hdlr box", ErrInvalidBoxStructure, trak.offset)
	}

	info, err := readRotationTkhd(r, *tkhd)
	if err != nil {
		return rotationTrackInfo{}, err
	}
	info.handlerType, err = readRotationHandler(r, *hdlr)
	if err != nil {
		return rotationTrackInfo{}, err
	}
	return info, nil
}

func readRotationTkhd(r io.ReadSeeker, box rotationBoxInfo) (rotationTrackInfo, error) {
	payloadSize := box.size - box.headerSize
	if payloadSize < 4 {
		return rotationTrackInfo{}, fmt.Errorf("%w: tkhd at offset %d is too short: payload=%d", ErrInvalidBoxStructure, box.offset, payloadSize)
	}
	head, err := readRotationBytes(r, box.payloadOffset(), 4)
	if err != nil {
		return rotationTrackInfo{}, err
	}

	var trackIDOffset, matrixOffset uint64
	switch head[0] {
	case 0:
		if payloadSize < 84 {
			return rotationTrackInfo{}, fmt.Errorf("%w: version 0 tkhd at offset %d is truncated: payload=%d, required=84", ErrInvalidBoxStructure, box.offset, payloadSize)
		}
		trackIDOffset, matrixOffset = 12, 40
	case 1:
		if payloadSize < 96 {
			return rotationTrackInfo{}, fmt.Errorf("%w: version 1 tkhd at offset %d is truncated: payload=%d, required=96", ErrInvalidBoxStructure, box.offset, payloadSize)
		}
		trackIDOffset, matrixOffset = 20, 52
	default:
		return rotationTrackInfo{}, fmt.Errorf("%w: tkhd at offset %d has version %d (%v)", ErrInvalidBoxStructure, box.offset, head[0], ErrUnsupportedBoxVersion)
	}

	idBytes, err := readRotationBytes(r, box.payloadOffset()+trackIDOffset, 4)
	if err != nil {
		return rotationTrackInfo{}, err
	}
	matrixBytes, err := readRotationBytes(r, box.payloadOffset()+matrixOffset, 36)
	if err != nil {
		return rotationTrackInfo{}, err
	}

	info := rotationTrackInfo{
		trackID:      binary.BigEndian.Uint32(idBytes),
		matrixOffset: box.payloadOffset() + matrixOffset,
	}
	for i := range info.matrix {
		info.matrix[i] = int32(binary.BigEndian.Uint32(matrixBytes[i*4:]))
	}
	return info, nil
}

func readRotationHandler(r io.ReadSeeker, box rotationBoxInfo) ([4]byte, error) {
	if box.size-box.headerSize < 12 {
		return [4]byte{}, fmt.Errorf("%w: hdlr at offset %d is too short: payload=%d", ErrInvalidBoxStructure, box.offset, box.size-box.headerSize)
	}
	// FullBox(4 bytes) followed by pre_defined(4 bytes), then handler_type.
	data, err := readRotationBytes(r, box.payloadOffset()+8, 4)
	if err != nil {
		return [4]byte{}, err
	}
	return [4]byte{data[0], data[1], data[2], data[3]}, nil
}

func selectVideoTrack(tracks []rotationTrackInfo, trackID uint32) (int, error) {
	for i := range tracks {
		if tracks[i].trackID != trackID {
			continue
		}
		if tracks[i].handlerType != [4]byte{'v', 'i', 'd', 'e'} {
			return 0, fmt.Errorf("%w: track ID %d has handler type %q", ErrTrackNotVideo, trackID, string(tracks[i].handlerType[:]))
		}
		return i, nil
	}
	return 0, fmt.Errorf("%w: track ID %d", ErrTrackNotFound, trackID)
}

// scanRotationBoxes reads a sequence of boxes in [start, limit) and validates
// their common headers. Only boxes at the top level may use size 0 (extend to
// EOF); inside a container the size must be explicit.
func scanRotationBoxes(r io.ReadSeeker, start, limit uint64, allowExtendToEOF bool) ([]rotationBoxInfo, error) {
	if start > limit {
		return nil, fmt.Errorf("%w: invalid scan range [%d, %d)", ErrInvalidBoxStructure, start, limit)
	}
	boxes := make([]rotationBoxInfo, 0, 16)
	pos := start
	var header [LargeHeaderSize]byte
	for pos < limit {
		remaining := limit - pos
		if remaining < SmallHeaderSize {
			return nil, fmt.Errorf("%w: %d trailing bytes at offset %d", ErrInvalidBoxStructure, remaining, pos)
		}
		if err := rotationSeek(r, pos); err != nil {
			return nil, err
		}
		if _, err := io.ReadFull(r, header[:SmallHeaderSize]); err != nil {
			return nil, fmt.Errorf("%w: failed to read box header at offset %d: %v", ErrInvalidBoxStructure, pos, err)
		}
		size := uint64(binary.BigEndian.Uint32(header[0:4]))
		boxType := BoxType{header[4], header[5], header[6], header[7]}
		headerSize := uint64(SmallHeaderSize)
		switch {
		case size == 0:
			if !allowExtendToEOF {
				return nil, fmt.Errorf("%w: box %s at offset %d uses size 0 inside a container", ErrInvalidBoxStructure, boxType, pos)
			}
			size = remaining
		case size == 1:
			if remaining < LargeHeaderSize {
				return nil, fmt.Errorf("%w: truncated large box header at offset %d", ErrInvalidBoxStructure, pos)
			}
			if _, err := io.ReadFull(r, header[SmallHeaderSize:LargeHeaderSize]); err != nil {
				return nil, fmt.Errorf("%w: failed to read extended box size at offset %d: %v", ErrInvalidBoxStructure, pos, err)
			}
			size = binary.BigEndian.Uint64(header[SmallHeaderSize:LargeHeaderSize])
			headerSize = uint64(LargeHeaderSize)
		}
		if size < headerSize {
			return nil, fmt.Errorf("%w: box %s at offset %d has size %d smaller than its header (%d bytes)", ErrInvalidBoxStructure, boxType, pos, size, headerSize)
		}
		if size > remaining {
			return nil, fmt.Errorf("%w: box %s at offset %d has size %d exceeding the remaining %d bytes", ErrInvalidBoxStructure, boxType, pos, size, remaining)
		}
		boxes = append(boxes, rotationBoxInfo{
			boxType:    boxType,
			offset:     pos,
			size:       size,
			headerSize: headerSize,
		})
		pos += size
	}
	return boxes, nil
}

func readRotationBytes(r io.ReadSeeker, offset uint64, size int) ([]byte, error) {
	if err := rotationSeek(r, offset); err != nil {
		return nil, err
	}
	buf := make([]byte, size)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, fmt.Errorf("%w: failed to read %d bytes at offset %d: %v", ErrInvalidBoxStructure, size, offset, err)
	}
	return buf, nil
}

func rotationSeek(r io.Seeker, offset uint64) error {
	if offset > math.MaxInt64 {
		return fmt.Errorf("%w: offset %d is out of range", ErrInvalidBoxStructure, offset)
	}
	if _, err := r.Seek(int64(offset), io.SeekStart); err != nil {
		return fmt.Errorf("%w: failed to seek to offset %d: %v", ErrInvalidBoxStructure, offset, err)
	}
	return nil
}

func rotationFileSize(r io.ReadSeeker) (uint64, error) {
	pos, err := r.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, fmt.Errorf("go-mp4: failed to get current offset: %w", err)
	}
	end, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, fmt.Errorf("go-mp4: failed to get file size: %w", err)
	}
	if _, err := r.Seek(pos, io.SeekStart); err != nil {
		return 0, fmt.Errorf("go-mp4: failed to restore offset: %w", err)
	}
	if end < 0 {
		return 0, fmt.Errorf("%w: negative file size", ErrInvalidBoxStructure)
	}
	return uint64(end), nil
}
