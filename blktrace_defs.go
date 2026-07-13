//go:build linux

package main

import "unsafe"

// Kernel ABI types and ioctl numbers for blktrace, from
// <linux/blktrace_api.h>. These used to come from a vendored fork of
// golang.org/x/sys; they are the only things hot-clone needed from it, so
// they live here instead and the stock golang.org/x/sys module is used for
// everything else.

// BLK_io_trace is struct blk_io_trace: the fixed-size event header that
// precedes each (optionally payload-carrying) event in the per-CPU relay
// files. Field order and sizes must match the kernel exactly because events
// are decoded with binary.Read.
type BLK_io_trace struct {
	Magic    uint32 // MAGIC_NATIVE | version
	Sequence uint32 // event number
	Time     uint64 // in nanoseconds
	Sector   uint64 // disk offset
	Bytes    uint32 // transfer length
	Action   uint32 // what happened
	Pid      uint32 // who did it
	Device   uint32 // device identifier
	Cpu      uint32 // on what cpu did it happen
	Error    uint16 // completion error
	Len      uint16 // length of data after this trace
}

// BLK_user_trace_setup is struct blk_user_trace_setup, the argument to
// BLKTRACESETUP. No explicit padding: Go's field alignment reproduces the
// kernel's layout on every arch where C does the same (amd64, arm64, 386),
// and BLKTRACESETUP below encodes sizeof, so the two cannot drift apart.
type BLK_user_trace_setup struct {
	Name      [32]byte // output: kernel fills in the debugfs dir name
	Act_mask  uint16   // input: event categories to trace
	Buf_size  uint32   // input: size of each relay sub-buffer
	Buf_nr    uint32   // input: number of relay sub-buffers
	Start_lba uint64
	End_lba   uint64
	Pid       uint32
}

// The BLKTRACE* ioctl numbers, _IOWR(0x12, 115, struct blk_user_trace_setup)
// and _IO(0x12, 116..118), using the generic ioctl encoding shared by x86,
// arm, arm64 and riscv64. (mips/ppc/sparc encode ioctl direction bits
// differently and would need their own values.)
const (
	BLKTRACESETUP    = 0xc0000000 | (unsafe.Sizeof(BLK_user_trace_setup{}) << 16) | 0x1273
	BLKTRACESTART    = 0x1274
	BLKTRACESTOP     = 0x1275
	BLKTRACETEARDOWN = 0x1276
)
