//go:build !windows

package main

import (
	"os"
	"syscall"
)

func gracefulSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM}
}
