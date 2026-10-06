// Package webrtc provides a bounded standalone WebRTC signaling and peer lifecycle server.
package webrtc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	pion "github.com/pion/webrtc/v4"
)

const MaxSignalBytes = 256 * 1024

var (
	ErrConfig   = errors.New("invalid WebRTC server configuration")
	ErrProtocol = errors.New("invalid WebRTC signaling message")
)

// ICEConfig defines one explicit STUN/TURN service. String methods never reveal credentials.
type ICEConfig struct {
	URLs       []string
	Username   string
	Credential string
}

func (c ICEConfig) String() string {
	return fmt.Sprintf("ICEConfig{url_count:%d credentials_configured:%t}", len(c.URLs), c.Credential != "")
}

func (c ICEConfig) GoString() string { return c.String() }

// Config controls signaling and bounds peer resources.
type Config struct {
	Address          string
	Path             string
	AllowedOrigins   []string
	ICEServers       []ICEConfig
	MaxPeers         int
	MaxMediaSections int
	MaxDataChannels  int
	MaxSDPBytes      int
	OfferTimeout     time.Duration
	ShutdownTimeout  time.Duration
	Authorize        func(*http.Request) error
	OnPeer           func(*pion.PeerConnection)
	OnTrack          func(*pion.TrackRemote, *pion.RTPReceiver)
	OnDataChannel    func(*pion.DataChannel)
}

func NewConfig() Config {
	return Config{
		Address:          "127.0.0.1:8767",
		Path:             "/rtc",
		MaxPeers:         64,
		MaxMediaSections: 16,
		MaxDataChannels:  32,
		MaxSDPBytes:      192 * 1024,
		OfferTimeout:     20 * time.Second,
		ShutdownTimeout:  10 * time.Second,
	}
}

func (c Config) validate() error {
	if _, _, err := net.SplitHostPort(c.Address); err != nil || !strings.HasPrefix(c.Path, "/") || strings.ContainsAny(c.Path, "?#") {
		return fmt.Errorf("%w: address or path is invalid", ErrConfig)
	}
	if c.MaxPeers < 1 || c.MaxPeers > 4096 || c.MaxMediaSections < 1 || c.MaxMediaSections > 64 || c.MaxDataChannels < 1 || c.MaxDataChannels > 256 || c.MaxSDPBytes < 1024 || c.MaxSDPBytes > MaxSignalBytes {
		return fmt.Errorf("%w: peer or SDP limit is outside supported range", ErrConfig)
	}
	for _, duration := range []time.Duration{c.OfferTimeout, c.ShutdownTimeout} {
		if duration < 100*time.Millisecond || duration > 5*time.Minute {
			return fmt.Errorf("%w: timeout is outside supported range", ErrConfig)
		}
	}
	for _, origin := range c.AllowedOrigins {
		u, err := url.Parse(origin)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("%w: origins must be exact HTTP(S) origins", ErrConfig)
		}
	}
	for _, server := range c.ICEServers {
		if len(server.URLs) == 0 || (server.Username == "") != (server.Credential == "") {
			return fmt.Errorf("%w: ICE URL and credential settings are incomplete", ErrConfig)
		}
		for _, raw := range server.URLs {
			u, err := url.Parse(raw)
			if err != nil || (u.Scheme != "stun" && u.Scheme != "stuns" && u.Scheme != "turn" && u.Scheme != "turns") || u.Opaque == "" && u.Host == "" || u.User != nil {
				return fmt.Errorf("%w: ICE URLs must use STUN/TURN schemes without embedded credentials", ErrConfig)
			}
		}
	}
	return nil
}

type signalMessage struct {
	Version int    `json:"v"`
	Type    string `json:"type"`
	SDP     string `json:"sdp"`
}

type Server struct {
	config   Config
	mu       sync.Mutex
	active   map[*peerLease]struct{}
	slots    chan struct{}
	http     *http.Server
	closed   bool
	activeWG sync.WaitGroup
}

type peerLease struct {
	conn *websocket.Conn
	pc   *pion.PeerConnection
}

