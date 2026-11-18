package mp4

import (
	"errors"
	"fmt"
	"io"
)

// Sample grouping types defined by ISO/IEC 14496-12 for random access recovery.
var (
	groupingTypeRoll = [4]byte{'r', 'o', 'l', 'l'}
	groupingTypeProl = [4]byte{'p', 'r', 'o', 'l'}
)

// sample_flags: sample_depends_on is a 2-bit field at bits 14..13.
// The value 2 means that the sample depends on other samples (not a sync sample).
const sampleDependsOnMask uint32 = 0x0000C000

// Sentinel errors returned while building a RecoveryPointIndex. Callers can
// detect them with errors.Is.
var (
	// ErrDuplicateSampleGroupBox is returned when a sample table or a track
	// fragment contains more than one sgpd/sbgp box for the same grouping type.
	ErrDuplicateSampleGroupBox = errors.New("duplicate sample group box")
	// ErrMissingGroupDescription is returned when an sbgp entry references a
	// group description index that does not exist in the paired sgpd box.
	ErrMissingGroupDescription = errors.New("sample group description not found")
	// ErrInvalidRollDistance is returned when an sgpd entry carries an illegal
	// roll_distance (a positive value).
	ErrInvalidRollDistance = errors.New("invalid roll distance")
	// ErrGroupRunOverflow is returned when the sum of SampleCount values of an
	// sbgp box exceeds the number of samples in the sample table or fragment.
	ErrGroupRunOverflow = errors.New("sample-to-group run exceeds sample count")
	// ErrGroupConflict is returned when a roll/prol sample group is assigned
	// to a sample that is not a random access point according to the stss box
	// or the fragment sample flags.
	ErrGroupConflict = errors.New("sample group assigned to non-random-access sample")
)

// Sentinel errors returned while querying a RecoveryPointIndex.
var (
	// ErrSampleIndexOutOfRange is returned when the specified sample number is
	// not contained in the index. Sample numbers are zero-based.
	ErrSampleIndexOutOfRange = errors.New("sample index out of range")
	// ErrNoRecoveryPoint is returned when the specified sample is neither a
	// sync sample nor a member of a roll/prol sample group, so no safe playback
	// start position can be derived for it.
	ErrNoRecoveryPoint = errors.New("sample has no recovery point")
)

// RecoveryPoint describes where a player has to start decoding and displaying
// to begin playback at a given sample. All sample numbers are zero-based.
type RecoveryPoint struct {
	// SampleNumber is the sample this recovery point was requested for.
	SampleNumber int
	// DecodeStartIndex is the first sample that must be fed to the decoder.
	// For a roll/prol random access point it precedes SampleNumber by the
	// roll distance; for an ordinary sync sample it equals SampleNumber.
	DecodeStartIndex int
	// PrerollStartIndex is the first sample of the preroll range. It equals
	// DecodeStartIndex for grouped random access points and equals
	// SampleNumber for samples without preroll.
	PrerollStartIndex int
	// PrerollEndIndex is the exclusive end of the preroll range, i.e. samples
	// [PrerollStartIndex, PrerollEndIndex) are decoded but their output is not
	// presented. It equals SampleNumber for grouped random access points and
	// equals SampleNumber for samples without preroll (an empty range).
	PrerollEndIndex int
	// DisplayStartIndex is the first sample whose decoded output has to be
	// presented. It always equals SampleNumber.
	DisplayStartIndex int
	// IsRandomAccess indicates that the sample is a random access point,
	// either because it is an ordinary sync sample or because it belongs to a
	// roll/prol sample group.
	IsRandomAccess bool
	// RollDistance is the signed roll distance of the roll group. It is zero
	// when the sample does not belong to a roll group.
	RollDistance int16
	// PrerollDistance is the signed roll distance of the prol group. It is
	// zero when the sample does not belong to a prol group.
	PrerollDistance int16
	// Degraded is set when the referenced roll distance cannot be satisfied,
	// for example because the required preroll samples do not exist at the
	// beginning of the track or fragment. The index fields are clamped to the
	// first available sample and callers should treat the result as a best
	// effort rather than a spec-compliant recovery point.
	Degraded bool
}

