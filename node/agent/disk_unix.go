//go:build !windows

package agent

import "syscall"

// diskAvailableBytes reports free space for the stack-placement watermark
// gate (hosted-compose-stacks 4.4/5.4), preferring the docker data root --
// that is the filesystem stack volumes and images actually land on -- and
// falling back to / for single-disk nodes with a relocated root. Zero on
// total failure: the wire field is omitempty, so the API stores unknown and
// its gate refuses on unknown rather than reading it as "full" or "free".
func diskAvailableBytes() uint64 {
	for _, path := range []string{"/var/lib/docker", "/"} {
		var st syscall.Statfs_t
		if err := syscall.Statfs(path, &st); err == nil {
			return uint64(st.Bavail) * uint64(st.Bsize)
		}
	}
	return 0
}
