//go:build !windows

package main

import (
	"context"
	"io"
	"log/slog"
)

// underServiceManager reports whether the Windows service manager started
// this process. systemd runs `meshbridge run` like any other command.
func underServiceManager() bool { return false }

func runAsService(string, func(context.Context, io.Writer) error) error { panic("unreachable") }

// keepAwake is Windows-only for now; on Linux a bridge host is usually a
// machine that doesn't sleep.
func keepAwake(context.Context, *slog.Logger) {}

const (
	installHint = "sudo meshbridge install"
	setupHint   = "sudo meshbridge setup"
)