// PrerollSampleCount returns the number of samples that must be decoded before
// the requested sample can be displayed.
func (rp *RecoveryPoint) PrerollSampleCount() int {
	return rp.PrerollEndIndex - rp.PrerollStartIndex
}

// RecoveryPointIndex maps every sample of a single sample table or track
// fragment to its recovery information derived from roll/prol sample groups
// (sgpd/sbgp) and the sync sample information (stss or trun/tfhd flags).
type RecoveryPointIndex struct {
	// TrackID is the track the index belongs to. It is zero when the index was
	// built directly from a sample table box.
	TrackID uint32
	// SampleCount is the number of samples covered by the index.
	SampleCount int
	// rollDistances[i] is the roll distance of the roll group assigned to
	// sample i, or zero when sample i is not a member of a roll group.
	rollDistances []int16
	// prolDistances[i] is the roll distance of the prol group assigned to
	// sample i, or zero when sample i is not a member of a prol group.
	prolDistances []int16
	// syncSamples[i] reports whether sample i is an ordinary sync sample
	// according to the stss box or the fragment sample flags.
	syncSamples []bool
	// noStss is true when no stss box was present, in which case ISO/IEC
	// 14496-12 defines every sample as a sync sample.
	noStss bool
}

// GetRecoveryPoint returns the recovery information for the specified sample.
// The sample number is zero-based.
//
// ErrSampleIndexOutOfRange is returned for a sample number outside the index.
// ErrNoRecoveryPoint is returned when the sample is neither a sync sample nor
// a member of a roll/prol group. A recovery point whose roll distance cannot
// be satisfied is returned together with the Degraded flag rather than an
// error.
func (idx *RecoveryPointIndex) GetRecoveryPoint(sampleNumber int) (*RecoveryPoint, error) {
	if sampleNumber < 0 || sampleNumber >= idx.SampleCount {
		return nil, fmt.Errorf("%w: sample=%d count=%d",
			ErrSampleIndexOutOfRange, sampleNumber, idx.SampleCount)
	}

	rollDist := idx.rollDistances[sampleNumber]
	prolDist := idx.prolDistances[sampleNumber]
	grouped := rollDist != 0 || prolDist != 0
	isSync := idx.noStss || idx.syncSamples[sampleNumber]
	if !grouped && !isSync {
		return nil, fmt.Errorf("%w: sample=%d", ErrNoRecoveryPoint, sampleNumber)
	}

	rp := &RecoveryPoint{
		SampleNumber:      sampleNumber,
		DisplayStartIndex: sampleNumber,
		RollDistance:      rollDist,
		PrerollDistance:   prolDist,
		IsRandomAccess:    grouped || isSync,
		DecodeStartIndex:  sampleNumber,
		PrerollStartIndex: sampleNumber,
		PrerollEndIndex:   sampleNumber,
	}

	// The roll distance determines how many preceding samples have to be
	// decoded. The prol distance is considered as well because a sample may
	// belong to both groups; the larger of the two distances wins.
	distance := rollDist
	if prolDist < distance {
		distance = prolDist
	}
	if distance < 0 {
		start := sampleNumber + int(distance)
		if start < 0 {
			rp.Degraded = true
			start = 0
		}
		rp.DecodeStartIndex = start
		rp.PrerollStartIndex = start
		rp.PrerollEndIndex = sampleNumber
	}

	return rp, nil
}

