package mp4

import (
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"time"
)

// Errors returned by the producer reference time (prft) helpers.
var (
	// ErrPrftNotFound is returned when a file contains no usable prft box.
	ErrPrftNotFound = errors.New("prft box not found")
	// ErrUnknownTrack is returned when a prft box or the requested track
	// references a track ID that does not exist in the moov box.
	ErrUnknownTrack = errors.New("unknown track ID")
	// ErrZeroTimescale is returned when a referenced track has a zero
	// media timescale, which makes time conversion impossible.
	ErrZeroTimescale = errors.New("zero media timescale")
	// ErrTruncatedBox is returned when a box required for time alignment
	// does not contain enough payload bytes for its declared fields.
	ErrTruncatedBox = errors.New("truncated box")
	// ErrMediaTimeWraparound is returned when a 32-bit media-time field
	// cannot be expanded to 64 bits unambiguously, or when the requested
	// media time is too far from the nearest prft anchor.
	ErrMediaTimeWraparound = errors.New("media time wraparound ambiguity")
	// ErrTimeOverflow is returned when wall-clock conversion would overflow
	// the 64-bit NTP timestamp space.
	ErrTimeOverflow = errors.New("time overflow")
)

// ntpFractionDen is the denominator of the 32-bit fractional part of an
// NTP timestamp (RFC 5905, section 6).
var ntpFractionDen = big.NewInt(1 << 32)

// ntpUnixEpochSeconds is the number of NTP seconds at the UNIX epoch
// (1970-01-01T00:00:00Z). NTP counts seconds from 1900-01-01.
const ntpUnixEpochSeconds = 2208988800

// ProducerReferenceTime is a single prft anchor reconstructed from a
// fragmented MP4 file.
type ProducerReferenceTime struct {
	// MoofOffset is the file offset of the moof box containing this prft
	// box, when it could be determined.
	MoofOffset uint64
	// ReferenceTrackID is the track the media time belongs to.
	ReferenceTrackID uint32
	// NtpTimestamp is the wall-clock time in NTP timestamp format: the
	// upper 32 bits are seconds since 1900-01-01 and the lower 32 bits
	// are the fractional seconds.
	NtpTimestamp uint64
	// MediaTime is the media timeline point corresponding to
	// NtpTimestamp, expanded to 64 bits. For a version 0 prft box the
	// expansion is derived from the fragment's tfdt.
	MediaTime uint64
	// MediaTimeSize is the on-wire width of MediaTime: 32 for a version 0
	// prft box and 64 for a version 1 box.
	MediaTimeSize uint
	// Timescale is the reference track's media timescale from mdhd.
	Timescale uint32
}

// minimum payload sizes (excluding the 8/16-byte box header)
const (
	prftV0PayloadSize = 4 /* full box */ + 4 /* track ID */ + 8 /* NTP */ + 4 /* media time */
	prftV1PayloadSize = 4 + 4 + 8 + 8
	tfdtV0PayloadSize = 4 + 4
	tfdtV1PayloadSize = 4 + 8
)

// ReadProducerReferenceTimes extracts every prft anchor from a fragmented
// MP4 stream. The moov box must exist and define every track referenced by
// a prft box, because track timescales are taken from mdhd.
//
// prft boxes may be placed directly in a moof box (referencing any track)
// or inside a traf box (referencing that traf's track); both placements
// are supported. The media payload is never read or modified.
func ReadProducerReferenceTimes(r io.ReadSeeker) ([]*ProducerReferenceTime, error) {
	timescales, err := readTrackTimescales(r)
	if err != nil {
		return nil, err
	}

	moofs, err := ExtractBox(r, nil, BoxPath{BoxTypeMoof()})
	if err != nil {
		return nil, wrapTruncated(err)
	}

	refs := make([]*ProducerReferenceTime, 0)
	for _, moof := range moofs {
		moofRefs, err := readMoofPrfts(r, moof, timescales)
		if err != nil {
			return nil, err
		}
		refs = append(refs, moofRefs...)
	}

	if len(refs) == 0 {
		return nil, ErrPrftNotFound
	}
	return refs, nil
}

