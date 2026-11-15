package mp4

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
)

// Errors returned by RewriteFile. They are wrapped with contextual messages,
// therefore callers should use errors.Is rather than direct comparison.
var (
	// ErrInvalidStructure means that the input file has a broken box layout
	// that cannot be parsed safely.
	ErrInvalidStructure = errors.New("invalid mp4 box structure")

	// ErrUnsafeOffset means that the file contains an absolute file-offset
	// structure which the rewriter cannot update safely (for example a 32bit
	// tfra entry that would overflow), or an offset value pointing outside of
	// any known media data (mdat) box.
	ErrUnsafeOffset = errors.New("unsafe or unupdatable offset structure")

	// ErrReplacementInvalid means that the bytes produced by the rewrite
	// handler do not form a valid single box.
	ErrReplacementInvalid = errors.New("invalid replacement box")
)

// DefaultRewriteBufferSize is the buffer size used while streaming payloads
// such as mdat, when RewriteFile is called without an explicit option.
const DefaultRewriteBufferSize = 1 * 1024 * 1024 // 1MiB

// RewriteHandle is passed to RewriteHandler for every box encountered while
// scanning the input file.
type RewriteHandle struct {
	// BoxInfo is the location and the size of the current box in the input.
	BoxInfo BoxInfo

	// Path is the absolute box path from the file root.
	Path BoxPath

	// Writer accumulates a replacement box when the handler wants to modify
	// the current box. The handler must write a complete box including its
	// header (typically via NewWriter/StartBox or WriteBoxInfo plus Marshal).
	//
	// If the handler writes nothing, the original box bytes are preserved
	// byte-for-byte. Writing anything replaces the whole box (including all of
	// its descendants) with the written bytes, so the handler is responsible
	// for re-emitting every child it wants to keep.
	Writer io.Writer
}

// RewriteHandler is invoked once for every box in the input file.
// See RewriteHandle for the replacement semantics.
type RewriteHandler func(handle *RewriteHandle) error

// RewriteOption customizes the behavior of RewriteFile.
type RewriteOption func(*rewriteOptions)

type rewriteOptions struct {
	bufferSize int
}

// WithRewriteBufferSize overrides the size of the scratch buffer used while
// streaming large payloads (mdat). The rewriter never loads such payloads into
// memory regardless of this value; the option only bounds the per-read buffer.
func WithRewriteBufferSize(size int) RewriteOption {
	return func(o *rewriteOptions) {
		if size > 0 {
			o.bufferSize = size
		}
	}
}

// rewriteNode is the in-memory representation of one input box built during
// the planning pass. It only stores structural metadata and small fixed-size
// payloads; media payloads are never buffered.
type rewriteNode struct {
	info     BoxInfo
	children []*rewriteNode

	// prefixSize is the number of payload bytes that are not child boxes
	// (FullBox header, fixed fields, ...). Zero for leaf/opaque boxes.
	prefixSize uint64
	leaf       bool

	// path is the absolute box path from the file root, retained for
	// context-sensitive validation such as saio placement.
	path BoxPath

	// replacement, when non-nil, is a complete serialized box that must be
	// emitted instead of the original bytes.
	replacement []byte

	// tfra is retained because its absolute moof offsets have to be shifted.
	// tfra tables are bounded by the number of fragments, not the media size.
	tfra *Tfra

	// widen marks an stco box that has to be emitted as a co64.
	widen bool

	// Output layout, filled by the layout pass.
	outOffset     uint64
	outSize       uint64
	outHeaderSize uint64
}

// rewriteState carries the shared input reader and planned tree through the
// validation and emission passes.
type rewriteState struct {
	r    io.ReadSeeker
	root []*rewriteNode
}

