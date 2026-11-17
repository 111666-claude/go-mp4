package rotate

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/abema/go-mp4"
)

// Main implements "mp4tool rotate": it rewrites the display matrix of the
// video track selected by track ID without re-encoding the media.
func Main(args []string) int {
	flagSet := flag.NewFlagSet("rotate", flag.ExitOnError)
	trackID := flagSet.Uint("track-id", 0, "track ID of the video track to edit")
	rotation := flagSet.Int("rotation", 0, "display rotation in degrees (0, 90, 180, 270)")
	flagSet.Usage = func() {
		println("USAGE: mp4tool rotate -track-id TRACK_ID -rotation DEGREES INPUT.mp4 OUTPUT.mp4")
		flagSet.PrintDefaults()
	}
	flagSet.Parse(args)

	if len(flagSet.Args()) < 2 {
		flagSet.Usage()
		return 1
	}
	inputPath := flagSet.Args()[0]
	outputPath := flagSet.Args()[1]

	err := rotateFile(inputPath, outputPath, uint32(*trackID), *rotation)
	if err != nil {
		fmt.Println("Error:", err)
		return 1
	}
	return 0
}

func rotateFile(inputPath, outputPath string, trackID uint32, rotation int) error {
	input, err := os.Open(inputPath)
	if err != nil {
		return err
	}
	defer input.Close()

	stat, err := input.Stat()
	if err != nil {
		return err
	}

	// Write next to the destination so the final rename is atomic and cannot
	// cross a filesystem boundary. A failure never leaves a partial file at
	// the requested output path.
	tmp, err := os.CreateTemp(filepath.Dir(outputPath), ".mp4tool-rotate-*.mp4")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpPath)
	}()

	if err := mp4.RewriteTrackRotation(input, tmp, trackID, rotation); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, stat.Mode().Perm()); err != nil {
		return err
	}
	return os.Rename(tmpPath, outputPath)
}
