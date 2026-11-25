Rolltop use: Video duration and resolution (video_tools/storage/storage.go:286)
  Replace now?: Yes
  Why: videoprobe.ProbeFile covers it on every platform. The Veo and Grok outputs are MP4.
  ────────────────────────────────────────
  Rolltop use: TTS audio duration (audio_tools/converter/ffmpeg.go:107)
  Replace now?: Partly
  Why: Gemini's WAV output works. ElevenLabs' default is a bare .mp3, which videoprobe rejects with UnsupportedFormatError because
    MP3 isn't a supported container. Adding MP3 frame/Xing header parsing to videoprobe would be small.
  ────────────────────────────────────────
  Rolltop use: Video thumbnails (storage.go:227)
  Replace now?: No
  Why: govideo has no public "decode the frame at time t" call; its frame lookup is internal to Timeline.Export. H.264 decoding
    also works only on macOS today: the Windows backend isn't written, and Linux will never have it. A govideo.Frame(ctx, path,
    at) (image.Image, error) API would solve macOS now and Windows later.
  ────────────────────────────────────────
  Rolltop use: Audio conversion before transcription (converter.Convert)
  Replace now?: No
  Why: It needs an MP3 encoder (govideo has none) and audio-only output (Export always writes a video). Its AAC encoder is
    macOS-only.