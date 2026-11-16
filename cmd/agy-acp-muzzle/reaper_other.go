//go:build !linux

package main

import "log/slog"

func registerKernel(int)       {}
func unregisterKernel(int)     {}
func startReaper(*slog.Logger) {}
