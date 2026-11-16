package prft

import (
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"

	mp4 "github.com/abema/go-mp4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildFixtureFile writes a minimal fragmented MP4 containing two prft
// anchors into a temporary file and returns its path.
func buildFixtureFile(t *testing.T) string {
	t.Helper()

	dir, err := ioutil.TempDir("", "mp4tool-prft")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "prft.mp4")

	f, err := os.Create(path)
	require.NoError(t, err)
	defer f.Close()

	w := mp4.NewWriter(f)

	start := func(typ mp4.BoxType) {
		t.Helper()
		_, err := w.StartBox(&mp4.BoxInfo{Type: typ})
		require.NoError(t, err)
	}
	end := func() {
		t.Helper()
		_, err := w.EndBox()
		require.NoError(t, err)
	}
	payload := func(box mp4.IImmutableBox) {
		t.Helper()
		_, err := mp4.Marshal(w, box, mp4.Context{})
		require.NoError(t, err)
	}
	box := func(b mp4.IImmutableBox) {
		t.Helper()
		start(b.GetType())
		payload(b)
		end()
	}

	const (
		trackID   uint32 = 7
		timescale uint32 = 90000
		ntp1      uint64 = 0xe000000000000000
		ntp2      uint64 = 0xe000001000000000
	)

	// ftyp
	start(mp4.BoxTypeFtyp())
	payload(&mp4.Ftyp{
		MajorBrand:       [4]byte{'i', 's', 'o', 'm'},
		CompatibleBrands: []mp4.CompatibleBrandElem{{CompatibleBrand: [4]byte{'i', 's', 'o', 'm'}}},
	})
	end()

	// moov > trak > (tkhd, mdia > mdhd)
	start(mp4.BoxTypeMoov())
	start(mp4.BoxTypeTrak())
	box(&mp4.Tkhd{
		TrackID: trackID,
		Matrix:  [9]int32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000},
	})
	start(mp4.BoxTypeMdia())
	box(&mp4.Mdhd{Timescale: timescale})
	end() // mdia
	end() // trak
	end() // moov

	writeFragment := func(seq uint32, base uint64, prftBox *mp4.Prft) {
		start(mp4.BoxTypeMoof())
		box(&mp4.Mfhd{SequenceNumber: seq})
		start(mp4.BoxTypeTraf())
		box(prftBox)
		box(&mp4.Tfhd{TrackID: trackID})
		tfdt := &mp4.Tfdt{}
		tfdt.SetVersion(1)
		tfdt.BaseMediaDecodeTimeV1 = base
		box(tfdt)
		end() // traf
		end() // moof
		start(mp4.BoxTypeMdat())
		_, err := w.Write([]byte{0xde, 0xad, 0xbe, 0xef})
		require.NoError(t, err)
		end()
	}

	prft1 := &mp4.Prft{ReferenceTrackID: trackID, NtpTimestamp: ntp1, MediaTimeV1: 1000000}
	prft1.SetVersion(1)
	prft2 := &mp4.Prft{ReferenceTrackID: trackID, NtpTimestamp: ntp2, MediaTimeV1: 3000000}
	prft2.SetVersion(1)
	writeFragment(1, 1000000, prft1)
	writeFragment(2, 3000000, prft2)

	return path
}

func runMainCapture(t *testing.T, args []string) (string, int) {
	t.Helper()
	stdout := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	defer func() { os.Stdout = stdout }()
	os.Stdout = w

	code := Main(args)
	require.NoError(t, w.Close())
	b, err := io.ReadAll(r)
	require.NoError(t, err)
	return string(b), code
}

func TestPrftListAnchors(t *testing.T) {
	path := buildFixtureFile(t)

	out, code := runMainCapture(t, []string{path})
	require.Zero(t, code)
	assert.Contains(t, out, `"reference_track_id": 7`)
	assert.Contains(t, out, `"ntp_timestamp": 16140901064495857664`) // 0xe000000000000000
	assert.Contains(t, out, `"ntp_timestamp_hex": "0xe000001000000000"`)
	assert.Contains(t, out, `"media_time": 1000000`)
	assert.Contains(t, out, `"media_time": 3000000`)
	assert.Contains(t, out, `"media_time_size": 64`)
	assert.Contains(t, out, `"timescale": 90000`)
	assert.Contains(t, out, `"wall_clock":`)
}

func TestPrftQuery(t *testing.T) {
	path := buildFixtureFile(t)

	// media time 3090000 is one second after the second anchor.
	out, code := runMainCapture(t, []string{
		"-query", "-track-id", "7", "-media-time", "3090000", path,
	})
	require.Zero(t, code)
	assert.Contains(t, out, `"track_id": 7`)
	assert.Contains(t, out, `"media_time": 3090000`)
	assert.Contains(t, out, `"ntp_timestamp_hex": "0xe000001100000000"`)
	assert.Contains(t, out, `"wall_clock":`)
}

func TestPrftYAMLFormat(t *testing.T) {
	path := buildFixtureFile(t)

	out, code := runMainCapture(t, []string{"-format", "yaml", path})
	require.Zero(t, code)
	assert.Contains(t, out, "reference_track_id: 7")
	assert.Contains(t, out, "media_time: 1000000")
}

func TestPrftErrors(t *testing.T) {
	path := buildFixtureFile(t)

	t.Run("missing file", func(t *testing.T) {
		require.NotZero(t, Main([]string{"not_found.mp4"}))
	})

	t.Run("query without track id", func(t *testing.T) {
		require.NotZero(t, Main([]string{"-query", path}))
	})

	t.Run("unknown track", func(t *testing.T) {
		require.NotZero(t, Main([]string{
			"-query", "-track-id", "4242", "-media-time", "100", path,
		}))
	})

	t.Run("invalid format", func(t *testing.T) {
		require.NotZero(t, Main([]string{"-format", "xml", path}))
	})

	t.Run("no args", func(t *testing.T) {
		require.NotZero(t, Main(nil))
	})
}
