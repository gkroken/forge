package config

import (
	"crypto/sha256"
	"fmt"
	"os"
	"time"
)

// Watch re-applies path whenever its contents change, so a committed config
// change takes effect without restarting the process. Mounted ConfigMaps are
// updated in place by the kubelet (roughly once a minute), so polling the file
// is enough — there is no need for inotify, which does not fire reliably on the
// symlink swap Kubernetes performs anyway.
//
// It triggers on the FILE changing, never on live state changing. Reverting an
// operator's out-of-band edit on a timer is self-heal, and forge deliberately
// does not do that: the write is refused at the door instead (see
// server.refuseConfigOwned). An override made during an incident therefore
// survives until someone actually changes the config file.
//
// Watch blocks until done is closed; run it in a goroutine.
func Watch(path string, a Appliers, every time.Duration, done <-chan struct{}, onApply func(Result, error)) {
	// Start with no known digest, so the FIRST tick always evaluates the file.
	// Apply is idempotent, so that costs one all-noop pass; in exchange there is
	// no window where a change landing between the boot apply and the watcher
	// starting is missed forever.
	var last string
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
		}
		cur, err := fileDigest(path)
		if err != nil || cur == last {
			continue
		}
		f, err := Load(path)
		if err != nil {
			// Report, but do NOT update the digest: a half-written file should be
			// retried on the next tick rather than silently skipped.
			onApply(Result{}, fmt.Errorf("config watch: load: %w", err))
			continue
		}
		res, err := Apply(f, a)
		if err != nil {
			onApply(res, fmt.Errorf("config watch: apply: %w", err))
			continue
		}
		last = cur
		onApply(res, nil)
	}
}

// fileDigest is the content hash used to detect a real change. Content rather
// than mtime, because a ConfigMap remount can change mtime with identical bytes.
func fileDigest(path string) (string, error) {
	b, err := os.ReadFile(path) // #nosec G304 -- operator-supplied -config path
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%x", sum), nil
}
