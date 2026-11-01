package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"io"
	"testing"
)

const (
	taQueue    = 1 // __BLK_TA_QUEUE
	taIssue    = 7 // __BLK_TA_ISSUE
	taComplete = 8 // __BLK_TA_COMPLETE

	tcRead        = 1 << 0
	tcQueue       = 1 << 4
	tcComplete    = 1 << 7
	tcFUA         = 1 << 15
	tcWriteZeroes = 1 << 22
)

func act(code uint64, categories uint64) uint64 { return code | categories<<blkTCShift }

// recordV1 builds a struct blk_io_trace record as the kernel lays it out.
func recordV1(sector uint64, nbytes uint32, action uint32, pdu []byte) []byte {
	b := make([]byte, blkIOTraceSize1+len(pdu))
	le := binary.LittleEndian
	le.PutUint32(b[0:], blkIOTraceMagic|blkIOTraceVersion1)
	le.PutUint32(b[4:], 42) // sequence
	le.PutUint64(b[8:], 123456789)
	le.PutUint64(b[16:], sector)
	le.PutUint32(b[24:], nbytes)
	le.PutUint32(b[28:], action)
	le.PutUint32(b[32:], 1000) // pid
	le.PutUint32(b[36:], 8<<20)
	le.PutUint32(b[40:], 3) // cpu
	le.PutUint16(b[46:], uint16(len(pdu)))
	copy(b[blkIOTraceSize1:], pdu)
	return b
}

// recordV2 builds a struct blk_io_trace2 record as the kernel lays it out.
func recordV2(sector uint64, nbytes uint32, action uint64, pdu []byte) []byte {
	b := make([]byte, blkIOTraceSize2+len(pdu))
	le := binary.LittleEndian
	le.PutUint32(b[0:], blkIOTraceMagic|blkIOTraceVersion2)
	le.PutUint32(b[4:], 42)
	le.PutUint64(b[8:], 123456789)
	le.PutUint64(b[16:], sector)
	le.PutUint32(b[24:], nbytes)
	le.PutUint32(b[28:], 1000) // pid
	le.PutUint64(b[32:], action)
	le.PutUint32(b[40:], 8<<20)
	le.PutUint32(b[44:], 3)
	le.PutUint16(b[50:], uint16(len(pdu)))
	copy(b[blkIOTraceSize2:], pdu)
	return b
}

func TestDecodeTraceHeader(t *testing.T) {
	v1, err := decodeTraceHeader(recordV1(2048, 4096, uint32(act(taIssue, blkTCWrite|1<<6)), nil))
	if err != nil {
		t.Fatal(err)
	}
	if v1.Sector != 2048 || v1.Bytes != 4096 || v1.Action != act(taIssue, blkTCWrite|1<<6) || v1.PDULen != 0 {
		t.Errorf("v1 decoded as %+v", v1)
	}

	wide := act(taQueue, blkTCWrite|tcQueue|tcWriteZeroes)
	v2, err := decodeTraceHeader(recordV2(4096, 16384, wide, []byte{1, 2, 3}))
	if err != nil {
		t.Fatal(err)
	}
	if v2.Sector != 4096 || v2.Bytes != 16384 || v2.Action != wide || v2.PDULen != 3 {
		t.Errorf("v2 decoded as %+v", v2)
	}
}

func TestTraceHeaderRejectsBadRecords(t *testing.T) {
	bad := recordV1(0, 0, 0, nil)
	binary.LittleEndian.PutUint32(bad, 0xdeadbeef)
	if _, err := decodeTraceHeader(bad); err == nil {
		t.Error("accepted a record with a bad magic")
	}
	future := recordV1(0, 0, 0, nil)
	binary.LittleEndian.PutUint32(future, blkIOTraceMagic|0x09)
	if _, err := decodeTraceHeader(future); err == nil {
		t.Error("accepted an unknown record version")
	}
}

