//go:build unix

package hub

import (
	"errors"
	"os"
	"syscall"
)

// tryLock takes an exclusive, non-blocking lock on path. The OS drops it
// when the process exits, however it exits. flock locks belong to the open
// file, so two Nodes in one process also exclude each other (as tests rely
// on).
func tryLock(path string) (unlock func(), ok bool, err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, true, nil
}
