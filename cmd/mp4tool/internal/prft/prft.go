package prft

import (
	"flag"
	"fmt"
	"os"

	mp4 "github.com/abema/go-mp4"
	"github.com/sunfish-shogi/bufseekio"
)

// Main is the entry point of the "mp4tool prft" subcommand.
func Main(args []string) int {
	flagSet := flag.NewFlagSet("prft", flag.ExitOnError)
	trackID := flagSet.Uint("track-id", 0, "reference track ID (required for -media-time queries)")
	mediaTime := flagSet.Uint64("media-time", 0, "media time on the reference track to convert to wall clock")
	queryTime := flagSet.Bool("query", false, "resolve the wall clock for -media-time (default: list prft anchors)")
	format := flagSet.String("format", "json", "output format (yaml|json)")
	flagSet.Usage = func() {
		fmt.Fprintln(os.Stderr, "USAGE: mp4tool prft [OPTIONS] INPUT.mp4")
		flagSet.PrintDefaults()
	}
	flagSet.Parse(args)

	if len(flagSet.Args()) < 1 {
		flagSet.Usage()
		return 1
	}
	if *queryTime && *trackID == 0 {
		fmt.Fprintln(os.Stderr, "Error: -track-id is required with -query")
		return 1
	}
	if *format != "json" && *format != "yaml" {
		fmt.Fprintln(os.Stderr, "Error: -format must be json or yaml")
		return 1
	}

	input, err := os.Open(flagSet.Args()[0])
	if err != nil {
		fmt.Println("Failed to open the input file:", err)
		return 1
	}
	defer input.Close()

	r := bufseekio.NewReadSeeker(input, 1024, 4)

	refs, err := mp4.ReadProducerReferenceTimes(r)
	if err != nil {
		fmt.Println("Error:", err)
		return 1
	}

	if !*queryTime {
		if err := printAnchors(refs, *format); err != nil {
			fmt.Println("Error:", err)
			return 1
		}
		return 0
	}

	ntp, err := mp4.ResolveNtpTimestamp(refs, uint32(*trackID), *mediaTime)
	if err != nil {
		fmt.Println("Error:", err)
		return 1
	}
	wallClock := mp4.NtpTimestampToTime(ntp)
	result := &queryResult{
		TrackID:      uint32(*trackID),
		MediaTime:    *mediaTime,
		NtpTimestamp: ntp,
		WallClock:    wallClock.Format("2006-01-02T15:04:05.000000000Z07:00"),
	}
	if err := printOne(result, *format); err != nil {
		fmt.Println("Error:", err)
		return 1
	}
	return 0
}

type anchorReport struct {
	MoofOffset       uint64 `yaml:"moof_offset" json:"moof_offset"`
	ReferenceTrackID uint32 `yaml:"reference_track_id" json:"reference_track_id"`
	NtpTimestamp     uint64 `yaml:"ntp_timestamp" json:"ntp_timestamp"`
	NtpHex           string `yaml:"ntp_timestamp_hex" json:"ntp_timestamp_hex"`
	MediaTime        uint64 `yaml:"media_time" json:"media_time"`
	MediaTimeSize    uint   `yaml:"media_time_size" json:"media_time_size"`
	Timescale        uint32 `yaml:"timescale" json:"timescale"`
	WallClock        string `yaml:"wall_clock" json:"wall_clock"`
}

type queryResult struct {
	TrackID      uint32 `yaml:"track_id" json:"track_id"`
	MediaTime    uint64 `yaml:"media_time" json:"media_time"`
	NtpTimestamp uint64 `yaml:"ntp_timestamp" json:"ntp_timestamp"`
	NtpHex       string `yaml:"ntp_timestamp_hex" json:"ntp_timestamp_hex"`
	WallClock    string `yaml:"wall_clock" json:"wall_clock"`
}

func toAnchorReports(refs []*mp4.ProducerReferenceTime) []*anchorReport {
	rep := make([]*anchorReport, 0, len(refs))
	for _, ref := range refs {
		rep = append(rep, &anchorReport{
			MoofOffset:       ref.MoofOffset,
			ReferenceTrackID: ref.ReferenceTrackID,
			NtpTimestamp:     ref.NtpTimestamp,
			NtpHex:           fmt.Sprintf("0x%016x", ref.NtpTimestamp),
			MediaTime:        ref.MediaTime,
			MediaTimeSize:    ref.MediaTimeSize,
			Timescale:        ref.Timescale,
			WallClock:        mp4.NtpTimestampToTime(ref.NtpTimestamp).Format("2006-01-02T15:04:05.000000000Z07:00"),
		})
	}
	return rep
}

func printAnchors(refs []*mp4.ProducerReferenceTime, format string) error {
	return encode(toAnchorReports(refs), format)
}

func printOne(result *queryResult, format string) error {
	result.NtpHex = fmt.Sprintf("0x%016x", result.NtpTimestamp)
	return encode(result, format)
}