func readTrackTimescales(r io.ReadSeeker) (map[uint32]uint32, error) {
	traks, err := ExtractBox(r, nil, BoxPath{BoxTypeMoov(), BoxTypeTrak()})
	if err != nil {
		return nil, wrapTruncated(err)
	}
	if len(traks) == 0 {
		return nil, errors.New("moov/trak box not found")
	}

	timescales := make(map[uint32]uint32, len(traks))
	for _, trak := range traks {
		bips, err := ExtractBoxesWithPayload(r, trak, []BoxPath{
			{BoxTypeTkhd()},
			{BoxTypeMdia(), BoxTypeMdhd()},
		})
		if err != nil {
			return nil, wrapTruncated(err)
		}
		var tkhd *Tkhd
		var mdhd *Mdhd
		for _, bip := range bips {
			switch bip.Info.Type {
			case BoxTypeTkhd():
				tkhd = bip.Payload.(*Tkhd)
			case BoxTypeMdhd():
				mdhd = bip.Payload.(*Mdhd)
			}
		}
		if tkhd == nil {
			return nil, errors.New("tkhd box not found")
		}
		if mdhd == nil {
			return nil, errors.New("mdhd box not found")
		}
		timescales[tkhd.TrackID] = mdhd.Timescale
	}
	return timescales, nil
}

// trafData accumulates the boxes of one traf while scanning a moof.
type trafData struct {
	info     *BoxInfo
	trackID  uint32
	tfhdSeen bool
	tfdt     *Tfdt
	tfdtInfo *BoxInfo
	prfts    []*Prft
	prftInfo []*BoxInfo
}

// contains reports whether the box at offset off lies inside this traf.
func (td *trafData) contains(off uint64) bool {
	return td.info.Offset <= off && off < td.info.Offset+td.info.Size
}

func readMoofPrfts(r io.ReadSeeker, moof *BoxInfo, timescales map[uint32]uint32) ([]*ProducerReferenceTime, error) {
	bips, err := ExtractBoxesWithPayload(r, moof, []BoxPath{
		{BoxTypePrft()},
		{BoxTypeTraf()},
		{BoxTypeTraf(), BoxTypePrft()},
		{BoxTypeTraf(), BoxTypeTfhd()},
		{BoxTypeTraf(), BoxTypeTfdt()},
	})
	if err != nil {
		return nil, wrapTruncated(err)
	}

	var trafs []*trafData
	var moofPrfts []*Prft
	var moofPrftInfos []*BoxInfo

	for _, bip := range bips {
		bi := bip.Info
		switch bi.Type {
		case BoxTypeTraf():
			trafs = append(trafs, &trafData{info: &bi})

		case BoxTypeTfhd():
			if td := findTraf(trafs, bi.Offset); td != nil {
				td.trackID = bip.Payload.(*Tfhd).TrackID
				td.tfhdSeen = true
			}

		case BoxTypeTfdt():
			if td := findTraf(trafs, bi.Offset); td != nil {
				payload := bip.Payload.(*Tfdt)
				td.tfdt = payload
				td.tfdtInfo = &bi
			}

		case BoxTypePrft():
			prft := bip.Payload.(*Prft)
			if td := findTraf(trafs, bi.Offset); td != nil {
				td.prfts = append(td.prfts, prft)
				td.prftInfo = append(td.prftInfo, &bi)
			} else {
				moofPrfts = append(moofPrfts, prft)
				moofPrftInfos = append(moofPrftInfos, &bi)
			}
		}
	}

	refs := make([]*ProducerReferenceTime, 0, len(moofPrfts)+len(trafs))

	// moof-level prft boxes: resolve each against the traf of the
	// referenced track (for 32-bit media-time expansion).
	for i, prft := range moofPrfts {
		ref, err := buildProducerReferenceTime(
			prft, moofPrftInfos[i], moof.Offset,
			findTrafByTrackID(trafs, prft.ReferenceTrackID),
			timescales,
		)
		if err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}

	// traf-level prft boxes.
	for _, td := range trafs {
		for i, prft := range td.prfts {
			ref, err := buildProducerReferenceTime(prft, td.prftInfo[i], moof.Offset, td, timescales)
			if err != nil {
				return nil, err
			}
			refs = append(refs, ref)
		}
	}

	return refs, nil
}

func findTraf(trafs []*trafData, offset uint64) *trafData {
	for _, td := range trafs {
		if td.contains(offset) {
			return td
		}
	}
	return nil
}

func findTrafByTrackID(trafs []*trafData, trackID uint32) *trafData {
	for _, td := range trafs {
		if td.tfhdSeen && td.trackID == trackID {
			return td
		}
	}
	return nil
}

