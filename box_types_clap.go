package mp4

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"

	"github.com/abema/go-mp4/internal/bitio"
)

/*************************** clap ****************************/

func BoxTypeClap() BoxType { return StrToBoxType("clap") }

func init() {
	AddAnyTypeBoxDef(&CleanApertureBox{}, BoxTypeClap())
}

// CleanApertureBox is the "clap" (clean aperture) box defined in
// ISO/IEC 14496-12. It is a child box of a video sample entry and describes,
// with rational numbers, the valid display area of the encoded picture.
type CleanApertureBox struct {
	AnyTypeBox
	CleanApertureWidthN  uint32 `mp4:"0,size=32"`
	CleanApertureWidthD  uint32 `mp4:"1,size=32"`
	CleanApertureHeightN uint32 `mp4:"2,size=32"`
	CleanApertureHeightD uint32 `mp4:"3,size=32"`
	HorizOffN            uint32 `mp4:"4,size=32"`
	HorizOffD            uint32 `mp4:"5,size=32"`
	VertOffN             uint32 `mp4:"6,size=32"`
	VertOffD             uint32 `mp4:"7,size=32"`
}

// GetType returns the BoxType.
func (*CleanApertureBox) GetType() BoxType {
	return BoxTypeClap()
}

// OnReadField reads and validates denominator fields while unmarshalling.
// The hook is invoked before the named field would be read, so the field is
// consumed here explicitly and the generic unmarshaller is told to skip it.
// A zero denominator makes the rational number undefined, so such a file is
// treated as corrupt and rejected.
func (clap *CleanApertureBox) OnReadField(name string, r bitio.ReadSeeker, leftBits uint64, ctx Context) (rbits uint64, override bool, err error) {
	switch name {
	case "CleanApertureWidthD",
		"CleanApertureHeightD",
		"HorizOffD",
		"VertOffD":
		data, err := r.ReadBits(32)
		if err != nil {
			return 0, false, err
		}
		val := binary.BigEndian.Uint32(data)
		if val == 0 {
			return 32, true, fmt.Errorf("clap box has zero denominator: field=%s", name)
		}
		clap.setDenominator(name, val)
		return 32, true, nil
	}
	return 0, false, nil
}

// OnWriteField validates denominator fields while marshalling.
func (clap *CleanApertureBox) OnWriteField(name string, w bitio.Writer, ctx Context) (wbits uint64, override bool, err error) {
	switch name {
	case "CleanApertureWidthD",
		"CleanApertureHeightD",
		"HorizOffD",
		"VertOffD":
		if clap.denominatorOf(name) == 0 {
			return 0, false, fmt.Errorf("clap box has zero denominator: field=%s", name)
		}
	}
	return 0, false, nil
}

func (clap *CleanApertureBox) denominatorOf(name string) uint32 {
	switch name {
	case "CleanApertureWidthD":
		return clap.CleanApertureWidthD
	case "CleanApertureHeightD":
		return clap.CleanApertureHeightD
	case "HorizOffD":
		return clap.HorizOffD
	case "VertOffD":
		return clap.VertOffD
	}
	return 0
}

func (clap *CleanApertureBox) setDenominator(name string, val uint32) {
	switch name {
	case "CleanApertureWidthD":
		clap.CleanApertureWidthD = val
	case "CleanApertureHeightD":
		clap.CleanApertureHeightD = val
	case "HorizOffD":
		clap.HorizOffD = val
	case "VertOffD":
		clap.VertOffD = val
	}
}

// Width returns the clean aperture width as an exact rational number.
func (clap *CleanApertureBox) Width() *big.Rat {
	return new(big.Rat).SetFrac(
		new(big.Int).SetUint64(uint64(clap.CleanApertureWidthN)),
		new(big.Int).SetUint64(uint64(clap.CleanApertureWidthD)),
	)
}

// Height returns the clean aperture height as an exact rational number.
func (clap *CleanApertureBox) Height() *big.Rat {
	return new(big.Rat).SetFrac(
		new(big.Int).SetUint64(uint64(clap.CleanApertureHeightN)),
		new(big.Int).SetUint64(uint64(clap.CleanApertureHeightD)),
	)
}

