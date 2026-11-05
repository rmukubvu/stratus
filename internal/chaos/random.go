package chaos

import "time"

// splitmix64 is a small, well-distributed mixing function. Faults are a pure
// function of (seed, rule index, match number, stream), so they replay
// exactly, regardless of timing or which goroutine served the request.
func splitmix64(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	return x ^ (x >> 31)
}

func draw(seed int64, rule, match int, stream uint64) float64 {
	h := splitmix64(uint64(seed))
	h = splitmix64(h ^ uint64(rule))
	h = splitmix64(h ^ uint64(match))
	h = splitmix64(h ^ stream)
	return float64(h>>11) / float64(1<<53) // uniform in [0, 1)
}

func fires(seed int64, rule, match int, probability float64) bool {
	return probability >= 1 || draw(seed, rule, match, 0) < probability
}

func latencyFor(seed int64, rule, match int, l LatencySpec) time.Duration {
	ms := l.DelayMS
	if l.JitterMS > 0 {
		ms += int(draw(seed, rule, match, 1) * float64(l.JitterMS+1))
	}
	return time.Duration(ms) * time.Millisecond
}
