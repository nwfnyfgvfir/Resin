package main

import (
	"bufio"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/config"
)

func TestClassifyInboundConn_KoyebTCP_SOCKS5(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = clientConn.Write([]byte{0x05, 0x01, 0x00})
	}()

	protocol, prefixedConn, err := classifyInboundConn(serverConn, config.DeploymentProfileKoyebTCP, true)
	if err != nil {
		t.Fatalf("classifyInboundConn: %v", err)
	}
	defer prefixedConn.Close()
	if protocol != inboundProtocolSOCKS5 {
		t.Fatalf("protocol: got %q, want %q", protocol, inboundProtocolSOCKS5)
	}

	reader := bufio.NewReader(prefixedConn)
	firstByte, err := reader.ReadByte()
	if err != nil {
		t.Fatalf("ReadByte: %v", err)
	}
	if firstByte != 0x05 {
		t.Fatalf("first byte: got 0x%x, want 0x05", firstByte)
	}
	<-done
}

func TestClassifyInboundConn_KoyebTCP_HTTP(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = clientConn.Write([]byte("GET /healthz HTTP/1.1\r\nHost: example.com\r\n\r\n"))
	}()

	protocol, prefixedConn, err := classifyInboundConn(serverConn, config.DeploymentProfileKoyebTCP, true)
	if err != nil {
		t.Fatalf("classifyInboundConn: %v", err)
	}
	defer prefixedConn.Close()
	if protocol != inboundProtocolHTTP {
		t.Fatalf("protocol: got %q, want %q", protocol, inboundProtocolHTTP)
	}

	reader := bufio.NewReader(prefixedConn)
	firstByte, err := reader.ReadByte()
	if err != nil {
		t.Fatalf("ReadByte: %v", err)
	}
	if firstByte != 'G' {
		t.Fatalf("first byte: got %q, want %q", firstByte, 'G')
	}
	<-done
}

func TestClassifyInboundConn_StandardAlwaysHTTP(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()

	protocol, prefixedConn, err := classifyInboundConn(serverConn, config.DeploymentProfileStandard, true)
	if err != nil {
		t.Fatalf("classifyInboundConn: %v", err)
	}
	defer prefixedConn.Close()
	if protocol != inboundProtocolHTTP {
		t.Fatalf("protocol: got %q, want %q", protocol, inboundProtocolHTTP)
	}
	if prefixedConn != serverConn {
		t.Fatal("standard profile should return original connection without peeking")
	}
}

func TestDescribeSocks5Listener(t *testing.T) {
	envCfg := &config.EnvConfig{
		ListenAddress:     "0.0.0.0",
		ResinPort:         2260,
		Socks5Port:        1080,
		DeploymentProfile: config.DeploymentProfileStandard,
	}
	if got := describeSocks5Listener(envCfg); got != "0.0.0.0:1080" {
		t.Fatalf("describeSocks5Listener standard: got %q", got)
	}

	envCfg.DeploymentProfile = config.DeploymentProfileKoyebTCP
	if got := describeSocks5Listener(envCfg); got != "shared on 0.0.0.0:2260" {
		t.Fatalf("describeSocks5Listener koyeb: got %q", got)
	}
}

func TestServeMixedInboundConn_HTTP(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()

	handlerDone := make(chan struct{})
	httpHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
		close(handlerDone)
	})

	go serveMixedInboundConn(serverConn, config.DeploymentProfileKoyebTCP, httpHandler, nil)
	_, err := clientConn.Write([]byte("GET /healthz HTTP/1.1\r\nHost: example.com\r\n\r\n"))
	if err != nil {
		t.Fatalf("client write: %v", err)
	}

	if err := clientConn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(clientConn), httptest.NewRequest(http.MethodGet, "http://example.com/healthz", nil))
	if err != nil {
		t.Fatalf("ReadResponse: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status: got %d, want %d", resp.StatusCode, http.StatusNoContent)
	}
	<-handlerDone
}
