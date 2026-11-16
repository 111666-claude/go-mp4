package mp4

/*************************** prft ****************************/

func BoxTypePrft() BoxType { return StrToBoxType("prft") }

func init() {
	AddBoxDef(&Prft{}, 0, 1)
}

// Prft is ISOBMFF prft (Producer Reference Time) box type.
// It associates a point on a track's media timeline with a wall-clock
// NTP timestamp captured by the producer, enabling synchronization with
// external event logs.
type Prft struct {
	FullBox          `mp4:"0,extend"`
	ReferenceTrackID uint32 `mp4:"1,size=32"`
	NtpTimestamp     uint64 `mp4:"2,size=64"`
	MediaTimeV0      uint32 `mp4:"3,size=32,ver=0"`
	MediaTimeV1      uint64 `mp4:"4,size=64,nver=0"`
}

// GetType returns the BoxType
func (*Prft) GetType() BoxType {
	return BoxTypePrft()
}

// GetMediaTime returns the media time stored in the box.
// Version 0 stores a 32-bit value, while version 1 stores a 64-bit value.
func (prft *Prft) GetMediaTime() uint64 {
	switch prft.GetVersion() {
	case 0:
		return uint64(prft.MediaTimeV0)
	case 1:
		return prft.MediaTimeV1
	default:
		return 0
	}
}
