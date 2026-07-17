//go:build windows

package main

import "os"

func gracefulSignals() []os.Signal {
	return []os.Signal{os.Interrupt}
}
