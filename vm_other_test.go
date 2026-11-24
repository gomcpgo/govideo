//go:build !darwin

package govideo_test

// virtualMachine reports false: only the macOS system decoder is retried on
// a virtual machine (see retryOnVM).
func virtualMachine() bool { return false }
