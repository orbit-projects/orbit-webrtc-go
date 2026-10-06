package webrtc_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func mapHeader(key, value string) mapHeaderType {
	return mapHeaderType{key: []string{value}}
}

type mapHeaderType = map[string][]string
