// Package proxy implements the M1 OpenAI-compatible passthrough proxy.
package proxy

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"model-router/internal/config"
)

// Path is the only API path served in M1.
const Path = "/v1/chat/completions"

var hopByHopHeaders = []string{
	"Connection",
	"Proxy-Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// Handler forwards /v1/chat/completions requests to a single OpenAI-compatible
// backend without translating the protocol.
type Handler struct {
	baseURL   *url.URL
	apiKey    string
	authToken string
	client    *http.Client
}

// New builds a Handler for the configured backend.
func New(cfg *config.Config) (*Handler, error) {
	if cfg.AuthToken == "" {
		return nil, errors.New("auth_token is required")
	}
	if cfg.Backend.APIKey == "" {
		return nil, errors.New("backend.api_key is required")
	}

	base, err := url.Parse(cfg.Backend.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse backend.base_url: %w", err)
	}
	if base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") {
		return nil, fmt.Errorf("backend.base_url must be an absolute http(s) URL, got %q", cfg.Backend.BaseURL)
	}

	timeout := cfg.Backend.Timeout.Duration()
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		ExpectContinueTimeout: time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   16,
		ForceAttemptHTTP2:     true,
	}

	return &Handler{
		baseURL:   base,
		apiKey:    cfg.Backend.APIKey,
		authToken: cfg.AuthToken,
		client:    &http.Client{Transport: transport},
	}, nil
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		if r.Header.Get("Authorization") == "" {
			writeError(w, http.StatusUnauthorized, "invalid_request_error", "missing_api_key", "missing Authorization header")
		} else {
			writeError(w, http.StatusUnauthorized, "invalid_request_error", "invalid_api_key", "invalid service access token")
		}
		return
	}

	if r.URL.Path != Path {
		writeError(w, http.StatusNotFound, "invalid_request_error", "not_found", "only "+Path+" is served")
		return
	}

	start := time.Now()
	status, err := h.forward(w, r)
	if err != nil {
		// The response may already be partially written; just record the failure.
		log.Printf("%s %s failed: %v", r.Method, r.URL.Path, err)
		return
	}
	log.Printf("%s %s -> %d in %s", r.Method, r.URL.Path, status, time.Since(start).Round(time.Millisecond))
}

// authorized reports whether the request carries the configured service token.
// The comparison is constant-time to avoid leaking the token through timing.
func (h *Handler) authorized(r *http.Request) bool {
	const prefix = "Bearer "
	value := r.Header.Get("Authorization")
	if len(value) <= len(prefix) || !strings.EqualFold(value[:len(prefix)], prefix) {
		return false
	}
	token := value[len(prefix):]
	return subtle.ConstantTimeCompare([]byte(token), []byte(h.authToken)) == 1
}

func (h *Handler) forward(w http.ResponseWriter, r *http.Request) (int, error) {
	target, err := url.JoinPath(h.baseURL.String(), r.URL.Path)
	if err != nil {
		writeError(w, http.StatusBadGateway, "server_error", "upstream_error", "cannot build upstream URL")
		return 0, fmt.Errorf("join upstream URL: %w", err)
	}
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}

	outReq, err := http.NewRequestWithContext(r.Context(), r.Method, target, r.Body)
	if err != nil {
		writeError(w, http.StatusBadGateway, "server_error", "upstream_error", "cannot build upstream request")
		return 0, fmt.Errorf("build upstream request: %w", err)
	}
	outReq.ContentLength = r.ContentLength
	copyHeader(outReq.Header, r.Header)
	removeHopByHop(outReq.Header)
	// ContentLength field is authoritative; a stale header would conflict with it.
	outReq.Header.Del("Content-Length")
	// Client credentials are only for this proxy; upstream always uses the
	// configured key, never the caller's token.
	outReq.Header.Set("Authorization", "Bearer "+h.apiKey)

	resp, err := h.client.Do(outReq)
	if err != nil {
		writeError(w, http.StatusBadGateway, "server_error", "upstream_error", "upstream request failed")
		return 0, fmt.Errorf("upstream request: %w", err)
	}
	defer resp.Body.Close()

	copyHeader(w.Header(), resp.Header)
	removeHopByHop(w.Header())
	w.WriteHeader(resp.StatusCode)

	if err := copyBody(w, resp.Body); err != nil {
		return resp.StatusCode, fmt.Errorf("copy response body: %w", err)
	}
	return resp.StatusCode, nil
}

// copyBody streams the upstream body to the client, flushing after each chunk
// so SSE responses reach the caller without buffering.
func copyBody(w http.ResponseWriter, src io.Reader) error {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func copyHeader(dst, src http.Header) {
	for key, values := range src {
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func removeHopByHop(header http.Header) {
	for _, value := range header.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			if token = strings.TrimSpace(token); token != "" {
				header.Del(token)
			}
		}
	}
	for _, name := range hopByHopHeaders {
		header.Del(name)
	}
}

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

func writeError(w http.ResponseWriter, status int, errType, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorBody{Error: errorDetail{Message: message, Type: errType, Code: code}})
}
