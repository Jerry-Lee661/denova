package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"denova/config"
)

// --- effectiveContextWindow ---

func TestEffectiveContextWindow(t *testing.T) {
	cases := []struct {
		name     string
		declared int
		actual   int
		want     int
	}{
		{"actual smaller", 262144, 50000, 50000},
		{"actual equal", 262144, 262144, 262144},
		{"actual larger", 262144, 300000, 262144},
		{"actual zero (no probe)", 262144, 0, 262144},
		{"declared zero", 0, 50000, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := effectiveContextWindow(c.declared, c.actual)
			if got != c.want {
				t.Fatalf("effectiveContextWindow(%d, %d) = %d, want %d", c.declared, c.actual, got, c.want)
			}
		})
	}
}

// --- physicalSafeWindow ---

func TestPhysicalSafeWindow(t *testing.T) {
	ratio := toolModePhysicalSafetyRatio
	cases := []struct {
		window int
		want   int
	}{
		{262144, int(float64(262144) * ratio)},
		{1000, int(float64(1000) * ratio)},
		{1, 1},
		{0, 0},
		{-5, 0},
	}
	for _, c := range cases {
		got := physicalSafeWindow(c.window)
		if got != c.want {
			t.Fatalf("physicalSafeWindow(%d) = %d, want %d", c.window, got, c.want)
		}
	}
}

// --- knownActualContextWindow ---

func TestKnownActualContextWindow(t *testing.T) {
	resetContextWindowProbeCacheForTest()
	defer resetContextWindowProbeCacheForTest()

	baseURL := "http://localhost:8187"

	// Neither source → 0
	if got := knownActualContextWindow(baseURL); got != 0 {
		t.Fatalf("knownActualContextWindow = %d, want 0 (no sources)", got)
	}

	// Only probe
	actualWindowProbeCache.Lock()
	actualWindowProbeCache.m[baseURL] = 262144
	actualWindowProbeCache.Unlock()
	if got := knownActualContextWindow(baseURL); got != 262144 {
		t.Fatalf("knownActualContextWindow = %d, want 262144 (probe only)", got)
	}

	// Probe + calibration (calibration smaller)
	serverRejectionCalibration.Lock()
	serverRejectionCalibration.m[baseURL] = 50000
	serverRejectionCalibration.Unlock()
	if got := knownActualContextWindow(baseURL); got != 50000 {
		t.Fatalf("knownActualContextWindow = %d, want 50000 (calibration smaller)", got)
	}

	// Probe + calibration (probe smaller)
	serverRejectionCalibration.Lock()
	serverRejectionCalibration.m[baseURL] = 300000
	serverRejectionCalibration.Unlock()
	if got := knownActualContextWindow(baseURL); got != 262144 {
		t.Fatalf("knownActualContextWindow = %d, want 262144 (probe smaller)", got)
	}
}

// --- probeActualContextWindow (HTTP) ---

func TestProbeActualContextWindow(t *testing.T) {
	t.Run("llama-server meta.n_ctx", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/models" {
				t.Fatalf("path = %q, want /models", r.URL.Path)
			}
			json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{
					{
						"id": "ornith-1.5-35b",
						"meta": map[string]any{
							"n_ctx":       262144,
							"n_ctx_train": 262144,
						},
					},
				},
			})
		}))
		defer srv.Close()

		got, err := probeActualContextWindow(context.Background(), srv.URL, "")
		if err != nil {
			t.Fatalf("probe error: %v", err)
		}
		if got != 262144 {
			t.Fatalf("probe = %d, want 262144", got)
		}
	})

	t.Run("standard openai no meta", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{
					{"id": "gpt-4o"},
				},
			})
		}))
		defer srv.Close()

		got, err := probeActualContextWindow(context.Background(), srv.URL, "")
		if err != nil {
			t.Fatalf("probe error: %v", err)
		}
		if got != 0 {
			t.Fatalf("probe = %d, want 0 (no meta.n_ctx)", got)
		}
	})

	t.Run("server error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "internal error", http.StatusInternalServerError)
		}))
		defer srv.Close()

		_, err := probeActualContextWindow(context.Background(), srv.URL, "")
		if err == nil {
			t.Fatal("probe should return error on 500")
		}
	})

	t.Run("empty base URL", func(t *testing.T) {
		_, err := probeActualContextWindow(context.Background(), "", "")
		if err == nil {
			t.Fatal("probe should return error on empty base URL")
		}
	})
}

