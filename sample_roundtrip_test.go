package mp4

import (
	"bytes"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSampleMP4StructuredBoxesRoundTrip reads the bundled sample.mp4 and
// verifies that the avcC and sgpd boxes are parsed into structured entries and
// re-marshal to the exact original payload bytes.
func TestSampleMP4StructuredBoxesRoundTrip(t *testing.T) {
	f, err := os.Open("./_examples/sample.mp4")
	require.NoError(t, err)
	defer f.Close()

	var avccPayload, sgpdPayload []byte
	var avccBox *AVCDecoderConfiguration
	var sgpdBox *Sgpd

	_, err = ReadBoxStructure(f, func(h *ReadHandle) (interface{}, error) {
		switch h.BoxInfo.Type {
		case StrToBoxType("avcC"):
			box, _, err := h.ReadPayload()
			require.NoError(t, err)
			avccBox = box.(*AVCDecoderConfiguration)

			raw := bytes.NewBuffer(nil)
			_, err = h.ReadData(raw)
			require.NoError(t, err)
			avccPayload = append([]byte(nil), raw.Bytes()...)

		case BoxTypeSgpd():
			box, _, err := h.ReadPayload()
			require.NoError(t, err)
			sgpdBox = box.(*Sgpd)

			raw := bytes.NewBuffer(nil)
			_, err = h.ReadData(raw)
			require.NoError(t, err)
			sgpdPayload = append([]byte(nil), raw.Bytes()...)

		default:
			// no-op
		}
		// Unsupported box types cannot be expanded; everything else can.
		if h.BoxInfo.Type.IsSupported() {
			if _, err := h.Expand(); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
	require.NoError(t, err)

	require.NotNil(t, avccBox)
	require.NotNil(t, sgpdBox)

	// avcC: the sample has one SPS and one PPS (two structured NAL units).
	assert.Len(t, avccBox.SequenceParameterSets, 1)
	assert.Len(t, avccBox.PictureParameterSets, 1)
	assert.Equal(t, 2,
		len(avccBox.SequenceParameterSets)+len(avccBox.PictureParameterSets))
	assert.Equal(t, uint16(25), avccBox.SequenceParameterSets[0].Length)
	assert.Equal(t, uint16(5), avccBox.PictureParameterSets[0].Length)

	avccOut := bytes.NewBuffer(nil)
	_, err = Marshal(avccOut, avccBox)
	require.NoError(t, err)
	assert.Equal(t, avccPayload, avccOut.Bytes())

	// sgpd: the sample uses "roll" with one entry.
	assert.Len(t, sgpdBox.RollDistances, 1)
	assert.Equal(t, int16(-1), sgpdBox.RollDistances[0])

	sgpdOut := bytes.NewBuffer(nil)
	_, err = Marshal(sgpdOut, sgpdBox)
	require.NoError(t, err)
	assert.Equal(t, sgpdPayload, sgpdOut.Bytes())
}
