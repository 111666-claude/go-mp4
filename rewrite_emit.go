package mp4

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
)

// computeLayout assigns output sizes bottom-up and offsets top-down. stco
// widening flags are decided in validateOffsets and simply reflected here.
func computeLayout(nodes []*rewriteNode, outOffset uint64) error {
	// Pass A: compute and persist every node's size/header size bottom-up.
	if err := computeSizes(nodes); err != nil {
		return err
	}
	// Pass B: assign absolute offsets top-down.
	return assignOffsets(nodes, outOffset)
}

// computeSizes walks the tree depth-first so child sizes are finalized before
// their parents, then writes the resulting size back onto each node.
func computeSizes(nodes []*rewriteNode) error {
	for _, node := range nodes {
		size, headerSize, err := nodeOutputSize(node)
		if err != nil {
			return err
		}
		node.outSize = size
		node.outHeaderSize = headerSize
	}
	return nil
}

// nodeOutputSize computes the emitted box size and header size. It relies on
// the children's already-computed outSize from the prior recursion level.
func nodeOutputSize(node *rewriteNode) (size, headerSize uint64, err error) {
	if node.replacement != nil {
		return uint64(len(node.replacement)), replacementHeaderSize(node), nil
	}

	headerSize = node.info.HeaderSize

	switch {
	case node.info.Type == BoxTypeStco() && node.widen:
		// stco -> co64: version/flags/count stay 8 bytes, entries go 64bit.
		// The count is derived from the original payload, independent of the
		// original 32/64bit box header size.
		oldEntries := (node.info.Size - node.info.HeaderSize - 8) / 4
		size = headerSize + 8 + oldEntries*8
	case node.leaf:
		// mdat and opaque boxes keep their exact payload size.
		size = node.info.Size
	default:
		// Children are finalized first, so their outSize values are authoritative.
		if err := computeSizes(node.children); err != nil {
			return 0, 0, err
		}
		var childrenSize uint64
		for _, child := range node.children {
			childrenSize += child.outSize
		}
		size = headerSize + node.prefixSize + childrenSize
	}

	// tfra payload width never changes; assert it still serializes.
	if node.tfra != nil {
		if _, err := marshalBox(node.tfra, node.info.Context); err != nil {
			return 0, 0, err
		}
	}

	if !node.info.ExtendToEOF && size > math.MaxUint32 && headerSize == SmallHeaderSize {
		headerSize = LargeHeaderSize
		size += LargeHeaderSize - SmallHeaderSize
	}
	return size, headerSize, nil
}

func assignOffsets(nodes []*rewriteNode, offset uint64) error {
	for _, node := range nodes {
		node.outOffset = offset
		offset += node.outSize

		if node.replacement == nil && !node.leaf {
			childOffset := node.outOffset + node.outHeaderSize + node.prefixSize
			if err := assignOffsets(node.children, childOffset); err != nil {
				return err
			}
		}
	}
	return nil
}

func replacementHeaderSize(node *rewriteNode) uint64 {
	if binary.BigEndian.Uint32(node.replacement) == 1 {
		return LargeHeaderSize
	}
	return SmallHeaderSize
}

// encodeRewriteHeader serializes a box header with the exact planned header
// size: a size-zero mdat keeps the implicit-EOF form, a 16 byte large header is
// forced when the size does not fit in 32 bits, otherwise an 8 byte header.
func encodeRewriteHeader(headerSize, size uint64, typ BoxType, extendToEOF bool) []byte {
	if extendToEOF {
		data := make([]byte, SmallHeaderSize)
		copy(data[4:], typ[:])
		return data
	}
	if headerSize == LargeHeaderSize {
		data := make([]byte, LargeHeaderSize)
		binary.BigEndian.PutUint32(data, 1)
		binary.BigEndian.PutUint64(data[SmallHeaderSize:], size)
		copy(data[4:], typ[:])
		return data
	}
	data := make([]byte, SmallHeaderSize)
	binary.BigEndian.PutUint32(data, uint32(size))
	copy(data[4:], typ[:])
	return data
}