// --- ensureActualContextWindow (caching) ---

func TestEnsureActualContextWindowCaches(t *testing.T) {
	resetContextWindowProbeCacheForTest()
	defer resetContextWindowProbeCacheForTest()

	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"id": "test", "meta": map[string]any{"n_ctx": 128000}},
			},
		})
	}))
	defer srv.Close()

	// First call probes
	got := ensureActualContextWindow(context.Background(), srv.URL, "")
	if got != 128000 {
		t.Fatalf("first probe = %d, want 128000", got)
	}
	if callCount != 1 {
		t.Fatalf("call count = %d, want 1", callCount)
	}

	// Second call uses cache
	got = ensureActualContextWindow(context.Background(), srv.URL, "")
	if got != 128000 {
		t.Fatalf("cached probe = %d, want 128000", got)
	}
	if callCount != 1 {
		t.Fatalf("call count = %d, want 1 (cached)", callCount)
	}
}

// --- recordServerRejectionCalibration ---

func TestRecordServerRejectionCalibration(t *testing.T) {
	resetContextWindowProbeCacheForTest()
	defer resetContextWindowProbeCacheForTest()

	baseURL := "http://localhost:8187"

	// No calibration → 0
	if got := serverRejectionCalibratedWindow(baseURL); got != 0 {
		t.Fatalf("calibration = %d, want 0 (none)", got)
	}

	// Record 100000 → calibrated = 90000
	recordServerRejectionCalibration(baseURL, 100000)
	if got := serverRejectionCalibratedWindow(baseURL); got != 90000 {
		t.Fatalf("calibration = %d, want 90000", got)
	}

	// Record smaller 50000 → calibrated = 45000 (replaces larger)
	recordServerRejectionCalibration(baseURL, 50000)
	if got := serverRejectionCalibratedWindow(baseURL); got != 45000 {
		t.Fatalf("calibration = %d, want 45000 (smaller replaces)", got)
	}

	// Record larger 200000 → calibrated = 180000 (does NOT replace smaller)
	recordServerRejectionCalibration(baseURL, 200000)
	if got := serverRejectionCalibratedWindow(baseURL); got != 45000 {
		t.Fatalf("calibration = %d, want 45000 (larger does not replace)", got)
	}

	// Zero tokens → no change
	recordServerRejectionCalibration(baseURL, 0)
	if got := serverRejectionCalibratedWindow(baseURL); got != 45000 {
		t.Fatalf("calibration = %d, want 45000 (zero no-op)", got)
	}
}

// --- triggerTokens with physical safety ratio ---

func TestTriggerTokensAppliesPhysicalSafetyRatio(t *testing.T) {
	policy := contextCompactionPolicy{
		Enabled:             true,
		ContextWindowTokens: 262144,
		Threshold:           0.80,
	}
	// triggerTokens should use physicalSafeWindow(262144) * 0.80, not 262144 * 0.80
	physical := physicalSafeWindow(262144)
	want := int(float64(physical) * 0.80)
	got := policy.triggerTokens()
	if got != want {
		t.Fatalf("triggerTokens = %d, want %d (physical %d * 0.80)", got, want, physical)
	}
	// Verify it's actually smaller than the naive calculation
	naiveWindow := 262144
	naive := int(float64(naiveWindow) * 0.80)
	if got >= naive {
		t.Fatalf("triggerTokens = %d should be < naive %d (physical safety ratio applied)", got, naive)
	}
}

// --- resolveContextCompactionPolicy uses actual window ---

func TestResolveContextCompactionPolicyUsesActualWindow(t *testing.T) {
	resetContextWindowProbeCacheForTest()
	defer resetContextWindowProbeCacheForTest()

	declared := 262144
	actual := 50000
	baseURL := "http://localhost:8187"

	// Set up probe cache with actual window smaller than declared
	actualWindowProbeCache.Lock()
	actualWindowProbeCache.m[baseURL] = actual
	actualWindowProbeCache.Unlock()

	cfg := &config.Config{
		ModelProfiles: []config.ModelProfileSettings{
			{
				ID:                  "default",
				OpenAIBaseURL:       baseURL,
				OpenAIModel:         "test-model",
				ContextWindowTokens: intPtr(declared),
			},
		},
	}

	policy := resolveContextCompactionPolicy(cfg, config.AgentKindIDE)
	if policy.ContextWindowTokens != actual {
		t.Fatalf("policy.ContextWindowTokens = %d, want %d (actual window)", policy.ContextWindowTokens, actual)
	}
}

