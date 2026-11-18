package proxy

import (
	"crypto/tls"
	"log"
	"net/http/httptrace"
	"sync"
	"time"
)

// §24.26 M118: pre-header timing breakdown. All fields are written by transport
// callbacks (possibly on other goroutines) and read after client.Do returns, so
// every access is guarded.
type headerProfile struct {
	mu             sync.Mutex
	connectAt      time.Time
	tlsAt          time.Time
	wroteHeadersAt time.Time
	wroteRequestAt time.Time
	firstByteAt    time.Time
	reused         bool
	remote         string
}

func (p *headerProfile) trace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		ConnectDone: func(network, addr string, err error) {
			if err != nil {
				return
			}
			p.mu.Lock()
			p.connectAt = time.Now()
			p.mu.Unlock()
		},
		TLSHandshakeDone: func(state tls.ConnectionState, err error) {
			if err != nil {
				return
			}
			p.mu.Lock()
			p.tlsAt = time.Now()
			p.mu.Unlock()
		},
		WroteHeaders: func() {
			p.mu.Lock()
			p.wroteHeadersAt = time.Now()
			p.mu.Unlock()
		},
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err != nil {
				return
			}
			p.mu.Lock()
			p.wroteRequestAt = time.Now()
			p.mu.Unlock()
		},
		GotFirstResponseByte: func() {
			p.mu.Lock()
			p.firstByteAt = time.Now()
			p.mu.Unlock()
		},
		GotConn: func(info httptrace.GotConnInfo) {
			p.mu.Lock()
			p.reused = info.Reused
			if info.Conn != nil {
				p.remote = info.Conn.RemoteAddr().String()
			}
			p.mu.Unlock()
		},
	}
}

// log prints one hdrprof line, but only when the header wait exceeded half of
// the instance timeout (§24.26 M118).
func (p *headerProfile) log(instName string, attemptStart, headersAt time.Time, timeout time.Duration, bodyBytes int, budget time.Duration) {
	if timeout <= 0 {
		return
	}
	hdr := headersAt.Sub(attemptStart)
	if hdr <= timeout/2 {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	connect, tlsPart, upload, serverWait := "-", "-", "-", "-"
	if !p.connectAt.IsZero() {
		connect = formatProfileDuration(p.connectAt.Sub(attemptStart))
	}
	if !p.connectAt.IsZero() && !p.tlsAt.IsZero() {
		tlsPart = formatProfileDuration(p.tlsAt.Sub(p.connectAt))
	}
	if !p.wroteHeadersAt.IsZero() && !p.wroteRequestAt.IsZero() {
		upload = formatProfileDuration(p.wroteRequestAt.Sub(p.wroteHeadersAt))
	}
	if !p.wroteRequestAt.IsZero() && !p.firstByteAt.IsZero() {
		serverWait = formatProfileDuration(p.firstByteAt.Sub(p.wroteRequestAt))
	}
	reused := 0
	if p.reused {
		reused = 1
	}
	remote := p.remote
	if remote == "" {
		remote = "-"
	}
	log.Printf("hdrprof inst=%s connect=%s tls=%s upload=%s server_wait=%s body_bytes=%d reused=%d remote=%s budget=%s",
		instName, connect, tlsPart, upload, serverWait, bodyBytes, reused, remote, formatProfileDuration(budget))
}

func formatProfileDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	return d.Round(time.Millisecond).String()
}