// validateOffsets proves that every absolute file-offset structure stays valid
// and representable after the rewrite, marking stco boxes that must be widened
// to co64. It returns changed=true when a new widening was decided, meaning the
// layout has to be recomputed. Offset tables are streamed, never fully buffered.
func (st *rewriteState) validateOffsets() (changed bool, err error) {
	mediaRanges := st.collectMdatRanges()
	boxRanges := st.collectAllRanges()

	var walk func([]*rewriteNode) error
	walk = func(ns []*rewriteNode) error {
		for _, node := range ns {
			if node.replacement != nil {
				continue
			}
			switch node.info.Type {
			case BoxTypeStco():
				widen, err := st.checkStco(node, mediaRanges)
				if err != nil {
					return err
				}
				if widen && !node.widen {
					node.widen = true
					changed = true
				}
			case BoxTypeCo64():
				if err := st.checkCo64(node, mediaRanges); err != nil {
					return err
				}
			case BoxTypeTfra():
				if err := checkTfra(node, boxRanges); err != nil {
					return err
				}
			case BoxTypeTfhd():
				if err := st.checkTfhd(node, boxRanges); err != nil {
					return err
				}
			case BoxTypeSaio():
				if err := st.checkSaio(node); err != nil {
					return err
				}
			}
			if err := walk(node.children); err != nil {
				return err
			}
		}
		return nil
	}
	return changed, walk(st.root)
}

func (st *rewriteState) checkStco(node *rewriteNode, mdats []mdatRegion) (widen bool, err error) {
	count, err := st.entryCount(node, 4)
	if err != nil {
		return false, err
	}
	if _, err := st.r.Seek(int64(node.info.Offset+node.info.HeaderSize+8), io.SeekStart); err != nil {
		return false, err
	}
	buf := make([]byte, 4)
	for i := uint32(0); i < count; i++ {
		if _, err := io.ReadFull(st.r, buf); err != nil {
			return false, err
		}
		shifted, err := shiftMediaOffset(uint64(binary.BigEndian.Uint32(buf)), mdats)
		if err != nil {
			return false, fmt.Errorf("stco[%d]: %w", i, err)
		}
		if !fitsUint32(shifted) {
			widen = true
		}
	}
	return widen, nil
}

func (st *rewriteState) checkCo64(node *rewriteNode, mdats []mdatRegion) error {
	count, err := st.entryCount(node, 8)
	if err != nil {
		return err
	}
	if _, err := st.r.Seek(int64(node.info.Offset+node.info.HeaderSize+8), io.SeekStart); err != nil {
		return err
	}
	buf := make([]byte, 8)
	for i := uint32(0); i < count; i++ {
		if _, err := io.ReadFull(st.r, buf); err != nil {
			return err
		}
		if _, err := shiftMediaOffset(binary.BigEndian.Uint64(buf), mdats); err != nil {
			return fmt.Errorf("co64[%d]: %w", i, err)
		}
	}
	return nil
}

func checkTfra(node *rewriteNode, boxRanges []mdatRegion) error {
	tfra := node.tfra
	for i, entry := range tfra.Entries {
		var offset uint64
		if tfra.GetVersion() == 0 {
			offset = uint64(entry.MoofOffsetV0)
		} else {
			offset = entry.MoofOffsetV1
		}
		shifted, err := shiftBoxOffset(offset, boxRanges)
		if err != nil {
			return fmt.Errorf("tfra[%d]: %w", i, err)
		}
		if tfra.GetVersion() == 0 && !fitsUint32(shifted) {
			return fmt.Errorf("%w: tfra version 0 moof offset %d overflows 32 bits after shift",
				ErrUnsafeOffset, offset)
		}
	}
	return nil
}

func (st *rewriteState) checkTfhd(node *rewriteNode, boxRanges []mdatRegion) error {
	tfhd, err := st.readTfhd(node)
	if err != nil {
		return err
	}
	if !tfhd.CheckFlag(TfhdBaseDataOffsetPresent) {
		return nil
	}
	if _, err := shiftBoxOffset(tfhd.BaseDataOffset, boxRanges); err != nil {
		return fmt.Errorf("tfhd: %w", err)
	}
	return nil
}

func (st *rewriteState) readTfhd(node *rewriteNode) (*Tfhd, error) {
	if _, err := st.r.Seek(int64(node.info.Offset+node.info.HeaderSize), io.SeekStart); err != nil {
		return nil, err
	}
	var tfhd Tfhd
	if _, err := Unmarshal(st.r, node.info.Size-node.info.HeaderSize, &tfhd, node.info.Context); err != nil {
		return nil, fmt.Errorf("%w: cannot parse tfhd: %v", ErrInvalidStructure, err)
	}
	return &tfhd, nil
}

