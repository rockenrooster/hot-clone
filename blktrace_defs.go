//go:build linux

package main

import "unsafe"

// Kernel ABI for setting up blktrace, from <linux/blktrace_api.h> and
// <linux/fs.h>. The records the kernel then writes are decoded in
// blktrace_record.go. These used to come from a vendored fork of
// golang.org/x/sys; they are the only things hot-clone needed from it, so
// they live here instead and the stock golang.org/x/sys module is used for
// everything else.

// BLK_user_trace_setup is struct blk_user_trace_setup, the argument to
// BLKTRACESETUP (records are struct blk_io_trace). No explicit padding:
// Go's field alignment reproduces the kernel's layout on every arch where
// C does the same (amd64, arm64, 386), and BLKTRACESETUP below encodes
// sizeof, so the two cannot drift apart.
type BLK_user_trace_setup struct {
	Name      [32]byte // output: kernel fills in the debugfs dir name
	Act_mask  uint16   // input: event categories to trace
	Buf_size  uint32   // input: size of each relay sub-buffer
	Buf_nr    uint32   // input: number of relay sub-buffers
	Start_lba uint64
	End_lba   uint64
	Pid       uint32
}

// BLK_user_trace_setup2 is struct blk_user_trace_setup2, the argument to
// BLKTRACESETUP2 (Linux 6.19+; records are struct blk_io_trace2, with a
// 64-bit action field).
type BLK_user_trace_setup2 struct {
	Name      [64]byte // output
	Act_mask  uint64   // input
	Buf_size  uint32   // input
	Buf_nr    uint32   // input
	Start_lba uint64
	End_lba   uint64
	Pid       uint32
	Flags     uint32 // must be zero
	Reserved  [11]uint64
}

// The BLKTRACE* ioctl numbers: _IOWR(0x12, 115, struct
// blk_user_trace_setup), _IO(0x12, 116..118) and _IOWR(0x12, 142, struct
// blk_user_trace_setup2), using the generic ioctl encoding shared by x86,
// arm, arm64 and riscv64. (mips/ppc/sparc encode ioctl direction bits
// differently and would need their own values.)
const (
	BLKTRACESETUP    = 0xc0000000 | (unsafe.Sizeof(BLK_user_trace_setup{}) << 16) | 0x1273
	BLKTRACESTART    = 0x1274
	BLKTRACESTOP     = 0x1275
	BLKTRACETEARDOWN = 0x1276
	BLKTRACESETUP2   = 0xc0000000 | (unsafe.Sizeof(BLK_user_trace_setup2{}) << 16) | 0x128e
)
