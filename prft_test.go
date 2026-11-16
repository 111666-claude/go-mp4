package mp4

import (
	"bytes"
	"errors"
	"io"
	"math"
	"math/big"
	"testing"
	"time"

	"github.com/orcaman/writerseeker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// prftFixtureParams describes one fragment of the synthetic fragmented
// MP4 file used by the prft tests.
type prftFixtureParams struct {
	sequenceNumber uint32
	tfdtBase       uint64
	tfdtVersion    uint8
	prft           *Prft
	// prftAtMoof places the prft box directly under moof instead of traf.
	prftAtMoof bool
}

// buildPrftFixture writes a minimal, well-formed fragmented MP4 stream:
// ftyp + moov(one trak) + one moof/mdat pair per fragment. The media
// payload of mdat is a fixed placeholder and is never parsed.
func buildPrftFixture(t *testing.T, trackID uint32, timescale uint32, fragments []prftFixtureParams) []byte {
	t.Helper()
	buf := &writerseeker.WriterSeeker{}
	w := NewWriter(buf)

	// ftyp
	startBox(t, w, BoxTypeFtyp())
	_, err := Marshal(w, &Ftyp{
		MajorBrand:       [4]byte{'i', 's', 'o', 'm'},
		MinorVersion:     0,
		CompatibleBrands: []CompatibleBrandElem{{CompatibleBrand: [4]byte{'i', 's', 'o', 'm'}}},
	}, Context{})
	require.NoError(t, err)
	endBox(t, w)

	// moov > trak > (tkhd, mdia > mdhd)
	startBox(t, w, BoxTypeMoov())
	startBox(t, w, BoxTypeTrak())
	startBox(t, w, BoxTypeTkhd())
	_, err = Marshal(w, &Tkhd{
		TrackID: trackID,
		Matrix:  [9]int32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000},
	}, Context{})
	require.NoError(t, err)
	endBox(t, w)

	startBox(t, w, BoxTypeMdia())
	startBox(t, w, BoxTypeMdhd())
	_, err = Marshal(w, &Mdhd{
		Timescale:  timescale,
		DurationV0: 0,
		Language:   [3]byte{0x55, 0xc4, 0x00}, // "und"
	}, Context{})
	require.NoError(t, err)
	endBox(t, w) // mdhd
	endBox(t, w) // mdia
	endBox(t, w) // trak
	endBox(t, w) // moov

	for _, frag := range fragments {
		// moof
		startBox(t, w, BoxTypeMoof())
		startBox(t, w, BoxTypeMfhd())
		_, err = Marshal(w, &Mfhd{SequenceNumber: frag.sequenceNumber}, Context{})
		require.NoError(t, err)
		endBox(t, w) // mfhd

		if frag.prftAtMoof && frag.prft != nil {
			marshalBox(t, w, frag.prft)
		}

		// traf > (prft?, tfhd, tfdt)
		startBox(t, w, BoxTypeTraf())
		if !frag.prftAtMoof && frag.prft != nil {
			marshalBox(t, w, frag.prft)
		}
		marshalBox(t, w, &Tfhd{TrackID: trackID})
		tfdt := &Tfdt{}
		if frag.tfdtVersion == 1 {
			tfdt.SetVersion(1)
			tfdt.BaseMediaDecodeTimeV1 = frag.tfdtBase
		} else {
			tfdt.SetVersion(0)
			tfdt.BaseMediaDecodeTimeV0 = uint32(frag.tfdtBase)
		}
		marshalBox(t, w, tfdt)
		endBox(t, w) // traf
		endBox(t, w) // moof

		// mdat with opaque payload
		startBox(t, w, BoxTypeMdat())
		_, err = w.Write([]byte{0xde, 0xad, 0xbe, 0xef})
		require.NoError(t, err)
		endBox(t, w)
	}

	out, err := io.ReadAll(buf.Reader())
	require.NoError(t, err)
	return out
}

