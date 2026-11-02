package webrtc_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	orbitwebrtc "github.com/orbit-projects/orbit-webrtc-go"
	pion "github.com/pion/webrtc/v4"
)

func TestConfigRejectsUnsafeValuesAndRedactsCredentials(t *testing.T) {
	config := orbitwebrtc.NewConfig()
	config.MaxPeers = 0
	if _, err := orbitwebrtc.NewServer(config); err == nil {
		t.Fatal("expected invalid peer limit")
	}
	ice := orbitwebrtc.ICEConfig{URLs: []string{"turn:turn.example:3478"}, Username: "short-lived", Credential: "secret-value"}
	if strings.Contains(fmt.Sprintf("%#v", ice), "secret-value") || strings.Contains(fmt.Sprintf("%#v", ice), "short-lived") {
		t.Fatal("ICE config formatting disclosed credential data")
	}
}

func TestConfigBoundsOriginAndIceLists(t *testing.T) {
	invalidOrigins := [][]string{
		{"https://*.example.test"},
		{"https://app.example.test", "HTTPS://APP.EXAMPLE.TEST:443"},
		{"https://app.example.test/path"},
		{strings.Repeat("a", 513)},
	}
	for _, origins := range invalidOrigins {
		config := orbitwebrtc.NewConfig()
		config.AllowedOrigins = origins
		if _, err := orbitwebrtc.NewServer(config); err == nil {
			t.Fatalf("expected invalid origins to be rejected: %#v", origins)
		}
	}
	config := orbitwebrtc.NewConfig()
	config.ICEServers = []orbitwebrtc.ICEConfig{{URLs: make([]string, 17)}}
	if _, err := orbitwebrtc.NewServer(config); err == nil {
		t.Fatal("expected oversized ICE URL list to be rejected")
	}
	config = orbitwebrtc.NewConfig()
	config.ICEServers = []orbitwebrtc.ICEConfig{{
		URLs: []string{"turn:turn.example:3478"}, Username: "u", Credential: strings.Repeat("x", 4097),
	}}
	if _, err := orbitwebrtc.NewServer(config); err == nil {
		t.Fatal("expected oversized TURN credential to be rejected")
	}
}

func TestConfigRejectsInvalidServerAndICEValues(t *testing.T) {
	tests := []struct {
		name   string
		change func(*orbitwebrtc.Config)
	}{
		{name: "address control character", change: func(c *orbitwebrtc.Config) { c.Address = "127.0.0.1:8080\n" }},
		{name: "path query", change: func(c *orbitwebrtc.Config) { c.Path = "/rtc?debug=true" }},
		{name: "zero offer timeout", change: func(c *orbitwebrtc.Config) { c.OfferTimeout = 0 }},
		{name: "incomplete ICE credentials", change: func(c *orbitwebrtc.Config) {
			c.ICEServers = []orbitwebrtc.ICEConfig{{URLs: []string{"turn:turn.example:3478"}, Username: "user"}}
		}},
		{name: "credential control character", change: func(c *orbitwebrtc.Config) {
			c.ICEServers = []orbitwebrtc.ICEConfig{{URLs: []string{"turn:turn.example:3478"}, Username: "user", Credential: "secret\nvalue"}}
		}},
		{name: "unsupported ICE scheme", change: func(c *orbitwebrtc.Config) {
			c.ICEServers = []orbitwebrtc.ICEConfig{{URLs: []string{"https://turn.example"}}}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := orbitwebrtc.NewConfig()
			test.change(&config)
			if _, err := orbitwebrtc.NewServer(config); err == nil {
				t.Fatal("expected unsafe server or ICE value to be rejected")
			}
		})
	}
}

