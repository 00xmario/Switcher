//go:build !linux && !darwin

package sysload

func platform() reader { return reader{} }
