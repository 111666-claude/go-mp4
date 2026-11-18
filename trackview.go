package mp4

import (
	"errors"
	"fmt"
	"io"
	"strings"
)

// Media types reported by TrackCandidate.MediaType.
// They correspond to the handler_type of the mdia/hdlr box.
const (
	MediaTypeVideo = "vide"
	MediaTypeAudio = "soun"
	MediaTypeHint  = "hint"
	MediaTypeMeta  = "meta"
	MediaTypeText  = "text"
)

// Sentinel errors used by ProbeTrackView. Fatal structural problems are
// returned as the error of ProbeTrackView itself; recoverable degradations
// are collected in TrackView.Issues and can be tested with errors.Is.
var (
	ErrMoovNotFound              = errors.New("moov box not found")
	ErrTkhdNotFound              = errors.New("tkhd box not found")
	ErrDuplicateTrackID          = errors.New("duplicate track ID")
	ErrInvalidTrackHierarchy     = errors.New("invalid box hierarchy in trak")
	ErrMdhdNotFound              = errors.New("mdhd box not found")
	ErrHdlrNotFound              = errors.New("hdlr box not found")
	ErrInvalidLanguageCode       = errors.New("invalid ISO 639-2/T language code")
	ErrConflictingAlternateGroup = errors.New("alternate group shared by different media types")
)

// TrackCandidate is a per-track view of the fields relevant for track
// selection: identity, media type, language, enabled state and the
// alternate group it belongs to.
type TrackCandidate struct {
	TrackID   uint32
	MediaType string // handler_type of mdia/hdlr ("vide", "soun", ...); empty when hdlr is missing
	// Language is the ISO 639-2/T code from mdia/mdhd (e.g. "eng", "zho").
	// It is empty when mdhd is missing or carries an undecodable code.
	Language      string
	LanguageValid bool // false when mdhd carries an undecodable language code
	Enabled       bool // tkhd track_enabled flag
	// AlternateGroup is the tkhd alternate_group value. Zero means the
	// track is not part of any alternate group and is never merged with
	// other zero-group tracks.
	AlternateGroup int16
}

// TrackGroup is a set of mutually exclusive track candidates: at most one
// of them should be selected for playback. Tracks whose alternate_group is
// zero each form a single-candidate group.
type TrackGroup struct {
	MediaType      string
	AlternateGroup int16
	Candidates     []*TrackCandidate // in file (moov/trak) order
}

// TrackView is a selection-oriented view over the tracks of moov.
type TrackView struct {
	Tracks []*TrackCandidate // all tracks, in file (moov/trak) order
	// Groups holds the alternate-group candidates. Tracks with
	// alternate_group == 0 appear as single-candidate groups and are never
	// merged with each other.
	Groups []*TrackGroup
	// Issues collects non-fatal degradations found while probing
	// (missing mdhd/hdlr, bad language codes, conflicting groups).
	Issues []error
}

// ProbeTrackView traverses moov/trak and builds a TrackView describing
// every track's ID, media type, language, enabled state and alternate
// group, plus the candidate groups used for track selection.
//
// Fatal problems (missing moov/tkhd, duplicated track IDs, boxes at
// illegal positions in the hierarchy) are returned as errors. Missing
// mdhd/hdlr boxes, undecodable language codes and conflicting alternate
// groups degrade the affected fields and are reported in TrackView.Issues.
func ProbeTrackView(r io.ReadSeeker) (*TrackView, error) {
	bis, err := ExtractBoxes(r, nil, []BoxPath{
		{BoxTypeMoov()},
		{BoxTypeMoov(), BoxTypeTrak()},
	})
	if err != nil {
		return nil, err
	}

	var moovFound bool
	view := &TrackView{
		Tracks: make([]*TrackCandidate, 0, 8),
		Groups: make([]*TrackGroup, 0, 8),
	}
	trackIDs := make(map[uint32]struct{})
	for _, bi := range bis {
		switch bi.Type {
		case BoxTypeMoov():
			moovFound = true
		case BoxTypeTrak():
			candidate, err := probeTrackCandidate(r, bi, view)
			if err != nil {
				return nil, err
			}
			if _, ok := trackIDs[candidate.TrackID]; ok {
				return nil, fmt.Errorf("track ID %d: %w", candidate.TrackID, ErrDuplicateTrackID)
			}
			trackIDs[candidate.TrackID] = struct{}{}
			view.Tracks = append(view.Tracks, candidate)
		}
	}
	if !moovFound {
		return nil, ErrMoovNotFound
	}

	view.buildGroups()
	return view, nil
}