func TestResolveContextCompactionPolicyFallsBackToDeclared(t *testing.T) {
	resetContextWindowProbeCacheForTest()
	defer resetContextWindowProbeCacheForTest()

	declared := 262144
	baseURL := "http://localhost:8187"

	// No probe cache → falls back to declared
	cfg := &config.Config{
		ModelProfiles: []config.ModelProfileSettings{
			{
				ID:                  "default",
				OpenAIBaseURL:       baseURL,
				OpenAIModel:         "test-model",
				ContextWindowTokens: intPtr(declared),
			},
		},
	}

	policy := resolveContextCompactionPolicy(cfg, config.AgentKindIDE)
	if policy.ContextWindowTokens != declared {
		t.Fatalf("policy.ContextWindowTokens = %d, want %d (declared fallback)", policy.ContextWindowTokens, declared)
	}
}

func intPtr(v int) *int {
	return &v
}

// --- physicalSafeWindow vs physicalToolSafetyWindow consistency ---

func TestPhysicalSafeWindowMatchesToolSafetyWindow(t *testing.T) {
	// physicalSafeWindow (new, for compaction) and physicalToolSafetyWindow
	// (existing, for tool budget) should produce the same result for windows
	// within the hard cap.
	for _, w := range []int{1000, 10000, 81920, 100000} {
		clamped := clampToolWindowTokens(w)
		want := physicalToolSafetyWindow(w)
		got := physicalSafeWindow(clamped)
		if got != want {
			t.Fatalf("physicalSafeWindow(%d) = %d, want %d (physicalToolSafetyWindow)", clamped, got, want)
		}
	}
}

// --- effectiveContextWindow with knownActualContextWindow integration ---

func TestEffectiveWindowWithServerRejection(t *testing.T) {
	resetContextWindowProbeCacheForTest()
	defer resetContextWindowProbeCacheForTest()

	baseURL := "http://localhost:8187"
	declared := 262144

	// No probe, but server rejection calibration exists
	recordServerRejectionCalibration(baseURL, 100000) // calibrated = 90000

	actual := knownActualContextWindow(baseURL)
	if actual != 90000 {
		t.Fatalf("knownActualContextWindow = %d, want 90000", actual)
	}

	effective := effectiveContextWindow(declared, actual)
	if effective != 90000 {
		t.Fatalf("effectiveContextWindow = %d, want 90000", effective)
	}
}

// --- probe with trailing slash ---

func TestProbeActualContextWindowTrailingSlash(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Fatalf("path = %q, want /models", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"id": "test", "meta": map[string]any{"n_ctx": 65536}},
			},
		})
	}))
	defer srv.Close()

	// Trailing slash should be trimmed
	got, err := probeActualContextWindow(context.Background(), srv.URL+"/", "")
	if err != nil {
		t.Fatalf("probe error: %v", err)
	}
	if got != 65536 {
		t.Fatalf("probe = %d, want 65536", got)
	}
}

// --- ensureActualContextWindow caches failure ---

func TestEnsureActualContextWindowCachesFailure(t *testing.T) {
	resetContextWindowProbeCacheForTest()
	defer resetContextWindowProbeCacheForTest()

	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	got := ensureActualContextWindow(context.Background(), srv.URL, "")
	if got != 0 {
		t.Fatalf("probe = %d, want 0 (failure)", got)
	}
	if callCount != 1 {
		t.Fatalf("call count = %d, want 1", callCount)
	}

	// Second call should use cached 0, not re-probe
	got = ensureActualContextWindow(context.Background(), srv.URL, "")
	if got != 0 {
		t.Fatalf("cached probe = %d, want 0", got)
	}
	if callCount != 1 {
		t.Fatalf("call count = %d, want 1 (cached failure)", callCount)
	}
}
