package faststart

import (
	"flag"
	"fmt"

	"github.com/abema/go-mp4"
)

// Main is the entry point of `mp4tool alpha faststart`.
// It rewrites a non-fragmented MP4 so that the moov box precedes the mdat box.
func Main(args []string) int {
	flagSet := flag.NewFlagSet("faststart", flag.ExitOnError)
	flagSet.Usage = func() {
		fmt.Fprintln(flagSet.Output(), "USAGE: mp4tool alpha faststart INPUT.mp4 OUTPUT.mp4")
		flagSet.PrintDefaults()
	}
	flagSet.Parse(args)

	if len(flagSet.Args()) < 2 {
		flagSet.Usage()
		return 1
	}
	inputPath := flagSet.Args()[0]
	outputPath := flagSet.Args()[1]

	if err := mp4.FastStartFile(inputPath, outputPath); err != nil {
		fmt.Println("Error:", err)
		return 1
	}
	return 0
}
