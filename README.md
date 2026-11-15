go-mp4
------

[![Go Reference](https://pkg.go.dev/badge/github.com/abema/go-mp4.svg)](https://pkg.go.dev/github.com/abema/go-mp4)
![Test](https://github.com/abema/go-mp4/actions/workflows/test.yml/badge.svg)
[![Coverage Status](https://coveralls.io/repos/github/abema/go-mp4/badge.svg)](https://coveralls.io/github/abema/go-mp4)

go-mp4 is a Go library that provides low-level I/O interfaces for MP4 files.
It allows you to parse or build any MP4 box (atom) directly.

go-mp4 provides very flexible interfaces for reading boxes.
If you want to read only specific parts of an MP4 file, this library can extract just those boxes via the io.ReadSeeker interface.

On the other hand, this library is not suitable for complex data conversions.

## Integration with your Go application

### Reading

Using [ReadBoxStructure](https://pkg.go.dev/github.com/abema/go-mp4#ReadBoxStructure) or [ReadBoxStructureFromInternal](https://pkg.go.dev/github.com/abema/go-mp4#ReadBoxStructureFromInternal), you can scan box(atom) tree by depth-first order.

```go
// expand all boxes
_, err := mp4.ReadBoxStructure(file, func(h *mp4.ReadHandle) (interface{}, error) {
	fmt.Println("depth", len(h.Path))

	// Box Type (e.g. "mdhd", "tfdt", "mdat")
	fmt.Println("type", h.BoxInfo.Type.String())

	// Box Size
	fmt.Println("size", h.BoxInfo.Size)

	if h.BoxInfo.IsSupportedType() {
		// Payload
		box, _, err := h.ReadPayload()
		if err != nil {
			return nil, err
		}
		str, err := mp4.Stringify(box, h.BoxInfo.Context)
		if err != nil {
			return nil, err
		}
		fmt.Println("payload", str)

		// Expands children
		return h.Expand()
	}
	return nil, nil
})
```

[ExtractBox](https://pkg.go.dev/github.com/abema/go-mp4#ExtractBox), [ExtractBoxes](https://pkg.go.dev/github.com/abema/go-mp4#ExtractBoxes), [ExtractBoxWithPayload](https://pkg.go.dev/github.com/abema/go-mp4#ExtractBoxWithPayload), [ExtractBoxesWithPayload](https://pkg.go.dev/github.com/abema/go-mp4#ExtractBoxesWithPayload), and [Probe](https://pkg.go.dev/github.com/abema/go-mp4#Probe) are wrapper functions of ReadBoxStructure.

```go
// extract specific boxes
boxes, err := mp4.ExtractBoxWithPayload(file, nil, mp4.BoxPath{mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeTkhd()})
if err != nil {
   :
}
for _, box := range boxes {
  tkhd := box.Payload.(*mp4.Tkhd)
  fmt.Println("track ID:", tkhd.TrackID)
}
```

```go
// get basic informations
info, err := mp4.Probe(bufseekio.NewReadSeeker(file, 1024, 4))  
if err != nil {
   :
}
fmt.Println("track num:", len(info.Tracks))
```

### Writing

Writer helps you write a box tree.
The following sample code edits an emsg box and writes the result to another file.

```go
r := bufseekio.NewReadSeeker(inputFile, 128*1024, 4)
w := mp4.NewWriter(outputFile)
_, err = mp4.ReadBoxStructure(r, func(h *mp4.ReadHandle) (interface{}, error) {
	switch h.BoxInfo.Type {
	case mp4.BoxTypeEmsg():
		// write box size and box type
		_, err := w.StartBox(&h.BoxInfo)
		if err != nil {
			return nil, err
		}
		// read payload
		box, _, err := h.ReadPayload()
		if err != nil {
			return nil, err
		}
		// update MessageData
		emsg := box.(*mp4.Emsg)
		emsg.MessageData = []byte("hello world")
		// write box playload
		if _, err := mp4.Marshal(w, emsg, h.BoxInfo.Context); err != nil {
			return nil, err
		}
		// rewrite box size
		_, err = w.EndBox()
		return nil, err
	default:
		// copy all
		return nil, w.CopyBox(r, &h.BoxInfo)
	}
})
```

Please note that the sample code above may not work for some MP4 files.
If your MP4 files contain certain box types (e.g., stco, mfra), you need to update them when the offset of the mdat box changes.
The following sample code adds a metadata box and updates the chunk offsets in the stco boxes.

[Sample Code: Insert Metadata and Update stco box](https://gist.github.com/sunfish-shogi/cccde016a38c66d32c07a0234368804e)

### Rewriting Metadata without Re-encoding Media

When you only change metadata such as a title or a cover image (typically
`udta`/`meta`), the media payload must not be re-encoded.
[RewriteFile](https://pkg.go.dev/github.com/abema/go-mp4#RewriteFile) performs a
file-oriented metadata rewrite: it streams the `mdat` payloads byte-for-byte
with bounded memory, recomputes every nested box size automatically, and keeps
the absolute file-offset structures consistent when boxes grow or shrink.

```go
input, err := os.Open("input.mp4")
if err != nil {
    // ...
}
defer input.Close()

output, err := os.Create("output.mp4")
if err != nil {
    // ...
}
defer output.Close()

r := bufseekio.NewReadSeeker(input, 128*1024, 4)
err = mp4.RewriteFile(r, output, func(h *mp4.RewriteHandle) error {
    if h.BoxInfo.Type == mp4.BoxTypeUdta() {
        // Build a complete replacement box (including its header) in a small
        // buffer and write it to h.Writer. Writing nothing preserves the
        // original box byte-for-byte.
        var buf bytes.Buffer
        w := mp4.NewWriter(&buf)
        if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeUdta()}); err != nil {
            return err
        }
        if _, err := w.Write([]byte("new user data")); err != nil {
            return err
        }
        if _, err := w.EndBox(); err != nil {
            return err
        }
        _, err := h.Writer.Write(buf.Bytes())
        return err
    }
    return nil
})
```

The rewriter handles both layouts (`moov` before or after `mdat`) and:

- shifts `stco`/`co64` chunk offsets, transparently converting an `stco` to a
  `co64` when a shifted offset no longer fits in 32 bits,
- shifts `tfhd` base-data offsets and `tfra` moof offsets,
- preserves unknown boxes that do not need rewriting byte-for-byte,
- rejects structures it cannot update safely (for example an absolute `saio`
  outside a `moof`, a 32bit `tfra` entry that would overflow, or a malformed
  input) before any output byte is produced.

### User-defined Boxes

You can define additional box types as follows:

```go
func BoxTypeXxxx() BoxType { return mp4.StrToBoxType("xxxx") }

func init() {
	mp4.AddBoxDef(&Xxxx{}, 0)
}

type Xxxx struct {
	FullBox  `mp4:"0,extend"`
	UI32      uint32 `mp4:"1,size=32"`
	ByteArray []byte `mp4:"2,size=8,len=dynamic"`
}

func (*Xxxx) GetType() BoxType {
	return BoxTypeXxxx()
}
```

### Buffering

go-mp4 does not buffer I/O.
If you need to reduce the number of Read calls, you can wrap your io.ReadSeeker with [bufseekio](https://github.com/sunfish-shogi/bufseekio).

## Command Line Tool

Install mp4tool as follows:

```sh
go install github.com/abema/go-mp4/cmd/mp4tool@latest

mp4tool -help
```

For example, `mp4tool dump MP4_FILE_NAME` command prints MP4 box tree as follows:

```
[moof] Size=504
  [mfhd] Size=16 Version=0 Flags=0x000000 SequenceNumber=1
  [traf] Size=480
    [tfhd] Size=28 Version=0 Flags=0x020038 TrackID=1 DefaultSampleDuration=9000 DefaultSampleSize=33550 DefaultSampleFlags=0x1010000
    [tfdt] Size=20 Version=1 Flags=0x000000 BaseMediaDecodeTimeV1=0
    [trun] Size=424 ... (use -a option to show all)
[mdat] Size=44569 Data=[...] (use -mdat option to expand)
```
