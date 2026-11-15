package mp4

import (
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The hex vectors below are legal AudioSpecificConfig bitstreams encoded
// according to ISO/IEC 14496-3. Each vector covers a different signaling
// variant: plain AAC-LC, explicit sampling rate, explicit/implicit SBR,
// parametric stereo (PS) and multi-channel configurations.
func TestDetectAACProfile(t *testing.T) {
	esdsWithASC := func(oti byte, ascHex string) *Esds {
		data, err := hex.DecodeString(ascHex)
		require.NoError(t, err)
		return &Esds{
			Descriptors: []Descriptor{
				{
					Tag: DecoderConfigDescrTag,
					DecoderConfigDescriptor: &DecoderConfigDescriptor{
						ObjectTypeIndication: oti,
					},
				},
				{
					Tag:  DecSpecificInfoTag,
					Data: data,
				},
			},
		}
	}

	testCases := []struct {
		name      string
		ascHex    string
		wantOTI   uint8
		wantAudOT uint8
	}{
		{
			// AAC-LC, samplingFrequencyIndex=4 (44.1kHz), 2 channels
			name:      "AAC-LC 44.1kHz stereo",
			ascHex:    "1210",
			wantOTI:   0x40,
			wantAudOT: 2,
		},
		{
			// AAC-LC, samplingFrequencyIndex=0xf followed by explicit
			// 24-bit sampling rate (12345Hz), 1 channel
			name:      "AAC-LC explicit sampling rate 12345Hz mono",
			ascHex:    "1780181c88",
			wantOTI:   0x40,
			wantAudOT: 2,
		},
		{
			// Explicit HE-AAC v1: audioObjectType=5 (SBR), core
			// samplingFrequencyIndex=7 (22.05kHz), extension index=4
			name:      "explicit SBR (AOT=5) 22.05kHz",
			ascHex:    "2b9210",
			wantOTI:   0x40,
			wantAudOT: 5,
		},
		{
			// Explicit HE-AAC v1 where the extension sampling rate is
			// signaled explicitly (extension index=0xf + 24-bit 44100Hz)
			name:      "explicit SBR with explicit extension sampling rate",
			ascHex:    "2b9780562210",
			wantOTI:   0x40,
			wantAudOT: 5,
		},
		{
			// AAC-LC core with implicit SBR signaling via the
			// syncExtensionType=0x2b7 extension (sbrPresentFlag=1)
			name:      "implicit SBR via sync extension",
			ascHex:    "139056e5a0",
			wantOTI:   0x40,
			wantAudOT: 5,
		},
		{
			// Explicit HE-AAC v2: audioObjectType=29 (PS), core mono,
			// extension sampling rate 44.1kHz, extension channels=2
			name:      "explicit parametric stereo (AOT=29)",
			ascHex:    "eb8a10",
			wantOTI:   0x40,
			wantAudOT: 29,
		},
		{
			// AAC-LC core with implicit SBR plus the 0x548 PS extension
			name:      "implicit SBR with parametric stereo",
			ascHex:    "138856e5a54880",
			wantOTI:   0x40,
			wantAudOT: 29,
		},
		{
			// AAC-LC, samplingFrequencyIndex=3 (48kHz), channel config 6 (5.1)
			name:      "AAC-LC 48kHz 5.1ch",
			ascHex:    "11b0",
			wantOTI:   0x40,
			wantAudOT: 2,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			oti, audOTI, err := detectAACProfile(esdsWithASC(0x40, tc.ascHex))
			require.NoError(t, err)
			assert.Equal(t, tc.wantOTI, oti, "object type indication")
			assert.Equal(t, tc.wantAudOT, audOTI, "audio object type")
		})
	}
}

func TestDetectAACProfileEdgeCases(t *testing.T) {
	t.Run("non-AAC object type indication is rejected", func(t *testing.T) {
		esds := &Esds{
			Descriptors: []Descriptor{
				{
					Tag: DecoderConfigDescrTag,
					DecoderConfigDescriptor: &DecoderConfigDescriptor{
						ObjectTypeIndication: 0x28,
					},
				},
			},
		}
		_, _, err := detectAACProfile(esds)
		require.Error(t, err)
	})

	t.Run("missing DecoderConfigDescriptor", func(t *testing.T) {
		_, _, err := detectAACProfile(&Esds{})
		require.Error(t, err)
	})

	t.Run("extended audio object type above 31 is decoded", func(t *testing.T) {
		// AOT=32 encoded as 0x1f (5 bits) + 0 (6 bits); not AAC-LC,
		// so the parser must reject it instead of mistaking it for AOT=2.
		esds := &Esds{
			Descriptors: []Descriptor{
				{
					Tag: DecoderConfigDescrTag,
					DecoderConfigDescriptor: &DecoderConfigDescriptor{
						ObjectTypeIndication: 0x40,
					},
				},
				{Tag: DecSpecificInfoTag, Data: []byte{0xf8, 0x08, 0x40}},
			},
		}
		_, _, err := detectAACProfile(esds)
		require.Error(t, err)
	})

	t.Run("sbrPresentFlag zero keeps AAC-LC", func(t *testing.T) {
		esds := &Esds{
			Descriptors: []Descriptor{
				{
					Tag: DecoderConfigDescrTag,
					DecoderConfigDescriptor: &DecoderConfigDescriptor{
						ObjectTypeIndication: 0x40,
					},
				},
				{Tag: DecSpecificInfoTag, Data: []byte{0x13, 0x90, 0x56, 0xe5, 0x20}},
			},
		}
		oti, audOTI, err := detectAACProfile(esds)
		require.NoError(t, err)
		assert.Equal(t, uint8(0x40), oti)
		assert.Equal(t, uint8(2), audOTI)
	})
}

// Each track owns its own esds box: descriptors found while probing one
// track must never be reused for another track. This test builds two
// independent esds structures and verifies they are evaluated separately.
func TestDetectAACProfileDoesNotMixTracks(t *testing.T) {
	buildEsds := func(ascHex string) *Esds {
		data, err := hex.DecodeString(ascHex)
		require.NoError(t, err)
		return &Esds{
			Descriptors: []Descriptor{
				{
					Tag: DecoderConfigDescrTag,
					DecoderConfigDescriptor: &DecoderConfigDescriptor{
						ObjectTypeIndication: 0x40,
					},
				},
				{
					Tag:  DecSpecificInfoTag,
					Data: data,
				},
				// trailing descriptor must not disturb tag lookup order
				{Tag: SLConfigDescrTag, Data: []byte{0x02}},
			},
		}
	}

	lcEsds := buildEsds("1210")          // AAC-LC track
	psEsds := buildEsds("eb8a10")        // HE-AAC v2 (PS) track

	oti1, aud1, err := detectAACProfile(lcEsds)
	require.NoError(t, err)
	assert.Equal(t, uint8(0x40), oti1)
	assert.Equal(t, uint8(2), aud1)

	oti2, aud2, err := detectAACProfile(psEsds)
	require.NoError(t, err)
	assert.Equal(t, uint8(0x40), oti2)
	assert.Equal(t, uint8(29), aud2)

	// re-evaluating the first track still yields the first track's type
	oti1Again, aud1Again, err := detectAACProfile(lcEsds)
	require.NoError(t, err)
	assert.Equal(t, uint8(0x40), oti1Again)
	assert.Equal(t, uint8(2), aud1Again)
}
