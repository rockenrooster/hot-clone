package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

var DST DirtySectorTracker
var debug = flag.Bool("verbose", false, "be extra verbose on whats happening")

func main() {
	dev := flag.String("device", "", "The device you wish to hot-clone")
	flag.Parse()

	// Nope, we are restoring instead
	if *reassemblePath != "" {
		reassembleMain()
		return
	}

	info := syscall.Sysinfo_t{}
	syscall.Sysinfo(&info)
	deviceBaseName := filepath.Base(*dev)
	eventConsumer := make(chan unix.BLK_io_trace, 100)

	f, err := os.Open(*dev)
	if err != nil {
		log.Fatalf("cannot open device %v - %v", *dev, err)
	}

	diskSectorsCount := getTotalDeviceSectorsSize(deviceBaseName)
	setupBlkTrace(err, f, eventConsumer, deviceBaseName)
	defer shutdownBlkTrace(f)

	DST = DirtySectorTracker{}
	DST.Setup(diskSectorsCount)

	go func() {
		for {
			time.Sleep(time.Second)
			DST.CountDirty()
			TotalRead := atomic.LoadInt64(&bytesRead)
			eventDrops := getBlkTraceDrops(deviceBaseName)
			log.Printf("Read %s -- %v Dirty sectors (%d event drops)", ByteCountIEC(TotalRead), DST.DirtySectors, eventDrops)
			if eventDrops != 0 {
				fatalf("Event drops detected, cannot safely image device anymore")
			}
		}
	}()

	trackerDone := make(chan struct{})
	go trackEvents(eventConsumer, info, trackerDone)

	// Begin reading the block device
	BlockF, err := os.Open(*dev)
	if err != nil {
		fatalf("cannot open block device %v - %v", *dev, err)
	}

	os.Stdout.WriteString("This-Is-A-Hot-Clone-Image See: https://github.com/benjojo/hot-clone\n")
	os.Stdout.WriteString(fmt.Sprintf("S:0\tL:%d\n", diskSectorsCount*512))
	TotalRead := int64(0) // for use below only!!!
	BytesLeftToRead := int(diskSectorsCount * 512)
	buf := make([]byte, 1024*1024)
	for BytesLeftToRead > 0 {
		expectedRead := 1024 * 1024
		if BytesLeftToRead < 1024*1024 {
			expectedRead = BytesLeftToRead
		}

		chunk := buf[:expectedRead]
		n, err := io.ReadFull(BlockF, chunk)
		if err != nil {
			fatalf("Disk read failure -- %v (read %d of %d bytes, had %d bytes left)", err, n, expectedRead, BytesLeftToRead)
		}
		TotalRead += int64(n)
		atomic.StoreInt64(&bytesRead, TotalRead)
		BytesLeftToRead = BytesLeftToRead - n

		_, err = os.Stdout.Write(chunk)
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

		if (uint64(bytesRead) > diskSectorsCount*512) && !alreadyWarnedAboutOverread {
			// Okay very interesting, the block layer let us read more data than there were sectors!
			alreadyWarnedAboutOverread = true
			log.Printf("Strange device! Lets us read more data than there are sectors!!!")
		}
		_, err = os.Stdout.WriteString(fmt.Sprintf("S:%d\tL:%d\n", tmpSectors, n))
		if err != nil {
			fatalf("Output file/device write failure -- %v", err)
		}
		_, err = os.Stdout.Write(overreadBuf[:n])
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

	// Stop tracing, then wait for every event still sitting in the kernel relay
	// buffers and the channel to be applied to the bitmap. Snapshotting the
	// dirty set with events still in flight would lose writes.
	stopAndDrainBlkTrace(f)
	close(eventConsumer)
	<-trackerDone

	// Final drop check: the counter file disappears at teardown, and the
	// periodic check may not run again before we finish.
	if drops := getBlkTraceDrops(deviceBaseName); drops != 0 {
		fatalf("Event drops detected (%d), cannot safely image device", drops)
	}
	shutdownBlkTrace(f)

	// now let's catch up
	DST.CountDirty()
	totalDirty := DST.DirtySectors
	progressStep := totalDirty / 10
	if progressStep < 1 {
		progressStep = 1
	}
	out := bufio.NewWriterSize(os.Stdout, 1024*1024)
	sectorBuf := make([]byte, 512)
	dirtySectorChannel := DST.GetDirtySectors()
	n := 0
	for sector := range dirtySectorChannel {
		_, err := BlockF.Seek(int64(sector)*512, 0)
		if err != nil {
			fatalf("Seek failure (to sector %d) for catchup -- %v", sector, err)
		}
		_, err = io.ReadFull(BlockF, sectorBuf)
		if err != nil {
			fatalf("Read for catchup failed (sector %d) -- %v", sector, err)
		}
		fmt.Fprintf(out, "S:%d\tL:%d\n", sector, 512)
		_, err = out.Write(sectorBuf)
		if err != nil {
			fatalf("Output file/device write failure -- %v", err)
		}
		n++
		if n%progressStep == 0 {
			log.Printf("Catching up %d/%d sectors", n, totalDirty)
		}
	}
	err = out.Flush()
	if err != nil {
		fatalf("Output file/device write failure -- %v", err)
	}
	log.Printf("Done")
}

var bytesRead int64

// tracedFile is set once blktrace is enabled on the device. log.Fatalf exits
// without running defers, so fatal paths must tear the trace down themselves.
var tracedFile *os.File

func fatalf(format string, v ...interface{}) {
	if tracedFile != nil {
		shutdownBlkTrace(tracedFile)
	}
	log.Fatalf(format, v...)
}

var dumpWrites = flag.Bool("print-writes", false, "print all writes happening")

func trackEvents(eventConsumer chan unix.BLK_io_trace, info syscall.Sysinfo_t, done chan struct{}) {
	defer close(done)
	for event := range eventConsumer {
		if event.Action&(1<<BLK_TC_WRITE) > 0 {
			if *dumpWrites {
				log.Printf("Write: Sector %#v (%d) (%d bytes) | F: %x (%s)", event.Sector, event.Sector, event.Bytes, event.Action, unpackBits(event.Action))
			}
			if event.Sector == 0 && event.Bytes == 0 {
				continue
			}
			// Every write is marked dirty regardless of how far the sequential
			// read has progressed: a queued write can complete after the read
			// passes it, so whether the read observed it cannot be known.
			DST.SetDirty(event.Sector)
			otherSectors := uint64(event.Bytes) / 512
			for i := uint64(1); i < otherSectors; i++ {
				DST.SetDirty(event.Sector + i)
			}
		} else {
			if *dumpWrites {
				log.Printf("????: Sector %#v (%d) (%d bytes) | F: %x (%s)", event.Sector, event.Sector, event.Bytes, event.Action, unpackBits(event.Action))
			}
		}
	}
}

func ByteCountIEC(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB",
		float64(b)/float64(div), "KMGTPE"[exp])
}