// requireErrIs mirrors require.ErrorIs for older testify versions.
func requireErrIs(t *testing.T, err, target error) {
	t.Helper()
	require.Error(t, err)
	if !errors.Is(err, target) {
		t.Fatalf("error %v does not wrap %v", err, target)
	}
}

func startBox(t *testing.T, w *Writer, typ BoxType) {
	t.Helper()
	_, err := w.StartBox(&BoxInfo{Type: typ})
	require.NoError(t, err)
}

func endBox(t *testing.T, w *Writer) {
	t.Helper()
	_, err := w.EndBox()
	require.NoError(t, err)
}

func marshalBox(t *testing.T, w *Writer, box IImmutableBox) {
	t.Helper()
	_, err := w.StartBox(&BoxInfo{Type: box.GetType()})
	require.NoError(t, err)
	_, err = Marshal(w, box, Context{})
	require.NoError(t, err)
	endBox(t, w)
}

// shiftNtpBig mirrors shiftNtpTimestamp with arbitrary-precision math so
// tests can compute expected values independently of the implementation.
func shiftNtpBig(ntp uint64, delta int64, timescale uint32) *big.Int {
	res := new(big.Int).SetUint64(ntp)
	d := big.NewInt(delta)
	d.Lsh(d, 32)
	d.Quo(d, big.NewInt(int64(timescale)))
	return res.Add(res, d)
}

const (
	prftTestTrackID   uint32 = 7
	prftTestTimescale uint32 = 90000
	// Fixed NTP anchors (seconds since 1900 in the high word).
	prftNtp1 uint64 = 0xe000000000000000
	prftNtp2 uint64 = 0xe000001000000000
)

func TestReadProducerReferenceTimes(t *testing.T) {
	prft1 := &Prft{
		ReferenceTrackID: prftTestTrackID,
		NtpTimestamp:     prftNtp1,
		MediaTimeV0:      1000000,
	}
	prft1.SetVersion(0)

	prft2 := &Prft{
		ReferenceTrackID: prftTestTrackID,
		NtpTimestamp:     prftNtp2,
		MediaTimeV1:      3000000,
	}
	prft2.SetVersion(1)

	data := buildPrftFixture(t, prftTestTrackID, prftTestTimescale, []prftFixtureParams{
		{sequenceNumber: 1, tfdtVersion: 1, tfdtBase: 1000000, prft: prft1},
		{sequenceNumber: 2, tfdtVersion: 1, tfdtBase: 3000000, prft: prft2},
	})

	refs, err := ReadProducerReferenceTimes(bytes.NewReader(data))
	require.NoError(t, err)
	require.Len(t, refs, 2)

	assert.Equal(t, prftTestTrackID, refs[0].ReferenceTrackID)
	assert.Equal(t, prftNtp1, refs[0].NtpTimestamp)
	assert.Equal(t, uint64(1000000), refs[0].MediaTime)
	assert.Equal(t, uint(32), refs[0].MediaTimeSize)
	assert.Equal(t, prftTestTimescale, refs[0].Timescale)
	assert.Less(t, refs[0].MoofOffset, refs[1].MoofOffset)

	assert.Equal(t, prftTestTrackID, refs[1].ReferenceTrackID)
	assert.Equal(t, prftNtp2, refs[1].NtpTimestamp)
	assert.Equal(t, uint64(3000000), refs[1].MediaTime)
	assert.Equal(t, uint(64), refs[1].MediaTimeSize)
	assert.Equal(t, prftTestTimescale, refs[1].Timescale)

	// Round-trip through the generic reader: the prft boxes must be
	// discoverable with the standard extraction API and equal to the
	// boxes written into the fixture.
	bips, err := ExtractBoxesWithPayload(bytes.NewReader(data), nil, []BoxPath{
		{BoxTypeMoof(), BoxTypeTraf(), BoxTypePrft()},
	})
	require.NoError(t, err)
	require.Len(t, bips, 2)
	assert.Equal(t, prft1, bips[0].Payload)
	assert.Equal(t, prft2, bips[1].Payload)
}

