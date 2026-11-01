//go:build linux && (amd64 || arm64)

package main

import "testing"

// The ioctl numbers from <linux/fs.h> on 64-bit x86/arm, which also pins
// both setup structs to the kernel's sizes (72 and 192 bytes).
func TestBlkTraceIoctlNumbers(t *testing.T) {
	if BLKTRACESETUP != 0xc0481273 {
		t.Errorf("BLKTRACESETUP = %#x, want 0xc0481273", BLKTRACESETUP)
	}
	if BLKTRACESETUP2 != 0xc0c0128e {
		t.Errorf("BLKTRACESETUP2 = %#x, want 0xc0c0128e", BLKTRACESETUP2)
	}
}
