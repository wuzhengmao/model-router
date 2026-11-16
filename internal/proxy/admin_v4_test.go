package proxy_test

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"model-router/internal/config"
)

// noRedirectRequest issues a request without following redirects so §24.7 D4/D6
// statuses and Location headers can be asserted directly.
func noRedirectRequest(t *testing.T, method, target, body string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, target, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("call server: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func basicAuthHeader(user, pass string) map[string]string {
	req, _ := http.NewRequest(http.MethodGet, "http://example.invalid/", nil)
	req.SetBasicAuth(user, pass)
	return map[string]string{"Authorization": req.Header.Get("Authorization")}
}

func postLogin(t *testing.T, serverURL string, payload string) *http.Response {
	t.Helper()
	return noRedirectRequest(t, http.MethodPost, serverURL+"/api/login", payload, nil)
}

func sessionCookie(t *testing.T, resp *http.Response) *http.Cookie {
	t.Helper()
	for _, cookie := range resp.Cookies() {
		if cookie.Name == "mr_session" {
			return cookie
		}
	}
	return nil
}

func setCookieRaw(resp *http.Response) string {
	return strings.Join(resp.Header.Values("Set-Cookie"), "\n")
}

// TestLoginAndPageRoutes covers E6: the login page is public, every admin page
// redirects to it while logged out, and /admin normalizes to /admin/ once
// authenticated.
func TestLoginAndPageRoutes(t *testing.T) {
	server, _ := newAdminServer(t, adminTestConfig)

	resp := noRedirectRequest(t, http.MethodGet, server.URL+"/admin/login", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /admin/login = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Errorf("login page Content-Type = %q, want text/html", got)
	}
	loginBody, _ := io.ReadAll(resp.Body)
	for _, want := range []string{"password", "remember", "/api/login"} {
		if !strings.Contains(string(loginBody), want) {
			t.Errorf("login page body lacks %q", want)
		}
	}

	for _, path := range []string{"/admin/", "/admin/monitor", "/admin/config", "/admin/json", "/admin"} {
		resp := noRedirectRequest(t, http.MethodGet, server.URL+path, "", nil)
		if resp.StatusCode != http.StatusFound {
			t.Fatalf("GET %s while logged out = %d, want 302", path, resp.StatusCode)
		}
		if location := resp.Header.Get("Location"); !strings.HasPrefix(location, "/admin/login") {
			t.Errorf("GET %s Location = %q, want /admin/login prefix", path, location)
		}
	}

	resp = noRedirectRequest(t, http.MethodGet, server.URL+"/admin", "", basicAuthHeader("admin", "secret"))
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/admin/" {
		t.Fatalf("authed GET /admin = %d %q, want 302 /admin/", resp.StatusCode, resp.Header.Get("Location"))
	}

	for _, path := range []string{"/admin/", "/admin/monitor", "/admin/config", "/admin/json"} {
		resp := noRedirectRequest(t, http.MethodGet, server.URL+path, "", basicAuthHeader("admin", "secret"))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("authed GET %s = %d, want 200", path, resp.StatusCode)
		}
	}
}