func TestReadProducerReferenceTimesMoofLevelPrft(t *testing.T) {
	prft1 := &Prft{
		ReferenceTrackID: prftTestTrackID,
		NtpTimestamp:     prftNtp1,
		MediaTimeV0:      1000000,
	}
	prft1.SetVersion(0)

	data := buildPrftFixture(t, prftTestTrackID, prftTestTimescale, []prftFixtureParams{
		{sequenceNumber: 1, tfdtVersion: 1, tfdtBase: 1000000, prft: prft1, prftAtMoof: true},
	})

	refs, err := ReadProducerReferenceTimes(bytes.NewReader(data))
	require.NoError(t, err)
	require.Len(t, refs, 1)
	assert.Equal(t, uint64(1000000), refs[0].MediaTime)
	assert.Equal(t, prftNtp1, refs[0].NtpTimestamp)
}

func TestResolveNtpTimestampAcrossFragments(t *testing.T) {
	prft1 := &Prft{
		ReferenceTrackID: prftTestTrackID,
		NtpTimestamp:     prftNtp1,
		MediaTimeV0:      1000000,
	}
	prft1.SetVersion(0)
	prft2 := &Prft{
		ReferenceTrackID: prftTestTrackID,
		NtpTimestamp:     prftNtp2,
		MediaTimeV1:      3000000,
	}
	prft2.SetVersion(1)

	data := buildPrftFixture(t, prftTestTrackID, prftTestTimescale, []prftFixtureParams{
		{sequenceNumber: 1, tfdtVersion: 1, tfdtBase: 1000000, prft: prft1},
		{sequenceNumber: 2, tfdtVersion: 1, tfdtBase: 3000000, prft: prft2},
	})
	refs, err := ReadProducerReferenceTimes(bytes.NewReader(data))
	require.NoError(t, err)

	t.Run("exact first anchor", func(t *testing.T) {
		ntp, err := ResolveNtpTimestamp(refs, prftTestTrackID, 1000000)
		require.NoError(t, err)
		assert.Equal(t, prftNtp1, ntp)
	})

	t.Run("between fragments uses first anchor", func(t *testing.T) {
		ntp, err := ResolveNtpTimestamp(refs, prftTestTrackID, 2500000)
		require.NoError(t, err)
		want := shiftNtpBig(prftNtp1, 1500000, prftTestTimescale)
		assert.Equal(t, want.Uint64(), ntp)
	})

	t.Run("at and after second anchor", func(t *testing.T) {
		ntp, err := ResolveNtpTimestamp(refs, prftTestTrackID, 3000000)
		require.NoError(t, err)
		assert.Equal(t, prftNtp2, ntp)

		ntp, err = ResolveNtpTimestamp(refs, prftTestTrackID, 3090000)
		require.NoError(t, err)
		want := shiftNtpBig(prftNtp2, 90000, prftTestTimescale) // +1.0 s
		assert.Equal(t, want.Uint64(), ntp)
	})

	t.Run("before first anchor extrapolates backwards", func(t *testing.T) {
		ntp, err := ResolveNtpTimestamp(refs, prftTestTrackID, 910000)
		require.NoError(t, err)
		want := shiftNtpBig(prftNtp1, -90000, prftTestTimescale) // -1.0 s
		assert.Equal(t, want.Uint64(), ntp)
	})

	t.Run("wall clock time matches NTP conversion", func(t *testing.T) {
		wt, err := ResolveWallClock(refs, prftTestTrackID, 3090000)
		require.NoError(t, err)
		ntp, err := ResolveNtpTimestamp(refs, prftTestTrackID, 3090000)
		require.NoError(t, err)
		assert.Equal(t, NtpTimestampToTime(ntp), wt)
	})
}