func NewServer(config Config) (*Server, error) {
	if err := config.validate(); err != nil {
		return nil, err
	}
	config.AllowedOrigins = append([]string(nil), config.AllowedOrigins...)
	config.ICEServers = append([]ICEConfig(nil), config.ICEServers...)
	for i := range config.ICEServers {
		config.ICEServers[i].URLs = append([]string(nil), config.ICEServers[i].URLs...)
	}
	return &Server{config: config, active: make(map[*peerLease]struct{}), slots: make(chan struct{}, config.MaxPeers)}, nil
}

// Handler returns a mountable HTTP handler for the configured signaling path.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != s.config.Path {
			http.NotFound(w, r)
			return
		}
		if !s.originAllowed(r.Header.Get("Origin")) {
			http.Error(w, "origin not allowed", http.StatusForbidden)
			return
		}
		if s.config.Authorize != nil {
			if err := s.config.Authorize(r); err != nil {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		select {
		case s.slots <- struct{}{}:
		default:
			http.Error(w, "peer capacity reached", http.StatusServiceUnavailable)
			return
		}
		// The explicit exact-origin check above runs before Accept; skip its broader host fallback.
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true, CompressionMode: websocket.CompressionDisabled})
		if err != nil {
			<-s.slots
			return
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = conn.Close(websocket.StatusGoingAway, "server shutting down")
			<-s.slots
			return
		}
		lease := &peerLease{conn: conn}
		s.active[lease] = struct{}{}
		s.activeWG.Add(1)
		s.mu.Unlock()
		defer s.release(lease)
		s.handle(r.Context(), lease)
	})
}

func (s *Server) originAllowed(origin string) bool {
	if origin == "" {
		return true
	}
	for _, allowed := range s.config.AllowedOrigins {
		if origin == allowed {
			return true
		}
	}
	return false
}

func (s *Server) handle(ctx context.Context, lease *peerLease) {
	ctx, cancel := context.WithTimeout(ctx, s.config.OfferTimeout)
	defer cancel()
	lease.conn.SetReadLimit(MaxSignalBytes)
	_, raw, err := lease.conn.Read(ctx)
	if err != nil || len(raw) > MaxSignalBytes {
		_ = lease.conn.Close(websocket.StatusPolicyViolation, "invalid offer")
		return
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || len(fields) != 3 || fields["v"] == nil || fields["type"] == nil || fields["sdp"] == nil {
		_ = lease.conn.Close(websocket.StatusPolicyViolation, "invalid offer")
		return
	}
	var message signalMessage
	if err := json.Unmarshal(raw, &message); err != nil || message.Version != 1 || message.Type != "offer" || message.SDP == "" || len(message.SDP) > s.config.MaxSDPBytes {
		_ = lease.conn.Close(websocket.StatusPolicyViolation, "invalid offer")
		return
	}
	servers := make([]pion.ICEServer, 0, len(s.config.ICEServers))
	for _, server := range s.config.ICEServers {
		servers = append(servers, pion.ICEServer{URLs: append([]string(nil), server.URLs...), Username: server.Username, Credential: server.Credential})
	}
	pc, err := pion.NewPeerConnection(pion.Configuration{ICEServers: servers})
	if err != nil {
		_ = lease.conn.Close(websocket.StatusInternalError, "peer setup failed")
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = pc.Close()
		_ = lease.conn.Close(websocket.StatusGoingAway, "server shutting down")
		return
	}
	lease.pc = pc
	s.mu.Unlock()
	if s.config.OnPeer != nil {
		s.config.OnPeer(pc)
	}
	if s.config.OnTrack != nil {
		pc.OnTrack(s.config.OnTrack)
	} else {
		pc.OnTrack(func(track *pion.TrackRemote, _ *pion.RTPReceiver) {
			go func() {
				for {
					if _, _, readErr := track.ReadRTP(); readErr != nil {
						return
					}
				}
			}()
		})
	}
	var dataChannelCount atomic.Int32
	pc.OnDataChannel(func(channel *pion.DataChannel) {
		if dataChannelCount.Add(1) > int32(s.config.MaxDataChannels) || s.config.OnDataChannel == nil {
			_ = channel.Close()
			return
		}
		s.config.OnDataChannel(channel)
	})
	if err := pc.SetRemoteDescription(pion.SessionDescription{Type: pion.SDPTypeOffer, SDP: message.SDP}); err != nil {
		_ = lease.conn.Close(websocket.StatusPolicyViolation, "invalid SDP offer")
		return
	}
	if len(pc.GetTransceivers()) > s.config.MaxMediaSections {
		_ = lease.conn.Close(websocket.StatusPolicyViolation, "media section limit exceeded")
		return
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		_ = lease.conn.Close(websocket.StatusInternalError, "answer failed")
		return
	}
	gathered := pion.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		_ = lease.conn.Close(websocket.StatusInternalError, "answer failed")
		return
	}
	select {
	case <-gathered:
	case <-ctx.Done():
		_ = lease.conn.Close(websocket.StatusPolicyViolation, "offer deadline exceeded")
		return
	}
	local := pc.LocalDescription()
	if local == nil || len(local.SDP) > s.config.MaxSDPBytes {
		_ = lease.conn.Close(websocket.StatusMessageTooBig, "answer exceeds configured limit")
		return
	}
	response, _ := json.Marshal(signalMessage{Version: 1, Type: "answer", SDP: local.SDP})
	if err := lease.conn.Write(ctx, websocket.MessageText, response); err != nil {
		return
	}
	for {
		_, data, readErr := lease.conn.Read(context.Background())
		if readErr != nil {
			return
		}
		var control map[string]json.RawMessage
		var version int
		var typ string
		if json.Unmarshal(data, &control) != nil || len(control) != 2 || json.Unmarshal(control["v"], &version) != nil || json.Unmarshal(control["type"], &typ) != nil || version != 1 || typ != "close" {
			_ = lease.conn.Close(websocket.StatusPolicyViolation, "unsupported signaling message")
			return
		}
		return
	}
}