// BuildRecoveryPointIndex builds a recovery point index for the track that owns
// the sample table stbl. The roll/prol sgpd and sbgp boxes are read from stbl
// and ordinary sync samples are taken from the optional stss box. When stbl has
// no stss box, every sample is treated as a sync sample as defined by
// ISO/IEC 14496-12.
//
// The number of samples is determined by the stsz box. Missing sgpd/sbgp boxes
// simply mean that the track does not use roll/prol sample groups; malformed or
// conflicting boxes produce the errors documented on the sentinel errors.
func BuildRecoveryPointIndex(r io.ReadSeeker, stbl *BoxInfo) (*RecoveryPointIndex, error) {
	if stbl == nil {
		return nil, errors.New("stbl box is nil")
	}

	bips, err := ExtractBoxesWithPayload(r, stbl, []BoxPath{
		{BoxTypeSgpd()},
		{BoxTypeSbgp()},
		{BoxTypeStss()},
		{BoxTypeStsz()},
	})
	if err != nil {
		return nil, err
	}

	descriptions := make(map[[4]byte]*Sgpd)
	mappings := make(map[[4]byte]*Sbgp)
	var stss *Stss
	var stsz *Stsz
	for _, bip := range bips {
		switch box := bip.Payload.(type) {
		case *Sgpd:
			if err := registerSgpd(descriptions, box); err != nil {
				return nil, err
			}
		case *Sbgp:
			if err := registerSbgp(mappings, box); err != nil {
				return nil, err
			}
		case *Stss:
			if stss != nil {
				return nil, fmt.Errorf("%w: type=stss", ErrDuplicateSampleGroupBox)
			}
			stss = box
		case *Stsz:
			if stsz != nil {
				return nil, fmt.Errorf("%w: type=stsz", ErrDuplicateSampleGroupBox)
			}
			stsz = box
		}
	}

	if stsz == nil {
		return nil, errors.New("stsz box not found")
	}
	sampleCount := int(stsz.SampleCount)

	idx := newRecoveryPointIndex(sampleCount)
	if stss == nil {
		idx.noStss = true
	} else {
		for _, sampleNumber := range stss.SampleNumber {
			if sampleNumber < 1 || int(sampleNumber) > sampleCount {
				return nil, fmt.Errorf("%w: stss sample=%d count=%d",
					ErrGroupRunOverflow, sampleNumber, sampleCount)
			}
			idx.syncSamples[sampleNumber-1] = true
		}
	}

	for _, groupingType := range [][4]byte{groupingTypeRoll, groupingTypeProl} {
		if err := applyGrouping(idx, mappings[groupingType], descriptions[groupingType], groupingType); err != nil {
			return nil, err
		}
	}

	if err := idx.validateGroupedSamples(); err != nil {
		return nil, err
	}
	return idx, nil
}

// BuildFragmentRecoveryPointIndex builds a recovery point index for the samples
// of a single track fragment traf. The sgpd boxes are usually carried by the
// movie-level sample table, therefore they have to be passed explicitly; sbgp
// boxes inside traf provide the per-sample mapping. Sync sample information is
// derived from the trun sample flags and the tfhd default sample flags.
//
// Each traf must be indexed independently so that fragments and tracks are
// never mixed.
func BuildFragmentRecoveryPointIndex(r io.ReadSeeker, traf *BoxInfo, sgpdBoxes []*Sgpd) (*RecoveryPointIndex, error) {
	if traf == nil {
		return nil, errors.New("traf box is nil")
	}

	descriptions := make(map[[4]byte]*Sgpd)
	for _, sgpdBox := range sgpdBoxes {
		if sgpdBox == nil {
			continue
		}
		if err := registerSgpd(descriptions, sgpdBox); err != nil {
			return nil, err
		}
	}

	bips, err := ExtractBoxesWithPayload(r, traf, []BoxPath{
		{BoxTypeSbgp()},
		{BoxTypeTfhd()},
		{BoxTypeTrun()},
	})
	if err != nil {
		return nil, err
	}

	mappings := make(map[[4]byte]*Sbgp)
	var tfhd *Tfhd
	var trun *Trun
	for _, bip := range bips {
		switch box := bip.Payload.(type) {
		case *Sbgp:
			if err := registerSbgp(mappings, box); err != nil {
				return nil, err
			}
		case *Tfhd:
			if tfhd != nil {
				return nil, fmt.Errorf("%w: type=tfhd", ErrDuplicateSampleGroupBox)
			}
			tfhd = box
		case *Trun:
			if trun != nil {
				return nil, fmt.Errorf("%w: type=trun", ErrDuplicateSampleGroupBox)
			}
			trun = box
		}
	}
	if tfhd == nil {
		return nil, errors.New("tfhd box not found")
	}

	sampleCount := 0
	if trun != nil {
		sampleCount = int(trun.SampleCount)
	}
	idx := newRecoveryPointIndex(sampleCount)
	idx.TrackID = tfhd.TrackID

	for i := 0; i < sampleCount; i++ {
		idx.syncSamples[i] = !fragmentSampleIsNonSync(tfhd, trun, i)
	}

	for _, groupingType := range [][4]byte{groupingTypeRoll, groupingTypeProl} {
		if err := applyGrouping(idx, mappings[groupingType], descriptions[groupingType], groupingType); err != nil {
			return nil, err
		}
	}

	if err := idx.validateGroupedSamples(); err != nil {
		return nil, err
	}
	return idx, nil
}