func probeTrackCandidate(r io.ReadSeeker, trak *BoxInfo, view *TrackView) (*TrackCandidate, error) {
	// Boxes that are only legal at a deeper nesting level must not appear
	// here; treat such files as structurally broken.
	illegal, err := ExtractBoxes(r, trak, []BoxPath{
		{BoxTypeMdhd()},
		{BoxTypeHdlr()},
		{BoxTypeMdia(), BoxTypeTkhd()},
	})
	if err != nil {
		return nil, err
	}
	if len(illegal) != 0 {
		return nil, fmt.Errorf("%s at offset %d: %w",
			illegal[0].Type.String(), illegal[0].Offset, ErrInvalidTrackHierarchy)
	}

	bips, err := ExtractBoxesWithPayload(r, trak, []BoxPath{
		{BoxTypeTkhd()},
		{BoxTypeMdia(), BoxTypeMdhd()},
		{BoxTypeMdia(), BoxTypeHdlr()},
	})
	if err != nil {
		return nil, err
	}

	var tkhd *Tkhd
	var mdhd *Mdhd
	var hdlr *Hdlr
	for _, bip := range bips {
		switch bip.Info.Type {
		case BoxTypeTkhd():
			tkhd = bip.Payload.(*Tkhd)
		case BoxTypeMdhd():
			mdhd = bip.Payload.(*Mdhd)
		case BoxTypeHdlr():
			hdlr = bip.Payload.(*Hdlr)
		}
	}

	if tkhd == nil {
		return nil, ErrTkhdNotFound
	}

	candidate := &TrackCandidate{
		TrackID:        tkhd.TrackID,
		Enabled:        tkhd.GetFlags()&0x1 != 0,
		AlternateGroup: tkhd.AlternateGroup,
	}

	if mdhd == nil {
		view.Issues = append(view.Issues,
			fmt.Errorf("track ID %d: %w", candidate.TrackID, ErrMdhdNotFound))
	} else {
		lang, ok := decodeISO639_2(mdhd.Language)
		if !ok {
			view.Issues = append(view.Issues,
				fmt.Errorf("track ID %d: %w", candidate.TrackID, ErrInvalidLanguageCode))
		} else {
			candidate.Language = lang
			candidate.LanguageValid = true
		}
	}

	if hdlr == nil {
		view.Issues = append(view.Issues,
			fmt.Errorf("track ID %d: %w", candidate.TrackID, ErrHdlrNotFound))
	} else {
		candidate.MediaType = string(hdlr.HandlerType[:])
	}

	return candidate, nil
}

// decodeISO639_2 converts the packed 5-bit ISO 639-2/T representation used
// by mdhd into a lower-case 3-letter code. It reports false when any of the
// packed values falls outside the 'a'..'z' range.
func decodeISO639_2(packed [3]byte) (string, bool) {
	var lang [3]byte
	for i, v := range packed {
		if v > 'z'-'a' {
			return "", false
		}
		lang[i] = v + 'a'
	}
	return string(lang[:]), true
}

// buildGroups groups candidates by (media type, alternate group) while
// preserving file order. Tracks with alternate_group == 0 are never merged:
// each becomes a single-candidate group. A non-zero group ID shared by
// different media types is reported as a conflict and the tracks are kept
// in separate groups.
func (view *TrackView) buildGroups() {
	type groupKey struct {
		mediaType string
		group     int16
	}
	groups := make(map[groupKey]*TrackGroup)
	groupMediaTypes := make(map[int16]string) // non-zero group ID -> first media type
	conflicted := make(map[int16]struct{})    // group IDs already reported
	for _, candidate := range view.Tracks {
		if candidate.AlternateGroup != 0 {
			if mt, ok := groupMediaTypes[candidate.AlternateGroup]; ok && mt != candidate.MediaType {
				if _, reported := conflicted[candidate.AlternateGroup]; !reported {
					conflicted[candidate.AlternateGroup] = struct{}{}
					view.Issues = append(view.Issues, fmt.Errorf(
						"alternate group %d used by both %q and %q tracks: %w",
						candidate.AlternateGroup, mt, candidate.MediaType,
						ErrConflictingAlternateGroup))
				}
			} else if !ok {
				groupMediaTypes[candidate.AlternateGroup] = candidate.MediaType
			}
		}

		key := groupKey{mediaType: candidate.MediaType, group: candidate.AlternateGroup}
		if candidate.AlternateGroup == 0 {
			// never merge zero-group tracks: use a unique key per track
			key = groupKey{mediaType: candidate.MediaType, group: int16(len(view.Groups) + 1)}
		}
		if g, ok := groups[key]; ok && candidate.AlternateGroup != 0 {
			g.Candidates = append(g.Candidates, candidate)
			continue
		}
		g := &TrackGroup{
			MediaType:      candidate.MediaType,
			AlternateGroup: candidate.AlternateGroup,
			Candidates:     []*TrackCandidate{candidate},
		}
		if candidate.AlternateGroup != 0 {
			groups[key] = g
		}
		view.Groups = append(view.Groups, g)
	}
}

// Select chooses a track of the given media type ("vide", "soun", ...)
// according to an ordered list of preferred ISO 639-2/T language codes
// (matched case-insensitively). Disabled tracks are only considered when
// no enabled track of the media type exists. Among the remaining
// candidates the one whose language appears earliest in langPriority wins;
// candidates whose language is not listed rank below all listed ones, and
// ties are broken by file order. It returns nil when no track of the media
// type exists.
func (view *TrackView) Select(mediaType string, langPriority ...string) *TrackCandidate {
	priority := make(map[string]int, len(langPriority))
	for i, lang := range langPriority {
		lang = strings.ToLower(lang)
		if _, ok := priority[lang]; !ok {
			priority[lang] = i
		}
	}

	var best *TrackCandidate
	bestRank := len(langPriority) + 1
	for _, candidate := range view.Tracks {
		if candidate.MediaType != mediaType {
			continue
		}
		if best != nil && !candidate.Enabled && best.Enabled {
			continue
		}
		rank, ok := priority[strings.ToLower(candidate.Language)]
		if !ok {
			rank = len(langPriority)
		}
		if best == nil ||
			(candidate.Enabled && !best.Enabled) ||
			(candidate.Enabled == best.Enabled && rank < bestRank) {
			best, bestRank = candidate, rank
		}
	}
	return best
}
