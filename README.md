# govideo

A Go package for combining video clips, still images, audio tracks and captions into one exported video, plus `videoprobe`, a pure-Go media inspector (an `ffprobe` subset).

```
go get github.com/gomcpgo/govideo
```

Requires Go 1.27 or later. No cgo.

> **Status: phase 1 in progress.** On **macOS**, H.264, HEVC and AAC are decoded through the system codecs (VideoToolbox, AudioToolbox), and the widely playable **H.264 + AAC** export works. On Windows the system codec backend is not written yet, and Linux has no system codecs, so there only Motion JPEG and LPCM work for now. The native VP8/VP9/AV1/Opus codecs are not written yet. See [Roadmap](#roadmap).

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
| Containers | MP4, MOV, 3GP, fragmented MP4, Matroska, WebM (including files with no duration or cues), AVI, WAV |
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
tl.AddGenerated(2*time.Second, govideo.GenerateOptions{
    Pattern: govideo.PatternGradient, Color: color.RGBA{20, 30, 90, 255}, Color2: color.Black,
    Text:    "Summer 2026",
})
tl.AddImage("title.png", 3*time.Second, govideo.ImageOptions{
    To: govideo.Placement{Scale: 1.15}, // a slow zoom in
})
tl.AddClip("intro.mov", govideo.ClipOptions{
    Trim:       govideo.Range(2*time.Second, 14*time.Second),
    Transition: govideo.Transition{Type: govideo.Crossfade, Duration: time.Second / 2},
    FadeOut:    time.Second,
})
tl.AddAudio("music.wav", govideo.AudioOptions{Mode: govideo.Mix, Gain: 0.3, FadeOut: 2 * time.Second})
tl.AddCaptions("subs.srt", govideo.CaptionOptions{Sidecar: true})

err := tl.Export(ctx, "out.mp4", govideo.ExportOptions{
    Profile:  govideo.ProfileAuto, // H.264 + AAC where the system has the encoders
    Progress: func(p govideo.Progress) { fmt.Printf("%d/%d\n", p.Frame, p.TotalFrames) },
})
```

What works today:

- **Clips** in MP4, MOV, 3GP, Matroska or AVI, with trimming. Video: H.264 (including interlaced, 10-bit and 4:2:2 streams) and HEVC (8-bit and 10-bit), both macOS only, and Motion JPEG. Interlaced H.264 is deinterlaced: each frame is shown as its two fields, in the order the stream gives, so a 50 or 60 fps canvas keeps all of the motion and a slower one shows no combing. Audio: AAC (LC, HE-AAC, HE-AACv2, xHE-AAC; macOS only), LPCM; multichannel audio is mixed down to stereo at the usual level (ITU-R BS.775). Rotation and mirroring metadata are applied; B-frame reordering, edit lists and encoder priming are handled, so picture and sound stay aligned across joins. Fragmented MP4 and variable frame rate sources work. A file that was cut short plays as far as it goes.
- **Still images** (PNG, JPEG with EXIF orientation, GIF, WebP) shown for a set duration. Transparent parts show the canvas background colour.
- **Mixed shapes and frame rates.** Every source is scaled onto the canvas (`Contain`, `Cover` or `Stretch`) and resampled to the canvas frame rate by timestamp.
- **Pan and zoom over a still** ("Ken Burns"): `ImageOptions.From` and `To` give where the picture sits at the start and at the end (a magnification and an offset from the centre), and it moves between them at a steady rate, positioned to a fraction of a pixel so that slow motion is smooth.
- **Fades and crossfades.** `FadeIn` and `FadeOut` on any clip, image or generated segment fade its picture from and to the canvas background and its sound from and to silence. `Transition` on a segment dissolves the one before into it, picture and sound, over the given time; the two overlap for that long, so the timeline gets shorter by it. Times given to `AddAudio` and caption files refer to the timeline as it then is.
- **Generated segments** (`AddGenerated`): a solid colour, colour bars, a gradient, a checkerboard, or an animated test card that carries a timecode and a machine-readable frame number; optional centred text for title cards (Latin script); optional sine tone or once-a-second beep.
- **Audio** from WAV files or the audio track of any supported clip: mixed into the programme with gain and fades, carried as a separate track, or replacing the programme audio. Everything is resampled to 48 kHz stereo.
- **Captions** from SRT or WebVTT as a soft subtitle track (`tx3g`), with an optional `.vtt` sidecar for web players.
- **Export**, with the index at the front of the file (faststart), in `.mp4` or `.mov`:
  - `ProfileCompatible`: H.264 High + AAC-LC, tagged BT.709. macOS only for now. Video is encoded to a picture quality chosen by `Quality`, so the size follows the content: a slide show comes out small, a busy scene keeps its detail and takes what it needs. Where the system encoder has no quality mode (Intel Macs without a hardware encoder, for one) an average bit rate chosen from the picture size is used. Set `VideoBitrate` when you need a predictable size instead (the two cannot be combined: the system encoder has no quality mode with a ceiling). `AudioBitrate` sets the audio rate.
  - `ProfileIntermediate`: Motion JPEG + 16-bit LPCM, using only native codecs, on every platform. The files are large; this is a format that editors and ffmpeg read, not a delivery format.
  - `ProfileAuto` (the default) picks `ProfileCompatible` when the system has the encoders, else `ProfileIntermediate`.

Known limits: HDR sources (HLG, PQ, Dolby Vision) are decoded but not tone-mapped yet, so they look flatter than they should; deinterlacing rebuilds each field on its own, so a stream marked interlaced whose pictures are not loses vertical detail (set `ClipOptions.Deinterlace` to `DeinterlaceOff` for such a clip, or to a field order for an interlaced stream that is not marked); H.264 output has no B-frames (on the system encoder they did not make files smaller); captions cannot be burned in yet.

Anything that needs a codec this host lacks fails with `*govideo.ErrCodecUnavailable`, which names the codec, the operation and the reason. That includes streams the system decoder refuses (AAC Main and LTP profiles, for example) and HEIC stills. A file in a container that is recognised but not read (MPEG-TS, MXF, Ogg and others) fails with `*videoprobe.UnsupportedFormatError`. `govideo.Codecs()` lists what is usable.

## Codec policy

govideo natively implements only codecs whose patents have expired or that are royalty-free. Patent-pooled codecs (H.264, HEVC, AAC) are used **only through the operating system's own codecs** (VideoToolbox/AudioToolbox on macOS today; Media Foundation on Windows is planned). The bindings use [purego](https://github.com/ebitengine/purego); there is no cgo.

- **If the platform has no codec, govideo has no codec.** Linux has no licensed OS codec, so on Linux govideo will never decode or encode H.264, HEVC or AAC. Only the royalty-free path (WebM/VP8/VP9/AV1/Opus input, AV1 + Opus output) will work there. There will be no OpenH264, ffmpeg or other fallback.
- **Windows:** HEVC decoding will need Microsoft's HEVC Video Extensions. "N" editions need the Media Feature Pack.
- **OS codec licences target end-user devices.** Check the OS licence terms before server or cloud use.
- VP9, AV1 and Opus have contested third-party patent claims. See [PATENTS.md](PATENTS.md). You are responsible for your own product's codec licensing.

No FFmpeg/libav code is used. FFmpeg and ffprobe binaries are run only by the tests, as black-box oracles.

## Roadmap

| Phase | Adds |
|---|---|
| 0 (done) | Containers, `videoprobe`, timeline, scaling, stills, LPCM audio mixing, soft captions, codec registry, test oracle |
| 1 (in progress) | Done: macOS system codecs (H.264, HEVC decode, AAC), H.264 + AAC export, generated segments, AVI input, deinterlacing of H.264, fades, crossfades, pan and zoom on stills. To do: Windows system codecs, HDR to SDR tone mapping, native Opus/VP8/VP9/MP3/FLAC decoders, burned-in captions |
| 2 | Native AV1 decoder and encoder, Opus encoder, AV1 + Opus export, Linux end to end |
| 3 | MPEG-TS, interlaced sources in other codecs, AC-3, ProRes via macOS, and other archive formats |
| 4 | SIMD and encoder efficiency |

A JSON project file for describing builds, and a small command line for simple operations, are planned.

## Testing

```
go test ./...
```

Tests that compare against ffprobe or decode exports with ffmpeg skip themselves when those binaries are not installed or lack an encoder a fixture needs, and tests of the system codecs skip where there are none. Since `go test` shows a skipped test as it shows a passing one, this lists them with their reasons:

```
go run ./internal/testreport ./...
```

It takes the arguments of `go test`. With `GOVIDEO_ORACLE_STRICT=1` a test that would skip for want of ffmpeg fails instead; CI sets it on Linux and macOS.

Test media is generated at test time and never committed, apart from two WebP stills of a few hundred bytes written out in `webp_test.go`.

To check your own footage, point the suite at a folder of media files. Each file is exported with every available profile and the picture is compared with ffmpeg's own decoding of the source:

```
GOVIDEO_TEST_MEDIA=/path/to/videos go test . -run TestRealMedia -v
```

`TestMeasureH264` (see its comment) encodes the same folder with a range of encoder settings and writes size, PSNR, SSIM and VMAF for each to a CSV file. It is how the encoder defaults were chosen.

The folder may instead hold the sub-folders `video/`, `audio/` (files for `AddAudio`; each must work or be refused as a missing codec), `stills/`, and `unsupported/` (files that must be refused with an error naming the codec or format).

## Licence

Apache-2.0. See [LICENSE](LICENSE), [NOTICE](NOTICE) and [PATENTS.md](PATENTS.md).