// RewriteFile rewrites the MP4 file exposed by r into w, invoking handler for
// every box. It is designed for metadata-only edits such as replacing udta or
// meta boxes: media data (mdat payloads) is streamed byte-for-byte without
// re-encoding, and memory usage stays bounded regardless of the file size.
//
// The rewriter keeps every nested box size consistent with the emitted bytes,
// and updates the absolute file-offset structures that move when a box grows
// or shrinks:
//
//   - stco and co64 chunk offset tables are shifted by the movement of the mdat
//     boxes they reference. An stco is converted transparently into a co64 when
//     a shifted offset would not fit into 32 bits.
//   - tfhd BaseDataOffset values and tfra moof offsets are shifted the same way.
//
// Structures that cannot be updated safely (a 32bit tfra entry that overflows,
// an offset pointing outside of every mdat, a malformed input, a size-zero box
// appearing before the end of the file, ...) cause RewriteFile to return an
// error before any output byte is produced, so a failure never leaves a
// partially written file behind.
//
// The output must not be the same stream as the input.
func RewriteFile(r io.ReadSeeker, w io.Writer, handler RewriteHandler, opts ...RewriteOption) error {
	options := &rewriteOptions{bufferSize: DefaultRewriteBufferSize}
	for _, opt := range opts {
		opt(options)
	}

	fileSize, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return err
	}

	// Pass 1: scan the whole tree, let the handler produce replacement boxes,
	// and validate everything that can be validated without writing.
	roots, err := scanRewriteNodes(r, 0, uint64(fileSize), true, nil, Context{}, handler)
	if err != nil {
		return err
	}
	if err := validateRewritePlan(roots); err != nil {
		return err
	}

	state := &rewriteState{r: r, root: roots}

	// Layout and offset validation are coupled: stco widening changes sizes,
	// which moves mdat and changes every shifted offset. Recompute until the
	// widening flags are stable (they are monotonic, so this converges).
	for {
		if err := computeLayout(roots, 0); err != nil {
			return err
		}
		changed, err := state.validateOffsets()
		if err != nil {
			return err
		}
		if !changed {
			break
		}
	}

	// Pass 2: stream the output. All validation already succeeded, so the only
	// possible failures here are I/O errors, which cannot be recovered: best
	// effort truncate the destination and report the error.
	if err := state.emit(w, options.bufferSize); err != nil {
		if trunc, ok := w.(interface{ Truncate(int64) error }); ok {
			_ = trunc.Truncate(0)
		}
		return err
	}
	return nil
}

// readRegionBoxInfo reads one box header. Unlike ReadBoxInfo, a size field of
// zero is resolved against regionEnd (the end of the enclosing region) instead
// of the physical file end, so nested size-zero boxes are handled correctly.
func readRegionBoxInfo(r io.ReadSeeker, regionEnd uint64) (*BoxInfo, error) {
	offset, err := r.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, err
	}
	hdr := make([]byte, SmallHeaderSize)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return nil, err
	}
	bi := &BoxInfo{
		Offset:     uint64(offset),
		HeaderSize: SmallHeaderSize,
		Size:       uint64(binary.BigEndian.Uint32(hdr)),
		Type:       BoxType{hdr[4], hdr[5], hdr[6], hdr[7]},
	}
	if bi.Size == 1 {
		ext := make([]byte, 8)
		if _, err := io.ReadFull(r, ext); err != nil {
			return nil, err
		}
		bi.HeaderSize = LargeHeaderSize
		bi.Size = binary.BigEndian.Uint64(ext)
	} else if bi.Size == 0 {
		bi.Size = regionEnd - bi.Offset
		bi.ExtendToEOF = true
	}
	if bi.Size < SmallHeaderSize {
		return nil, fmt.Errorf("invalid box size %d for %s", bi.Size, bi.Type.String())
	}
	return bi, nil
}

