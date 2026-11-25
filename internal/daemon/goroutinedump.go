package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/pprof"
	"syscall"
	"time"
)

// dumpGoroutinesOnSignal writes every goroutine's stack to a file under
// home/diagnostics each time the daemon receives SIGUSR1, so a wedged daemon
// can be diagnosed without killing it (SIGQUIT's dump also ends the process,
// and journald may truncate it). Usage: kill -USR1 $(pidof tandem).
func dumpGoroutinesOnSignal(ctx context.Context, home string) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGUSR1)
	go func() {
		defer signal.Stop(ch)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ch:
				path, err := writeGoroutineDump(filepath.Join(home, "diagnostics"))
				if err != nil {
					slog.Error("goroutine dump failed", "error", err)
					continue
				}
				slog.Info("wrote goroutine dump", "path", path)
			}
		}
	}()
}

func writeGoroutineDump(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, fmt.Sprintf("goroutines-%s.txt", time.Now().Format("20060102-150405")))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	if err := pprof.Lookup("goroutine").WriteTo(f, 2); err != nil {
		f.Close()
		return "", err
	}
	return path, f.Close()
}
