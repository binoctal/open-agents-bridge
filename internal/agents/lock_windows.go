//go:build windows

package agents

// On Windows there is no flock; provisioning concurrency is bounded by the
// staging-dir pid suffix and the atomic swap, so mutual exclusion degrades to
// last-writer-wins instead of blocking. The npm install itself is safe to
// overlap (each staging dir is unique per pid).

func lockExclusive(fd int) error { return nil }
func lockRelease(fd int) error   { return nil }