// scanRewriteNodes reads box headers in [start, start+regionSize) and builds
// the rewrite node tree. It mirrors the context bookkeeping performed by
// ReadBoxStructure so that payload unmarshalling sees the same Context.
func scanRewriteNodes(r io.ReadSeeker, start, regionSize uint64, isRoot bool, path BoxPath, ctx Context, handler RewriteHandler) ([]*rewriteNode, error) {
	if _, err := r.Seek(int64(start), io.SeekStart); err != nil {
		return nil, err
	}

	nodes := make([]*rewriteNode, 0, 8)
	cursor := start
	regionEnd := start + regionSize
	for isRoot || cursor < regionEnd {
		if !isRoot && regionEnd-cursor < SmallHeaderSize {
			return nil, fmt.Errorf("%w: %d trailing byte(s) inside container", ErrInvalidStructure, regionEnd-cursor)
		}

		bi, err := readRegionBoxInfo(r, regionEnd)
		if err != nil {
			if isRoot && errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("%w: %v", ErrInvalidStructure, err)
		}

		if !isRoot && bi.Offset+bi.Size > regionEnd {
			return nil, fmt.Errorf("%w: box %s exceeds its container", ErrInvalidStructure, bi.Type.String())
		}

		if bi.ExtendToEOF {
			// A size-zero box extends to the end of its enclosing region. It
			// may only be the final box, and only an mdat may keep the
			// size-zero form through the rewrite (its size is recomputed).
			if bi.Offset+bi.Size != regionEnd {
				return nil, fmt.Errorf("%w: size-zero %s box is not the last box", ErrInvalidStructure, bi.Type.String())
			}
			if !(isRoot && bi.Type == BoxTypeMdat()) {
				return nil, fmt.Errorf("%w: size-zero %s box cannot be rewritten", ErrUnsafeOffset, bi.Type.String())
			}
		}

		node := &rewriteNode{info: *bi}

		childCtx := ctx
		switch bi.Type {
		case BoxTypeFtyp():
			var ftyp Ftyp
			if _, err := bi.SeekToPayload(r); err != nil {
				return nil, err
			}
			if _, err := Unmarshal(r, bi.Size-bi.HeaderSize, &ftyp, ctx); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrInvalidStructure, err)
			}
			if ftyp.HasCompatibleBrand(BrandQT()) {
				childCtx.IsQuickTimeCompatible = true
			}
		case BoxTypeKeys():
			var keys Keys
			if _, err := bi.SeekToPayload(r); err != nil {
				return nil, err
			}
			if _, err := Unmarshal(r, bi.Size-bi.HeaderSize, &keys, ctx); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrInvalidStructure, err)
			}
			childCtx.QuickTimeKeysMetaEntryCount = int(keys.EntryCount)
		case BoxTypeWave():
			childCtx.UnderWave = true
		case BoxTypeIlst():
			childCtx.UnderIlst = true
		case BoxTypeUdta():
			childCtx.UnderUdta = true
		}
		if childCtx.UnderIlst && !childCtx.UnderIlstMeta && IsIlstMetaBoxType(bi.Type) {
			childCtx.UnderIlstMeta = true
			if bi.Type == StrToBoxType("----") {
				childCtx.UnderIlstFreeMeta = true
			}
		}

		newPath := append(append(make(BoxPath, 0, len(path)+1), path...), bi.Type)
		node.info.Context = childCtx
		node.path = newPath

		// Invoke the handler and capture any replacement box.
		buf := &bytes.Buffer{}
		h := &RewriteHandle{
			BoxInfo: *bi,
			Path:    newPath,
			Writer:  buf,
		}
		if err := handler(h); err != nil {
			return nil, err
		}
		if buf.Len() > 0 {
			if err := setReplacement(node, buf.Bytes()); err != nil {
				return nil, err
			}
		}

		if bi.Type == BoxTypeMdat() {
			if node.replacement != nil {
				return nil, fmt.Errorf("%w: mdat boxes cannot be replaced", ErrReplacementInvalid)
			}
			node.leaf = true
		}

		// Parse tfra eagerly so that absolute offsets can be shifted later.
		if bi.Type == BoxTypeTfra() && node.replacement == nil {
			if _, err := bi.SeekToPayload(r); err != nil {
				return nil, err
			}
			var tfra Tfra
			if _, err := Unmarshal(r, bi.Size-bi.HeaderSize, &tfra, childCtx); err != nil {
				return nil, fmt.Errorf("%w: cannot parse tfra: %v", ErrInvalidStructure, err)
			}
			node.tfra = &tfra
		}

		// Descend only into whitelisted container boxes; every other box is an
		// opaque leaf whose bytes are preserved verbatim. This runs before any
		// nested scan so leaf payloads (e.g. stco tables) are never interpreted
		// as child boxes.
		if node.replacement == nil && !node.leaf && isContainerBoxType(bi.Type) {
			prefix, leaf, err := childRegion(r, bi, childCtx)
			if err != nil {
				return nil, err
			}
			if leaf {
				node.leaf = true
			} else {
				childStart := bi.Offset + bi.HeaderSize + prefix
				childSize := bi.Size - bi.HeaderSize - prefix
				children, err := scanRewriteNodes(r, childStart, childSize, false, newPath, childCtx, handler)
				if err != nil {
					// The declared container does not actually hold a clean
					// sequence of child boxes (e.g. a udta carrying raw data).
					// Preserve the whole box byte-for-byte instead of failing.
					if errors.Is(err, ErrInvalidStructure) {
						node.leaf = true
						node.prefixSize = 0
						node.children = nil
					} else {
						return nil, err
					}
				} else {
					node.prefixSize = prefix
					node.children = children
				}
			}
		} else if node.replacement == nil && !node.leaf {
			node.leaf = true
		}

		nodes = append(nodes, node)
		cursor = bi.Offset + bi.Size
		// All the helpers above (payload unmarshal, child probing) move the
		// shared reader; reposition at the next box header unconditionally.
		if _, err := r.Seek(int64(cursor), io.SeekStart); err != nil {
			return nil, err
		}
	}

	if !isRoot && cursor != regionEnd {
		return nil, fmt.Errorf("%w: container ended %d byte(s) early", ErrInvalidStructure, regionEnd-cursor)
	}
	if isRoot && cursor != regionEnd {
		return nil, fmt.Errorf("%w: %d trailing byte(s) after last top level box", ErrInvalidStructure, regionEnd-cursor)
	}
	return nodes, nil
}

