package main

import (
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"strings"
)

// The .hc stream layout:
//
//	banner line
//	repeated sections: "S:<sector>\tL:<bytes>\n" followed by <bytes> raw bytes
//	end trailer:       "END\tCRC:<8 hex digits>\tBYTES:<decimal>\n"
//
// CRC and BYTES cover every byte of the stream before the trailer line
// (banner, section headers and section data), using CRC-32C (Castagnoli).
// The trailer is what lets a restore tell a complete image apart from one
// that was truncated in flight or corrupted at rest. Images made before the
// trailer existed (hot-clone <= 1.0.1) can still be restored with
// -reassemble-allow-legacy.

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// countingCRCWriter wraps the image output; everything written through it is
// folded into the running CRC and byte count that the end trailer records.
type countingCRCWriter struct {
	w   io.Writer
	crc hash.Hash32
	n   int64
}

func newCountingCRCWriter(w io.Writer) *countingCRCWriter {
	return &countingCRCWriter{w: w, crc: crc32.New(castagnoli)}
}

func (c *countingCRCWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.crc.Write(p[:n])
	c.n += int64(n)
	return n, err
}

func (c *countingCRCWriter) Sum32() uint32 { return c.crc.Sum32() }
func (c *countingCRCWriter) Bytes() int64  { return c.n }

const endTrailerPrefix = "END\t"

func formatEndTrailer(crc uint32, bytes int64) string {
	return fmt.Sprintf("END\tCRC:%08x\tBYTES:%d\n", crc, bytes)
}

// parseEndTrailer returns the CRC and byte count from a trailer line, or
// ok=false if the line is not an end trailer (e.g. a section header).
func parseEndTrailer(line string) (crc uint32, bytes int64, ok bool) {
	if !strings.HasPrefix(line, endTrailerPrefix) {
		return 0, 0, false
	}
	parsed, err := fmt.Sscanf(line, "END\tCRC:%x\tBYTES:%d\n", &crc, &bytes)
	if parsed != 2 || err != nil {
		return 0, 0, false
	}
	return crc, bytes, true
}
