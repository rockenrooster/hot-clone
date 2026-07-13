package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type testSection struct {
	sector int64
	data   []byte
}

// buildTestImage produces a .hc stream the same way imageMain does: banner
// and sections through a countingCRCWriter, trailer (optionally) outside it.
func buildTestImage(t *testing.T, sections []testSection, withTrailer bool) []byte {
	t.Helper()
	var raw bytes.Buffer
	cw := newCountingCRCWriter(&raw)
	fmt.Fprintf(cw, "This-Is-A-Hot-Clone-Image See: https://github.com/benjojo/hot-clone\n")
	for _, s := range sections {
		fmt.Fprintf(cw, "S:%d\tL:%d\n", s.sector, len(s.data))
		cw.Write(s.data)
	}
	if withTrailer {
		raw.WriteString(formatEndTrailer(cw.Sum32(), cw.Bytes()))
	}
	return raw.Bytes()
}

// expectedContent applies the sections in order, like a restore would.
func expectedContent(sections []testSection) []byte {
	end := int64(0)
	for _, s := range sections {
		if e := s.sector*512 + int64(len(s.data)); e > end {
			end = e
		}
	}
	out := make([]byte, end)
	for _, s := range sections {
		copy(out[s.sector*512:], s.data)
	}
	return out
}

// runReassemble drives reassembleMain in-process and returns the restored
// file's content.
func runReassemble(t *testing.T, image []byte, allowLegacy bool) []byte {
	t.Helper()
	dir := t.TempDir()
	imgPath := filepath.Join(dir, "img.hc")
	outPath := filepath.Join(dir, "restored.img")
	if err := os.WriteFile(imgPath, image, 0o644); err != nil {
		t.Fatal(err)
	}
	oldPath, oldOut, oldLegacy := *reassemblePath, *reassembleOutput, *reassembleAllowLegacy
	defer func() {
		*reassemblePath, *reassembleOutput, *reassembleAllowLegacy = oldPath, oldOut, oldLegacy
	}()
	*reassemblePath = imgPath
	*reassembleOutput = outPath
	*reassembleAllowLegacy = allowLegacy
	reassembleMain()
	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func testSections() []testSection {
	// Main pass: nonzero chunk, all-zero chunk (sparse skip), nonzero chunk,
	// all-zero tail (exercises Truncate reinstating the full length).
	main := make([]byte, 12800)
	for i := 0; i < 4096; i++ {
		main[i] = byte(i%251) + 1
	}
	for i := 8192; i < 12288; i++ {
		main[i] = byte(i%250) + 1
	}
	// A catch-up section overwriting sector 5 inside the main range, and an
	// overread-style section past the end of it.
	catch := bytes.Repeat([]byte{0xCC}, 512)
	over := bytes.Repeat([]byte{0xDD}, 512)
	return []testSection{{0, main}, {5, catch}, {25, over}}
}

func TestReassembleRoundTrip(t *testing.T) {
	sections := testSections()
	got := runReassemble(t, buildTestImage(t, sections, true), false)
	want := expectedContent(sections)
	if len(got) != len(want) {
		t.Fatalf("restored size %d, want %d", len(got), len(want))
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("restored content differs from source")
	}
}

func TestReassembleLegacyImageAllowed(t *testing.T) {
	sections := testSections()
	got := runReassemble(t, buildTestImage(t, sections, false), true)
	if !bytes.Equal(got, expectedContent(sections)) {
		t.Fatalf("legacy restore content differs from source")
	}
}

// runReassembleFatal re-executes this test binary so reassembleMain's
// log.Fatalf paths can be observed as a process exit. Returns the combined
// output after asserting the subprocess did abort.
func runReassembleFatal(t *testing.T, testName string, image []byte) string {
	t.Helper()
	if os.Getenv("HC_TEST_SUBPROC") == "1" {
		*reassemblePath = os.Getenv("HC_TEST_IMG")
		*reassembleOutput = os.Getenv("HC_TEST_OUT")
		reassembleMain()
		os.Exit(0) // not reached when reassembleMain aborts as expected
	}
	dir := t.TempDir()
	imgPath := filepath.Join(dir, "img.hc")
	if err := os.WriteFile(imgPath, image, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^"+testName+"$")
	cmd.Env = append(os.Environ(),
		"HC_TEST_SUBPROC=1",
		"HC_TEST_IMG="+imgPath,
		"HC_TEST_OUT="+filepath.Join(dir, "restored.img"),
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected reassemble to abort, but it succeeded. output:\n%s", out)
	}
	return string(out)
}

func TestReassembleFatalMissingTrailer(t *testing.T) {
	out := runReassembleFatal(t, "TestReassembleFatalMissingTrailer",
		buildTestImage(t, testSections(), false))
	if !strings.Contains(out, "no end trailer") {
		t.Fatalf("unexpected abort message:\n%s", out)
	}
}

func TestReassembleFatalCorruptPayload(t *testing.T) {
	img := buildTestImage(t, testSections(), true)
	// 0xCC only occurs in section payloads (headers and trailer are ASCII),
	// so flipping the first one corrupts data without breaking a header.
	idx := bytes.IndexByte(img, 0xCC)
	if idx < 0 {
		t.Fatal("test image did not contain expected payload byte")
	}
	img[idx] ^= 0xFF
	out := runReassembleFatal(t, "TestReassembleFatalCorruptPayload", img)
	if !strings.Contains(out, "CRC mismatch") {
		t.Fatalf("unexpected abort message:\n%s", out)
	}
}

func TestReassembleFatalTruncated(t *testing.T) {
	img := buildTestImage(t, testSections(), true)
	out := runReassembleFatal(t, "TestReassembleFatalTruncated", img[:len(img)/2])
	if !strings.Contains(out, "truncated") {
		t.Fatalf("unexpected abort message:\n%s", out)
	}
}

func TestReassembleFatalTrailingData(t *testing.T) {
	img := buildTestImage(t, testSections(), true)
	img = append(img, []byte("stray bytes after the trailer")...)
	out := runReassembleFatal(t, "TestReassembleFatalTrailingData", img)
	if !strings.Contains(out, "after the end trailer") {
		t.Fatalf("unexpected abort message:\n%s", out)
	}
}