func (s *Server) release(lease *peerLease) {
	s.mu.Lock()
	delete(s.active, lease)
	pc := lease.pc
	s.mu.Unlock()
	if pc != nil {
		_ = pc.Close()
	}
	_ = lease.conn.Close(websocket.StatusNormalClosure, "peer closed")
	<-s.slots
	s.activeWG.Done()
}

// ListenAndServe starts the standalone signaling HTTP/WebSocket service.
func (s *Server) ListenAndServe() error {
	s.mu.Lock()
	if s.closed || s.http != nil {
		s.mu.Unlock()
		return ErrConfig
	}
	s.http = &http.Server{
		Addr:              s.config.Address,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 * 1024,
	}
	httpServer := s.http
	s.mu.Unlock()
	return httpServer.ListenAndServe()
}

// Shutdown stops new connections, closes active peers, and waits for HTTP handlers.
func (s *Server) Shutdown(ctx context.Context) error {
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.config.ShutdownTimeout)
		defer cancel()
	}
	s.mu.Lock()
	s.closed = true
	httpServer := s.http
	leases := make([]*peerLease, 0, len(s.active))
	for lease := range s.active {
		leases = append(leases, lease)
	}
	pcs := make([]*pion.PeerConnection, 0, len(leases))
	for _, lease := range leases {
		if lease.pc != nil {
			pcs = append(pcs, lease.pc)
		}
	}
	s.mu.Unlock()
	for _, lease := range leases {
		_ = lease.conn.Close(websocket.StatusGoingAway, "server shutting down")
	}
	for _, pc := range pcs {
		_ = pc.Close()
	}
	var shutdownErr error
	if httpServer != nil {
		shutdownErr = httpServer.Shutdown(ctx)
	}
	done := make(chan struct{})
	go func() {
		s.activeWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return shutdownErr
	case <-ctx.Done():
		return errors.Join(shutdownErr, ctx.Err())
	}
}
