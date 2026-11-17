package mp4

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChplMarshalUnmarshal(t *testing.T) {
	src := &Chpl{
		Version: 1,
		Entries: []ChplEntry{
			{StartTime: 0, Title: "Chapter One"},
			{StartTime: 20_000_000, Title: "Chapter Two"}, // 2.0 s in 100ns units
		},
	}
	bin := []byte{
		0x01,             // version
		0x00, 0x00, 0x00, // flags
		0x00, 0x00, 0x00, 0x00, // unknown
		0x02,                                           // chapter count
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // start time 0
		0x0b, // title length 11
		'C', 'h', 'a', 'p', 't', 'e', 'r', ' ', 'O', 'n', 'e',
		0x00, 0x00, 0x00, 0x00, 0x01, 0x31, 0x2d, 0x00, // start time 20000000
		0x0b, // title length 11
		'C', 'h', 'a', 'p', 't', 'e', 'r', ' ', 'T', 'w', 'o',
	}

	// Marshal
	buf := bytes.NewBuffer(nil)
	n, err := Marshal(buf, src, Context{})
	require.NoError(t, err)
	assert.Equal(t, uint64(len(bin)), n)
	assert.Equal(t, bin, buf.Bytes())

	// Unmarshal
	dst := &Chpl{}
	r := bytes.NewReader(bin)
	n, err = Unmarshal(r, uint64(len(bin)), dst, Context{})
	require.NoError(t, err)
	assert.Equal(t, uint64(len(bin)), n)
	assert.Equal(t, src, dst)
}

func TestChplMarshalUTF8(t *testing.T) {
	title := "チャプター" // multi-byte UTF-8
	src := &Chpl{
		Version: 1,
		Entries: []ChplEntry{
			{StartTime: 1_000_000, Title: title}, // 0.1 s
		},
	}
	buf := bytes.NewBuffer(nil)
	_, err := Marshal(buf, src, Context{})
	require.NoError(t, err)

	dst := &Chpl{}
	_, err = Unmarshal(bytes.NewReader(buf.Bytes()), uint64(buf.Len()), dst, Context{})
	require.NoError(t, err)
	require.Len(t, dst.Entries, 1)
	assert.Equal(t, uint64(1_000_000), dst.Entries[0].StartTime)
	assert.Equal(t, title, dst.Entries[0].Title)
	// the length prefix counts bytes, not runes:
	// version(1)+flags(3)+unknown(4)+count(1)+start-time(8) then the length byte
	assert.Equal(t, len(title), int(buf.Bytes()[1+3+4+1+8]))
}

func TestChplUnmarshalErrors(t *testing.T) {
	t.Run("truncated count", func(t *testing.T) {
		// version+flags+unknown only, no count byte
		bin := []byte{0x01, 0, 0, 0, 0, 0, 0, 0}
		dst := &Chpl{}
		_, err := Unmarshal(bytes.NewReader(bin), uint64(len(bin)), dst, Context{})
		require.Error(t, err)
	})

	t.Run("truncated entry", func(t *testing.T) {
		bin := []byte{
			0x01, 0, 0, 0,
			0x00, 0x00, 0x00, 0x00,
			0x01,                               // one chapter
			0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // only 6 of 8 time bytes
		}
		dst := &Chpl{}
		_, err := Unmarshal(bytes.NewReader(bin), uint64(len(bin)), dst, Context{})
		require.Error(t, err)
	})

	t.Run("title length exceeds box", func(t *testing.T) {
		bin := []byte{
			0x01, 0, 0, 0,
			0x00, 0x00, 0x00, 0x00,
			0x01,
			0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01,
			0x05, 'a', 'b', // claims 5 bytes but only 2 follow
		}
		dst := &Chpl{}
		_, err := Unmarshal(bytes.NewReader(bin), uint64(len(bin)), dst, Context{})
		require.Error(t, err)
	})

	t.Run("invalid UTF-8", func(t *testing.T) {
		bin := []byte{
			0x01, 0, 0, 0,
			0x00, 0x00, 0x00, 0x00,
			0x01,
			0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01,
			0x02, 0xff, 0xfe,
		}
		dst := &Chpl{}
		_, err := Unmarshal(bytes.NewReader(bin), uint64(len(bin)), dst, Context{})
		require.Error(t, err)
	})
}

func TestChplMarshalErrors(t *testing.T) {
	t.Run("too many chapters", func(t *testing.T) {
		chpl := &Chpl{Entries: make([]ChplEntry, 256)}
		_, err := Marshal(bytes.NewBuffer(nil), chpl, Context{})
		require.Error(t, err)
	})

	t.Run("title too long", func(t *testing.T) {
		chpl := &Chpl{Entries: []ChplEntry{{Title: string(make([]byte, 256))}}}
		_, err := Marshal(bytes.NewBuffer(nil), chpl, Context{})
		require.Error(t, err)
	})

	t.Run("maximum length title is accepted", func(t *testing.T) {
		title := string(make([]byte, MaxChapterTitleLen))
		chpl := &Chpl{Entries: []ChplEntry{{Title: title}}}
		_, err := Marshal(bytes.NewBuffer(nil), chpl, Context{})
		require.NoError(t, err)
	})
}
