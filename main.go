package main

import (
	"flag"
	"fmt"
)

var (
	debug = flag.Bool("verbose", false, "be extra verbose on whats happening")
	dev   = flag.String("device", "", "The device you wish to hot-clone")
)

func main() {
	flag.Parse()

	// Nope, we are restoring instead
	if *reassemblePath != "" {
		reassembleMain()
		return
	}

	imageMain()
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
