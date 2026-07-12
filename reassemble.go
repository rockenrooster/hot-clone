package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
)

var (
	reassemblePath   = flag.String("reassemble", "", "use this hot-clone backup file to restore into a file or block device")
	reassembleOutput = flag.String("reassemble-output", "", "The path of the file or block device that is going to be restored to")
)

func reassembleMain() {
	var imageFd *os.File
	var err error
	if *reassemblePath == "-" {
		imageFd = os.Stdin
	} else {
		imageFd, err = os.Open(*reassemblePath)
		if err != nil {
			log.Fatalf("Can't open image file -reassemble %v -- %v", *reassemblePath, err)
		}
	}
	imageReader := bufio.NewReaderSize(imageFd, 1024*1024)

	if *reassembleOutput == "" {
		log.Fatalf("You must provide a -reassemble-output to restore to")
	}

	// First we should 100% check that we are dealing with a hot-clone image file
	ReadBanner, err := imageReader.ReadString('\n')
	if err != nil && err != io.EOF {
		log.Fatalf("Failed to read image banner %v", err)
	}

	if !strings.Contains(ReadBanner, "Hot-Clone") {
		log.Fatalf("This image does not seem to be the output of hot-clone")
	}

	outputStat, err := os.Stat(*reassembleOutput)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Fatalf("Unable to stat output target %v", err)
		}
	}

	var outputFD *os.File
	if outputStat != nil {
		if outputStat.Mode()&os.ModeDevice != 0 {
			outputFD, err = os.OpenFile(*reassembleOutput, os.O_RDWR, 0777)
		} else {
			outputFD, err = os.Create(*reassembleOutput)
		}
	} else {
		outputFD, err = os.Create(*reassembleOutput)
	}

	if err != nil {
		log.Fatalf("Can't open/create output %v", err)
	}

	n := 0
	buf := make([]byte, 4096)
	for {
		ReadHeader, err := imageReader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			log.Fatalf("Failed to read image header %v", err)
		}

		var SectorStart, BytesLeftToRead int64
		parsed, err := fmt.Sscanf(ReadHeader, "S:%d\tL:%d\n", &SectorStart, &BytesLeftToRead)
		if parsed != 2 {
			log.Fatalf("Failed to parse header (%v) -- aborting (%v (%x) - %v - %v)", ReadHeader, ReadHeader, err, SectorStart, BytesLeftToRead)
		}
		if SectorStart < 0 || BytesLeftToRead < 0 {
			log.Fatalf("Invalid header values (%v) -- aborting", strings.Trim(ReadHeader, "\n"))
		}

		_, err = outputFD.Seek(SectorStart*512, 0)
		if err != nil {
			log.Fatalf("Seek failure (to %d) on output file/device %v", SectorStart, err)
		}
		if n == 0 || n%1000 == 0 {
			if *debug {
				log.Printf("Restoring section (Sector: %v (len %d bytes) (debug: '%s')", SectorStart, BytesLeftToRead, strings.Trim(ReadHeader, "\n"))
			} else {
				log.Printf("Restoring section (Sector: %v (len %d bytes)", SectorStart, BytesLeftToRead)
			}
		}
		n++

		for BytesLeftToRead > 0 {
			expectedRead := 4096
			if BytesLeftToRead < 4096 {
				expectedRead = int(BytesLeftToRead)
			}

			rn, err := imageReader.Read(buf[:expectedRead])
			if err != nil {
				log.Fatalf("Image read failure -- %v", err)
			}
			if rn != expectedRead {
				log.Printf("Image short read -- %v != %v (have %d bytes left)", rn, expectedRead, BytesLeftToRead)
			}
			BytesLeftToRead = BytesLeftToRead - int64(rn)

			_, err = outputFD.Write(buf[:rn])
			if err != nil {
				log.Fatalf("Output file/device write failure -- %v", err)
			}
		}

	}

	if err := outputFD.Sync(); err != nil {
		log.Fatalf("Failed to sync restored data to disk -- %v", err)
	}
	if err := outputFD.Close(); err != nil {
		log.Fatalf("Failed to close output -- %v", err)
	}
}
