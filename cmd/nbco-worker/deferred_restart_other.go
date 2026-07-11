//go:build !linux

package main

func scheduleDeferredWorkerRestart() (bool, error) { return false, nil }