func buildProducerReferenceTime(
	prft *Prft,
	prftInfo *BoxInfo,
	moofOffset uint64,
	traf *trafData,
	timescales map[uint32]uint32,
) (*ProducerReferenceTime, error) {
	if err := checkPrftSize(prftInfo, prft.Version); err != nil {
		return nil, err
	}

	timescale, ok := timescales[prft.ReferenceTrackID]
	if !ok {
		return nil, fmt.Errorf("%w: trackID=%d", ErrUnknownTrack, prft.ReferenceTrackID)
	}
	if timescale == 0 {
		return nil, fmt.Errorf("%w: trackID=%d", ErrZeroTimescale, prft.ReferenceTrackID)
	}

	mediaTimeSize := uint(64)
	mediaTime := prft.GetMediaTime()
	if prft.Version == 0 {
		mediaTimeSize = 32
		expanded, err := expandMediaTimeV0(prft, prftInfo, traf)
		if err != nil {
			return nil, err
		}
		mediaTime = expanded
	}

	return &ProducerReferenceTime{
		MoofOffset:       moofOffset,
		ReferenceTrackID: prft.ReferenceTrackID,
		NtpTimestamp:     prft.NtpTimestamp,
		MediaTime:        mediaTime,
		MediaTimeSize:    mediaTimeSize,
		Timescale:        timescale,
	}, nil
}

// expandMediaTimeV0 reconstructs the full 64-bit media time of a version 0
// prft box using the containing fragment's tfdt base media decode time.
// Only the low 32 bits are carried on the wire, so anything that cannot be
// mapped to a single 32-bit window around the tfdt base is reported as a
// wraparound ambiguity instead of being guessed.
func expandMediaTimeV0(prft *Prft, prftInfo *BoxInfo, traf *trafData) (uint64, error) {
	stored := uint64(prft.MediaTimeV0)

	if traf == nil || traf.tfdt == nil {
		return 0, fmt.Errorf("%w: prft at offset=%d: no tfdt found for 32-bit media time expansion",
			ErrMediaTimeWraparound, prftInfo.Offset)
	}
	if err := checkTfdtSize(traf.tfdtInfo, traf.tfdt.Version); err != nil {
		return 0, err
	}

	base := traf.tfdt.GetBaseMediaDecodeTime()

	if traf.tfdt.Version == 1 {
		// The 64-bit tfdt pins the high word. The prft value must fall in
		// the same 32-bit window (or exactly one window above it).
		high := base &^ uint64(math.MaxUint32)
		candidate := high | stored
		if candidate < base {
			candidate += uint64(1) << 32
		}
		if distance64(candidate, base) > math.MaxInt32 {
			return 0, fmt.Errorf("%w: prft media time 0x%08x cannot be aligned with tfdt 0x%016x",
				ErrMediaTimeWraparound, stored, base)
		}
		return candidate, nil
	}

	// Both fields are 32-bit: nothing on the wire disambiguates a wrap.
	// Require the values to lie within one signed 32-bit window.
	if distance32(uint32(stored), uint32(base)) > math.MaxInt32 {
		return 0, fmt.Errorf("%w: prft media time 0x%08x vs tfdt base 0x%08x",
			ErrMediaTimeWraparound, stored, uint32(base))
	}
	return stored, nil
}

// distance32 returns the absolute distance between two 32-bit timeline
// values.
func distance32(a, b uint32) uint64 {
	if a > b {
		return uint64(a - b)
	}
	return uint64(b - a)
}

func distance64(a, b uint64) uint64 {
	if a > b {
		return a - b
	}
	return b - a
}

func checkPrftSize(bi *BoxInfo, version uint8) error {
	min := uint64(prftV0PayloadSize)
	if version == 1 {
		min = prftV1PayloadSize
	}
	return checkPayloadMinSize(bi, "prft", min)
}

func checkTfdtSize(bi *BoxInfo, version uint8) error {
	if bi == nil {
		return nil
	}
	min := uint64(tfdtV0PayloadSize)
	if version == 1 {
		min = tfdtV1PayloadSize
	}
	return checkPayloadMinSize(bi, "tfdt", min)
}

func checkPayloadMinSize(bi *BoxInfo, name string, min uint64) error {
	payload := bi.Size - bi.HeaderSize
	if payload < min {
		return fmt.Errorf("%w: type=%s offset=%d payloadSize=%d minimumSize=%d",
			ErrTruncatedBox, name, bi.Offset, payload, min)
	}
	return nil
}

func wrapTruncated(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("%w: %v", ErrTruncatedBox, err)
	}
	return err
}