// TestLoginCookieBehavior covers E7.
func TestLoginCookieBehavior(t *testing.T) {
	server, _ := newAdminServer(t, adminTestConfig)

	t.Run("remember true", func(t *testing.T) {
		resp := postLogin(t, server.URL, `{"user":"admin","password":"secret","remember":true}`)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("login status = %d, want 200", resp.StatusCode)
		}
		raw := setCookieRaw(resp)
		for _, want := range []string{"Max-Age=2592000", "HttpOnly", "SameSite=Lax", "Path=/"} {
			if !strings.Contains(raw, want) {
				t.Errorf("Set-Cookie = %q, want %q", raw, want)
			}
		}
		if strings.Contains(raw, "Secure") {
			t.Errorf("Set-Cookie = %q, must not set Secure", raw)
		}
		cookie := sessionCookie(t, resp)
		if cookie == nil {
			t.Fatalf("no mr_session cookie in %q", raw)
		}
		if len(cookie.Value) < 43 {
			t.Errorf("session token length = %d, want >= 43 base64url chars for 32 bytes", len(cookie.Value))
		}
		if strings.Contains(cookie.Value, "admin") || strings.Contains(cookie.Value, "secret") {
			t.Error("session token embeds a credential")
		}
		var body loginBody
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("login response is not JSON: %v", err)
		}
		if body.Redirect != "/admin/" {
			t.Errorf("redirect = %q, want /admin/", body.Redirect)
		}
	})

	t.Run("remember false", func(t *testing.T) {
		resp := postLogin(t, server.URL, `{"user":"admin","password":"secret","remember":false}`)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("login status = %d, want 200", resp.StatusCode)
		}
		if raw := setCookieRaw(resp); strings.Contains(raw, "Max-Age") {
			t.Errorf("session cookie must omit Max-Age, got %q", raw)
		}
		if cookie := sessionCookie(t, resp); cookie == nil || cookie.MaxAge != 0 {
			t.Errorf("session cookie = %+v, want a browser-session cookie", cookie)
		}
	})

	t.Run("bad credentials", func(t *testing.T) {
		resp := postLogin(t, server.URL, `{"user":"admin","password":"wrong-password","remember":true}`)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("login status = %d, want 401", resp.StatusCode)
		}
		if cookie := sessionCookie(t, resp); cookie != nil {
			t.Errorf("failed login set a cookie: %+v", cookie)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read login response: %v", err)
		}
		if strings.Contains(string(body), "wrong-password") || strings.Contains(string(body), "secret") {
			t.Errorf("failure body leaks credentials: %q", body)
		}
	})

	t.Run("next is validated", func(t *testing.T) {
		cases := map[string]string{
			"/admin":                     "/admin",
			"/admin/monitor":             "/admin/monitor",
			"/admin/%2E%2E/other":        "/admin/%2E%2E/other",
			"//evil.example/admin":       "/admin/",
			"https://evil.example/admin": "/admin/",
			"/otherapp":                  "/admin/",
			`/\evil.example/admin`:       "/admin/",
		}
		for next, want := range cases {
			resp := noRedirectRequest(t, http.MethodPost,
				server.URL+"/api/login?next="+url.QueryEscape(next),
				`{"user":"admin","password":"secret","remember":false}`, nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("next=%q login status = %d, want 200", next, resp.StatusCode)
			}
			var body loginBody
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatalf("decode login response: %v", err)
			}
			if body.Redirect != want {
				t.Errorf("next=%q redirect = %q, want %q", next, body.Redirect, want)
			}
		}
	})
}

type loginBody struct {
	Redirect string `json:"redirect"`
}

// TestSessionCookieAuth covers E8: a session cookie authorizes pages, logout
// kills it server-side, and the opaque token never appears in a response body.
func TestSessionCookieAuth(t *testing.T) {
	server, _ := newAdminServer(t, adminTestConfig)

	login := postLogin(t, server.URL, `{"user":"admin","password":"secret","remember":true}`)
	cookie := sessionCookie(t, login)
	if cookie == nil {
		t.Fatal("login did not set mr_session")
	}
	cookieHeader := map[string]string{"Cookie": cookie.Name + "=" + cookie.Value}

	resp := noRedirectRequest(t, http.MethodGet, server.URL+"/admin/monitor", "", cookieHeader)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /admin/monitor with session = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), cookie.Value) {
		t.Error("page body contains the session token")
	}

	logout := noRedirectRequest(t, http.MethodPost, server.URL+"/api/logout", "", cookieHeader)
	if logout.StatusCode != http.StatusOK {
		t.Fatalf("logout status = %d, want 200", logout.StatusCode)
	}
	if raw := setCookieRaw(logout); !strings.Contains(raw, "Max-Age=0") {
		t.Errorf("logout Set-Cookie = %q, want Max-Age=0", raw)
	}
	logoutBody, _ := io.ReadAll(logout.Body)
	if strings.Contains(string(logoutBody), cookie.Value) {
		t.Error("logout body contains the session token")
	}

	resp = noRedirectRequest(t, http.MethodGet, server.URL+"/admin/monitor", "", cookieHeader)
	if resp.StatusCode != http.StatusFound || !strings.HasPrefix(resp.Header.Get("Location"), "/admin/login") {
		t.Fatalf("after logout GET /admin/monitor = %d %q, want 302 /admin/login",
			resp.StatusCode, resp.Header.Get("Location"))
	}

	api := noRedirectRequest(t, http.MethodGet, server.URL+"/api/stats", "", cookieHeader)
	if api.StatusCode != http.StatusUnauthorized {
		t.Fatalf("after logout GET /api/stats = %d, want 401", api.StatusCode)
	}
}