func TestDuplicateSignalingKeysAreRejectedBeforeUpgradeCompletes(t *testing.T) {
	server, err := orbitwebrtc.NewServer(orbitwebrtc.NewConfig())
	if err != nil {
		t.Fatal("server configuration failed")
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(httpServer.URL, "http")+"/rtc", nil)
	if err != nil {
		t.Fatal("signaling connection failed")
	}
	defer conn.Close(websocket.StatusNormalClosure, "test complete")
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"v":1,"type":"offer","type":"offer","sdp":"v=0"}`)); err != nil {
		t.Fatal("offer send failed")
	}
	_, _, err = conn.Read(ctx)
	var closeErr websocket.CloseError
	if !errors.As(err, &closeErr) || closeErr.Code != websocket.StatusPolicyViolation {
		t.Fatalf("duplicate signaling key was not rejected: %v", err)
	}
}

func TestAuthorizationRunsBeforeWebSocketUpgrade(t *testing.T) {
	config := orbitwebrtc.NewConfig()
	config.Authorize = func(_ *http.Request) error { return errors.New("denied") }
	server, err := orbitwebrtc.NewServer(config)
	if err != nil {
		t.Fatal("server configuration failed")
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(httpServer.URL, "http")+"/rtc", nil)
	if err == nil || response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatal("unauthorized signaling request was not rejected before upgrade")
	}
}

func TestDuplicateOriginHeadersAreRejectedBeforeUpgrade(t *testing.T) {
	config := orbitwebrtc.NewConfig()
	config.AllowedOrigins = []string{"https://app.example"}
	server, err := orbitwebrtc.NewServer(config)
	if err != nil {
		t.Fatal("server configuration failed")
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	options := &websocket.DialOptions{HTTPHeader: http.Header{
		"Origin": []string{"https://app.example", "https://app.example"},
	}}
	_, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(httpServer.URL, "http")+"/rtc", options)
	if err == nil || response == nil || response.StatusCode != http.StatusForbidden {
		t.Fatal("duplicate Origin headers were not rejected before upgrade")
	}
}

func TestUnlistedOriginIsRejectedBeforeWebSocketUpgrade(t *testing.T) {
	config := orbitwebrtc.NewConfig()
	config.AllowedOrigins = []string{"https://app.example"}
	server, err := orbitwebrtc.NewServer(config)
	if err != nil {
		t.Fatal("server configuration failed")
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	options := &websocket.DialOptions{HTTPHeader: mapHeader("Origin", "https://attacker.example")}
	_, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(httpServer.URL, "http")+"/rtc", options)
	if err == nil || response == nil || response.StatusCode != http.StatusForbidden {
		t.Fatal("unlisted browser origin was not rejected before upgrade")
	}
}

func TestOversizedAndExcessiveMediaOffersAreRejected(t *testing.T) {
	tests := []struct {
		name   string
		config func(*orbitwebrtc.Config)
		sdp    string
	}{
		{
			name:   "SDP byte limit",
			config: func(c *orbitwebrtc.Config) { c.MaxSDPBytes = 1024 },
			sdp:    "v=0" + strings.Repeat("x", 1022),
		},
		{
			name:   "media section limit",
			config: func(c *orbitwebrtc.Config) { c.MaxMediaSections = 1 },
			sdp:    "v=0\r\nm=audio 9 RTP/AVP 0\r\nm=video 9 RTP/AVP 96\r\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := orbitwebrtc.NewConfig()
			test.config(&config)
			server, err := orbitwebrtc.NewServer(config)
			if err != nil {
				t.Fatal("server configuration failed")
			}
			httpServer := httptest.NewServer(server.Handler())
			defer httpServer.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(httpServer.URL, "http")+"/rtc", nil)
			if err != nil {
				t.Fatal("signaling connection failed")
			}
			defer conn.Close(websocket.StatusNormalClosure, "test complete")
			offer, err := json.Marshal(map[string]any{"v": 1, "type": "offer", "sdp": test.sdp})
			if err != nil {
				t.Fatal("offer encoding failed")
			}
			if err := conn.Write(ctx, websocket.MessageText, offer); err != nil {
				t.Fatal("offer send failed")
			}
			_, _, err = conn.Read(ctx)
			var closeErr websocket.CloseError
			if !errors.As(err, &closeErr) || closeErr.Code != websocket.StatusPolicyViolation {
				t.Fatalf("invalid offer was not rejected with a policy close: %v", err)
			}
		})
	}
}

func TestPeerCapacityIsBoundedAndReleasedAfterDisconnect(t *testing.T) {
	config := orbitwebrtc.NewConfig()
	config.MaxPeers = 1
	server, err := orbitwebrtc.NewServer(config)
	if err != nil {
		t.Fatal("server configuration failed")
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/rtc"
	first, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatal("first peer connection failed")
	}
	defer first.Close(websocket.StatusNormalClosure, "test complete")

	_, response, err := websocket.Dial(ctx, wsURL, nil)
	if err == nil || response == nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatal("peer capacity was not enforced before upgrade")
	}
	if err := first.Close(websocket.StatusNormalClosure, "release peer slot"); err != nil {
		t.Fatal("first peer connection close failed")
	}

	var second *websocket.Conn
	for {
		conn, response, dialErr := websocket.Dial(ctx, wsURL, nil)
		if dialErr == nil {
			second = conn
			break
		}
		if response == nil || response.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("peer slot did not recover after disconnect: %v", dialErr)
		}
		select {
		case <-ctx.Done():
			t.Fatal("peer slot was not released before the deadline")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := second.Close(websocket.StatusNormalClosure, "test complete"); err != nil {
		t.Fatal("second peer connection close failed")
	}

	if err := server.Shutdown(ctx); err != nil {
		t.Fatalf("server shutdown failed after peer disconnect: %v", err)
	}
}

func TestLoopbackPeerAndDataChannelRoundTrip(t *testing.T) {
	echoed := make(chan struct{}, 1)
	config := orbitwebrtc.NewConfig()
	config.AllowedOrigins = []string{"https://app.example"}
	config.OnDataChannel = func(channel *pion.DataChannel) {
		channel.OnMessage(func(message pion.DataChannelMessage) {
			if string(message.Data) == "ping" {
				_ = channel.Send(message.Data)
			}
		})
	}
	server, err := orbitwebrtc.NewServer(config)
	if err != nil {
		t.Fatal("server configuration failed")
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	client, err := pion.NewPeerConnection(pion.Configuration{})
	if err != nil {
		t.Fatal("client peer creation failed")
	}
	defer client.Close()
	dataChannel, err := client.CreateDataChannel("echo", nil)
	if err != nil {
		t.Fatal("client data channel creation failed")
	}
	open := make(chan struct{}, 1)
	dataChannel.OnOpen(func() { open <- struct{}{} })
	dataChannel.OnMessage(func(message pion.DataChannelMessage) {
		if string(message.Data) == "ping" {
			echoed <- struct{}{}
		}
	})
	offer, err := client.CreateOffer(nil)
	if err != nil {
		t.Fatal("offer creation failed")
	}
	gathered := pion.GatheringCompletePromise(client)
	if err = client.SetLocalDescription(offer); err != nil {
		t.Fatal("client local description failed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	select {
	case <-gathered:
	case <-ctx.Done():
		t.Fatal("client ICE gathering timed out")
	}
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/rtc"
	ws, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: mapHeader("Origin", "https://app.example")})
	if err != nil {
		t.Fatal("signaling connection failed")
	}
	defer ws.Close(websocket.StatusNormalClosure, "test complete")
	offerMessage, err := json.Marshal(map[string]any{"v": 1, "type": "offer", "sdp": client.LocalDescription().SDP})
	if err != nil {
		t.Fatal("offer encoding failed")
	}
	if err = ws.Write(ctx, websocket.MessageText, offerMessage); err != nil {
		t.Fatal("offer send failed")
	}
	_, rawAnswer, err := ws.Read(ctx)
	if err != nil {
		t.Fatal("answer receive failed")
	}
	var answer struct {
		Version int    `json:"v"`
		Type    string `json:"type"`
		SDP     string `json:"sdp"`
	}
	if err = json.Unmarshal(rawAnswer, &answer); err != nil || answer.Version != 1 || answer.Type != "answer" || answer.SDP == "" {
		t.Fatal("server returned an invalid answer")
	}
	if err = client.SetRemoteDescription(pion.SessionDescription{Type: pion.SDPTypeAnswer, SDP: answer.SDP}); err != nil {
		t.Fatal("client rejected server answer")
	}
	select {
	case <-open:
	case <-ctx.Done():
		t.Fatal("data channel did not open")
	}
	if err = dataChannel.SendText("ping"); err != nil {
		t.Fatal("data channel send failed")
	}
	select {
	case <-echoed:
	case <-ctx.Done():
		t.Fatal("data channel echo timed out")
	}
	if err = server.Shutdown(ctx); err != nil {
		t.Fatal("server shutdown failed")
	}
}

func TestListenAndServeShutdownLifecycle(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("could not reserve a local test address")
	}
	address := probe.Addr().String()
	if err := probe.Close(); err != nil {
		t.Fatal("could not release the local test address")
	}
	config := orbitwebrtc.NewConfig()
	config.Address = address
	server, err := orbitwebrtc.NewServer(config)
	if err != nil {
		t.Fatal("server configuration failed")
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.ListenAndServe() }()

	deadline := time.Now().Add(2 * time.Second)
	for {
		conn, dialErr := net.DialTimeout("tcp", address, 50*time.Millisecond)
		if dialErr == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("HTTP server did not begin listening")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := server.ListenAndServe(); !errors.Is(err, orbitwebrtc.ErrConfig) {
		t.Fatalf("second server start was not rejected: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		t.Fatalf("server shutdown failed: %v", err)
	}
	select {
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("server returned unexpected listen error: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("listener did not stop before the shutdown deadline")
	}
}

func mapHeader(key, value string) mapHeaderType {
	return mapHeaderType{key: []string{value}}
}

type mapHeaderType = map[string][]string
