//go:build amd64

package client

import "strings"

// cpuid executes the x86 CPUID instruction with the given leaf (EAX) and subleaf
// (ECX) selectors and returns the EAX/EBX/ECX/EDX registers. Implemented in
// cpuid_amd64.s.
func cpuid(eaxArg, ecxArg uint32) (eax, ebx, ecx, edx uint32)

// cpuidVendorString returns the 12-character CPU vendor ID from CPUID leaf 0
// (e.g. "GenuineIntel", "AuthenticAMD") read straight from the silicon. This is
// exactly the token the head's HRClass keys on, so no further mapping is needed.
//
// Per the Intel/AMD CPUID specification, leaf 0 returns the vendor bytes laid
// out across EBX, then EDX, then ECX. The field reflects the physical CPU vendor
// even inside a virtual machine (a hypervisor advertises its own identity on
// leaf 0x40000000, not leaf 0), so this is robust under virtualization. Some
// emulated vendors pad the field with NUL or spaces, which we trim.
func cpuidVendorString() string {
	_, ebx, ecx, edx := cpuid(0, 0)
	b := []byte{
		byte(ebx), byte(ebx >> 8), byte(ebx >> 16), byte(ebx >> 24),
		byte(edx), byte(edx >> 8), byte(edx >> 16), byte(edx >> 24),
		byte(ecx), byte(ecx >> 8), byte(ecx >> 16), byte(ecx >> 24),
	}
	return strings.Trim(string(b), " \x00")
}