func newRecoveryPointIndex(sampleCount int) *RecoveryPointIndex {
	return &RecoveryPointIndex{
		SampleCount:   sampleCount,
		rollDistances: make([]int16, sampleCount),
		prolDistances: make([]int16, sampleCount),
		syncSamples:   make([]bool, sampleCount),
	}
}

// validateGroupedSamples ensures that roll/prol groups are only attached to
// random access points, as required by ISO/IEC 14496-12. When no stss box is
// present every sample is a sync sample and the check always passes.
func (idx *RecoveryPointIndex) validateGroupedSamples() error {
	if idx.noStss {
		return nil
	}
	for i := 0; i < idx.SampleCount; i++ {
		if (idx.rollDistances[i] != 0 || idx.prolDistances[i] != 0) && !idx.syncSamples[i] {
			return fmt.Errorf("%w: sample=%d", ErrGroupConflict, i)
		}
	}
	return nil
}

func registerSgpd(descriptions map[[4]byte]*Sgpd, sgpdBox *Sgpd) error {
	groupingType := sgpdBox.GroupingType
	if _, exists := descriptions[groupingType]; exists {
		return fmt.Errorf("%w: type=sgpd groupingType=%s",
			ErrDuplicateSampleGroupBox, groupingTypeName(groupingType))
	}
	if groupingType == groupingTypeRoll || groupingType == groupingTypeProl {
		if _, err := rollDistancesOf(sgpdBox, groupingType); err != nil {
			return err
		}
	}
	descriptions[groupingType] = sgpdBox
	return nil
}

func registerSbgp(mappings map[[4]byte]*Sbgp, sbgpBox *Sbgp) error {
	groupingType := uint32ToBoxType(sbgpBox.GroupingType)
	if _, exists := mappings[groupingType]; exists {
		return fmt.Errorf("%w: type=sbgp groupingType=%s",
			ErrDuplicateSampleGroupBox, groupingTypeName(groupingType))
	}
	mappings[groupingType] = sbgpBox
	return nil
}

// rollDistancesOf extracts the signed roll distances of an roll/prol sgpd box,
// supporting both the DefaultLength layout and the per-entry
// description-length layout. roll distances must be zero or negative while
// positive-roll (prol) distances may be positive.
func rollDistancesOf(sgpdBox *Sgpd, groupingType [4]byte) ([]int16, error) {
	var distances []int16
	if len(sgpdBox.RollDistances) > 0 {
		distances = sgpdBox.RollDistances
	} else {
		distances = make([]int16, 0, len(sgpdBox.RollDistancesL))
		for _, entry := range sgpdBox.RollDistancesL {
			distances = append(distances, entry.RollDistance)
		}
	}
	if groupingType == groupingTypeRoll {
		for i, distance := range distances {
			if distance > 0 {
				return nil, fmt.Errorf("%w: index=%d distance=%d",
					ErrInvalidRollDistance, i, distance)
			}
		}
	}
	return distances, nil
}

