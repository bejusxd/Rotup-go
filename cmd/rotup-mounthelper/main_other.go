//go:build !linux

// The mount helper performs Linux-specific privileged filesystem operations.
// This stub exists so the module still builds and vets on a development
// workstation; it refuses to run rather than pretending to manage media.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/bejusxd/rotup/internal/platform/media"
)

func main() {
	result := media.Result{
		OK:           false,
		ErrorCode:    "unsupported_platform",
		ErrorMessage: "the ROTUP mount helper manages removable media on Linux only",
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		fmt.Fprintln(os.Stderr, "the helper result could not be encoded")
		os.Exit(2)
	}
	fmt.Println(string(encoded))
	os.Exit(1)
}
