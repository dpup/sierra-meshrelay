//go:build windows

package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"runtime"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"

	"github.com/dpup/sierra-meshrelay/internal/logfile"
	msvc "github.com/dpup/sierra-meshrelay/internal/svc"
)

// LogFile is the service's log, beside its config.
const LogFile = "meshbridge.log"

func underServiceManager() bool {
	ok, err := svc.IsWindowsService()
	return err == nil && ok
}

// runAsService runs run under the service manager until it is told to stop.
// An error from run (a bad config, say) is logged and ends the service with
// a failure, which its recovery settings answer by restarting it.
func runAsService(dir string, run func(context.Context, io.Writer) error) error {
	out, err := logfile.Open(filepath.Join(dir, LogFile))
	if err != nil {
		return err
	}
	defer out.Close()
	h := &serviceHandler{run: run, out: out}
	if err := svc.Run(msvc.ServiceName, h); err != nil {
		fmt.Fprintf(out, "meshbridge: service manager: %v\n", err)
		return err
	}
	return h.err
}

type serviceHandler struct {
	run func(context.Context, io.Writer) error
	out io.Writer
	err error
}

func (h *serviceHandler) Execute(_ []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.run(ctx, h.out) }()
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case err := <-done:
			// The bridge only returns on its own when something is wrong.
			if err == nil {
				err = fmt.Errorf("the bridge stopped unexpectedly")
			}
			h.err = err
			fmt.Fprintf(h.out, "time=%s level=ERROR msg=\"meshbridge exiting\" error=%q\n",
				time.Now().Format(time.RFC3339), err.Error())
			return true, 1
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending}
				cancel()
				select {
				case <-done:
				case <-time.After(15 * time.Second):
				}
				return false, 0
			}
		}
	}
}

const (
	esContinuous     = 0x80000000
	esSystemRequired = 0x00000001
)

var setThreadExecutionState = windows.NewLazySystemDLL("kernel32.dll").NewProc("SetThreadExecutionState")

// keepAwake stops Windows sleeping on idle while ctx lives (host.keep_awake).
// The request belongs to the thread that made it, so it is made and later
// cleared from one locked thread. It doesn't keep the display on, and it
// doesn't stop someone choosing Sleep.
func keepAwake(ctx context.Context, log *slog.Logger) {
	ok := make(chan bool)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		r, _, err := setThreadExecutionState.Call(esContinuous | esSystemRequired)
		if r == 0 {
			log.Warn("can't keep this computer awake; it may sleep and take the bridge offline", "error", err)
			ok <- false
			return
		}
		ok <- true
		<-ctx.Done()
		setThreadExecutionState.Call(esContinuous)
	}()
	if <-ok {
		log.Info("keeping this computer awake while the bridge runs (host.keep_awake)")
	}
}

const (
	installHint = "meshbridge install, as administrator"
	setupHint   = "meshbridge setup, as administrator"
)
