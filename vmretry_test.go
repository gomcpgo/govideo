package govideo_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/gomcpgo/govideo/codec/oscodec"
)

// retryOnVM runs an export that decodes through the system video decoder,
// and runs it once more if the decoder failed during it on a virtual
// machine.
//
// GitHub's Apple silicon macOS runners are virtual machines whose
// VideoToolbox fails about one H.264 frame in 12,000 with OSStatus
// -536870212 (kIOReturnError), at random, idle or busy; a session that has
// failed can go on failing every frame with -19092. Measured on 480,000
// frames: a new session decoded the failed frame every time, and neither a
// real Mac nor the Intel runner failed one (SPEC §14.1). The retry is here,
// not in the library, so that a failure on a real machine still fails the
// test, and one that is not the host's fails again.
//
// export is called afresh each time and should take any counts it reports
// inside itself. A cancelled export is not retried. Each retry is also
// noted in the GitHub Actions job summary, since go test shows nothing of a
// package that passes.
func retryOnVM(t *testing.T, export func() error) error {
	t.Helper()
	before := oscodec.VideoDecoderStats().Failures
	err := export()
	failed := oscodec.VideoDecoderStats().Failures - before
	if failed == 0 || errors.Is(err, context.Canceled) || !virtualMachine() {
		return err
	}
	t.Logf("the system video decoder failed %d times on this virtual machine (export: %v); exporting again", failed, err)
	retried := export()
	summary(t, fmt.Sprintf("- `%s`: the system video decoder failed %d times (export: %v); exported again: %v\n",
		t.Name(), failed, err, outcome(retried)))
	return retried
}

// outcome describes how an export ended.
func outcome(err error) string {
	if err == nil {
		return "ok"
	}
	return err.Error()
}

// summary appends a line to the GitHub Actions job summary, when there is
// one.
func summary(t *testing.T, line string) {
	t.Helper()
	path := os.Getenv("GITHUB_STEP_SUMMARY")
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		t.Logf("job summary: %v", err)
		return
	}
	defer f.Close()
	if _, err := f.WriteString(line); err != nil {
		t.Logf("job summary: %v", err)
	}
}
