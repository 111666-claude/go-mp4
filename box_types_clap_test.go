package mp4

import (
	"bytes"
	"io"
	"math/big"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCleanApertureBoxMarshalUnmarshal(t *testing.T) {
	src := &CleanApertureBox{
		AnyTypeBox:           AnyTypeBox{Type: StrToBoxType("clap")},
		CleanApertureWidthN:  700,
		CleanApertureWidthD:  1,
		CleanApertureHeightN: 576,
		CleanApertureHeightD: 1,
		HorizOffN:            10,
		HorizOffD:            4, // non-integer offset: 2.5
		VertOffN:             0,
		VertOffD:             1,
	}
	bin := []byte{
		0x00, 0x00, 0x02, 0xbc, // cleanApertureWidthN = 700
		0x00, 0x00, 0x00, 0x01, // cleanApertureWidthD
		0x00, 0x00, 0x02, 0x40, // cleanApertureHeightN = 576
		0x00, 0x00, 0x00, 0x01, // cleanApertureHeightD
		0x00, 0x00, 0x00, 0x0a, // horizOffN = 10
		0x00, 0x00, 0x00, 0x04, // horizOffD = 4
		0x00, 0x00, 0x00, 0x00, // vertOffN
		0x00, 0x00, 0x00, 0x01, // vertOffD
	}

	// Marshal
	buf := bytes.NewBuffer(nil)
	n, err := Marshal(buf, src, Context{})
	require.NoError(t, err)
	assert.Equal(t, uint64(len(bin)), n)
	assert.Equal(t, bin, buf.Bytes())

	// Unmarshal
	dst := &CleanApertureBox{AnyTypeBox: AnyTypeBox{Type: StrToBoxType("clap")}}
	r := bytes.NewReader(bin)
	n, err = Unmarshal(r, uint64(len(bin)), dst, Context{})
	require.NoError(t, err)
	assert.Equal(t, uint64(len(bin)), n)
	assert.Equal(t, src, dst)
	s, err := r.Seek(0, io.SeekCurrent)
	require.NoError(t, err)
	assert.Equal(t, int64(len(bin)), s)

	// UnmarshalAny
	box, n, err := UnmarshalAny(bytes.NewReader(bin), BoxTypeClap(), uint64(len(bin)), Context{})
	require.NoError(t, err)
	assert.Equal(t, uint64(len(bin)), n)
	assert.Equal(t, src, box)

	// Stringify
	str, err := Stringify(src, Context{})
	require.NoError(t, err)
	assert.Equal(t,
		`CleanApertureWidthN=700 CleanApertureWidthD=1 `+
			`CleanApertureHeightN=576 CleanApertureHeightD=1 `+
			`HorizOffN=10 HorizOffD=4 VertOffN=0 VertOffD=1`,
		str)

	// Rational getters, including the non-integer horizontal offset
	assert.Equal(t, big.NewRat(700, 1), src.Width())
	assert.Equal(t, big.NewRat(576, 1), src.Height())
	assert.Equal(t, big.NewRat(10, 4), src.HorizOffset())
	assert.Equal(t, 0, big.NewRat(10, 4).Cmp(big.NewRat(5, 2)))
	assert.Equal(t, big.NewRat(0, 1), src.VertOffset())
}

func TestCleanApertureBoxRationalsAreExact(t *testing.T) {
	// A non-integer offset like 1/3 must not lose precision.
	clap := &CleanApertureBox{
		AnyTypeBox:           AnyTypeBox{Type: StrToBoxType("clap")},
		CleanApertureWidthN:  10,
		CleanApertureWidthD:  3,
		CleanApertureHeightN: 20,
		CleanApertureHeightD: 3,
		HorizOffN:            1,
		HorizOffD:            3,
		VertOffN:             2,
		VertOffD:             3,
	}
	assert.Equal(t, "10/3", clap.Width().RatString())
	assert.Equal(t, "20/3", clap.Height().RatString())
	assert.Equal(t, "1/3", clap.HorizOffset().RatString())
	assert.Equal(t, "2/3", clap.VertOffset().RatString())
}

func TestCleanApertureBoxZeroDenominator(t *testing.T) {
	zeroDenomFields := []struct {
		name string
		clap *CleanApertureBox
	}{
		{
			name: "width denominator",
			clap: &CleanApertureBox{
				AnyTypeBox:           AnyTypeBox{Type: StrToBoxType("clap")},
				CleanApertureWidthN:  100,
				CleanApertureWidthD:  0,
				CleanApertureHeightN: 100,
				CleanApertureHeightD: 1,
				HorizOffN:            0, HorizOffD: 1,
				VertOffN: 0, VertOffD: 1,
			},
		},
		{
			name: "height denominator",
			clap: &CleanApertureBox{
				AnyTypeBox:           AnyTypeBox{Type: StrToBoxType("clap")},
				CleanApertureWidthN:  100,
				CleanApertureWidthD:  1,
				CleanApertureHeightN: 100,
				CleanApertureHeightD: 0,
				HorizOffN:            0, HorizOffD: 1,
				VertOffN: 0, VertOffD: 1,
			},
		},
		{
			name: "horizontal offset denominator",
			clap: &CleanApertureBox{
				AnyTypeBox:           AnyTypeBox{Type: StrToBoxType("clap")},
				CleanApertureWidthN:  100,
				CleanApertureWidthD:  1,
				CleanApertureHeightN: 100,
				CleanApertureHeightD: 1,
				HorizOffN:            1, HorizOffD: 0,
				VertOffN: 0, VertOffD: 1,
			},
		},
		{
			name: "vertical offset denominator",
			clap: &CleanApertureBox{
				AnyTypeBox:           AnyTypeBox{Type: StrToBoxType("clap")},
				CleanApertureWidthN:  100,
				CleanApertureWidthD:  1,
				CleanApertureHeightN: 100,
				CleanApertureHeightD: 1,
				HorizOffN:            0, HorizOffD: 1,
				VertOffN: 1, VertOffD: 0,
			},
		},
	}

	for _, tc := range zeroDenomFields {
		t.Run(tc.name+" rejected on write", func(t *testing.T) {
			_, err := Marshal(bytes.NewBuffer(nil), tc.clap, Context{})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "zero denominator")
		})
		t.Run(tc.name+" rejected on read", func(t *testing.T) {
			// Build the corrupt wire bytes from a box that marshals fine,
			// then zero the target denominator to simulate a damaged file.
			good := *tc.clap
			good.CleanApertureWidthD = nonZero(good.CleanApertureWidthD)
			good.CleanApertureHeightD = nonZero(good.CleanApertureHeightD)
			good.HorizOffD = nonZero(good.HorizOffD)
			good.VertOffD = nonZero(good.VertOffD)
			buf := bytes.NewBuffer(nil)
			_, err := Marshal(buf, &good, Context{})
			require.NoError(t, err)
			wire := buf.Bytes()
			zeroDenominatorBytes(tc.name, wire)

			dst := &CleanApertureBox{AnyTypeBox: AnyTypeBox{Type: StrToBoxType("clap")}}
			_, err = Unmarshal(bytes.NewReader(wire), uint64(len(wire)), dst, Context{})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "zero denominator")
		})
	}
}

