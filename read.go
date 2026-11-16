package mp4

import (
	"errors"
	"fmt"
	"io"
)

func checkPayloadSize(r io.ReadSeeker, bi *BoxInfo) error {
	fileSize, err := r.Seek(0, io.SeekEnd)
	if _, seekErr := r.Seek(int64(bi.Offset+bi.HeaderSize), io.SeekStart); seekErr != nil {
		return seekErr
	}
	if err != nil {
		return nil
	}
	if bi.Offset+bi.Size > uint64(fileSize) {
		return fmt.Errorf("box size exceeds file size: offset=%d, size=%d, fileSize=%d", bi.Offset, bi.Size, fileSize)
	}
	return nil
}

type BoxPath []BoxType

// String renders the box path as slash-separated box types for diagnostics.
func (p BoxPath) String() string {
	if len(p) == 0 {
		return ""
	}
	b := make([]byte, 0, len(p)*5)
	for i, t := range p {
		if i != 0 {
			b = append(b, '/')
		}
		b = append(b, t[0], t[1], t[2], t[3])
	}
	return string(b)
}

func (lhs BoxPath) compareWith(rhs BoxPath) (forwardMatch bool, match bool) {
	if len(lhs) > len(rhs) {
		return false, false
	}
	for i := 0; i < len(lhs); i++ {
		if !lhs[i].MatchWith(rhs[i]) {
			return false, false
		}
	}
	if len(lhs) < len(rhs) {
		return true, false
	}
	return false, true
}

type ReadHandle struct {
	Params      []interface{}
	BoxInfo     BoxInfo
	Path        BoxPath
	ReadPayload func() (box IBox, n uint64, err error)
	ReadData    func(io.Writer) (n uint64, err error)
	Expand      func(params ...interface{}) (vals []interface{}, err error)
}

type ReadHandler func(handle *ReadHandle) (val interface{}, err error)

// hdrValidationState tracks the static HDR metadata boxes (mdcv, clli)
// encountered while traversing a box tree, so that their placement
// (directly inside a video sample entry) and uniqueness can be validated.
// It is intentionally kept out of Context and BoxInfo because it is purely
// internal traversal state and must not affect the externally visible
// values returned to callers.
type hdrValidationState struct {
	// seen maps the file offset of each video sample entry to the set of HDR
	// box types already found directly inside that entry. The offset uniquely
	// identifies a sample entry, so equally typed entries in different tracks
	// are tracked independently.
	seen map[uint64]map[BoxType]struct{}
}

func newHDRValidationState() *hdrValidationState {
	return &hdrValidationState{seen: make(map[uint64]map[BoxType]struct{})}
}

// validate enforces the hierarchy and uniqueness rules for the mdcv and
// clli boxes. parentPath is the path of the box that directly contains
// boxType and parentOffset is that parent box's file offset. It is a no-op
// for every other box type.
func (s *hdrValidationState) validate(boxType BoxType, parentPath BoxPath, parentOffset uint64, ctx Context) error {
	if boxType != BoxTypeMdcv() && boxType != BoxTypeClli() {
		return nil
	}
	if len(parentPath) == 0 || !isVisualSampleEntryType(parentPath[len(parentPath)-1], ctx) {
		return fmt.Errorf("%w: type=%s path=%s", ErrInvalidHDRBoxPlacement,
			boxType.String(), append(append(BoxPath{}, parentPath...), boxType).String())
	}
	set := s.seen[parentOffset]
	if set == nil {
		set = make(map[BoxType]struct{})
		s.seen[parentOffset] = set
	}
	if _, duplicated := set[boxType]; duplicated {
		return fmt.Errorf("%w: type=%s path=%s", ErrDuplicateHDRBox,
			boxType.String(), append(append(BoxPath{}, parentPath...), boxType).String())
	}
	set[boxType] = struct{}{}
	return nil
}

func ReadBoxStructure(r io.ReadSeeker, handler ReadHandler, params ...interface{}) ([]interface{}, error) {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return readBoxStructure(r, 0, true, nil, 0, Context{}, handler, params, newHDRValidationState())
}

func ReadBoxStructureFromInternal(r io.ReadSeeker, bi *BoxInfo, handler ReadHandler, params ...interface{}) (interface{}, error) {
	return readBoxStructureFromInternal(r, bi, nil, 0, handler, params, newHDRValidationState())
}

