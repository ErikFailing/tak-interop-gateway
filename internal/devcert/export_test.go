package devcert

import (
	"errors"
	"os"
)

// FailWritesAfter makes every file write after the first n fail, so a test can check what a failure partway through
// Generate leaves behind. It returns a function that restores the real writer.
func FailWritesAfter(n int) (restore func()) {
	calls := 0
	write = func(path, kind string, v any, mode os.FileMode) error {
		if calls++; calls > n {
			return errors.New("devcert: simulated write failure")
		}
		return writePEM(path, kind, v, mode)
	}
	return func() { write = writePEM }
}