// dialWSAt is dialMonitor with an explicit endpoint so /api/ws can be exercised
// without changing the §24 monitor tests.
func dialWSAt(t *testing.T, serverURL, path, rawQuery string, header map[string]string) (*testWSClient, *http.Response) {
	t.Helper()
	u, err := url.Parse(serverURL)
	if err != nil {
		t.Fatalf("parse server url: %v", err)
	}
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatalf("dial server: %v", err)
	}
	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		conn.Close()
		t.Fatalf("generate websocket key: %v", err)
	}
	target := path
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	var request strings.Builder
	fmt.Fprintf(&request, "GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n", target, u.Host)
	fmt.Fprintf(&request, "Sec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n", base64.StdEncoding.EncodeToString(keyBytes))
	for name, value := range header {
		fmt.Fprintf(&request, "%s: %s\r\n", name, value)
	}
	request.WriteString("\r\n")
	if _, err := io.WriteString(conn, request.String()); err != nil {
		conn.Close()
		t.Fatalf("write websocket handshake: %v", err)
	}
	reader := bufio.NewReader(conn)
	req, _ := http.NewRequest(http.MethodGet, serverURL+target, nil)
	resp, err := http.ReadResponse(reader, req)
	if err != nil {
		conn.Close()
		t.Fatalf("read websocket handshake response: %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		resp.Body.Close()
		conn.Close()
		return nil, resp
	}
	client := &testWSClient{conn: conn, reader: reader}
	t.Cleanup(func() { conn.Close() })
	return client, resp
}

// TestStatsAPIAndWSKeysMatch covers E12: GET /api/stats and the WS stats frame
// come from the same serializer, so their key sets are identical.
func TestStatsAPIAndWSKeysMatch(t *testing.T) {
	backend, _, _ := newBackend(t, http.StatusOK, nil, `{"ok":true}`)
	server, _ := newAdminServer(t, monitorOpenAIConfig(backend.URL, serviceToken, "gpt-x"))

	req := send(t, http.MethodPost, server.URL+"/v1/chat/completions", `{"model":"gpt-x"}`,
		withAuth(config.ProtocolOpenAI, nil))
	if req.StatusCode != http.StatusOK {
		t.Fatalf("proxy status = %d, want 200", req.StatusCode)
	}
	_, _ = io.Copy(io.Discard, req.Body)

	apiResp := noRedirectRequest(t, http.MethodGet, server.URL+"/api/stats", "", basicAuthHeader("admin", "secret"))
	if apiResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/stats = %d, want 200", apiResp.StatusCode)
	}
	var apiStats map[string]any
	if err := json.NewDecoder(apiResp.Body).Decode(&apiStats); err != nil {
		t.Fatalf("stats response is not JSON: %v", err)
	}
	if got := requestCounters(apiStats)["total"]; got != float64(1) {
		t.Errorf("stats.requests.total = %v, want 1", got)
	}

	client, resp := dialWSAt(t, server.URL, "/api/ws", "", basicAuthHeader("admin", "secret"))
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("GET /api/ws handshake = %d, want 101", resp.StatusCode)
	}
	snapshot, err := client.readEvent(5 * time.Second)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if got := eventType(t, snapshot); got != "snapshot" {
		t.Fatalf("first frame = %q, want snapshot", got)
	}
	statsEvent, err := client.readEvent(5 * time.Second)
	if err != nil {
		t.Fatalf("read stats: %v", err)
	}
	if got := eventType(t, statsEvent); got != "stats" {
		t.Fatalf("second frame = %q, want stats", got)
	}
	wsStats, ok := statsEvent["data"].(map[string]any)
	if !ok {
		t.Fatalf("stats frame has no data object: %#v", statsEvent)
	}

	apiKeys := map[string]bool{}
	wsKeys := map[string]bool{}
	collectKeys(apiStats, "", apiKeys)
	collectKeys(wsStats, "", wsKeys)
	missing, extra := diffKeys(apiKeys, wsKeys)
	if len(missing) != 0 || len(extra) != 0 {
		t.Errorf("stats key sets differ: missing in WS %v, extra in WS %v", missing, extra)
	}
}