// checkSaio enforces the one saio layout the rewriter cannot reason about:
// absolute auxiliary-information offsets stored outside a moof. saio boxes
// inside moof use offsets relative to the enclosing moof, and a moof is always
// emitted as an unchanged-size subtree, so those values stay valid. An saio
// directly under moov carries file-absolute offsets that would dangle after a
// metadata edit; instead of emitting a corrupt file, reject it explicitly.
func (st *rewriteState) checkSaio(node *rewriteNode) error {
	if node.path != nil {
		for _, t := range node.path {
			if t == BoxTypeMoof() {
				// moof-relative offsets remain valid because the whole moof
				// subtree is emitted without changing its internal layout.
				return nil
			}
		}
	}
	if _, err := st.r.Seek(int64(node.info.Offset+node.info.HeaderSize), io.SeekStart); err != nil {
		return err
	}
	var saio Saio
	if _, err := Unmarshal(st.r, node.info.Size-node.info.HeaderSize, &saio, node.info.Context); err != nil {
		return fmt.Errorf("%w: cannot parse saio: %v", ErrInvalidStructure, err)
	}
	if saio.EntryCount > 0 {
		return fmt.Errorf("%w: absolute saio offsets outside moof cannot be rewritten", ErrUnsafeOffset)
	}
	return nil
}

