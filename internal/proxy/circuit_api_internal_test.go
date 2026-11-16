package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"model-router/internal/config"
)

// cbStatsRow mirrors the GET /api/stats instance shape needed by §24.15.
type cbStatsRow struct {
	Name          string `json:"name"`
	Protocol      string `json:"protocol"`
	Priority      int    `json:"priority"`
	Requests      int64  `json:"requests"`
	State         string `json:"state"`
	Locked        bool   `json:"locked"`
	Degraded      bool   `json:"degraded"`
	DegradedSince int64  `json:"degradedSince"`
	Reason        string `json:"reason"`
	Current       bool   `json:"current"`
	Removed       bool   `json:"removed"`
}

type cbStatsPayload struct {
	Instances []cbStatsRow `json:"instances"`
	Requests  struct {
		Total int64 `json:"total"`
	} `json:"requests"`
}

func cbGetStats(t *testing.T, serverURL string) cbStatsPayload {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, serverURL+PathStats, nil)
	if err != nil {
		t.Fatalf("build stats request: %v", err)
	}
	req.SetBasicAuth("admin", "secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/stats: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/stats = %d, want 200", resp.StatusCode)
	}
	var payload cbStatsPayload
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode stats: %v", err)
	}
	return payload
}

func cbAdminRequest(t *testing.T, method, serverURL, path, body string) int {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, serverURL+path, reader)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, path, err)
	}
	req.SetBasicAuth("admin", "secret")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func cbRow(rows []cbStatsRow, protocol, name string) *cbStatsRow {
	for i := range rows {
		if rows[i].Protocol == protocol && rows[i].Name == name {
			return &rows[i]
		}
	}
	return nil
}

func cbStatsTestConfig(openAIURL, anthropicURL string) *config.Config {
	return &config.Config{
		AuthToken:     "tok",
		AdminUser:     "admin",
		AdminPassword: "secret",
		Instances: config.Instances{
			{
				Protocol: config.ProtocolOpenAI,
				Name:     "shared",
				BaseURL:  openAIURL,
				APIKey:   "k1",
				Timeout:  config.Duration(2 * time.Second),
				Models:   []string{"gpt-a"},
			},
			{
				Protocol: config.ProtocolOpenAI,
				Name:     "only",
				BaseURL:  openAIURL,
				APIKey:   "k2",
				Timeout:  config.Duration(2 * time.Second),
				Models:   []string{"gpt-b"},
			},
			{
				Protocol: config.ProtocolAnthropic,
				Name:     "shared",
				BaseURL:  anthropicURL,
				APIKey:   "k3",
				Timeout:  config.Duration(2 * time.Second),
				Roles:    map[string]string{"*": "m"},
			},
		},
	}
}

