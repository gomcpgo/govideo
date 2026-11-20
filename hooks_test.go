package govideo

import (
	"github.com/gomcpgo/govideo/codec"
	"github.com/gomcpgo/govideo/codec/oscodec"
)

// HookH264 installs the encoder test hooks (see tuneH264) and returns a
// function that removes them. Tests using it must not run in parallel.
func HookH264(tune func(*oscodec.EncodeConfig), tap func(*codec.VideoFrame)) (restore func()) {
	tuneH264, tapH264 = tune, tap
	return func() { tuneH264, tapH264 = nil, nil }
}