func nonZero(v uint32) uint32 {
	if v == 0 {
		return 1
	}
	return v
}

// zeroDenominatorBytes overwrites the denominator field selected by name in
// the 32-byte clap payload.
func zeroDenominatorBytes(name string, wire []byte) {
	fieldOffset := map[string]int{
		"width denominator":             4,
		"height denominator":            12,
		"horizontal offset denominator": 20,
		"vertical offset denominator":   28,
	}
	off := fieldOffset[name]
	for i := off; i < off+4; i++ {
		wire[i] = 0
	}
}

func TestCleanApertureBoxValidate(t *testing.T) {
	base := func() *CleanApertureBox {
		return &CleanApertureBox{
			AnyTypeBox:           AnyTypeBox{Type: StrToBoxType("clap")},
			CleanApertureWidthN:  700,
			CleanApertureWidthD:  1,
			CleanApertureHeightN: 576,
			CleanApertureHeightD: 1,
			HorizOffN:            10,
			HorizOffD:            4, // 2.5
			VertOffN:             0,
			VertOffD:             1,
		}
	}

	t.Run("valid with non-integer offset", func(t *testing.T) {
		require.NoError(t, base().Validate(720, 576))
	})

	t.Run("zero denominator", func(t *testing.T) {
		clap := base()
		clap.HorizOffD = 0
		err := clap.Validate(720, 576)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "zero denominator")
	})

	t.Run("zero width numerator", func(t *testing.T) {
		clap := base()
		clap.CleanApertureWidthN = 0
		err := clap.Validate(720, 576)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "non-positive")
	})

	t.Run("aperture wider than image", func(t *testing.T) {
		clap := base()
		clap.CleanApertureWidthN = 721 // exceeds 720
		err := clap.Validate(720, 576)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "exceeds image width")
	})

	t.Run("aperture taller than image", func(t *testing.T) {
		clap := base()
		clap.CleanApertureHeightN = 577
		err := clap.Validate(720, 576)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "exceeds image height")
	})

	t.Run("horizontal region out of bounds via offset", func(t *testing.T) {
		// center-relative offset +20 pushes the right edge to
		// 360 + 20 + 350 = 730, beyond the 720-pixel image
		clap := base()
		clap.HorizOffN = 20
		clap.HorizOffD = 1
		err := clap.Validate(720, 576)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "horizontal aperture is out of bounds")
	})

	t.Run("horizontal region at exact valid boundary", func(t *testing.T) {
		// offset exactly (imageWidth-apertureWidth)/2 = 10:
		// left = 0 and right = 720, both still valid
		clap := base()
		clap.HorizOffN = 10
		clap.HorizOffD = 1
		require.NoError(t, clap.Validate(720, 576))
	})

	t.Run("vertical region out of bounds via offset", func(t *testing.T) {
		clap := base()
		clap.VertOffN = 20
		clap.VertOffD = 1
		// bottom = 288 + 20 + 288 = 596 > 576
		err := clap.Validate(720, 576)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "vertical aperture is out of bounds")
	})

	t.Run("vertical region at exact valid boundary", func(t *testing.T) {
		// full-height aperture with zero offset touches both edges
		clap := base()
		clap.VertOffN = 0
		clap.VertOffD = 1
		require.NoError(t, clap.Validate(720, 576))
	})

	t.Run("fractional aperture fully inside", func(t *testing.T) {
		clap := base()
		// width 719.5 (= 1439/2), height 575.5, centered; leaves 0.25 margins
		clap.CleanApertureWidthN = 1439
		clap.CleanApertureWidthD = 2
		clap.CleanApertureHeightN = 1151
		clap.CleanApertureHeightD = 2
		clap.HorizOffN = 0
		clap.HorizOffD = 1
		clap.VertOffN = 0
		clap.VertOffD = 1
		require.NoError(t, clap.Validate(720, 576))
	})
}

func TestCleanApertureBoxTruncatedPayload(t *testing.T) {
	// A clap box whose payload is shorter than 32 bytes is corrupt.
	wire := []byte{
		0x00, 0x00, 0x02, 0xbc,
		0x00, 0x00, 0x00, 0x01,
		0x00, 0x00, 0x02, 0x40,
		0x00, 0x00, 0x00, 0x01,
	}
	dst := &CleanApertureBox{AnyTypeBox: AnyTypeBox{Type: StrToBoxType("clap")}}
	_, err := Unmarshal(bytes.NewReader(wire), uint64(len(wire)), dst, Context{})
	require.Error(t, err)
}