func TestPrftV0MediaTimeExpansion(t *testing.T) {
	t.Run("32-bit tfdt within window", func(t *testing.T) {
		prft1 := &Prft{
			ReferenceTrackID: prftTestTrackID,
			NtpTimestamp:     prftNtp1,
			MediaTimeV0:      1000050,
		}
		prft1.SetVersion(0)
		data := buildPrftFixture(t, prftTestTrackID, prftTestTimescale, []prftFixtureParams{
			{sequenceNumber: 1, tfdtVersion: 0, tfdtBase: 1000000, prft: prft1},
		})
		refs, err := ReadProducerReferenceTimes(bytes.NewReader(data))
		require.NoError(t, err)
		require.Len(t, refs, 1)
		assert.Equal(t, uint64(1000050), refs[0].MediaTime)
	})

	t.Run("64-bit tfdt pins the high word", func(t *testing.T) {
		// tfdt base sits above the 32-bit boundary; the low word stored in
		// the version 0 prft box must be expanded using that high word.
		const base = uint64(0x1_0000_0100)
		prft1 := &Prft{
			ReferenceTrackID: prftTestTrackID,
			NtpTimestamp:     prftNtp1,
			MediaTimeV0:      0x00000200,
		}
		prft1.SetVersion(0)
		data := buildPrftFixture(t, prftTestTrackID, prftTestTimescale, []prftFixtureParams{
			{sequenceNumber: 1, tfdtVersion: 1, tfdtBase: base, prft: prft1},
		})
		refs, err := ReadProducerReferenceTimes(bytes.NewReader(data))
		require.NoError(t, err)
		require.Len(t, refs, 1)
		assert.Equal(t, uint64(0x1_0000_0200), refs[0].MediaTime)
	})

	t.Run("ambiguous wrap is rejected", func(t *testing.T) {
		prft1 := &Prft{
			ReferenceTrackID: prftTestTrackID,
			NtpTimestamp:     prftNtp1,
			MediaTimeV0:      0x00000000,
		}
		prft1.SetVersion(0)
		// tfdt low word is more than MaxInt32 away: no single window fits.
		data := buildPrftFixture(t, prftTestTrackID, prftTestTimescale, []prftFixtureParams{
			{sequenceNumber: 1, tfdtVersion: 1, tfdtBase: 0x1_8000_0000, prft: prft1},
		})
		_, err := ReadProducerReferenceTimes(bytes.NewReader(data))
		requireErrIs(t, err, ErrMediaTimeWraparound)
	})

	t.Run("32-bit tfdt ambiguous wrap is rejected", func(t *testing.T) {
		prft1 := &Prft{
			ReferenceTrackID: prftTestTrackID,
			NtpTimestamp:     prftNtp1,
			MediaTimeV0:      0x00000000,
		}
		prft1.SetVersion(0)
		data := buildPrftFixture(t, prftTestTrackID, prftTestTimescale, []prftFixtureParams{
			{sequenceNumber: 1, tfdtVersion: 0, tfdtBase: 0x80000001, prft: prft1},
		})
		_, err := ReadProducerReferenceTimes(bytes.NewReader(data))
		requireErrIs(t, err, ErrMediaTimeWraparound)
	})

	t.Run("moof-level v0 prft without matching traf is ambiguous", func(t *testing.T) {
		prft1 := &Prft{
			ReferenceTrackID: 4242, // no traf advertises this track ID
			NtpTimestamp:     prftNtp1,
			MediaTimeV0:      1000000,
		}
		prft1.SetVersion(0)
		data := buildPrftFixture(t, prftTestTrackID, prftTestTimescale, []prftFixtureParams{
			{sequenceNumber: 1, tfdtVersion: 1, tfdtBase: 1000000, prft: prft1, prftAtMoof: true},
		})
		// The track itself is unknown, so this surfaces as ErrUnknownTrack.
		_, err := ReadProducerReferenceTimes(bytes.NewReader(data))
		requireErrIs(t, err, ErrUnknownTrack)
	})
}