// containerBoxTypes is the whitelist of box types whose payload is a sequence
// of child boxes. Every other type is treated as an opaque leaf and copied
// byte-for-byte, which is the only safe choice for unknown or leaf boxes (it
// also avoids interpreting raw sample tables such as stco as nested boxes).
var containerBoxTypes = map[BoxType]bool{
	BoxTypeMoov(): true, BoxTypeTrak(): true, BoxTypeMdia(): true,
	BoxTypeMinf(): true, BoxTypeStbl(): true, BoxTypeEdts(): true,
	BoxTypeDinf(): true, BoxTypeDref(): true, BoxTypeUdta(): true,
	BoxTypeMeta(): true, BoxTypeIlst(): true, BoxTypeMvex(): true,
	BoxTypeMoof(): true, BoxTypeTraf(): true, BoxTypeMfra(): true,
	BoxTypeStsd(): true, BoxTypeWave(): true, BoxTypeSchi(): true,
	BoxTypeSinf(): true, BoxTypeEmsg(): true,
}

func isContainerBoxType(t BoxType) bool {
	return containerBoxTypes[t]
}

// childRegion determines where child boxes begin inside bi. Whitelisted
// containers are parsed to learn the size of their fixed prefix; anything not
// on the whitelist is an opaque leaf and never recursed into.
func childRegion(r io.ReadSeeker, bi *BoxInfo, ctx Context) (prefixSize uint64, leaf bool, err error) {
	if !isContainerBoxType(bi.Type) {
		return 0, true, nil
	}

	if _, err := bi.SeekToPayload(r); err != nil {
		return 0, false, err
	}
	if bi.IsSupportedType() {
		box, _, err := UnmarshalAny(r, bi.Type, bi.Size-bi.HeaderSize, ctx)
		if err != nil {
			return 0, false, fmt.Errorf("%w: cannot parse container %s: %v", ErrInvalidStructure, bi.Type.String(), err)
		}
		// A whitelisted container embeds either a plain Box (no fixed prefix)
		// or a FullBox whose 4 byte version/flags precede its child boxes. We
		// never guess from the remaining bytes: unknown payload under a known
		// container is simply traversed as child boxes starting at the prefix.
		var n uint64
		if embedsFullBox(box) {
			n = 4
		}
		payloadSize := bi.Size - bi.HeaderSize
		if n > payloadSize {
			return 0, false, fmt.Errorf("%w: container %s fixed prefix overruns payload", ErrInvalidStructure, bi.Type.String())
		}
		if n == payloadSize {
			// Declared container with no child bytes: treat as opaque leaf.
			return 0, true, nil
		}
		return n, false, nil
	}
	// An unsupported type on the container whitelist: assume no fixed prefix
	// and verify the remainder parses as a clean sequence of boxes.
	return tryChildRegion(r, bi)
}