// ResolveNtpTimestamp converts a media time on trackID's timeline to the
// corresponding NTP wall-clock timestamp, using the supplied prft anchors.
//
// The anchors normally come from ReadProducerReferenceTimes and may span
// multiple consecutive fragments: the anchor whose media time is closest
// without being after the requested time is used. Media payloads are not
// required or inspected.
func ResolveNtpTimestamp(refs []*ProducerReferenceTime, trackID uint32, mediaTime uint64) (uint64, error) {
	var anchor *ProducerReferenceTime
	for _, ref := range refs {
		if ref.ReferenceTrackID != trackID {
			continue
		}
		if ref.Timescale == 0 {
			return 0, fmt.Errorf("%w: trackID=%d", ErrZeroTimescale, trackID)
		}
		if anchor == nil {
			anchor = ref
			continue
		}
		// Prefer the latest anchor not later than mediaTime; while no such
		// anchor exists keep the earliest future anchor.
		if ref.MediaTime <= mediaTime {
			if anchor.MediaTime > mediaTime || ref.MediaTime > anchor.MediaTime {
				anchor = ref
			}
		} else if anchor.MediaTime > mediaTime && ref.MediaTime < anchor.MediaTime {
			anchor = ref
		}
	}
	if anchor == nil {
		return 0, fmt.Errorf("%w: no prft anchor for trackID=%d", ErrUnknownTrack, trackID)
	}

	// A 32-bit anchor is only unambiguous within one signed 32-bit window.
	if anchor.MediaTimeSize == 32 && distance64(anchor.MediaTime, mediaTime) > math.MaxInt32 {
		return 0, fmt.Errorf("%w: mediaTime=%d anchorMediaTime=%d",
			ErrMediaTimeWraparound, mediaTime, anchor.MediaTime)
	}

	delta, err := signedDelta(mediaTime, anchor.MediaTime)
	if err != nil {
		return 0, err
	}

	ntp, err := shiftNtpTimestamp(anchor.NtpTimestamp, delta, anchor.Timescale)
	if err != nil {
		return 0, err
	}
	return ntp, nil
}

// ResolveWallClock is a convenience wrapper around ResolveNtpTimestamp that
// returns the wall-clock time as a time.Time in UTC.
func ResolveWallClock(refs []*ProducerReferenceTime, trackID uint32, mediaTime uint64) (time.Time, error) {
	ntp, err := ResolveNtpTimestamp(refs, trackID, mediaTime)
	if err != nil {
		return time.Time{}, err
	}
	return NtpTimestampToTime(ntp), nil
}

func signedDelta(mediaTime, anchorTime uint64) (int64, error) {
	if mediaTime >= anchorTime {
		d := mediaTime - anchorTime
		if d > math.MaxInt64 {
			return 0, fmt.Errorf("%w: media delta %d", ErrTimeOverflow, d)
		}
		return int64(d), nil
	}
	d := anchorTime - mediaTime
	if d > math.MaxInt64 {
		return 0, fmt.Errorf("%w: media delta %d", ErrTimeOverflow, d)
	}
	return -int64(d), nil
}

// shiftNtpTimestamp adds delta media-time units (in the given timescale) to
// an NTP timestamp, performing the arithmetic in arbitrary precision so
// that 64-bit overflows are detected instead of wrapping silently.
func shiftNtpTimestamp(ntp uint64, delta int64, timescale uint32) (uint64, error) {
	if timescale == 0 {
		return 0, ErrZeroTimescale
	}

	cur := new(big.Int).SetUint64(ntp)
	d := big.NewInt(delta)
	d.Lsh(d, 32)
	d.Quo(d, big.NewInt(int64(timescale)))

	res := new(big.Int).Add(cur, d)
	if res.Sign() < 0 {
		return 0, fmt.Errorf("%w: NTP result before 1900", ErrTimeOverflow)
	}
	if !res.IsUint64() {
		return 0, fmt.Errorf("%w: NTP timestamp exceeds 64 bits", ErrTimeOverflow)
	}
	return res.Uint64(), nil
}

// NtpTimestampToTime converts an NTP timestamp (32-bit seconds since
// 1900-01-01 plus 32-bit fractional seconds) to a time.Time.
func NtpTimestampToTime(ntp uint64) time.Time {
	secs := int64(ntp >> 32)
	frac := uint32(ntp)
	nanos := int64(math.Round(float64(frac) * 1e9 / float64(uint64(1)<<32)))
	return time.Unix(secs-ntpUnixEpochSeconds, nanos).UTC()
}

// TimeToNtpTimestamp converts a time.Time to an NTP timestamp (32-bit
// seconds since 1900-01-01 plus 32-bit fractional seconds). It is the
// inverse of NtpTimestampToTime, up to nanosecond rounding.
func TimeToNtpTimestamp(t time.Time) (uint64, error) {
	secs := t.UTC().Unix() + ntpUnixEpochSeconds
	if secs < 0 || secs > math.MaxUint32 {
		return 0, fmt.Errorf("%w: time %s is outside the 32-bit NTP seconds range", ErrTimeOverflow, t.UTC())
	}
	frac := new(big.Int).Mul(big.NewInt(int64(t.UTC().Nanosecond())), ntpFractionDen)
	frac.Quo(frac, big.NewInt(1e9))
	return uint64(secs)<<32 | frac.Uint64(), nil
}
