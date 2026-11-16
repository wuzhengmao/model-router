package proxy

import (
	"bufio"
	"net"
	"net/http/httptest"
	"testing"
	"time"
)

// TestMonitorHubNoClientNoCollection covers §24.2 F1/A2: without a connected
// monitor client the handler does not create a per-request tracker, and a
// broadcast is a no-op.
func TestMonitorHubNoClientNoCollection(t *testing.T) {
	hub := newMonitorHub()
	if hub.hasClients() {
		t.Fatal("new hub reports connected clients")
	}

	r := httptest.NewRequest("POST", "/v1/messages", nil)
	if tracker := hub.newRequest(r, protocolAnthropic, "sonnet"); tracker != nil {
		t.Fatal("no client connected, but a request tracker was created")
	}
	hub.broadcast(&monitorStartEvent{Type: "start"})

	serverConn, peerConn := net.Pipe()
	defer peerConn.Close()
	defer serverConn.Close()
	client := newMonitorClient(hub, serverConn,
		bufio.NewReadWriter(bufio.NewReader(serverConn), bufio.NewWriter(serverConn)))
	hub.add(client)
	defer hub.remove(client)

	if !hub.hasClients() {
		t.Fatal("hub does not report the connected client")
	}
	if tracker := hub.newRequest(r, protocolAnthropic, "sonnet"); tracker == nil {
		t.Fatal("client connected, but no request tracker was created")
	}
}

// TestMonitorSlowClientDisconnected covers §24.2 F3/M6: once more than 1024
// events are queued undelivered, the connection is dropped instead of blocking
// the proxy.
func TestMonitorSlowClientDisconnected(t *testing.T) {
	hub := newMonitorHub()
	serverConn, peerConn := net.Pipe()
	defer peerConn.Close()

	client := newMonitorClient(hub, serverConn,
		bufio.NewReadWriter(bufio.NewReader(serverConn), bufio.NewWriter(serverConn)))
	hub.add(client)
	go client.writeLoop()
	go client.readLoop()

	// The peer never reads, so the writer blocks on the first frame and the
	// queue fills up; the enqueue after the limit must drop the connection.
	for i := 0; i < monitorQueueLimit+64 && hub.hasClients(); i++ {
		client.enqueue([]byte(`{"type":"chunk"}`))
	}

	select {
	case <-client.done:
	case <-time.After(2 * time.Second):
		t.Fatal("slow client was not disconnected after exceeding the queue limit")
	}
	if hub.hasClients() {
		t.Fatal("slow client is still registered with the hub")
	}

	// Further broadcasts must not panic or block.
	hub.broadcast(&monitorChunkEvent{Type: "chunk"})
}
