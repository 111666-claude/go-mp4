package mp4

import (
	"errors"
	"fmt"
	"io"
	"reflect"

	"github.com/abema/go-mp4/internal/bitio"
)

// This file implements the static HDR metadata boxes defined in
// ISO/IEC 14496-15 (Carriage of network abstraction layer (NAL) unit
// structured video in ISO base media file format):
//
//   - mdcv: Mastering Display Colour Volume (8.3.1), based on
//     SMPTE ST 2086 "Mastering Display Color Volume Metadata".
//   - clli: Content Light Level Information (8.3.2), based on
//     CEA-861.3 "HDR Static Metadata".
//
// Both boxes are optional child boxes of a video sample entry
// (VisualSampleEntry, e.g. avc1, hev1, hvc1, av01, vp09, encv).

const (
	// MdcvPayloadSize is the mdcv payload size excluding the box header:
	// 8 chromaticity coordinates (16 bits each) + 2 luminance values (32 bits each).
	MdcvPayloadSize = 8*2 + 2*4
	// ClliPayloadSize is the clli payload size excluding the box header:
	// MaxCLL (16 bits) + MaxFALL (16 bits).
	ClliPayloadSize = 4
	// MaxChromaticity is the upper bound of a normalized 0.0001-unit
	// chromaticity coordinate stored as a 16-bit fixed point value.
	MaxChromaticity = 50000
)

// Chromaticity16 is a normalized chromaticity coordinate expressed as a
// 16-bit fixed point number. The represented real-world value in the
// 0.0..1.0 range equals Chromaticity * 0.0001, therefore the valid
// range is 0..50000.
type Chromaticity16 struct {
	BaseCustomFieldObject
	Chromaticity uint16 `mp4:"0,size=16"`
}

/*************************** mdcv ****************************/

// BoxTypeMdcv returns the BoxType of the Mastering Display Colour Volume box.
func BoxTypeMdcv() BoxType { return StrToBoxType("mdcv") }

func init() {
	AddBoxDef(&Mdcv{})
}

// Mdcv is the Mastering Display Colour Volume box (ISO/IEC 14496-15 8.3.1).
//
// The chromaticity coordinates are 16-bit fixed point values on a 0.0001
// grid (real value = stored value * 0.0001, valid range 0..50000). Each
// DisplayPrimariesX/Y and the WhitePoint pair holds the x/y coordinates of
// one colour point, in declaration order: R, G, B, white point.
// MinMasteringLuminance and MaxMasteringLuminance are 32-bit fixed point
// values on a 0.0001 grid, expressed in candela per square metre (cd/m^2).
type Mdcv struct {
	Box
	DisplayPrimariesR     [2]Chromaticity16 `mp4:"0,size=16"`
	DisplayPrimariesG     [2]Chromaticity16 `mp4:"1,size=16"`
	DisplayPrimariesB     [2]Chromaticity16 `mp4:"2,size=16"`
	WhitePoint            [2]Chromaticity16 `mp4:"3,size=16"`
	MinMasteringLuminance uint32            `mp4:"4,size=32"`
	MaxMasteringLuminance uint32            `mp4:"5,size=32"`
}

// GetType returns the BoxType.
func (*Mdcv) GetType() BoxType {
	return BoxTypeMdcv()
}

// BeforeUnmarshal validates the declared box payload size before the
// fields are read. The mdcv box has a fixed layout, therefore any other
// payload size is invalid.
func (mdcv *Mdcv) BeforeUnmarshal(r io.ReadSeeker, size uint64, ctx Context) (n uint64, override bool, err error) {
	if size != MdcvPayloadSize {
		return 0, false, fmt.Errorf("mdcv box payload size must be %d bytes: actual=%d", MdcvPayloadSize, size)
	}
	return 0, false, nil
}

// OnReadField validates the box values while it is unmarshalled. Field
// hooks run immediately before the named field itself is unmarshalled:
//   - The eight chromaticity coordinates are all read by the time
//     MinMasteringLuminance is reached, so coordinate range validation is
//     attached there.
//   - MaxMasteringLuminance has not been populated when its hook runs. Its
//     upcoming value is therefore read directly from the stream, validated
//     against the already-read minimum, and the stream is rewound so the
//     normal field unmarshal still consumes it.
func (mdcv *Mdcv) OnReadField(name string, r bitio.ReadSeeker, leftBits uint64, ctx Context) (rbits uint64, override bool, err error) {
	switch name {
	case "MinMasteringLuminance":
		if err := mdcv.validateChromaticity(); err != nil {
			return 0, false, err
		}
	case "MaxMasteringLuminance":
		pos, err := r.Seek(0, io.SeekCurrent)
		if err != nil {
			return 0, false, err
		}
		data := make([]byte, 4)
		if _, err := io.ReadFull(r, data); err != nil {
			return 0, false, err
		}
		if _, err := r.Seek(pos, io.SeekStart); err != nil {
			return 0, false, err
		}
		maxLum := uint32(data[0])<<24 | uint32(data[1])<<16 | uint32(data[2])<<8 | uint32(data[3])
		if mdcv.MinMasteringLuminance > maxLum {
			return 0, false, fmt.Errorf("mdcv MinMasteringLuminance must not exceed MaxMasteringLuminance: min=%d max=%d",
				mdcv.MinMasteringLuminance, maxLum)
		}
	}
	return 0, false, nil
}

