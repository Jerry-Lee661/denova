package agent

import "sync"

// ttftStatsWindowSize is the number of most recent TTFT samples kept per
// model base URL for the sliding-window average.
const ttftStatsWindowSize = 8

// ttftStats is a process-level sliding-window aggregate of first-chunk
// latency (TTFT) keyed by model base URL. It feeds the adaptive preheat
// compaction decision (T-B1): the average of the last N calls approximates
// the current server's TTFT without persisting anything to user config.
var ttftStats = struct {
	mu  sync.Mutex
	win map[string][]float64
}{win: map[string][]float64{}}

// recordTTFT records one first-chunk latency sample (ms) into the sliding
// window for baseURL. An empty baseURL is ignored so unconfigured models do
// not pollute the aggregate.
func recordTTFT(baseURL string, ms float64) {
	if baseURL == "" {
		return
	}
	ttftStats.mu.Lock()
	defer ttftStats.mu.Unlock()
	win := append(ttftStats.win[baseURL], ms)
	if len(win) > ttftStatsWindowSize {
		win = win[len(win)-ttftStatsWindowSize:]
	}
	ttftStats.win[baseURL] = win
}

// averageTTFT returns the mean of the most recent TTFT samples for baseURL.
// The second return value is false when no sample has been recorded yet.
func averageTTFT(baseURL string) (float64, bool) {
	ttftStats.mu.Lock()
	defer ttftStats.mu.Unlock()
	win := ttftStats.win[baseURL]
	if len(win) == 0 {
		return 0, false
	}
	sum := 0.0
	for _, v := range win {
		sum += v
	}
	return sum / float64(len(win)), true
}

// resetTTFTStatsForTest clears all aggregates; intended for tests only.
func resetTTFTStatsForTest() {
	ttftStats.mu.Lock()
	defer ttftStats.mu.Unlock()
	ttftStats.win = map[string][]float64{}
}