func readBoxStructureFromInternal(r io.ReadSeeker, bi *BoxInfo, path BoxPath, parentOffset uint64, handler ReadHandler, params []interface{}, state *hdrValidationState) (interface{}, error) {
	if _, err := bi.SeekToPayload(r); err != nil {
		return nil, err
	}

	// check comatible-brands
	if len(path) == 0 && bi.Type == BoxTypeFtyp() {
		var ftyp Ftyp
		if err := checkPayloadSize(r, bi); err != nil {
			return nil, err
		}
		if _, err := Unmarshal(r, bi.Size-bi.HeaderSize, &ftyp, bi.Context); err != nil {
			return nil, err
		}
		if ftyp.HasCompatibleBrand(BrandQT()) {
			bi.IsQuickTimeCompatible = true
		}
		if _, err := bi.SeekToPayload(r); err != nil {
			return nil, err
		}
	}

	// parse numbered ilst items after keys box by saving EntryCount field to context
	if bi.Type == BoxTypeKeys() {
		var keys Keys
		if err := checkPayloadSize(r, bi); err != nil {
			return nil, err
		}
		if _, err := Unmarshal(r, bi.Size-bi.HeaderSize, &keys, bi.Context); err != nil {
			return nil, err
		}
		bi.QuickTimeKeysMetaEntryCount = int(keys.EntryCount)
		if _, err := bi.SeekToPayload(r); err != nil {
			return nil, err
		}
	}

	ctx := bi.Context
	if bi.Type == BoxTypeWave() {
		ctx.UnderWave = true
	} else if bi.Type == BoxTypeIlst() {
		ctx.UnderIlst = true
	} else if bi.UnderIlst && !bi.UnderIlstMeta && IsIlstMetaBoxType(bi.Type) {
		ctx.UnderIlstMeta = true
		if bi.Type == StrToBoxType("----") {
			ctx.UnderIlstFreeMeta = true
		}
	} else if bi.Type == BoxTypeUdta() {
		ctx.UnderUdta = true
	}

	newPath := make(BoxPath, len(path)+1)
	copy(newPath, path)
	newPath[len(path)] = bi.Type

	// Validate the placement and uniqueness of the static HDR metadata
	// boxes (mdcv, clli), which must appear at most once and only directly
	// inside a video sample entry (VisualSampleEntry).
	if err := state.validate(bi.Type, path, parentOffset, ctx); err != nil {
		return nil, err
	}

	h := &ReadHandle{
		Params:  params,
		BoxInfo: *bi,
		Path:    newPath,
	}

	var childrenOffset uint64

	h.ReadPayload = func() (IBox, uint64, error) {
		if _, err := bi.SeekToPayload(r); err != nil {
			return nil, 0, err
		}

		if err := checkPayloadSize(r, bi); err != nil {
			return nil, 0, err
		}
		box, n, err := UnmarshalAny(r, bi.Type, bi.Size-bi.HeaderSize, bi.Context)
		if err != nil {
			return nil, 0, err
		}
		childrenOffset = bi.Offset + bi.HeaderSize + n
		return box, n, nil
	}

	h.ReadData = func(w io.Writer) (uint64, error) {
		if _, err := bi.SeekToPayload(r); err != nil {
			return 0, err
		}

		size := bi.Size - bi.HeaderSize
		if _, err := io.CopyN(w, r, int64(size)); err != nil {
			return 0, err
		}
		return size, nil
	}

	h.Expand = func(params ...interface{}) ([]interface{}, error) {
		if childrenOffset == 0 {
			if _, err := bi.SeekToPayload(r); err != nil {
				return nil, err
			}

			if err := checkPayloadSize(r, bi); err != nil {
				return nil, err
			}
			_, n, err := UnmarshalAny(r, bi.Type, bi.Size-bi.HeaderSize, bi.Context)
			if err != nil {
				return nil, err
			}
			childrenOffset = bi.Offset + bi.HeaderSize + n
		} else {
			if _, err := r.Seek(int64(childrenOffset), io.SeekStart); err != nil {
				return nil, err
			}
		}

		childrenSize := bi.Offset + bi.Size - childrenOffset
		return readBoxStructure(r, childrenSize, false, newPath, bi.Offset, ctx, handler, params, state)
	}

	if val, err := handler(h); err != nil {
		return nil, err
	} else if _, err := bi.SeekToEnd(r); err != nil {
		return nil, err
	} else {
		return val, nil
	}
}

func readBoxStructure(r io.ReadSeeker, totalSize uint64, isRoot bool, path BoxPath, parentOffset uint64, ctx Context, handler ReadHandler, params []interface{}, state *hdrValidationState) ([]interface{}, error) {
	vals := make([]interface{}, 0, 8)

	for isRoot || totalSize >= SmallHeaderSize {
		bi, err := ReadBoxInfo(r)
		if isRoot && err == io.EOF {
			return vals, nil
		} else if err != nil {
			return nil, err
		}

		if !isRoot && bi.Size > totalSize {
			return nil, fmt.Errorf("too large box size: type=%s, size=%d, actualBufSize=%d", bi.Type.String(), bi.Size, totalSize)
		}
		totalSize -= bi.Size

		bi.Context = ctx

		val, err := readBoxStructureFromInternal(r, bi, path, parentOffset, handler, params, state)
		if err != nil {
			return nil, err
		}
		vals = append(vals, val)

		if bi.IsQuickTimeCompatible {
			ctx.IsQuickTimeCompatible = true
		}

		// preserve keys entry count on context for subsequent ilst number item box
		if bi.Type == BoxTypeKeys() {
			ctx.QuickTimeKeysMetaEntryCount = bi.QuickTimeKeysMetaEntryCount
		}
	}

	if totalSize != 0 && !ctx.IsQuickTimeCompatible {
		return nil, errors.New("unexpected EOF")
	}

	return vals, nil
}