func TestReadProducerReferenceTimesErrors(t *testing.T) {
	t.Run("no prft box", func(t *testing.T) {
		data := buildPrftFixture(t, prftTestTrackID, prftTestTimescale, []prftFixtureParams{
			{sequenceNumber: 1, tfdtVersion: 1, tfdtBase: 1000000},
		})
		_, err := ReadProducerReferenceTimes(bytes.NewReader(data))
		requireErrIs(t, err, ErrPrftNotFound)
	})

	t.Run("unknown reference track", func(t *testing.T) {
		prft1 := &Prft{
			ReferenceTrackID: 999,
			NtpTimestamp:     prftNtp1,
			MediaTimeV1:      1000000,
		}
		prft1.SetVersion(1)
		data := buildPrftFixture(t, prftTestTrackID, prftTestTimescale, []prftFixtureParams{
			{sequenceNumber: 1, tfdtVersion: 1, tfdtBase: 1000000, prft: prft1},
		})
		_, err := ReadProducerReferenceTimes(bytes.NewReader(data))
		requireErrIs(t, err, ErrUnknownTrack)
	})

	t.Run("zero timescale", func(t *testing.T) {
		prft1 := &Prft{
			ReferenceTrackID: prftTestTrackID,
			NtpTimestamp:     prftNtp1,
			MediaTimeV1:      1000000,
		}
		prft1.SetVersion(1)
		data := buildPrftFixture(t, prftTestTrackID, 0, []prftFixtureParams{
			{sequenceNumber: 1, tfdtVersion: 1, tfdtBase: 1000000, prft: prft1},
		})
		_, err := ReadProducerReferenceTimes(bytes.NewReader(data))
		requireErrIs(t, err, ErrZeroTimescale)
	})

	t.Run("truncated prft box declared size", func(t *testing.T) {
		// Build the raw prft header+payload and shrink only the declared
		// size so checkPrftSize rejects it before unmarshalling.
		prft1 := &Prft{
			ReferenceTrackID: prftTestTrackID,
			NtpTimestamp:     prftNtp1,
			MediaTimeV1:      1000000,
		}
		prft1.SetVersion(1)
		payload := bytes.NewBuffer(nil)
		_, err := Marshal(payload, prft1, Context{})
		require.NoError(t, err)

		header := BoxInfo{Type: BoxTypePrft(), Size: SmallHeaderSize + uint64(payload.Len())}
		raw := bytes.NewBuffer(nil)
		_, err = raw.Write(EncodeBoxInfo(&header))
		require.NoError(t, err)
		_, err = raw.Write(payload.Bytes())
		require.NoError(t, err)

		fullSize := SmallHeaderSize + uint64(payload.Len())
		bi, err := ReadBoxInfo(bytes.NewReader(raw.Bytes()))
		require.NoError(t, err)
		require.Equal(t, fullSize, bi.Size)
		require.NoError(t, checkPrftSize(bi, 1))

		bi.Size -= 1
		requireErrIs(t, checkPrftSize(bi, 1), ErrTruncatedBox)
	})

	t.Run("truncated stream inside moof", func(t *testing.T) {
		prft1 := &Prft{
			ReferenceTrackID: prftTestTrackID,
			NtpTimestamp:     prftNtp1,
			MediaTimeV1:      1000000,
		}
		prft1.SetVersion(1)
		data := buildPrftFixture(t, prftTestTrackID, prftTestTimescale, []prftFixtureParams{
			{sequenceNumber: 1, tfdtVersion: 1, tfdtBase: 1000000, prft: prft1},
		})
		// Locate the moof box and cut within its declared region so box
		// walking hits a short read instead of completing.
		moofIdx := bytes.Index(data, []byte("moof"))
		require.NotEqual(t, -1, moofIdx)
		moofSize := uint32(data[moofIdx-4])<<24 | uint32(data[moofIdx-3])<<16 |
			uint32(data[moofIdx-2])<<8 | uint32(data[moofIdx-1])
		moofStart := moofIdx - 4
		cut := moofStart + int(moofSize)/2
		_, err := ReadProducerReferenceTimes(bytes.NewReader(data[:cut]))
		require.Error(t, err)
	})
}