func collectKeys(value any, prefix string, out map[string]bool) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			out[prefix+key] = true
			collectKeys(child, prefix+key+".", out)
		}
	case []any:
		for _, child := range typed {
			collectKeys(child, prefix+"[].", out)
		}
	}
}

func diffKeys(left, right map[string]bool) (missing, extra []string) {
	for key := range left {
		if !right[key] {
			missing = append(missing, key)
		}
	}
	for key := range right {
		if !left[key] {
			extra = append(extra, key)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	return missing, extra
}

// TestSnapshotHoldsLastHundred covers E14: 120 completed requests with no WS
// client still leave exactly the newest 100 in the snapshot, newest first.
func TestSnapshotHoldsLastHundred(t *testing.T) {
	backend, _, _ := newBackend(t, http.StatusOK, nil, `{"ok":true}`)
	server, _ := newAdminServer(t, monitorOpenAIConfig(backend.URL, serviceToken, "gpt-x"))

	for i := 0; i < 120; i++ {
		resp := send(t, http.MethodPost, server.URL+"/v1/chat/completions", `{"model":"gpt-x"}`,
			withAuth(config.ProtocolOpenAI, nil))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d status = %d, want 200", i, resp.StatusCode)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
	}

	client, resp := dialWSAt(t, server.URL, "/api/ws", "", basicAuthHeader("admin", "secret"))
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("handshake = %d, want 101", resp.StatusCode)
	}
	snapshot, err := client.readEvent(5 * time.Second)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if got := eventType(t, snapshot); got != "snapshot" {
		t.Fatalf("first frame = %q, want snapshot", got)
	}
	requests, ok := snapshot["requests"].([]any)
	if !ok {
		t.Fatalf("snapshot.requests is not an array: %#v", snapshot["requests"])
	}
	if len(requests) != 100 {
		t.Fatalf("snapshot.requests = %d, want 100", len(requests))
	}
	lastTS := int64(1<<62 - 1)
	for i, raw := range requests {
		request, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("snapshot request %d is not an object", i)
		}
		if ended, _ := request["ended"].(bool); !ended {
			t.Errorf("snapshot request %d ended = %v, want true", i, request["ended"])
		}
		ts := int64(request["ts"].(float64))
		if ts > lastTS {
			t.Errorf("snapshot request %d ts = %d is newer than its predecessor %d", i, ts, lastTS)
		}
		lastTS = ts
	}
}

// TestStatsAccumulateWithoutClients covers E15: collection keeps running with no
// WS client connected.
func TestStatsAccumulateWithoutClients(t *testing.T) {
	backend, _, _ := newBackend(t, http.StatusOK, nil, `{"ok":true}`)
	server, _ := newAdminServer(t, monitorOpenAIConfig(backend.URL, serviceToken, "gpt-x"))

	before := getStats(t, server.URL)
	if got := requestCounters(before)["total"]; got != float64(0) {
		t.Fatalf("initial total = %v, want 0", got)
	}

	for i := 0; i < 3; i++ {
		resp := send(t, http.MethodPost, server.URL+"/v1/chat/completions", `{"model":"gpt-x"}`,
			withAuth(config.ProtocolOpenAI, nil))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d status = %d, want 200", i, resp.StatusCode)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
	}

	after := getStats(t, server.URL)
	if got := requestCounters(after)["total"]; got != float64(3) {
		t.Errorf("total after 3 requests = %v, want 3 (collection must not depend on clients)", got)
	}
	if got := requestCounters(after)["success"]; got != float64(3) {
		t.Errorf("success = %v, want 3", got)
	}
	byProtocol, _ := after["byProtocol"].(map[string]any)
	if got := byProtocol["openai"]; got != float64(3) {
		t.Errorf("byProtocol.openai = %v, want 3", got)
	}
}