// OnWriteField validates the struct values before serialization begins.
func (mdcv *Mdcv) OnWriteField(name string, w bitio.Writer, ctx Context) (wbits uint64, override bool, err error) {
	if name == "DisplayPrimariesR" {
		if err := mdcv.validateChromaticity(); err != nil {
			return 0, false, err
		}
		if err := mdcv.validateLuminance(); err != nil {
			return 0, false, err
		}
	}
	return 0, false, nil
}

func (mdcv *Mdcv) validateChromaticity() error {
	points := []struct {
		name string
		xy   [2]Chromaticity16
	}{
		{"DisplayPrimariesR", mdcv.DisplayPrimariesR},
		{"DisplayPrimariesG", mdcv.DisplayPrimariesG},
		{"DisplayPrimariesB", mdcv.DisplayPrimariesB},
		{"WhitePoint", mdcv.WhitePoint},
	}
	for _, point := range points {
		for i, coord := range point.xy {
			if coord.Chromaticity > MaxChromaticity {
				axis := "x"
				if i == 1 {
					axis = "y"
				}
				return fmt.Errorf("mdcv %s.%s out of range: value=%d max=%d",
					point.name, axis, coord.Chromaticity, MaxChromaticity)
			}
		}
	}
	return nil
}

// validateLuminance enforces the semantic ordering of the mastering
// luminance values: the minimum mastering display luminance must not exceed
// the maximum. Both values are otherwise only bounded by their uint32 type.
func (mdcv *Mdcv) validateLuminance() error {
	if mdcv.MinMasteringLuminance > mdcv.MaxMasteringLuminance {
		return fmt.Errorf("mdcv MinMasteringLuminance must not exceed MaxMasteringLuminance: min=%d max=%d",
			mdcv.MinMasteringLuminance, mdcv.MaxMasteringLuminance)
	}
	return nil
}

/*************************** clli ****************************/

// BoxTypeClli returns the BoxType of the Content Light Level Information box.
func BoxTypeClli() BoxType { return StrToBoxType("clli") }

func init() {
	AddBoxDef(&Clli{})
}

// Clli is the Content Light Level Information box (ISO/IEC 14496-15 8.3.2).
//
// MaxContentLightLevel (MaxCLL) and MaxPictureAverageLightLevel (MaxFALL)
// are expressed in candela per square metre (cd/m^2).
type Clli struct {
	Box
	MaxContentLightLevel        uint16 `mp4:"0,size=16"`
	MaxPictureAverageLightLevel uint16 `mp4:"1,size=16"`
}

// GetType returns the BoxType.
func (*Clli) GetType() BoxType {
	return BoxTypeClli()
}

// BeforeUnmarshal validates the declared box payload size before the
// fields are read. The clli box has a fixed layout, therefore any other
// payload size is invalid.
func (clli *Clli) BeforeUnmarshal(r io.ReadSeeker, size uint64, ctx Context) (n uint64, override bool, err error) {
	if size != ClliPayloadSize {
		return 0, false, fmt.Errorf("clli box payload size must be %d bytes: actual=%d", ClliPayloadSize, size)
	}
	return 0, false, nil
}

// ErrInvalidHDRBoxPlacement is returned when an mdcv or clli box is found
// outside of a video sample entry (VisualSampleEntry).
var ErrInvalidHDRBoxPlacement = errors.New("mdcv and clli boxes must be placed in a video sample entry")

// ErrDuplicateHDRBox is returned when the same HDR metadata box appears
// more than once inside one video sample entry.
var ErrDuplicateHDRBox = errors.New("duplicate HDR metadata box in video sample entry")

// isVisualSampleEntryType reports whether the box type is registered as a
// VisualSampleEntry (video sample entry).
func isVisualSampleEntryType(boxType BoxType, ctx Context) bool {
	def := boxType.getBoxDef(ctx)
	if def == nil {
		return false
	}
	return def.dataType == reflect.TypeOf(VisualSampleEntry{})
}
