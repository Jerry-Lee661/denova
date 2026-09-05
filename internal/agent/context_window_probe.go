package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// actualWindowProbeTimeout bounds the /v1/models probe so a slow or
// unreachable model server does not stall the run.
const actualWindowProbeTimeout = 3 * time.Second

// actualWindowProbeCache is a process-level cache of probed actual context
// windows keyed by model base URL. The actual window is a property of the
// model server (not the run), so caching across runs avoids repeated probes.
// This is runtime state only — never persisted to user config.
var actualWindowProbeCache = struct {
	sync.Mutex
	m map[string]int
}{m: map[string]int{}}

// serverRejectionCalibration is a process-level cache of the actual context
// window upper bound inferred from server rejections (isServerRejectionError).
// When the server rejects a request due to context overflow, the rejected
// token count × 0.9 is recorded as the actual window ceiling. This is a
// fallback calibration: it only applies when the probe did not yield a
// smaller window.
var serverRejectionCalibration = struct {
	sync.Mutex
	m map[string]int
}{m: map[string]int{}}

// probeActualContextWindow calls GET {baseURL}/models on an OpenAI-compatible
// endpoint and reads data[].meta.n_ctx to discover the actual context window.
// llama-server exposes n_ctx in the model metadata; standard OpenAI does not,
// in which case 0 is returned (caller falls back to the declared window).
func probeActualContextWindow(ctx context.Context, baseURL, apiKey string) (int, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return 0, fmt.Errorf("empty base URL")
	}
	probeURL := baseURL + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
	if err != nil {
		return 0, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: actualWindowProbeTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return 0, fmt.Errorf("probe status %d: %s", resp.StatusCode, string(body))
	}

	var payload struct {
		Data []struct {
			ID   string `json:"id"`
			Meta struct {
				NCtx int `json:"n_ctx"`
			} `json:"meta"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return 0, err
	}
	for _, m := range payload.Data {
		if m.Meta.NCtx > 0 {
			return m.Meta.NCtx, nil
		}
	}
	return 0, nil
}

// ensureActualContextWindow probes the actual context window for the given
// base URL and caches the result. Returns the actual window (0 if probe
// failed or the server does not expose n_ctx).
func ensureActualContextWindow(ctx context.Context, baseURL, apiKey string) int {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return 0
	}
	actualWindowProbeCache.Lock()
	if w, ok := actualWindowProbeCache.m[baseURL]; ok {
		actualWindowProbeCache.Unlock()
		return w
	}
	actualWindowProbeCache.Unlock()

	actual, err := probeActualContextWindow(ctx, baseURL, apiKey)
	if err != nil {
		slog.Debug("actual_context_window_probe_failed",
			slog.String("base_url", baseURL),
			slog.Any("error", err),
		)
		// Cache 0 to avoid re-probing on every run within this process.
		actualWindowProbeCache.Lock()
		actualWindowProbeCache.m[baseURL] = 0
		actualWindowProbeCache.Unlock()
		return 0
	}
	if actual > 0 {
		actualWindowProbeCache.Lock()
		actualWindowProbeCache.m[baseURL] = actual
		actualWindowProbeCache.Unlock()
		slog.Info("actual_context_window_probed",
			slog.String("base_url", baseURL),
			slog.Int("actual_context_window_tokens", actual),
		)
	}
	return actual
}

// actualContextWindowForBaseURL returns the cached actual context window for
// a base URL without probing. Returns 0 if not yet probed.
func actualContextWindowForBaseURL(baseURL string) int {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	actualWindowProbeCache.Lock()
	defer actualWindowProbeCache.Unlock()
	return actualWindowProbeCache.m[baseURL]
}

// recordServerRejectionCalibration records the actual context window upper
// bound inferred from a server rejection. The rejected token count is
// multiplied by 0.9 as a safety margin. This is a fallback calibration:
// it only applies when the probe did not yield a smaller window.
func recordServerRejectionCalibration(baseURL string, rejectedTokens int) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" || rejectedTokens <= 0 {
		return
	}
	calibrated := int(float64(rejectedTokens) * 0.9)
	if calibrated < 1 {
		calibrated = 1
	}
	serverRejectionCalibration.Lock()
	if existing, ok := serverRejectionCalibration.m[baseURL]; !ok || calibrated < existing {
		serverRejectionCalibration.m[baseURL] = calibrated
	}
	serverRejectionCalibration.Unlock()
	slog.Info("server_rejection_calibration_recorded",
		slog.String("base_url", baseURL),
		slog.Int("rejected_tokens", rejectedTokens),
		slog.Int("calibrated_window", calibrated),
	)
}

// serverRejectionCalibratedWindow returns the calibrated window from server
// rejections for a base URL. Returns 0 if no calibration has been recorded.
func serverRejectionCalibratedWindow(baseURL string) int {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	serverRejectionCalibration.Lock()
	defer serverRejectionCalibration.Unlock()
	return serverRejectionCalibration.m[baseURL]
}

// knownActualContextWindow returns the best-known actual context window for a
// base URL: the minimum of the /v1/models probe result and the server
// rejection calibration. Returns 0 if neither source has produced a value.
func knownActualContextWindow(baseURL string) int {
	probed := actualContextWindowForBaseURL(baseURL)
	calibrated := serverRejectionCalibratedWindow(baseURL)
	switch {
	case probed <= 0:
		return calibrated
	case calibrated <= 0:
		return probed
	}
	if calibrated < probed {
		return calibrated
	}
	return probed
}

// effectiveContextWindow returns the effective context window by taking the
// minimum of the declared window and the actual window (from probe or server
// rejection calibration). If no actual window is known, the declared window
// is returned unchanged.
func effectiveContextWindow(declared, actual int) int {
	if actual <= 0 || actual >= declared {
		return declared
	}
	return actual
}

// physicalSafeWindow applies the physical safety ratio to a context window,
// producing a conservative trigger line that accounts for hardware limits
// (KV cache + prefill buffers may OOM before the declared window is reached).
// Returns 0 if the input window is not positive.
func physicalSafeWindow(window int) int {
	if window <= 0 {
		return 0
	}
	scaled := int(float64(window) * toolModePhysicalSafetyRatio)
	if scaled < 1 {
		return 1
	}
	return scaled
}

// resetContextWindowProbeCacheForTest clears both the probe cache and the
// server rejection calibration cache. Exported only for test use.
func resetContextWindowProbeCacheForTest() {
	actualWindowProbeCache.Lock()
	actualWindowProbeCache.m = map[string]int{}
	actualWindowProbeCache.Unlock()
	serverRejectionCalibration.Lock()
	serverRejectionCalibration.m = map[string]int{}
	serverRejectionCalibration.Unlock()
}