// applyGrouping resolves one roll/prol grouping type on the index. It covers
// both the explicit per-sample mapping of an sbgp box and the sgpd version 2
// default group description, which applies to every sample when no sbgp box
// for that grouping type is present.
func applyGrouping(idx *RecoveryPointIndex, sbgpBox *Sbgp, sgpdBox *Sgpd, groupingType [4]byte) error {
	if sbgpBox == nil && sgpdBox == nil {
		return nil
	}
	if sbgpBox != nil && sgpdBox == nil {
		return fmt.Errorf("%w: groupingType=%s",
			ErrMissingGroupDescription, groupingTypeName(groupingType))
	}

	distances, err := rollDistancesOf(sgpdBox, groupingType)
	if err != nil {
		return err
	}

	if sbgpBox != nil {
		return applySbgpRuns(idx, sbgpBox, distances, groupingType)
	}

	// No sbgp box: only the sgpd v2 default group description can assign
	// samples. A zero value means that no default group is defined.
	defaultIndex := sgpdBox.DefaultSampleDescriptionIndex
	if defaultIndex == 0 {
		return nil
	}
	if int(defaultIndex) > len(distances) {
		return fmt.Errorf("%w: default index=%d entries=%d",
			ErrMissingGroupDescription, defaultIndex, len(distances))
	}
	distance := distances[defaultIndex-1]
	for sampleNumber := 0; sampleNumber < idx.SampleCount; sampleNumber++ {
		switch groupingType {
		case groupingTypeRoll:
			idx.rollDistances[sampleNumber] = distance
		case groupingTypeProl:
			idx.prolDistances[sampleNumber] = distance
		}
	}
	return nil
}

// applySbgpRuns resolves the runs of an sbgp box and writes the referenced
// roll distance into the per-sample arrays of the index.
func applySbgpRuns(idx *RecoveryPointIndex, sbgpBox *Sbgp, distances []int16, groupingType [4]byte) error {
	position := 0
	for runIndex, entry := range sbgpBox.Entries {
		if entry.SampleCount == 0 {
			continue
		}
		end := position + int(entry.SampleCount)
		if end > idx.SampleCount {
			return fmt.Errorf("%w: groupingType=%s run=%d",
				ErrGroupRunOverflow, groupingTypeName(groupingType), runIndex)
		}

		var distance int16
		if entry.GroupDescriptionIndex == 0 {
			// ISO/IEC 14496-12: a group_description_index of zero means that
			// the samples of this run are not members of any group. The
			// position still has to advance so that later runs line up.
			position = end
			continue
		}
		if int(entry.GroupDescriptionIndex) > len(distances) {
			return fmt.Errorf("%w: index=%d entries=%d",
				ErrMissingGroupDescription, entry.GroupDescriptionIndex, len(distances))
		}
		distance = distances[entry.GroupDescriptionIndex-1]

		for sampleNumber := position; sampleNumber < end; sampleNumber++ {
			switch groupingType {
			case groupingTypeRoll:
				idx.rollDistances[sampleNumber] = distance
			case groupingTypeProl:
				idx.prolDistances[sampleNumber] = distance
			}
		}
		position = end
	}
	return nil
}

// fragmentSampleIsNonSync reports whether the sample_depends_on bits of the
// effective sample flags mark the sample as dependent on other samples.
func fragmentSampleIsNonSync(tfhd *Tfhd, trun *Trun, sampleNumber int) bool {
	var flags uint32
	hasFlags := false
	if trun != nil && trun.CheckFlag(0x000400) {
		if sampleNumber < len(trun.Entries) {
			flags = trun.Entries[sampleNumber].SampleFlags
			hasFlags = true
		}
	} else if sampleNumber == 0 && trun != nil && trun.CheckFlag(0x000004) {
		flags = trun.FirstSampleFlags
		hasFlags = true
	}
	if !hasFlags && tfhd.CheckFlag(TfhdDefaultSampleFlagsPresent) {
		flags = tfhd.DefaultSampleFlags
		hasFlags = true
	}
	if !hasFlags {
		// No flag information available: treat the sample as sync, matching
		// the no-stss rule of ISO/IEC 14496-12.
		return false
	}
	return (flags&sampleDependsOnMask)>>14 == 2
}

func uint32ToBoxType(value uint32) [4]byte {
	return [4]byte{
		byte(value >> 24),
		byte(value >> 16),
		byte(value >> 8),
		byte(value),
	}
}

func groupingTypeName(groupingType [4]byte) string {
	return string(groupingType[:])
}
