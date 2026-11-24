package govideo_test

import "syscall"

// virtualMachine reports whether the tests run in a virtual machine, as the
// kernel says to software that asks (kern.hv_vmm_present).
func virtualMachine() bool {
	v, err := syscall.SysctlUint32("kern.hv_vmm_present")
	return err == nil && v == 1
}