// fullBoxType is the reflect type of the embedded FullBox marker struct.
var fullBoxType = reflect.TypeOf(FullBox{})

// embedsFullBox reports whether box is a pointer to a struct that embeds
// FullBox (directly), meaning its payload starts with a 4 byte version/flags
// prefix before any child boxes.
func embedsFullBox(box IBox) bool {
	v := reflect.ValueOf(box)
	if v.Kind() != reflect.Ptr {
		return false
	}
	v = v.Elem()
	if v.Kind() != reflect.Struct {
		return false
	}
	for i := 0; i < v.NumField(); i++ {
		if v.Type().Field(i).Type == fullBoxType {
			return true
		}
	}
	return false
}

// tryChildRegion assumes no prefix and verifies that the payload parses as a
// sequence of boxes filling it exactly. It returns leaf=true when the payload
// cannot be interpreted that way, so the caller preserves it opaquely.
func tryChildRegion(r io.ReadSeeker, bi *BoxInfo) (uint64, bool, error) {
	payloadSize := bi.Size - bi.HeaderSize
	if payloadSize == 0 {
		return 0, true, nil
	}
	if _, err := bi.SeekToPayload(r); err != nil {
		return 0, false, err
	}
	remaining := payloadSize
	for remaining >= SmallHeaderSize {
		child, err := ReadBoxInfo(r)
		if err != nil {
			return 0, true, nil
		}
		if child.Size < SmallHeaderSize || child.Offset+child.Size > bi.Offset+bi.Size {
			return 0, true, nil
		}
		if _, err := r.Seek(int64(child.Offset+child.Size), io.SeekStart); err != nil {
			return 0, false, err
		}
		remaining -= child.Size
	}
	if remaining != 0 {
		return 0, true, nil
	}
	return 0, false, nil
}

// setReplacement validates that data is exactly one complete box matching the
// original type and records it on the node.
func setReplacement(node *rewriteNode, data []byte) error {
	sr := bytes.NewReader(data)
	bi, err := ReadBoxInfo(sr)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrReplacementInvalid, err)
	}
	if bi.Type != node.info.Type {
		return fmt.Errorf("%w: replacement type %s does not match original %s",
			ErrReplacementInvalid, bi.Type.String(), node.info.Type.String())
	}
	if bi.Offset != 0 || bi.Size != uint64(len(data)) {
		return fmt.Errorf("%w: replacement %s must be a single complete box",
			ErrReplacementInvalid, bi.Type.String())
	}
	node.replacement = data
	return nil
}

// validateRewritePlan enforces structural rules that are independent of the
// final layout.
func validateRewritePlan(nodes []*rewriteNode) error {
	for _, node := range nodes {
		if node.replacement != nil {
			// Replaced subtrees are opaque by definition.
			continue
		}
		if node.info.Type == BoxTypeTfra() && node.tfra == nil {
			return fmt.Errorf("%w: unreadable tfra box", ErrUnsafeOffset)
		}
		if err := validateRewritePlan(node.children); err != nil {
			return err
		}
	}
	return nil
}

// mdatRegion describes an input byte range and its displacement in the output.
type mdatRegion struct {
	start, end uint64 // input byte range, end exclusive
	delta      int64
}

// collectMdatRanges returns the payload ranges of every mdat box and the
// displacement of each payload in the output. Chunk offsets (stco/co64) are
// defined to point into media payloads, so they are resolved strictly against
// these ranges.
func (st *rewriteState) collectMdatRanges() []mdatRegion {
	var out []mdatRegion
	var walk func([]*rewriteNode)
	walk = func(ns []*rewriteNode) {
		for _, n := range ns {
			if n.info.Type == BoxTypeMdat() {
				out = append(out, mdatRegion{
					start: n.info.Offset + n.info.HeaderSize,
					end:   n.info.Offset + n.info.Size,
					delta: int64(n.outOffset) - int64(n.info.Offset),
				})
			}
			if n.replacement == nil {
				walk(n.children)
			}
		}
	}
	walk(st.root)
	return out
}

