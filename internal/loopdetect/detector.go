package loopdetect

import "sync"

type Level int

const (
	None Level = iota
	Warning
	Critical
)

type Result struct {
	Level   Level
	Message string
}

type call struct {
	tool string
	hash string
}

type Detector struct {
	mu            sync.Mutex
	window        []call
	pos           int
	size          int
	warnThreshold int
	critThreshold int
}

func New(windowSize, warnThreshold, critThreshold int) *Detector {
	return &Detector{
		window:        make([]call, windowSize),
		warnThreshold: warnThreshold,
		critThreshold: critThreshold,
	}
}

func (d *Detector) Record(toolName, inputHash string) Result {
	d.mu.Lock()
	defer d.mu.Unlock()

	c := call{toolName, inputHash}
	d.window[d.pos] = c
	d.pos = (d.pos + 1) % len(d.window)
	if d.size < len(d.window) {
		d.size++
	}

	// Count exact repetitions
	count := 0
	for i := 0; i < d.size; i++ {
		if d.window[i] == c {
			count++
		}
	}
	if count >= d.critThreshold {
		return Result{Critical, "tool loop detected: " + toolName + " repeated " + itoa(count) + " times"}
	}
	if count >= d.warnThreshold {
		return Result{Warning, "potential tool loop: " + toolName + " repeated " + itoa(count) + " times"}
	}

	// Ping-pong detection (A->B->A->B)
	if d.size >= 6 {
		r := make([]call, 6)
		for i := 0; i < 6; i++ {
			idx := (d.pos - 1 - i + len(d.window)) % len(d.window)
			r[i] = d.window[idx]
		}
		if r[0] == r[2] && r[2] == r[4] && r[1] == r[3] && r[3] == r[5] && r[0] != r[1] {
			return Result{Warning, "ping-pong pattern: " + r[0].tool + " <-> " + r[1].tool}
		}
	}

	return Result{None, ""}
}

func (d *Detector) Reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pos = 0
	d.size = 0
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	buf := make([]byte, 0, 4)
	for n > 0 {
		buf = append(buf, byte('0'+n%10))
		n /= 10
	}
	for i, j := 0, len(buf)-1; i < j; i, j = i+1, j-1 {
		buf[i], buf[j] = buf[j], buf[i]
	}
	return string(buf)
}
