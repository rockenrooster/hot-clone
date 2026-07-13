//go:build !linux

package main

import "log"

// Imaging needs blktrace, which is Linux-only. Restoring an image
// (-reassemble/-reassemble-output) works on any OS.
func imageMain() {
	log.Fatalf("Imaging requires Linux (blktrace). On this OS only -reassemble/-reassemble-output are available.")
}