// collectAllRanges returns, for every non-replaced output box, its input byte
// range and displacement. tfra/tfhd offsets point at whole boxes (moof), so
// they resolve against this generic table.
func (st *rewriteState) collectAllRanges() []mdatRegion {
	var out []mdatRegion
	var walk func([]*rewriteNode)
	walk = func(ns []*rewriteNode) {
		for _, n := range ns {
			if n.replacement == nil {
				out = append(out, mdatRegion{
					start: n.info.Offset,
					end:   n.info.Offset + n.info.Size,
					delta: int64(n.outOffset) - int64(n.info.Offset),
				})
				walk(n.children)
			}
		}
	}
	walk(st.root)
	return out
}

// shiftMediaOffset resolves a chunk offset against mdat payload ranges.
//
// When the file contains no mdat at all (e.g. a pure-moov recording template),
// chunk offsets cannot reference media data and metadata edits cannot move any,
// so the value is returned unchanged rather than rejected. As soon as at least
// one mdat exists, every chunk offset is required to land inside one of them
// and is shifted strictly; an out-of-range value is then an explicit error.
func shiftMediaOffset(offset uint64, mdats []mdatRegion) (uint64, error) {
	if len(mdats) == 0 {
		return offset, nil
	}
	for _, m := range mdats {
		if offset >= m.start && offset < m.end {
			shifted := int64(offset) + m.delta
			if shifted < 0 {
				return 0, fmt.Errorf("%w: offset %d shifts to a negative position", ErrUnsafeOffset, offset)
			}
			return uint64(shifted), nil
		}
	}
	return 0, fmt.Errorf("%w: chunk offset %d is outside of any mdat box", ErrUnsafeOffset, offset)
}

// shiftBoxOffset resolves an offset that points at a whole box (tfra/tfhd).
// When no range contains it, the displacement is zero: pure-moov recordings
// have no media data to move, so the stored value must be preserved verbatim.
func shiftBoxOffset(offset uint64, regions []mdatRegion) (uint64, error) {
	// Tightest containing range wins so an offset resolves to the actual box it
	// points at rather than an ancestor.
	best := -1
	for i, m := range regions {
		if offset >= m.start && offset < m.end {
			if best == -1 || (m.end-m.start) < (regions[best].end-regions[best].start) {
				best = i
			}
		}
	}
	if best == -1 {
		return offset, nil
	}
	shifted := int64(offset) + regions[best].delta
	if shifted < 0 {
		return 0, fmt.Errorf("%w: offset %d shifts to a negative position", ErrUnsafeOffset, offset)
	}
	return uint64(shifted), nil
}

// readSmall reads n bytes from the input box at the given box-relative offset.
// It is only used for the tiny fixed headers of offset-bearing boxes.
func (st *rewriteState) readSmall(node *rewriteNode, boxRelative uint64, n int) ([]byte, error) {
	if _, err := st.r.Seek(int64(node.info.Offset+boxRelative), io.SeekStart); err != nil {
		return nil, err
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(st.r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// entryCount reads and validates the entry count of an stco (entryWidth 4) or
// co64 (entryWidth 8) box.
func (st *rewriteState) entryCount(node *rewriteNode, entryWidth uint64) (uint32, error) {
	payloadSize := node.info.Size - node.info.HeaderSize
	if payloadSize < 8 {
		return 0, fmt.Errorf("%w: truncated %s box", ErrInvalidStructure, node.info.Type.String())
	}
	buf, err := st.readSmall(node, node.info.HeaderSize+4, 4)
	if err != nil {
		return 0, err
	}
	count := binary.BigEndian.Uint32(buf)
	if 8+uint64(count)*entryWidth != payloadSize {
		return 0, fmt.Errorf("%w: %s entry count %d is inconsistent with box size",
			ErrInvalidStructure, node.info.Type.String(), count)
	}
	return count, nil
}

// fitsUint32 reports whether v is representable in 32 bits.
func fitsUint32(v uint64) bool { return v <= math.MaxUint32 }
