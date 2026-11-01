package main

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"unsafe"
)

func TestAlignedBuffer(t *testing.T) {
	for _, align := range []int{512, 4096, 65536} {
		b := alignedBuffer(12345, align)
		if len(b) != 12345 || cap(b) != 12345 {
			t.Errorf("align %d: len %d cap %d, want 12345", align, len(b), cap(b))
		}
		if uintptr(unsafe.Pointer(&b[0]))%uintptr(align) != 0 {
			t.Errorf("align %d: buffer not aligned", align)
		}
	}
}

func TestAlignRun(t *testing.T) {
	cases := []struct{ start, count, lbs, device, first, end uint64 }{
		{5, 1, 1, 100, 5, 6},    // 512-byte logical blocks: unchanged
		{5, 1, 8, 100, 0, 8},    // 4Kn: widened to the whole block
		{7, 2, 8, 100, 0, 16},   // straddles two blocks
		{16, 8, 8, 100, 16, 24}, // already aligned
		{90, 8, 8, 96, 88, 96},  // clipped at the device end
		{95, 1, 8, 96, 88, 96},  // last sector
		{2040, 2048, 8, 1e9, 2040, 4088},
	}
	for _, c := range cases {
		first, end := alignRun(c.start, c.count, c.lbs, c.device)
		if first != c.first || end != c.end {
			t.Errorf("alignRun(%d, %d, %d, %d) = %d, %d; want %d, %d", c.start, c.count, c.lbs, c.device, first, end, c.first, c.end)
		}
	}
}

// copyDirty sections must restore to exactly the device's current content
// over a stale main-pass copy, for 512-byte and 4 KiB logical blocks.
func TestCopyDirtyRestoresCurrentData(t *testing.T) {
	for _, lbsSectors := range []uint64{1, 8} {
		t.Run(fmt.Sprintf("lbs=%d", lbsSectors*512), func(t *testing.T) {
			const sectors = 4096
			rng := rand.New(rand.NewSource(int64(lbsSectors)))
			old := make([]byte, sectors*512)
			rng.Read(old)

			// The device as it is after some writes: a few changed runs,
			// one turned to zeros, one at the very end.
			now := append([]byte(nil), old...)
			d := DirtySectorTracker{}
			d.Setup(sectors)
			for _, w := range []struct{ start, count uint64 }{{3, 1}, {100, 37}, {1000, 3000}, {sectors - 1, 1}} {
				rng.Read(now[w.start*512 : (w.start+w.count)*512])
				d.SetRange(w.start, w.count)
			}
			for i := 200 * 512; i < 300*512; i++ {
				now[i] = 0
			}
			d.SetRange(200, 100)

			devPath := filepath.Join(t.TempDir(), "dev")
			if err := os.WriteFile(devPath, now, 0o644); err != nil {
				t.Fatal(err)
			}
			dev, err := os.Open(devPath)
			if err != nil {
				t.Fatal(err)
			}
			defer dev.Close()

			// The image: the stale main-pass copy, then the catch-up.
			var img bytes.Buffer
			cw := newCountingCRCWriter(&img)
			fmt.Fprintf(cw, "This-Is-A-Hot-Clone-Image See: https://github.com/benjojo/hot-clone\n")
			fmt.Fprintf(cw, "S:0\tL:%d\n", len(old))
			cw.Write(old)
			buf := alignedBuffer(int(maxRunSectors+2*lbsSectors)*512, 4096)
			taken, err := copyDirty(dev, cw, &d, lbsSectors, sectors, buf)
			if err != nil {
				t.Fatal(err)
			}
			if taken != 1+37+3000+1+100 {
				t.Errorf("took %d dirty sectors, want %d", taken, 1+37+3000+1+100)
			}
			img.WriteString(formatEndTrailer(cw.Sum32(), cw.Bytes()))

			got := runReassemble(t, img.Bytes(), false)
			if !bytes.Equal(got, now) {
				t.Fatal("restored image differs from the device's current content")
			}
		})
	}
}