func TestResolveNtpTimestampErrors(t *testing.T) {
	good := &Prft{
		ReferenceTrackID: prftTestTrackID,
		NtpTimestamp:     prftNtp1,
		MediaTimeV1:      1000000,
	}
	good.SetVersion(1)
	data := buildPrftFixture(t, prftTestTrackID, prftTestTimescale, []prftFixtureParams{
		{sequenceNumber: 1, tfdtVersion: 1, tfdtBase: 1000000, prft: good},
	})
	refs, err := ReadProducerReferenceTimes(bytes.NewReader(data))
	require.NoError(t, err)

	t.Run("unknown track", func(t *testing.T) {
		_, err := ResolveNtpTimestamp(refs, 4242, 1000000)
		requireErrIs(t, err, ErrUnknownTrack)
	})

	t.Run("32-bit anchor wraparound window", func(t *testing.T) {
		v0 := &Prft{
			ReferenceTrackID: prftTestTrackID,
			NtpTimestamp:     prftNtp1,
			MediaTimeV0:      1000000,
		}
		v0.SetVersion(0)
		data := buildPrftFixture(t, prftTestTrackID, prftTestTimescale, []prftFixtureParams{
			{sequenceNumber: 1, tfdtVersion: 1, tfdtBase: 1000000, prft: v0},
		})
		refs32, err := ReadProducerReferenceTimes(bytes.NewReader(data))
		require.NoError(t, err)

		// In-range query works.
		_, err = ResolveNtpTimestamp(refs32, prftTestTrackID, 1000000+math.MaxInt32)
		require.NoError(t, err)

		// One media tick beyond the unambiguous 32-bit window is rejected.
		_, err = ResolveNtpTimestamp(refs32, prftTestTrackID, 1000000+math.MaxInt32+1)
		requireErrIs(t, err, ErrMediaTimeWraparound)
	})

	t.Run("NTP overflow", func(t *testing.T) {
		// Anchor near the top of the 64-bit NTP space and a positive delta.
		overflowRefs := []*ProducerReferenceTime{{
			ReferenceTrackID: prftTestTrackID,
			NtpTimestamp:     math.MaxUint64,
			MediaTime:        0,
			MediaTimeSize:    64,
			Timescale:        prftTestTimescale,
		}}
		_, err := ResolveNtpTimestamp(overflowRefs, prftTestTrackID, 90000)
		requireErrIs(t, err, ErrTimeOverflow)
	})

	t.Run("NTP underflow before 1900", func(t *testing.T) {
		underflowRefs := []*ProducerReferenceTime{{
			ReferenceTrackID: prftTestTrackID,
			NtpTimestamp:     1 << 32, // 1 second after 1900
			MediaTime:        1000000,
			MediaTimeSize:    64,
			Timescale:        prftTestTimescale,
		}}
		// 1,000,000 media units backwards at 90kHz ~= 11.1 s: negative NTP.
		_, err := ResolveNtpTimestamp(underflowRefs, prftTestTrackID, 0)
		requireErrIs(t, err, ErrTimeOverflow)
	})

	t.Run("media delta exceeds int64", func(t *testing.T) {
		// A 64-bit anchor and a target more than MaxInt64 media units away
		// cannot be expressed as a signed delta and must be rejected.
		hugeRefs := []*ProducerReferenceTime{{
			ReferenceTrackID: prftTestTrackID,
			NtpTimestamp:     prftNtp1,
			MediaTime:        0,
			MediaTimeSize:    64,
			Timescale:        prftTestTimescale,
		}}
		_, err := ResolveNtpTimestamp(hugeRefs, prftTestTrackID, math.MaxInt64+1)
		requireErrIs(t, err, ErrTimeOverflow)
	})

	t.Run("zero timescale anchor", func(t *testing.T) {
		bad := []*ProducerReferenceTime{{
			ReferenceTrackID: prftTestTrackID,
			NtpTimestamp:     prftNtp1,
			MediaTime:        1000000,
			MediaTimeSize:    64,
			Timescale:        0,
		}}
		_, err := ResolveNtpTimestamp(bad, prftTestTrackID, 1000000)
		requireErrIs(t, err, ErrZeroTimescale)
	})

	// Ensure the well-formed refs still resolve.
	_, err = ResolveNtpTimestamp(refs, prftTestTrackID, 1000090)
	require.NoError(t, err)
}

