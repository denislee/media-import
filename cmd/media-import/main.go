// Command media-import is a native Wayland (Gio) GUI that imports photos,
// videos and audio from a folder or removable device into an organised
// destination tree while preserving file dates.
package main

import (
	"flag"
	"fmt"

	"github.com/denislee/media-import/internal/ui"
)

var version = "dev" // overridden via -ldflags "-X main.version=..."

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("media-import", version)
		return
	}
	ui.Version = version
	ui.Run()
}
