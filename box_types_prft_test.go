package mp4

import (
	"bytes"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrftBox(t *testing.T) {
	testCases := []struct {
		name string
		src  IImmutableBox
		dst  IBox
		bin  []byte
		str  string
	}{
		{
			name: "prft: version 0",
			src: &Prft{
				FullBox: FullBox{
					Version: 0,
					Flags:   [3]byte{0x00, 0x00, 0x00},
				},
				ReferenceTrackID: 0x01020304,
				NtpTimestamp:     0x1122334455667788,
				MediaTimeV0:      0x89abcdef,
			},
			dst: &Prft{},
			bin: []byte{
				0,                // version
				0x00, 0x00, 0x00, // flags
				0x01, 0x02, 0x03, 0x04, // reference track ID
				0x11, 0x22, 0x33, 0x44, // NTP timestamp (seconds)
				0x55, 0x66, 0x77, 0x88, // NTP timestamp (fraction)
				0x89, 0xab, 0xcd, 0xef, // media time (32-bit)
			},
			str: `Version=0 Flags=0x000000 ReferenceTrackID=16909060 NtpTimestamp=1234605616436508552 MediaTimeV0=2309737967`,
		},
		{
			name: "prft: version 1",
			src: &Prft{
				FullBox: FullBox{
					Version: 1,
					Flags:   [3]byte{0x00, 0x00, 0x01},
				},
				ReferenceTrackID: 0x05060708,
				NtpTimestamp:     0x0102030405060708,
				MediaTimeV1:      0x89abcdef01234567,
			},
			dst: &Prft{},
			bin: []byte{
				1,                // version
				0x00, 0x00, 0x01, // flags
				0x05, 0x06, 0x07, 0x08, // reference track ID
				0x01, 0x02, 0x03, 0x04, // NTP timestamp (seconds)
				0x05, 0x06, 0x07, 0x08, // NTP timestamp (fraction)
				0x89, 0xab, 0xcd, 0xef, // media time (64-bit, high)
				0x01, 0x23, 0x45, 0x67, // media time (64-bit, low)
			},
			str: `Version=1 Flags=0x000001 ReferenceTrackID=84281096 NtpTimestamp=72623859790382856 MediaTimeV1=9920249030613615975`,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Marshal: exact on-wire bytes
			buf := bytes.NewBuffer(nil)
			n, err := Marshal(buf, tc.src, Context{})
			require.NoError(t, err)
			assert.Equal(t, uint64(len(tc.bin)), n)
			assert.Equal(t, tc.bin, buf.Bytes())

			// Unmarshal: full round trip
			r := bytes.NewReader(tc.bin)
			n, err = Unmarshal(r, uint64(len(tc.bin)), tc.dst, Context{})
			require.NoError(t, err)
			assert.Equal(t, uint64(buf.Len()), n)
			assert.Equal(t, tc.src, tc.dst)
			s, err := r.Seek(0, io.SeekCurrent)
			require.NoError(t, err)
			assert.Equal(t, int64(buf.Len()), s)

			// UnmarshalAny
			dst, n, err := UnmarshalAny(bytes.NewReader(tc.bin), tc.src.GetType(), uint64(len(tc.bin)), Context{})
			require.NoError(t, err)
			assert.Equal(t, uint64(buf.Len()), n)
			assert.Equal(t, tc.src, dst)

			// Stringify
			str, err := Stringify(tc.src, Context{})
			require.NoError(t, err)
			assert.Equal(t, tc.str, str)
		})
	}
}

func TestPrftGetMediaTime(t *testing.T) {
	prft := &Prft{
		MediaTimeV0: 0x89abcdef,
		MediaTimeV1: 0x0123456789abcdef,
	}
	prft.SetVersion(0)
	assert.Equal(t, uint64(0x89abcdef), prft.GetMediaTime())
	prft.SetVersion(1)
	assert.Equal(t, uint64(0x0123456789abcdef), prft.GetMediaTime())
}