func requestCounters(stats map[string]any) map[string]any {
	counters, _ := stats["requests"].(map[string]any)
	if counters == nil {
		return map[string]any{}
	}
	return counters
}

func getStats(t *testing.T, serverURL string) map[string]any {
	t.Helper()
	resp := noRedirectRequest(t, http.MethodGet, serverURL+"/api/stats", "", basicAuthHeader("admin", "secret"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/stats = %d, want 200", resp.StatusCode)
	}
	var stats map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		t.Fatalf("stats response is not JSON: %v", err)
	}
	return stats
}

// TestMonitorClear covers E16: clearing empties the snapshot ring without
// touching cumulative counters.
func TestMonitorClear(t *testing.T) {
	backend, _, _ := newBackend(t, http.StatusOK, nil, `{"ok":true}`)
	server, _ := newAdminServer(t, monitorOpenAIConfig(backend.URL, serviceToken, "gpt-x"))

	for i := 0; i < 2; i++ {
		resp := send(t, http.MethodPost, server.URL+"/v1/chat/completions", `{"model":"gpt-x"}`,
			withAuth(config.ProtocolOpenAI, nil))
		_, _ = io.Copy(io.Discard, resp.Body)
	}

	clear := noRedirectRequest(t, http.MethodPost, server.URL+"/api/monitor/clear", "", basicAuthHeader("admin", "secret"))
	if clear.StatusCode != http.StatusOK {
		t.Fatalf("clear status = %d, want 200", clear.StatusCode)
	}

	client, resp := dialWSAt(t, server.URL, "/api/ws", "", basicAuthHeader("admin", "secret"))
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("handshake = %d, want 101", resp.StatusCode)
	}
	snapshot, err := client.readEvent(5 * time.Second)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	requests, _ := snapshot["requests"].([]any)
	if len(requests) != 0 {
		t.Errorf("snapshot.requests after clear = %d, want 0", len(requests))
	}
	if got := requestCounters(getStats(t, server.URL))["total"]; got != float64(2) {
		t.Errorf("clear changed cumulative total: %v, want 2", got)
	}
}

// TestClearAndStatsRequireLogin covers the §24.7 D4 API rule: no redirect, 401.
func TestClearAndStatsRequireLogin(t *testing.T) {
	server, _ := newAdminServer(t, adminTestConfig)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/stats"},
		{http.MethodPost, "/api/monitor/clear"},
		{http.MethodGet, "/api/logout"},
	} {
		resp := noRedirectRequest(t, tc.method, server.URL+tc.path, "", nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s = %d, want 401", tc.method, tc.path, resp.StatusCode)
		}
	}
}

// TestLegacyAuthStillWorks covers E9 at the HTTP level: Basic and query
// credentials continue to open the admin API and the legacy monitor endpoint.
func TestLegacyAuthStillWorks(t *testing.T) {
	server, _ := newAdminServer(t, adminTestConfig)

	basic := noRedirectRequest(t, http.MethodGet, server.URL+"/api/stats", "", basicAuthHeader("admin", "secret"))
	if basic.StatusCode != http.StatusOK {
		t.Errorf("Basic /api/stats = %d, want 200", basic.StatusCode)
	}

	query := url.Values{"user": {"admin"}, "password": {"secret"}}.Encode()
	ws, resp := dialWSAt(t, server.URL, "/api/ws", query, nil)
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("/api/ws with query credentials = %d, want 101", resp.StatusCode)
	}
	if _, err := ws.readEvent(5 * time.Second); err != nil {
		t.Fatalf("read snapshot from query-authed /api/ws: %v", err)
	}
	// §24.11 H2: the old endpoint no longer exists.
	removed := noRedirectRequest(t, http.MethodGet, server.URL+"/api/monitor", "",
		basicAuthHeader("admin", "secret"))
	if removed.StatusCode != http.StatusNotFound {
		t.Errorf("/api/monitor with Basic = %d, want 404", removed.StatusCode)
	}
}