// Every write-category event marks its range -- including queue and
// completion events, the only ones a bio-based device (LVM, md) produces,
// and FUA / write-zeroes writes. Reads and notify records never do.
func TestDirtyRange(t *testing.T) {
	cases := []struct {
		name   string
		ev     traceEvent
		marked bool
		count  uint64
	}{
		{"queued write (bio-based device)", traceEvent{Sector: 100, Bytes: 4096, Action: act(taQueue, blkTCWrite|tcQueue)}, true, 8},
		{"completed write", traceEvent{Sector: 100, Bytes: 4096, Action: act(taComplete, blkTCWrite|tcComplete)}, true, 8},
		{"issued write", traceEvent{Sector: 100, Bytes: 512, Action: act(taIssue, blkTCWrite|1<<6)}, true, 1},
		{"FUA write", traceEvent{Sector: 7, Bytes: 4096, Action: act(taQueue, blkTCWrite|tcQueue|tcFUA)}, true, 8},
		{"write-zeroes", traceEvent{Sector: 0, Bytes: 1 << 20, Action: act(taQueue, blkTCWrite|tcQueue|tcWriteZeroes)}, true, 2048},
		{"partial sector rounds up", traceEvent{Sector: 0, Bytes: 513, Action: act(taIssue, blkTCWrite)}, true, 2},
		{"read", traceEvent{Sector: 100, Bytes: 4096, Action: act(taIssue, tcRead|1<<6)}, false, 0},
		{"process notify", traceEvent{Sector: 0, Bytes: 0, Action: act(0, blkTCNotify)}, false, 0},
		{"zero-length write (flush)", traceEvent{Sector: 0, Bytes: 0, Action: act(taIssue, blkTCWrite)}, false, 0},
	}
	for _, c := range cases {
		start, count, ok := c.ev.dirtyRange()
		if ok != c.marked || (ok && (start != c.ev.Sector || count != c.count)) {
			t.Errorf("%s: dirtyRange() = %d, %d, %v; want %d, %d, %v", c.name, start, count, ok, c.ev.Sector, c.count, c.marked)
		}
	}
}

// trickleReader returns its data a little at a time and reports EOF
// whenever it has handed out everything released so far, like a relay
// file that has caught up with the kernel.
type trickleReader struct {
	data     []byte
	released int
	pos      int
}

func (r *trickleReader) Read(p []byte) (int, error) {
	if r.pos >= r.released {
		return 0, io.EOF
	}
	n := copy(p[:min(len(p), 5)], r.data[r.pos:r.released])
	r.pos += n
	return n, nil
}

// Records must come out whole however the bytes arrive, and running dry
// mid-record must not consume anything.
func TestNextTraceRecordPartialData(t *testing.T) {
	var stream bytes.Buffer
	stream.Write(recordV1(10, 4096, uint32(act(taIssue, blkTCWrite)), nil))
	stream.Write(recordV2(20, 8192, act(taQueue, blkTCWrite|tcFUA), []byte("payload")))
	stream.Write(recordV1(30, 512, uint32(act(taComplete, blkTCWrite)), nil))

	src := &trickleReader{data: stream.Bytes()}
	r := bufio.NewReaderSize(src, 1<<16)
	var got []uint64
	for src.released < len(src.data) || r.Buffered() > 0 {
		ev, size, err := nextTraceRecord(r)
		if err != nil {
			t.Fatal(err)
		}
		if size == 0 {
			// caught up: let the "kernel" produce 7 more bytes
			src.released = min(src.released+7, len(src.data))
			continue
		}
		got = append(got, ev.Sector)
		r.Discard(size)
	}
	if len(got) != 3 || got[0] != 10 || got[1] != 20 || got[2] != 30 {
		t.Fatalf("decoded sectors %v, want [10 20 30]", got)
	}
}

func TestNextTraceRecordRejectsGarbage(t *testing.T) {
	r := bufio.NewReader(bytes.NewReader([]byte("this is not a blktrace record at all")))
	if _, _, err := nextTraceRecord(r); err == nil {
		t.Fatal("accepted garbage")
	}
}
