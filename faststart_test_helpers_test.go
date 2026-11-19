package mp4

import (
	"bytes"
	"encoding/binary"
)

// mp4Builder is a minimal in-memory MP4 box builder used by the FastStart
// tests. It emits small (8-byte header) boxes only.
type mp4Builder struct {
	buf bytes.Buffer
}

func (b *mp4Builder) box(typ string, payload []byte) {
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[:4], uint32(8+len(payload)))
	copy(hdr[4:8], typ)
	b.buf.Write(hdr[:])
	b.buf.Write(payload)
}

func (b *mp4Builder) raw(p []byte) {
	b.buf.Write(p)
}

func (b *mp4Builder) bytes() []byte {
	return b.buf.Bytes()
}

// fullBoxPrefix builds the 4-byte version/flags prefix of a FullBox.
func fullBoxPrefix(version uint8, flags uint32) []byte {
	return []byte{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}
}

// stcoBox builds an stco box with the given chunk offsets.
func stcoBox(offsets []uint32) []byte {
	var p bytes.Buffer
	p.Write(fullBoxPrefix(0, 0))
	var cnt [4]byte
	binary.BigEndian.PutUint32(cnt[:], uint32(len(offsets)))
	p.Write(cnt[:])
	for _, o := range offsets {
		var v [4]byte
		binary.BigEndian.PutUint32(v[:], o)
		p.Write(v[:])
	}
	var out bytes.Buffer
	writeBox(&out, "stco", p.Bytes())
	return out.Bytes()
}

// co64Box builds a co64 box with the given chunk offsets.
func co64Box(offsets []uint64) []byte {
	var p bytes.Buffer
	p.Write(fullBoxPrefix(0, 0))
	var cnt [4]byte
	binary.BigEndian.PutUint32(cnt[:], uint32(len(offsets)))
	p.Write(cnt[:])
	for _, o := range offsets {
		var v [8]byte
		binary.BigEndian.PutUint64(v[:], o)
		p.Write(v[:])
	}
	var out bytes.Buffer
	writeBox(&out, "co64", p.Bytes())
	return out.Bytes()
}

func writeBox(buf *bytes.Buffer, typ string, payload []byte) {
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[:4], uint32(8+len(payload)))
	copy(hdr[4:8], typ)
	buf.Write(hdr[:])
	buf.Write(payload)
}

// boxAt returns the (offset, size, payload) of the first box with the given
// type found at the top level of data. It understands both 8-byte and
// 16-byte (largesize) box headers.
func boxAt(data []byte, typ string) (offset, size int, payload []byte) {
	for i := 0; i+8 <= len(data); {
		s32 := binary.BigEndian.Uint32(data[i : i+4])
		t := string(data[i+4 : i+8])
		headerSize := 8
		var s int
		if s32 == 1 {
			if i+16 > len(data) {
				return -1, 0, nil
			}
			s = int(binary.BigEndian.Uint64(data[i+8 : i+16]))
			headerSize = 16
		} else {
			s = int(s32)
		}
		if t == typ {
			return i, s, data[i+headerSize : i+s]
		}
		i += s
	}
	return -1, 0, nil
}

// findBoxes returns the offsets of every box of the given type anywhere in
// data, found by a naive type-tag scan (sufficient for the synthetic files).
func findBoxes(data []byte, typ string) []int {
	var out []int
	tag := []byte(typ)
	for i := 0; i+8 <= len(data); i++ {
		if bytes.Equal(data[i+4:i+8], tag) {
			out = append(out, i)
		}
	}
	return out
}

// parseStco parses an stco box payload (without its 8-byte header).
func parseStco(payload []byte) []uint32 {
	cnt := binary.BigEndian.Uint32(payload[4:8])
	out := make([]uint32, cnt)
	for i := range out {
		out[i] = binary.BigEndian.Uint32(payload[8+i*4 : 12+i*4])
	}
	return out
}

// parseCo64 parses a co64 box payload (without its 8-byte header).
func parseCo64(payload []byte) []uint64 {
	cnt := binary.BigEndian.Uint32(payload[4:8])
	out := make([]uint64, cnt)
	for i := range out {
		out[i] = binary.BigEndian.Uint64(payload[8+i*8 : 16+i*8])
	}
	return out
}

// buildTwoTrackMP4 builds a non-fragmented MP4 with two tracks, each with its
// own stco box, plus an unknown top-level box and unknown moov boxes. The
// layout is ftyp, free(unknown), mdat, moov, udta(unknown).
func buildTwoTrackMP4() []byte {
	const mdatPayloadSize = 200

	var b mp4Builder
	// ftyp
	b.box("ftyp", []byte("isom\x00\x00\x02\x00isom"))

	// unknown top-level box before mdat
	b.box("free", []byte{0xaa, 0xbb})

	// mdat (payload starts at offset mdatHeaderEnd)
	mdatOffset := b.buf.Len()
	mdatHeaderEnd := mdatOffset + 8
	b.box("mdat", bytes.Repeat([]byte{0xcd}, mdatPayloadSize))

	// Build two minimal traks. Each stbl contains an stco whose offsets point
	// inside the mdat payload.
	trak := func(offsets []uint32, extra string) []byte {
		var stbl mp4Builder
		stbl.raw(stcoBox(offsets))
		stbl.box(extra, []byte{1, 2, 3, 4}) // unknown box inside stbl
		var minf mp4Builder
		minf.box("stbl", stbl.bytes())
		var mdia mp4Builder
		mdia.box("minf", minf.bytes())
		var trakB mp4Builder
		trakB.box("mdia", mdia.bytes())
		return trakB.bytes()
	}

	off1 := []uint32{
		uint32(mdatHeaderEnd),
		uint32(mdatHeaderEnd + 50),
		uint32(mdatHeaderEnd + 120),
	}
	off2 := []uint32{
		uint32(mdatHeaderEnd + 10),
		uint32(mdatHeaderEnd + 90),
	}

	var moov mp4Builder
	moov.box("mvhd", fullBoxPrefix(0, 0))
	moov.box("trak", trak(off1, "zzzz"))
	moov.box("trak", trak(off2, "yyyy"))
	// unknown box directly inside moov (opaque leaf with a box-aligned payload)
	var unknownLeaf bytes.Buffer
	writeBox(&unknownLeaf, "unkn", []byte{9, 8, 7, 6})
	moov.raw(unknownLeaf.Bytes())
	moovBoxBytes := moov.bytes()

	// wrap into a moov box at top level
	var top mp4Builder
	top.raw(b.bytes())
	writeBox(&top.buf, "moov", moovBoxBytes)
	// unknown trailing top-level box
	top.box("wide", []byte{0, 0, 0, 0})
	return top.bytes()
}