func TestNtpTimestampConversions(t *testing.T) {
	// A fixed instant with both seconds and a nonzero fractional part.
	ntp := uint64(0xe000000012345678)
	wt := NtpTimestampToTime(ntp)
	roundTrip, err := TimeToNtpTimestamp(wt)
	require.NoError(t, err)
	// Nanosecond rounding may differ by at most one NTP fractional unit
	// (~0.23 ns), so compare against the exact fraction recomputed from
	// the nanosecond value.
	wantFrac := new(big.Int).Mul(big.NewInt(int64(wt.Nanosecond())), big.NewInt(1<<32))
	wantFrac.Quo(wantFrac, big.NewInt(1e9))
	assert.Equal(t, ntp&^uint64(math.MaxUint32), roundTrip&^uint64(math.MaxUint32))
	assert.Equal(t, wantFrac.Uint64(), roundTrip&math.MaxUint32)

	// Whole-second instant converts exactly.
	whole := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	ntpWhole, err := TimeToNtpTimestamp(whole)
	require.NoError(t, err)
	assert.Equal(t, uint32(0), uint32(ntpWhole))
	assert.Equal(t, whole, NtpTimestampToTime(ntpWhole))

	// Out-of-range time (year 2036+) is rejected.
	_, err = TimeToNtpTimestamp(time.Date(2037, 1, 1, 0, 0, 0, 0, time.UTC))
	requireErrIs(t, err, ErrTimeOverflow)
}

func TestReadProducerReferenceTimesDoesNotModifyPayload(t *testing.T) {
	prft1 := &Prft{
		ReferenceTrackID: prftTestTrackID,
		NtpTimestamp:     prftNtp1,
		MediaTimeV1:      1000000,
	}
	prft1.SetVersion(1)
	data := buildPrftFixture(t, prftTestTrackID, prftTestTimescale, []prftFixtureParams{
		{sequenceNumber: 1, tfdtVersion: 1, tfdtBase: 1000000, prft: prft1},
	})
	original := make([]byte, len(data))
	copy(original, data)

	r := bytes.NewReader(data)
	_, err := ReadProducerReferenceTimes(r)
	require.NoError(t, err)
	assert.Equal(t, original, data)

	// The reader position and underlying bytes remain usable for another
	// full structure walk.
	_, err = ReadBoxStructure(r, func(h *ReadHandle) (interface{}, error) {
		return nil, nil
	})
	require.NoError(t, err)
}

func TestShiftNtpTimestampTruncatesFraction(t *testing.T) {
	// Non-integer number of NTP units must be truncated toward zero,
	// matching the big-integer reference implementation.
	ntp, err := shiftNtpTimestamp(prftNtp1, 1, prftTestTimescale)
	require.NoError(t, err)
	assert.Equal(t, shiftNtpBig(prftNtp1, 1, prftTestTimescale).Uint64(), ntp)
}

func TestTruncatedPrftRawBox(t *testing.T) {
	// A prft box whose declared size cannot hold its version 1 payload
	// must fail unmarshalling through the generic API as well.
	prft1 := &Prft{
		ReferenceTrackID: prftTestTrackID,
		NtpTimestamp:     prftNtp1,
		MediaTimeV1:      1000000,
	}
	prft1.SetVersion(1)
	full := bytes.NewBuffer(nil)
	_, err := Marshal(full, prft1, Context{})
	require.NoError(t, err)
	payload := full.Bytes()

	var dst Prft
	_, err = Unmarshal(bytes.NewReader(payload[:len(payload)-1]), uint64(len(payload)-1), &dst, Context{})
	require.Error(t, err)
	assert.True(t, errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF))
}
