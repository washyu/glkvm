//go:build !windows

package main

import (
	"fmt"
	"os"
)

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

func main() {
	fmt.Fprintln(os.Stderr, "glkvm-agent runs on Windows. On Linux hosts use qemu-guest-agent (see docs/guest-agent.md).")
	os.Exit(1)
}
