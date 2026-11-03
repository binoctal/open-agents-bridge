//go:build windows

package agent

// Windows is not a node platform; the daemon form of the node subcommand is
// not expected there. Zero mirrors the total-failure path: the wire field is
// omitempty, so the API stores unknown and its watermark gate refuses on
// unknown rather than reading it as "full" or "free".
func diskAvailableBytes() uint64 { return 0 }
