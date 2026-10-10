package main

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
)

// prepareDataDir creates the eval-mode data directory and proves it writable,
// so a bad mount fails with a message that says what to do instead of a bare
// "mkdir /data/meta: permission denied". The container image runs as the
// distroless nonroot user (uid/gid 65532) and keeps its data in /data — a
// tmpfs or bind mount that ignores either fact is the usual cause.
func prepareDataDir(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	if err := os.MkdirAll(abs, 0o750); err != nil {
		return explainDataDirErr(abs, err)
	}
	probe, err := os.CreateTemp(abs, ".forge-write-probe-*")
	if err != nil {
		return explainDataDirErr(abs, err)
	}
	probe.Close()
	os.Remove(probe.Name())
	slog.Info("data directory", "path", abs)
	return nil
}

func explainDataDirErr(abs string, err error) error {
	switch {
	case errors.Is(err, syscall.EROFS):
		return fmt.Errorf("data directory %s is on a read-only filesystem (%w); "+
			"point -data (or FORGE_DATA) at a writable volume — the container image uses /data", abs, err)
	case errors.Is(err, fs.ErrPermission):
		return fmt.Errorf("data directory %s is not writable by uid %d gid %d (%w); "+
			"chown it to that user, or mount it writable for them — the container image runs as "+
			"distroless nonroot 65532, e.g. --tmpfs /data:uid=65532,gid=65532", abs, os.Getuid(), os.Getgid(), err)
	}
	return fmt.Errorf("data directory %s: %w", abs, err)
}
