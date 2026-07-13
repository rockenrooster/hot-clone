package main

import (
	"bytes"
	"hash/crc32"
	"testing"
)

func TestEndTrailerRoundTrip(t *testing.T) {
	line := formatEndTrailer(0xdeadbeef, 123456789)
	crc, n, ok := parseEndTrailer(line)
	if !ok {
		t.Fatalf("parseEndTrailer rejected its own output: %q", line)
	}
	if crc != 0xdeadbeef || n != 123456789 {
		t.Fatalf("round trip mismatch: got crc=%08x n=%d", crc, n)
	}
}

func TestEndTrailerRejectsHeaders(t *testing.T) {
	for _, line := range []string{
		"S:0\tL:512\n",
		"S:12345\tL:4096\n",
		"This-Is-A-Hot-Clone-Image See: https://github.com/benjojo/hot-clone\n",
		"END\n",
		"END\tgarbage\n",
		"",
	} {
		if _, _, ok := parseEndTrailer(line); ok {
			t.Errorf("parseEndTrailer accepted non-trailer line %q", line)
		}
	}
}

func TestCountingCRCWriter(t *testing.T) {
	var sink bytes.Buffer
	cw := newCountingCRCWriter(&sink)

	part1 := []byte("hello ")
	part2 := []byte("world, this is an image payload\x00\x01\x02")
	cw.Write(part1)
	cw.Write(part2)

	all := append(append([]byte{}, part1...), part2...)
	if !bytes.Equal(sink.Bytes(), all) {
		t.Fatalf("writer did not pass bytes through verbatim")
	}
	if cw.Bytes() != int64(len(all)) {
		t.Fatalf("byte count = %d, want %d", cw.Bytes(), len(all))
	}
	want := crc32.Checksum(all, castagnoli)
	if cw.Sum32() != want {
		t.Fatalf("crc = %08x, want %08x", cw.Sum32(), want)
	}
}
