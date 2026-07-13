package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"hash/crc32"
	"io"
	"log"
	"os"
	"strings"
)

var (
	reassemblePath        = flag.String("reassemble", "", "use this hot-clone backup file to restore into a file or block device")
	reassembleOutput      = flag.String("reassemble-output", "", "The path of the file or block device that is going to be restored to")
	reassembleAllowLegacy = flag.Bool("reassemble-allow-legacy", false, "allow restoring an image without an end trailer (made by hot-clone v1.0.1 or older); completeness and integrity cannot be verified")
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
		defer imageFd.Close()
	}
	imageReader := bufio.NewReaderSize(imageFd, 1024*1024)

	if *reassembleOutput == "" {
		log.Fatalf("You must provide a -reassemble-output to restore to")
	}

	// Mirror of the imaging side's countingCRCWriter: every stream byte
	// before the end trailer is folded in, so the trailer can be verified.
	crc := crc32.New(castagnoli)
	streamBytes := int64(0)

	// First we should 100% check that we are dealing with a hot-clone image file
	ReadBanner, err := imageReader.ReadString('\n')
	if err != nil && err != io.EOF {
		log.Fatalf("Failed to read image banner %v", err)
	}

	if !strings.Contains(ReadBanner, "Hot-Clone") {
		log.Fatalf("This image does not seem to be the output of hot-clone")
	}
	crc.Write([]byte(ReadBanner))
	streamBytes += int64(len(ReadBanner))

	outputStat, err := os.Stat(*reassembleOutput)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Fatalf("Unable to stat output target %v", err)
		}
	}

	isDevice := outputStat != nil && outputStat.Mode()&os.ModeDevice != 0
	isBlockDevice := isDevice && outputStat.Mode()&os.ModeCharDevice == 0

	var outputFD *os.File
	if isDevice {
		outputFD, err = os.OpenFile(*reassembleOutput, os.O_RDWR, 0777)
	} else {
		outputFD, err = os.Create(*reassembleOutput)
	}

	if err != nil {
		log.Fatalf("Can't open/create output %v", err)
	}

	// A block device that is smaller than the image can never hold it;
	// refuse before writing anything rather than failing partway through.
	deviceSize := int64(-1)
	if isBlockDevice {
		deviceSize, err = outputFD.Seek(0, io.SeekEnd)
		if err != nil {
			log.Fatalf("Cannot determine size of output device %v -- %v", *reassembleOutput, err)
		}
		if _, err := outputFD.Seek(0, io.SeekStart); err != nil {
			log.Fatalf("Seek failure on output device %v -- %v", *reassembleOutput, err)
		}
	}

	// Zero-filled chunks are skipped (left as holes) when restoring to a
	// regular file: os.Create truncated it, so unwritten ranges read back
	// as zeros anyway. Devices keep their old contents and must be
	// overwritten in full.
	sparseOK := !isDevice

	n := 0
	buf := make([]byte, 4096)
	zeroBuf := make([]byte, 4096)
	sawTrailer := false
	maxEnd := int64(0)
	for {
		ReadHeader, err := imageReader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				if ReadHeader != "" {
					log.Fatalf("Image ends with a truncated header (%q) -- the image is incomplete", ReadHeader)
				}
				break
			}
			log.Fatalf("Failed to read image header %v", err)
		}

		if trailerCRC, trailerBytes, ok := parseEndTrailer(ReadHeader); ok {
			if streamBytes != trailerBytes {
				log.Fatalf("Image is damaged: trailer says %d bytes, stream contained %d -- the restored data must not be trusted", trailerBytes, streamBytes)
			}
			if crc.Sum32() != trailerCRC {
				log.Fatalf("Image is damaged: CRC mismatch (trailer %08x, computed %08x) -- the restored data must not be trusted", trailerCRC, crc.Sum32())
			}
			if _, err := imageReader.ReadByte(); err != io.EOF {
				log.Fatalf("Unexpected data after the end trailer -- image is damaged or was concatenated")
			}
			sawTrailer = true
			break
		}
		crc.Write([]byte(ReadHeader))
		streamBytes += int64(len(ReadHeader))

		var SectorStart, SectionLength int64
		parsed, err := fmt.Sscanf(ReadHeader, "S:%d\tL:%d\n", &SectorStart, &SectionLength)
		if parsed != 2 {
			log.Fatalf("Failed to parse header (%v) -- aborting (%v (%x) - %v - %v)", ReadHeader, ReadHeader, err, SectorStart, SectionLength)
		}
		if SectorStart < 0 || SectionLength < 0 {
			log.Fatalf("Invalid header values (%v) -- aborting", strings.Trim(ReadHeader, "\n"))
		}

		sectionEnd := SectorStart*512 + SectionLength
		if deviceSize >= 0 && sectionEnd > deviceSize {
			log.Fatalf("Image needs %s but output device %v only holds %s -- refusing to restore",
				ByteCountIEC(sectionEnd), *reassembleOutput, ByteCountIEC(deviceSize))
		}
		if sectionEnd > maxEnd {
			maxEnd = sectionEnd
		}

		if n == 0 || n%1000 == 0 {
			if *debug {
				log.Printf("Restoring section (Sector: %v (len %d bytes) (debug: '%s')", SectorStart, SectionLength, strings.Trim(ReadHeader, "\n"))
			} else {
				log.Printf("Restoring section (Sector: %v (len %d bytes)", SectorStart, SectionLength)
			}
		}
		n++

		writePos := SectorStart * 512
		positioned := false
		BytesLeftToRead := SectionLength
		for BytesLeftToRead > 0 {
			expectedRead := int64(4096)
			if BytesLeftToRead < expectedRead {
				expectedRead = BytesLeftToRead
			}

			chunk := buf[:expectedRead]
			if _, err := io.ReadFull(imageReader, chunk); err != nil {
				log.Fatalf("Image read failure (truncated mid-section?) -- %v", err)
			}
			crc.Write(chunk)
			streamBytes += expectedRead
			BytesLeftToRead -= expectedRead

			if sparseOK && bytes.Equal(chunk, zeroBuf[:expectedRead]) {
				writePos += expectedRead
				positioned = false
				continue
			}
			if !positioned {
				if _, err := outputFD.Seek(writePos, io.SeekStart); err != nil {
					log.Fatalf("Seek failure (to %d) on output file/device %v", writePos, err)
				}
				positioned = true
			}
			if _, err := outputFD.Write(chunk); err != nil {
				log.Fatalf("Output file/device write failure -- %v", err)
			}
			writePos += expectedRead
		}
	}

	if !sawTrailer {
		if *reassembleAllowLegacy {
			log.Printf("WARNING: image has no end trailer; completeness and integrity were NOT verified")
		} else {
			log.Fatalf("Image has no end trailer: it is either truncated or was created by hot-clone v1.0.1 or older. Pass -reassemble-allow-legacy to restore it without verification.")
		}
	}

	// Holes skipped at the end of a sparse restore still have to count
	// towards the file size.
	if !isDevice {
		if err := outputFD.Truncate(maxEnd); err != nil {
			log.Fatalf("Failed to set restored file size to %d -- %v", maxEnd, err)
		}
	}

	if err := outputFD.Sync(); err != nil {
		log.Fatalf("Failed to sync restored data to disk -- %v", err)
	}
	if err := outputFD.Close(); err != nil {
		log.Fatalf("Failed to close output -- %v", err)
	}
	log.Printf("Restored %d sections (%s)", n, ByteCountIEC(maxEnd))
}
