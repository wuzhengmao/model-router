package proxy_test

import (
	"bytes"
	"encoding/binary"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestFaviconAnonymous covers §24.13 K6d: browsers request the icon without
// credentials, so it must never be redirected to the login page.
func TestFaviconAnonymous(t *testing.T) {
	server, _ := newAdminServer(t, adminTestConfig)
	resp := noRedirectRequest(t, http.MethodGet, server.URL+"/favicon.ico", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("anonymous GET /favicon.ico = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "image/x-icon" {
		t.Errorf("Content-Type = %q, want image/x-icon", got)
	}
	if got := resp.Header.Get("Cache-Control"); !strings.Contains(got, "max-age=86400") {
		t.Errorf("Cache-Control = %q, want max-age=86400", got)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read favicon: %v", err)
	}
	if len(body) < 6 || !bytes.Equal(body[:4], []byte{0x00, 0x00, 0x01, 0x00}) {
		t.Fatalf("favicon does not start with the ICO magic: %x", body[:min(6, len(body))])
	}
	count := int(binary.LittleEndian.Uint16(body[4:6]))
	if count < 3 {
		t.Fatalf("ICONDIR count = %d, want >= 3", count)
	}
	sizes := make(map[int]bool)
	for i := 0; i < count; i++ {
		entry := body[6+i*16:]
		width := int(entry[0])
		if width == 0 {
			width = 256
		}
		sizes[width] = true
	}
	for _, want := range []int{16, 32, 48} {
		if !sizes[want] {
			t.Errorf("ICO is missing the %dx%d entry (sizes %v)", want, want, sizes)
		}
	}

	// The admin surface being disabled must not remove the brand icon.
	disabled, _ := newAdminServer(t, `{
		"auth_token": "tok",
		"instances": {"openai": [{"base_url": "https://example.com", "api_key": "k", "models": ["m"]}]}
	}`)
	resp = noRedirectRequest(t, http.MethodGet, disabled.URL+"/favicon.ico", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("favicon with admin disabled = %d, want 200", resp.StatusCode)
	}
}

// TestLogoSVGAuth covers §24.13 K6e: the header logo requires login and is
// served as SVG.
func TestLogoSVGAuth(t *testing.T) {
	server, _ := newAdminServer(t, adminTestConfig)

	anonymous := noRedirectRequest(t, http.MethodGet, server.URL+"/admin/logo.svg", "", nil)
	if anonymous.StatusCode != http.StatusFound {
		t.Fatalf("anonymous GET /admin/logo.svg = %d, want 302", anonymous.StatusCode)
	}
	if location := anonymous.Header.Get("Location"); !strings.HasPrefix(location, "/admin/login") {
		t.Errorf("Location = %q, want /admin/login", location)
	}

	login := postLogin(t, server.URL, `{"user":"admin","password":"secret","remember":false}`)
	cookie := sessionCookie(t, login)
	if cookie == nil {
		t.Fatal("login did not set mr_session")
	}
	resp := noRedirectRequest(t, http.MethodGet, server.URL+"/admin/logo.svg", "",
		map[string]string{"Cookie": cookie.Name + "=" + cookie.Value})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("logged-in GET /admin/logo.svg = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "image/svg+xml" {
		t.Errorf("Content-Type = %q, want image/svg+xml", got)
	}
	if got := resp.Header.Get("Cache-Control"); !strings.Contains(got, "max-age=86400") {
		t.Errorf("Cache-Control = %q, want max-age=86400", got)
	}
	body := readBody(t, resp)
	if !strings.HasPrefix(body, "<svg") && !strings.HasPrefix(body, "<?xml") {
		t.Errorf("SVG body does not start with <svg/<?xml: %.40q", body)
	}
	if !strings.Contains(body, `viewBox="0 0 256 256"`) {
		t.Errorf("SVG body is missing the expected viewBox: %.120q", body)
	}
}
