//go:build linux

package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

var DST DirtySectorTracker

var (
	dumpWrites     = flag.Bool("print-writes", false, "print all trace events (writes) as they arrive")
	allowPartition = flag.Bool("allow-partition", false, "allow imaging a partition device (UNSAFE: blktrace reports absolute disk sectors and partition handling is unverified, concurrent writes may be tracked at the wrong offsets)")
)

var bytesRead int64

// catchingUp is set once the main pass is done, so the status reporter
// stops printing read progress.
var catchingUp int32

// tracedFile is set once blktrace is enabled on the device. log.Fatalf exits
// without running defers, so fatal paths must tear the trace down themselves.
var tracedFile *os.File

func fatalf(format string, v ...interface{}) {
	if tracedFile != nil {
		shutdownBlkTrace(tracedFile)
	}
	log.Fatalf(format, v...)
}

const (
	// mainReadSize is how much the main pass reads per O_DIRECT request.
	mainReadSize = 4 << 20

	// Catch-up rounds run with the trace still on, so that writes landing
	// meanwhile are caught by the next round, until a round has at most
	// convergedSectors to re-read, stops shrinking, or maxCatchUpRounds
	// have run. Only the last, small round runs with tracing stopped.
	convergedSectors = 2048
	maxCatchUpRounds = 8
)