func marshalBox(box IImmutableBox, ctx Context) ([]byte, error) {
	buf := &bytes.Buffer{}
	if _, err := Marshal(buf, box, ctx); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// emit streams the planned tree into w with a bounded scratch buffer. Media
// payloads are copied through the buffer and never materialized in memory.
func (st *rewriteState) emit(w io.Writer, bufferSize int) error {
	buf := make([]byte, bufferSize)
	for _, node := range st.root {
		if err := st.emitNode(w, node, buf); err != nil {
			return err
		}
	}
	return nil
}

func (st *rewriteState) emitNode(w io.Writer, node *rewriteNode, buf []byte) error {
	// 1. header, regenerated from the planned size and (possibly widened) type.
	header := encodeRewriteHeader(node.outHeaderSize, node.outSize, node.outputType(), node.info.ExtendToEOF)
	if uint64(len(header)) != node.outHeaderSize {
		return fmt.Errorf("%w: internal header size mismatch on %s", ErrInvalidStructure, node.info.Type.String())
	}
	if _, err := w.Write(header); err != nil {
		return err
	}

	// 2. payload.
	switch {
	case node.replacement != nil:
		_, err := w.Write(node.replacement[node.outHeaderSize:])
		return err

	case node.info.Type == BoxTypeStco():
		return st.emitStco(w, node, buf)

	case node.info.Type == BoxTypeCo64():
		return st.emitCo64(w, node, buf)

	case node.tfra != nil:
		return st.emitTfra(w, node)

	case node.info.Type == BoxTypeTfhd():
		return st.emitTfhd(w, node)

	case node.leaf:
		// mdat or opaque box: stream the original payload verbatim.
		return st.streamBoxBytes(w, node, node.info.Offset+node.info.HeaderSize,
			node.info.Size-node.info.HeaderSize, buf)

	default:
		// container: preserve prefix bytes, then recurse into children.
		if err := st.streamBoxBytes(w, node, node.info.Offset+node.info.HeaderSize,
			node.prefixSize, buf); err != nil {
			return err
		}
		for _, child := range node.children {
			if err := st.emitNode(w, child, buf); err != nil {
				return err
			}
		}
		return nil
	}
}

func (node *rewriteNode) outputType() BoxType {
	if node.info.Type == BoxTypeStco() && node.widen {
		return BoxTypeCo64()
	}
	return node.info.Type
}

// streamBoxBytes copies n bytes from the input at srcOffset through the bounded
// scratch buffer.
func (st *rewriteState) streamBoxBytes(w io.Writer, node *rewriteNode, srcOffset, n uint64, buf []byte) error {
	if _, err := st.r.Seek(int64(srcOffset), io.SeekStart); err != nil {
		return err
	}
	written, err := io.CopyBuffer(w, io.LimitReader(st.r, int64(n)), buf)
	if err != nil {
		return err
	}
	if uint64(written) != n {
		return io.ErrUnexpectedEOF
	}
	return nil
}

// emitFixedHeader copies the 8 byte version/flags + entry-count prefix of an
// stco/co64 box verbatim.
func (st *rewriteState) emitFixedHeader(w io.Writer, node *rewriteNode, buf []byte) error {
	return st.streamBoxBytes(w, node, node.info.Offset+node.info.HeaderSize, 8, buf)
}

func (st *rewriteState) emitStco(w io.Writer, node *rewriteNode, buf []byte) error {
	mdats := st.collectMdatRanges()
	count, err := st.entryCount(node, 4)
	if err != nil {
		return err
	}
	if err := st.emitFixedHeader(w, node, buf); err != nil {
		return err
	}
	if _, err := st.r.Seek(int64(node.info.Offset+node.info.HeaderSize+8), io.SeekStart); err != nil {
		return err
	}
	in := make([]byte, 4)
	for i := uint32(0); i < count; i++ {
		if _, err := io.ReadFull(st.r, in); err != nil {
			return err
		}
		shifted, err := shiftMediaOffset(uint64(binary.BigEndian.Uint32(in)), mdats)
		if err != nil {
			return fmt.Errorf("stco[%d]: %w", i, err)
		}
		if node.widen {
			out := make([]byte, 8)
			binary.BigEndian.PutUint64(out, shifted)
			if _, err := w.Write(out); err != nil {
				return err
			}
		} else {
			binary.BigEndian.PutUint32(in, uint32(shifted))
			if _, err := w.Write(in); err != nil {
				return err
			}
		}
	}
	return nil
}

func (st *rewriteState) emitCo64(w io.Writer, node *rewriteNode, buf []byte) error {
	mdats := st.collectMdatRanges()
	count, err := st.entryCount(node, 8)
	if err != nil {
		return err
	}
	if err := st.emitFixedHeader(w, node, buf); err != nil {
		return err
	}
	if _, err := st.r.Seek(int64(node.info.Offset+node.info.HeaderSize+8), io.SeekStart); err != nil {
		return err
	}
	in := make([]byte, 8)
	for i := uint32(0); i < count; i++ {
		if _, err := io.ReadFull(st.r, in); err != nil {
			return err
		}
		shifted, err := shiftMediaOffset(binary.BigEndian.Uint64(in), mdats)
		if err != nil {
			return fmt.Errorf("co64[%d]: %w", i, err)
		}
		binary.BigEndian.PutUint64(in, shifted)
		if _, err := w.Write(in); err != nil {
			return err
		}
	}
	return nil
}

func (st *rewriteState) emitTfra(w io.Writer, node *rewriteNode) error {
	boxRanges := st.collectAllRanges()
	tfra := node.tfra
	for i := range tfra.Entries {
		if tfra.GetVersion() == 0 {
			shifted, err := shiftBoxOffset(uint64(tfra.Entries[i].MoofOffsetV0), boxRanges)
			if err != nil {
				return fmt.Errorf("tfra[%d]: %w", i, err)
			}
			tfra.Entries[i].MoofOffsetV0 = uint32(shifted)
		} else {
			shifted, err := shiftBoxOffset(tfra.Entries[i].MoofOffsetV1, boxRanges)
			if err != nil {
				return fmt.Errorf("tfra[%d]: %w", i, err)
			}
			tfra.Entries[i].MoofOffsetV1 = shifted
		}
	}
	data, err := marshalBox(tfra, node.info.Context)
	if err != nil {
		return err
	}
	if uint64(len(data)) != node.info.Size-node.info.HeaderSize {
		return fmt.Errorf("%w: tfra payload size changed during rewrite", ErrInvalidStructure)
	}
	_, err = w.Write(data)
	return err
}

func (st *rewriteState) emitTfhd(w io.Writer, node *rewriteNode) error {
	tfhd, err := st.readTfhd(node)
	if err != nil {
		return err
	}
	if tfhd.CheckFlag(TfhdBaseDataOffsetPresent) {
		boxRanges := st.collectAllRanges()
		shifted, err := shiftBoxOffset(tfhd.BaseDataOffset, boxRanges)
		if err != nil {
			return fmt.Errorf("tfhd: %w", err)
		}
		tfhd.BaseDataOffset = shifted
	}
	data, err := marshalBox(tfhd, node.info.Context)
	if err != nil {
		return err
	}
	if uint64(len(data)) != node.info.Size-node.info.HeaderSize {
		return fmt.Errorf("%w: tfhd payload size changed during rewrite", ErrInvalidStructure)
	}
	_, err = w.Write(data)
	return err
}
