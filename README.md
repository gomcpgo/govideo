# govideo

A Go package for combining video clips, still images, audio tracks and captions into one exported video, plus `videoprobe`, a pure-Go media inspector (an `ffprobe` subset).

```
go get github.com/gomcpgo/govideo
```

Requires Go 1.27 or later. No cgo.

> **Status: phase 0 (foundation).** Media inspection is complete for MP4/MOV and Matroska/WebM. The editing pipeline works end to end, but only with the codecs implemented so far: **Motion JPEG and LPCM**. H.264, HEVC and AAC (through the operating-system codecs) and the native VP8/VP9/AV1/Opus codecs are not written yet, so phone and camera footage cannot be decoded and the widely playable H.264 + AAC export does not exist yet. See [Roadmap](#roadmap).

## Inspecting media: `videoprobe`

```go
info, err := videoprobe.ProbeFile(ctx, "clip.mov")
for _, s := range info.Streams {
    fmt.Println(s.Kind, s.Codec, s.Profile, s.Duration)
    if v := s.Video; v != nil {
        fmt.Println(v.Width, v.Height, v.FrameRate, v.Rotation, v.HDR, v.VFR)
    }
}
js, _ := info.JSON() // field names close to `ffprobe -show_format -show_streams -of json`
```

`videoprobe` reads container structure and codec configuration headers only. It never decodes media, has no third-party dependencies, and works the same on macOS, Windows and Linux.

| | Supported |
|---|---|
| Containers | MP4, MOV, fragmented MP4, Matroska, WebM (including files with no duration or cues), WAV |
| Video headers | H.264, HEVC, AV1, VP8, VP9; ProRes and Motion JPEG are identified |
| Audio headers | AAC (LC, HE-AAC, HE-AACv2), Opus, FLAC, MP3, ALAC, LPCM |
| Reported | duration, start time, frame rate, variable-frame-rate flag, coded and display size, aspect ratio, rotation, bit depth, chroma format, colour tags, HLG/PQ, Dolby Vision configuration, sample rate, channels, language, faststart, fragmentation, tags |

## Building a video: `govideo`

```go
tl := govideo.NewTimeline(govideo.Canvas{
    Width: 1920, Height: 1080,
    FrameRate: govideo.R(30, 1),
    Fit:       govideo.Contain,
})
tl.AddImage("title.png", 3*time.Second, govideo.ImageOptions{})
tl.AddClip("intro.mov", govideo.ClipOptions{Trim: govideo.Range(2*time.Second, 14*time.Second)})
tl.AddAudio("music.wav", govideo.AudioOptions{Mode: govideo.Mix, Gain: 0.3, FadeOut: 2 * time.Second})
tl.AddCaptions("subs.srt", govideo.CaptionOptions{Sidecar: true})

err := tl.Export(ctx, "out.mov", govideo.ExportOptions{
    Progress: func(p govideo.Progress) { fmt.Printf("%d/%d\n", p.Frame, p.TotalFrames) },
})
```

What works today:

- **Clips** in MP4, MOV or Matroska containing Motion JPEG video and/or LPCM audio, with trimming. Rotation metadata is applied.
- **Still images** (PNG, JPEG with EXIF orientation, GIF, WebP) shown for a set duration.
- **Mixed shapes and frame rates.** Every source is scaled onto the canvas (`Contain`, `Cover` or `Stretch`) and resampled to the canvas frame rate by timestamp.
- **Audio** from WAV or LPCM tracks: mixed into the programme with gain and fades, carried as a separate track, or replacing the programme audio. Everything is resampled to 48 kHz stereo.
- **Captions** from SRT or WebVTT as a soft subtitle track (`tx3g`), with an optional `.vtt` sidecar for web players.
- **Export** as Motion JPEG + 16-bit LPCM in `.mov` or `.mp4`, with the index at the front of the file (faststart). The files are large; this is an intermediate format that editors and ffmpeg read, not a delivery format.

Anything that needs a codec this host lacks fails with `*govideo.ErrCodecUnavailable`, which names the codec, the operation and the reason. `govideo.Codecs()` lists what is usable.

## Codec policy

govideo natively implements only codecs whose patents have expired or that are royalty-free. Patent-pooled codecs (H.264, HEVC, AAC) will be used **only through the operating system's own codecs** (VideoToolbox/AudioToolbox on macOS, Media Foundation on Windows).

- **If the platform has no codec, govideo has no codec.** Linux has no licensed OS codec, so on Linux govideo will never decode or encode H.264, HEVC or AAC. Only the royalty-free path (WebM/VP8/VP9/AV1/Opus input, AV1 + Opus output) will work there. There will be no OpenH264, ffmpeg or other fallback.
- **Windows:** HEVC decoding will need Microsoft's HEVC Video Extensions. "N" editions need the Media Feature Pack.
- **OS codec licences target end-user devices.** Check the OS licence terms before server or cloud use.
- VP9, AV1 and Opus have contested third-party patent claims. See [PATENTS.md](PATENTS.md). You are responsible for your own product's codec licensing.

No FFmpeg/libav code is used. FFmpeg and ffprobe binaries are run only by the tests, as black-box oracles.

## Roadmap

| Phase | Adds |
|---|---|
| 0 (this version) | Containers, `videoprobe`, timeline, scaling, stills, LPCM audio mixing, soft captions, codec registry, test oracle |
| 1 | macOS and Windows OS codecs (H.264, HEVC decode, AAC), HDR to SDR tone mapping, native Opus/VP8/VP9/MP3/FLAC decoders, burned-in captions, H.264 + AAC export |
| 2 | Native AV1 decoder and encoder, Opus encoder, AV1 + Opus export, Linux end to end |
| 3 | MPEG-TS, interlaced sources, AC-3, ProRes via macOS, and other archive formats |
| 4 | SIMD and encoder efficiency |

A JSON project file for describing builds, and a small command line for simple operations, are planned.

## Testing

```
go test ./...
```

Tests that compare against ffprobe or decode exports with ffmpeg skip themselves when those binaries are not installed. Test media is generated at test time and never committed.

## Licence

Apache-2.0. See [LICENSE](LICENSE), [NOTICE](NOTICE) and [PATENTS.md](PATENTS.md).
