//go:build windows

package main

import "errors"

func makeFIFO(_ string, _ uint32) error {
	return errors.New("fifo unsupported on windows")
}
