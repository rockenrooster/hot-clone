package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"strings"
)

// Decoding of the binary records in the kernel's per-CPU blktrace relay
// files. Two layouts exist, from <linux/blktrace_api.h>: struct
// blk_io_trace (version 7, written for sessions set up with
// BLKTRACESETUP) and struct blk_io_trace2 (version 8, for BLKTRACESETUP2,
// Linux 6.19+). Records are in the kernel's byte order; hot-clone targets
// little-endian machines, and the magic check rejects anything else.

const (
	blkIOTraceMagic    = 0x65617400
	blkIOTraceVersion1 = 0x07
	blkIOTraceVersion2 = 0x08

	blkIOTraceSize1 = 48 // sizeof(struct blk_io_trace)
	blkIOTraceSize2 = 64 // sizeof(struct blk_io_trace2)

	// The action field holds the event type in its low 16 bits and a mask
	// of trace categories (BLK_TC_*) above them.
	blkTCShift  = 16
	blkTCWrite  = 1 << 1
	blkTCNotify = 1 << 10
)

// traceEvent is the part of a blktrace record that hot-clone uses.
type traceEvent struct {
	Sector uint64 // in 512-byte units on the traced device
	Bytes  uint32
	Action uint64 // a v1 record's 32-bit action, zero-extended
	Error  uint16
	PDULen uint16 // payload bytes that follow the header
}

// traceHeaderSize checks a record's magic word and returns the length of
// the header it starts.
func traceHeaderSize(magic uint32) (int, error) {
	if magic&0xffffff00 != blkIOTraceMagic {
		return 0, fmt.Errorf("bad magic %#x", magic)
	}
	switch magic & 0xff {
	case blkIOTraceVersion1:
		return blkIOTraceSize1, nil
	case blkIOTraceVersion2:
		return blkIOTraceSize2, nil
	}
	return 0, fmt.Errorf("unsupported blktrace record version %d", magic&0xff)
}

// decodeTraceHeader decodes a complete record header.
func decodeTraceHeader(b []byte) (traceEvent, error) {
	le := binary.LittleEndian
	if len(b) < 4 {
		return traceEvent{}, fmt.Errorf("short record header (%d bytes)", len(b))
	}
	size, err := traceHeaderSize(le.Uint32(b))
	if err != nil {
		return traceEvent{}, err
	}
	if len(b) < size {
		return traceEvent{}, fmt.Errorf("short record header (%d of %d bytes)", len(b), size)
	}
	ev := traceEvent{Sector: le.Uint64(b[16:]), Bytes: le.Uint32(b[24:])}
	if size == blkIOTraceSize1 {
		ev.Action = uint64(le.Uint32(b[28:]))
		ev.Error = le.Uint16(b[44:])
		ev.PDULen = le.Uint16(b[46:])
	} else {
		ev.Action = le.Uint64(b[32:])
		ev.Error = le.Uint16(b[48:])
		ev.PDULen = le.Uint16(b[50:])
	}
	return ev, nil
}

// nextTraceRecord decodes the next complete record from a relay file
// without consuming it. size is 0 when no complete record is available
// yet, i.e. the reader has caught up with the kernel; otherwise the caller
// must Discard(size) once the event is handled. Nothing is consumed until
// a whole record is buffered, so running dry mid-record loses nothing.
func nextTraceRecord(r *bufio.Reader) (ev traceEvent, size int, err error) {
	head, err := peekFull(r, 4)
	if head == nil {
		return ev, 0, err
	}
	hdrSize, err := traceHeaderSize(binary.LittleEndian.Uint32(head))
	if err != nil {
		return ev, 0, err
	}
	hdr, err := peekFull(r, hdrSize)
	if hdr == nil {
		return ev, 0, err
	}
	if ev, err = decodeTraceHeader(hdr); err != nil {
		return ev, 0, err
	}
	size = hdrSize + int(ev.PDULen)
	if rec, err := peekFull(r, size); rec == nil {
		return ev, 0, err
	}
	return ev, size, nil
}

// peekFull returns the next n bytes without consuming them, or nil (and a
// nil error) if fewer than n are available right now.
func peekFull(r *bufio.Reader, n int) ([]byte, error) {
	b, err := r.Peek(n)
	if len(b) == n {
		return b, nil
	}
	if err == io.EOF {
		return nil, nil
	}
	return nil, err
}

// dirtyRange returns the sectors a record shows being written. Every event
// in the write category counts -- queue, merge, request issue, completion,
// remap and the rest -- because which of them a write produces depends on
// the device: a bio-based device (device-mapper/LVM, md RAID) never issues
// requests, and a write already in flight when tracing started shows up
// only as its completion. Marking a sector more than once is harmless.
func (e traceEvent) dirtyRange() (start, count uint64, ok bool) {
	if e.Action&(blkTCWrite<<blkTCShift) == 0 || e.Bytes == 0 {
		return 0, 0, false
	}
	return e.Sector, (uint64(e.Bytes) + 511) / 512, true
}

// Event types (the low byte of the action) as blkparse abbreviates them.
var traceActionCodes = map[uint64]string{
	1: "Q", 2: "M", 3: "F", 4: "G", 5: "S", 6: "R", 7: "D", 8: "C",
	9: "P", 10: "U", 11: "UT", 12: "I", 13: "X", 14: "B", 15: "A",
	16: "abort", 17: "drv-data", 18: "zone-plug", 19: "zone-unplug",
}

// Trace categories, indexed by bit above blkTCShift.
var traceCategories = []string{
	"read", "write", "flush", "sync", "queue", "requeue", "issue",
	"complete", "fs", "pc", "notify", "readahead", "meta", "discard",
	"drv-data", "fua", "zone-append", "zone-reset", "zone-reset-all",
	"zone-finish", "zone-open", "zone-close", "write-zeroes",
}

// describe renders an event for -print-writes.
func (e traceEvent) describe() string {
	code := traceActionCodes[e.Action&0xff]
	if e.Action&(blkTCNotify<<blkTCShift) != 0 {
		code = "N"
	}
	if code == "" {
		code = fmt.Sprintf("?%d", e.Action&0xff)
	}
	var cats []string
	for i, name := range traceCategories {
		if e.Action&(1<<(blkTCShift+uint(i))) != 0 {
			cats = append(cats, name)
		}
	}
	return fmt.Sprintf("%s sector %d (%d bytes) [%s] action %#x", code, e.Sector, e.Bytes, strings.Join(cats, ","), e.Action)
}