// HorizOffset returns the horizontal offset of the clean aperture center from
// the encoded image center as an exact rational number.
func (clap *CleanApertureBox) HorizOffset() *big.Rat {
	return new(big.Rat).SetFrac(
		new(big.Int).SetUint64(uint64(clap.HorizOffN)),
		new(big.Int).SetUint64(uint64(clap.HorizOffD)),
	)
}

// VertOffset returns the vertical offset of the clean aperture center from
// the encoded image center as an exact rational number.
func (clap *CleanApertureBox) VertOffset() *big.Rat {
	return new(big.Rat).SetFrac(
		new(big.Int).SetUint64(uint64(clap.VertOffN)),
		new(big.Int).SetUint64(uint64(clap.VertOffD)),
	)
}

// Validate checks the intrinsic invariants of the box: all four denominators
// must be non-zero, the aperture width and height must be positive, and the
// clean aperture rectangle must lie completely inside an encoded image of
// imageWidth x imageHeight pixels. It does not inspect where the box is
// located in the box tree; hierarchy is enforced by the edit APIs.
func (clap *CleanApertureBox) Validate(imageWidth, imageHeight uint32) error {
	if clap.CleanApertureWidthD == 0 {
		return errors.New("clap box has zero denominator: CleanApertureWidthD")
	}
	if clap.CleanApertureHeightD == 0 {
		return errors.New("clap box has zero denominator: CleanApertureHeightD")
	}
	if clap.HorizOffD == 0 {
		return errors.New("clap box has zero denominator: HorizOffD")
	}
	if clap.VertOffD == 0 {
		return errors.New("clap box has zero denominator: VertOffD")
	}
	if clap.CleanApertureWidthN == 0 {
		return errors.New("clap box has non-positive clean aperture width")
	}
	if clap.CleanApertureHeightN == 0 {
		return errors.New("clap box has non-positive clean aperture height")
	}

	width := clap.Width()
	height := clap.Height()

	if width.Sign() <= 0 || height.Sign() <= 0 {
		return errors.New("clap box has non-positive clean aperture area")
	}
	if width.Cmp(big.NewRat(int64(imageWidth), 1)) > 0 {
		return fmt.Errorf("clap box clean aperture width %s exceeds image width %d",
			width.FloatString(4), imageWidth)
	}
	if height.Cmp(big.NewRat(int64(imageHeight), 1)) > 0 {
		return fmt.Errorf("clap box clean aperture height %s exceeds image height %d",
			height.FloatString(4), imageHeight)
	}

	// ISO/IEC 14496-12: horizOff/vertOff are offsets of the clean aperture
	// center from the center of the encoded image. In pixel coordinates whose
	// origin is the top-left corner, the aperture edges therefore are:
	//   left   = imageWidth/2  + horizOff - cleanApertureWidth/2
	//   right  = imageWidth/2  + horizOff + cleanApertureWidth/2
	// and likewise for top/bottom. The whole rectangle must stay inside
	// [0, imageWidth] x [0, imageHeight].
	halfImageWidth := big.NewRat(int64(imageWidth), 2)
	halfWidth := new(big.Rat).Quo(width, big.NewRat(2, 1))
	left := new(big.Rat).Sub(new(big.Rat).Add(halfImageWidth, clap.HorizOffset()), halfWidth)
	right := new(big.Rat).Add(new(big.Rat).Add(halfImageWidth, clap.HorizOffset()), halfWidth)
	if left.Sign() < 0 || right.Cmp(big.NewRat(int64(imageWidth), 1)) > 0 {
		return fmt.Errorf("clap box horizontal aperture is out of bounds: offset=%s width=%s imageWidth=%d",
			clap.HorizOffset().FloatString(4), width.FloatString(4), imageWidth)
	}

	halfImageHeight := big.NewRat(int64(imageHeight), 2)
	halfHeight := new(big.Rat).Quo(height, big.NewRat(2, 1))
	top := new(big.Rat).Sub(new(big.Rat).Add(halfImageHeight, clap.VertOffset()), halfHeight)
	bottom := new(big.Rat).Add(new(big.Rat).Add(halfImageHeight, clap.VertOffset()), halfHeight)
	if top.Sign() < 0 || bottom.Cmp(big.NewRat(int64(imageHeight), 1)) > 0 {
		return fmt.Errorf("clap box vertical aperture is out of bounds: offset=%s height=%s imageHeight=%d",
			clap.VertOffset().FloatString(4), height.FloatString(4), imageHeight)
	}

	return nil
}