func imageMain() {
	if *dev == "" {
		log.Fatalf("-device is required (or -reassemble to restore an image)")
	}
	d := resolveDevice(*dev)
	if *resetTrace {
		resetTraceMain(d)
		return
	}
	if d.isPartition && !*allowPartition {
		log.Fatalf("%v is a partition. blktrace reports absolute disk sectors and hot-clone's partition handling is unverified, so writes landing during the copy could be tracked at the wrong offsets and leave silently stale data in the image. Image the whole disk instead, or pass -allow-partition to accept the risk.", *dev)
	}
	lockDevice(d)

	f, err := os.Open(*dev)
	if err != nil {
		log.Fatalf("cannot open device %v - %v", *dev, err)
	}

	// All image data is read with O_DIRECT, straight from the device. The
	// whole-disk page cache is not kept coherent with writes made through
	// partitions, filesystems or stacked devices, so a cached page can be
	// stale; reading one would put old data in the image even for sectors
	// the trace marked dirty. It also keeps a whole-device read from
	// flushing the live system's cache.
	BlockF, err := os.OpenFile(*dev, os.O_RDONLY|unix.O_DIRECT, 0)
	if err != nil {
		log.Fatalf("cannot open block device %v - %v", *dev, err)
	}
	lbs, err := unix.IoctlGetInt(int(BlockF.Fd()), unix.BLKSSZGET)
	if err != nil {
		log.Fatalf("cannot get logical block size of %v - %v", *dev, err)
	}
	align := lbs
	if align < 4096 {
		align = 4096
	}
	lbsSectors := uint64(lbs) / 512

	diskSectorsCount := getTotalDeviceSectorsSize(d.name)
	DST = DirtySectorTracker{}
	DST.Setup(diskSectorsCount)

	setupBlkTrace(f, d)
	defer shutdownBlkTrace(f)

	// Periodic progress/safety reporter. It must be stopped before blktrace
	// teardown removes the dropped-events file it polls.
	totalBytes := int64(diskSectorsCount * 512)
	statusDone := make(chan struct{})
	statusStopped := make(chan struct{})
	go func() {
		defer close(statusStopped)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		lastBytes := int64(0)
		for {
			select {
			case <-statusDone:
				return
			case <-ticker.C:
			}
			checkBlkTraceDrops(d.name)
			if atomic.LoadInt32(&catchingUp) != 0 {
				continue
			}
			TotalRead := atomic.LoadInt64(&bytesRead)
			rate := TotalRead - lastBytes
			lastBytes = TotalRead
			eta := "?"
			if rate > 0 && TotalRead <= totalBytes {
				eta = (time.Duration((totalBytes-TotalRead)/rate) * time.Second).String()
			}
			log.Printf("Read %s of %s (%s/s, ETA %s) -- %v dirty sectors",
				ByteCountIEC(TotalRead), ByteCountIEC(totalBytes), ByteCountIEC(rate), eta, DST.Count())
		}
	}()

	// Everything written through cw is covered by the CRC/byte count that
	// the end trailer records.
	cw := newCountingCRCWriter(os.Stdout)
	if _, err := fmt.Fprintf(cw, "This-Is-A-Hot-Clone-Image See: https://github.com/benjojo/hot-clone\n"); err != nil {
		fatalf("Output file/device write failure -- %v", err)
	}
	if _, err := fmt.Fprintf(cw, "S:0\tL:%d\n", diskSectorsCount*512); err != nil {
		fatalf("Output file/device write failure -- %v", err)
	}
	TotalRead := int64(0) // for use below only!!!
	BytesLeftToRead := int64(diskSectorsCount * 512)
	buf := alignedBuffer(mainReadSize, align)
	for BytesLeftToRead > 0 {
		expectedRead := int64(mainReadSize)
		if BytesLeftToRead < expectedRead {
			expectedRead = BytesLeftToRead
		}

		chunk := buf[:expectedRead]
		n, err := io.ReadFull(BlockF, chunk)
		if err != nil {
			fatalf("Disk read failure -- %v (read %d of %d bytes, had %d bytes left)", err, n, expectedRead, BytesLeftToRead)
		}
		TotalRead += int64(n)
		atomic.StoreInt64(&bytesRead, TotalRead)
		BytesLeftToRead -= int64(n)

		_, err = cw.Write(chunk)
		if err != nil {
			fatalf("Output file/device write failure -- %v", err)
		}
	}

	alreadyWarnedAboutOverread := false
	tmpSectors := diskSectorsCount
	overreadBuf := make([]byte, 512)
	// Attempt to over-read, in case the block device is actually bigger
	for {
		n, err := BlockF.Read(overreadBuf)
		TotalRead += int64(n)
		atomic.StoreInt64(&bytesRead, TotalRead)
		if err == io.EOF {
			// we are done! time to image the other bits
			break
		} else if err != nil {
			break
		}

		if (uint64(TotalRead) > diskSectorsCount*512) && !alreadyWarnedAboutOverread {
			// Okay very interesting, the block layer let us read more data than there were sectors!
			alreadyWarnedAboutOverread = true
			log.Printf("Strange device! Lets us read more data than there are sectors!!!")
		}
		_, err = fmt.Fprintf(cw, "S:%d\tL:%d\n", tmpSectors, n)
		if err != nil {
			fatalf("Output file/device write failure -- %v", err)
		}
		_, err = cw.Write(overreadBuf[:n])
		if err != nil {
			fatalf("Output file/device write failure -- %v", err)
		}
		tmpSectors++
		if n != 512 {
			// we are now out of alignment, time to leave
			log.Printf("*And* the strange device gave us a shorter than sector read()?!")
			break
		}
	}

	if alreadyWarnedAboutOverread {
		log.Printf("Device overread by %d sectors", tmpSectors-diskSectorsCount)
	}

	// Catch up. Each round re-reads what was written since the previous
	// one; tracing stays on, so writes landing during a round are marked
	// for the next. A write is marked again when it completes, so a round
	// that read a sector before an in-flight write to it landed re-reads
	// it next time.
	atomic.StoreInt32(&catchingUp, 1)
	out := bufio.NewWriterSize(cw, 1024*1024)
	runBuf := alignedBuffer(int(maxRunSectors+2*lbsSectors)*512, align)
	catchUp := func(round string) int64 {
		taken, err := copyDirty(BlockF, out, &DST, lbsSectors, diskSectorsCount, runBuf)
		if err != nil {
			fatalf("%v", err)
		}
		log.Printf("Catch-up %s: re-read %d dirty sectors (%s)", round, taken, ByteCountIEC(taken*512))
		return taken
	}
	prev := int64(-1)
	for round := 1; round <= maxCatchUpRounds; round++ {
		taken := catchUp(fmt.Sprintf("round %d", round))
		if taken <= convergedSectors || (prev >= 0 && taken*4 > prev*3) {
			break
		}
		prev = taken
	}

	// Stop tracing, then wait for every event still sitting in the kernel
	// relay buffers to be applied to the bitmap. Taking the final dirty set
	// with events still in flight would lose writes.
	stopAndDrainBlkTrace(f)

	// Final drop check: the counter file disappears at teardown, and the
	// periodic check may not run again before we finish.
	checkBlkTraceDrops(d.name)
	close(statusDone)
	<-statusStopped
	shutdownBlkTrace(f)

	catchUp("final round, tracing stopped")
	err = out.Flush()
	if err != nil {
		fatalf("Output file/device write failure -- %v", err)
	}

	// The trailer itself is written outside cw: its CRC/BYTES describe
	// everything before it.
	if _, err := os.Stdout.WriteString(formatEndTrailer(cw.Sum32(), cw.Bytes())); err != nil {
		fatalf("Output file/device write failure -- %v", err)
	}
	log.Printf("Done")
}
