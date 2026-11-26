# Patent notice

govideo implements only codecs whose known patents have expired or that are offered royalty-free (MPEG-2 Video, MPEG-4 Part 2, Motion JPEG, MP3, AC-3, LPCM, FLAC, Vorbis, ALAC, VP8, VP9, AV1, Opus). Third parties have asserted patent claims against VP9, AV1 and Opus through patent pools and litigation; the project's authors make no representation that any codec is free of third-party patents.

govideo does **not** contain implementations of H.264/AVC, H.265/HEVC or AAC. When available, it calls the codecs provided and licensed by the operating system (Apple VideoToolbox/AudioToolbox, Microsoft Media Foundation). Use of those codecs is governed by your operating-system licence.

You are responsible for any patent licences your product requires. This notice is not legal advice.

## What is in this version

Of the codecs listed above, this version implements Motion JPEG (through the Go standard library), LPCM and a FLAC encoder. The `videoprobe` package additionally parses the *configuration headers* of H.264, HEVC, AV1, VP8, VP9, AAC, Opus, FLAC and MP3 to report stream metadata. That is metadata parsing only: FLAC apart, which is encoded, no audio or video of those formats is decoded or encoded.