// TestStatsInstanceFieldsOrderAndLockAPI covers M14/M15a/M15c/M15d/M15l and the
// lock/unlock endpoints of §24.15.3.
func TestStatsInstanceFieldsOrderAndLockAPI(t *testing.T) {
	openAIBackend := newCBBackend(t, http.StatusOK)
	anthropicBackend := newCBBackend(t, http.StatusOK)
	cfg := cbStatsTestConfig(openAIBackend.server.URL, anthropicBackend.server.URL)
	configPath := filepath.Join(t.TempDir(), "config.json")
	server, err := NewServer(configPath, cfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(server.Close)
	httpServer := httptest.NewServer(server)
	t.Cleanup(httpServer.Close)

	// One request per protocol so the two same-named rows get separate counters.
	if code := cbSend(t, httpServer.URL, protocolOpenAI, "gpt-a"); code != http.StatusOK {
		t.Fatalf("openai request status = %d, want 200", code)
	}
	if code := cbSend(t, httpServer.URL, protocolAnthropic, "sonnet"); code != http.StatusOK {
		t.Fatalf("anthropic request status = %d, want 200", code)
	}

	stats := cbGetStats(t, httpServer.URL)
	if len(stats.Instances) != 3 {
		t.Fatalf("instances = %+v, want 3 rows", stats.Instances)
	}
	wantOrder := [][2]string{
		{"openai", "shared"},
		{"openai", "only"},
		{"anthropic", "shared"},
	}
	for i, want := range wantOrder {
		got := stats.Instances[i]
		if got.Protocol != want[0] || got.Name != want[1] {
			t.Fatalf("instance[%d] = %s/%s, want %s/%s", i, got.Protocol, got.Name, want[0], want[1])
		}
	}
	if got := stats.Instances[0]; got.Priority != 0 || got.Requests != 1 || got.State != stateClosed ||
		got.Locked || got.Degraded || got.DegradedSince != 0 || got.Reason != "" || got.Current {
		t.Errorf("openai/shared row = %+v", got)
	}
	if got := stats.Instances[1]; got.Priority != 1 || got.Requests != 0 || got.Current {
		t.Errorf("openai/only row = %+v", got)
	}
	if got := stats.Instances[2]; got.Priority != 0 || got.Requests != 1 || got.Protocol != "anthropic" || got.Current {
		t.Errorf("anthropic/shared row = %+v", got)
	}
	// §24.17 M34/M15ak: the state domain is closed/open/locked/- and the removed
	// current pointer is never reported as true.
	for _, row := range stats.Instances {
		switch row.State {
		case stateClosed, stateOpen, stateLocked, stateShadowed:
		default:
			t.Errorf("state %q outside the §24.17 domain", row.State)
		}
		if row.Current {
			t.Errorf("row %s/%s reports current=true", row.Protocol, row.Name)
		}
	}

	// Force open state directly to exercise the stats fields.
	state := server.state.Load()
	only := state.handler.byProtocol[protocolOpenAI][1]
	only.recordFailure(time.Now(), 1, time.Minute)
	stats = cbGetStats(t, httpServer.URL)
	row := cbRow(stats.Instances, "openai", "only")
	if row == nil {
		t.Fatal("openai/only row missing")
	}
	if row.State != stateOpen || !row.Degraded || row.DegradedSince <= 0 || row.Reason != reasonFailureThreshold {
		t.Errorf("open row = %+v, want state open/degraded with degradedSince and reason", row)
	}

	// Lock the open instance: it becomes locked, the protocol peer is shadowed.
	if code := cbAdminRequest(t, http.MethodPost, httpServer.URL, PathInstancesLock,
		`{"protocol":"openai","name":"only"}`); code != http.StatusOK {
		t.Fatalf("POST lock = %d, want 200", code)
	}
	stats = cbGetStats(t, httpServer.URL)
	locked := cbRow(stats.Instances, "openai", "only")
	shadow := cbRow(stats.Instances, "openai", "shared")
	if locked == nil || locked.State != stateLocked || !locked.Locked || locked.Current {
		t.Errorf("locked row = %+v", locked)
	}
	if shadow == nil || shadow.State != "-" || shadow.Locked || shadow.Degraded || shadow.Current {
		t.Errorf("shadowed row = %+v", shadow)
	}
	if anthropicRow := cbRow(stats.Instances, "anthropic", "shared"); anthropicRow == nil ||
		anthropicRow.State != stateClosed || anthropicRow.Locked {
		t.Errorf("anthropic row must be unaffected: %+v", anthropicRow)
	}

	// Idempotent lock and input validation.
	if code := cbAdminRequest(t, http.MethodPost, httpServer.URL, PathInstancesLock,
		`{"protocol":"openai","name":"only"}`); code != http.StatusOK {
		t.Errorf("second lock = %d, want 200 (idempotent)", code)
	}
	if code := cbAdminRequest(t, http.MethodPost, httpServer.URL, PathInstancesLock,
		`{"protocol":"openai","name":"missing"}`); code != http.StatusNotFound {
		t.Errorf("lock missing instance = %d, want 404", code)
	}
	if code := cbAdminRequest(t, http.MethodGet, httpServer.URL, PathInstancesLock, ""); code != http.StatusMethodNotAllowed {
		t.Errorf("GET lock = %d, want 405", code)
	}

	// A fresh Handler/Server is all closed and unlocked (M11/M15l).
	restarted, err := NewServer(configPath, cfg)
	if err != nil {
		t.Fatalf("NewServer restart: %v", err)
	}
	t.Cleanup(restarted.Close)
	restartedHTTP := httptest.NewServer(restarted)
	t.Cleanup(restartedHTTP.Close)
	restartedStats := cbGetStats(t, restartedHTTP.URL)
	for _, row := range restartedStats.Instances {
		if row.Locked || row.State != stateClosed || row.DegradedSince != 0 || row.Current {
			t.Errorf("restarted row = %+v, want closed and unlocked", row)
		}
	}

	// Unlocking resets the protocol; §24.17 M36 then routes at the first closed
	// instance, which is openai/shared (priority 0).
	if code := cbAdminRequest(t, http.MethodPost, httpServer.URL, PathInstancesUnlock,
		`{"protocol":"openai"}`); code != http.StatusOK {
		t.Fatalf("POST unlock = %d, want 200", code)
	}
	stats = cbGetStats(t, httpServer.URL)
	for _, row := range stats.Instances {
		if row.Protocol != "openai" {
			continue
		}
		if row.State != stateClosed || row.Locked || row.Degraded || row.DegradedSince != 0 || row.Current {
			t.Errorf("post-unlock row = %+v, want closed/unlocked/clean", row)
		}
	}
	if row := cbRow(stats.Instances, "openai", "shared"); row == nil || row.Priority != 0 {
		t.Errorf("post-unlock first row = %+v, want openai/shared", row)
	}
}
