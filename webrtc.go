// Package webrtc provides a bounded standalone WebRTC signaling and peer lifecycle server.
package webrtc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	// ErrConfig indicates invalid WebRTC server configuration.
	ErrConfig = errors.New("invalid WebRTC server configuration")
	// ErrProtocol indicates invalid or unsupported signaling input.
	ErrProtocol = errors.New("invalid WebRTC signaling message")
)

// ICEConfig defines one explicit STUN/TURN service. String methods never reveal credentials.
type ICEConfig struct {
	// URLs lists bounded STUN/TURN endpoints without embedded user information.
	URLs []string
	// Username is the optional TURN username; it must be paired with Credential.
	Username string
	// Credential is the optional TURN secret and is omitted from formatted output.
	Credential string
}

// String reports whether credentials are configured without revealing them.
func (c ICEConfig) String() string {
	return fmt.Sprintf("ICEConfig{url_count:%d credentials_configured:%t}", len(c.URLs), c.Credential != "")
}

// GoString keeps formatted ICE configuration from revealing credentials.
func (c ICEConfig) GoString() string { return c.String() }

// Config controls signaling and bounds peer resources.
type Config struct {
	// Address is the HTTP listener address; NewConfig binds to loopback by default.
	Address string
	// Path is the exact WebSocket signaling route.
	Path string
	// AllowedOrigins lists exact HTTP(S) browser origins; an empty list allows originless clients only.
	AllowedOrigins []string
	// ICEServers supplies explicit STUN/TURN configuration for peer negotiation.
	ICEServers []ICEConfig
	// MaxPeers bounds active peer connections.
	MaxPeers int
	// MaxMediaSections bounds the media sections accepted in an offer.
	MaxMediaSections int
	// MaxDataChannels bounds data channels per peer.
	MaxDataChannels int
	// MaxSDPBytes bounds an incoming or outgoing session description.
	MaxSDPBytes int
	// OfferTimeout bounds offer processing and peer negotiation setup.
	OfferTimeout time.Duration
	// ShutdownTimeout bounds graceful peer and listener shutdown.
	ShutdownTimeout time.Duration
	// Authorize optionally rejects an HTTP request before the WebSocket upgrade.
	Authorize func(*http.Request) error
	// OnPeer receives each new peer connection for application-owned signaling policy.
	OnPeer func(*pion.PeerConnection)
	// OnTrack receives remote media tracks; the application owns their processing and forwarding.
	OnTrack func(*pion.TrackRemote, *pion.RTPReceiver)
	// OnDataChannel receives negotiated data channels for application-owned message handling.
	OnDataChannel func(*pion.DataChannel)
}

// NewConfig returns loopback-only defaults with finite peer, message, and timeout bounds.
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
	if _, _, err := net.SplitHostPort(c.Address); err != nil || len(c.Address) > 255 || strings.ContainsAny(c.Address, "\r\n\t ") || !strings.HasPrefix(c.Path, "/") || len(c.Path) > 1024 || strings.ContainsAny(c.Path, "?#\\\r\n\t") {
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
	if len(c.AllowedOrigins) > 64 || len(c.ICEServers) > 16 {
		return fmt.Errorf("%w: origin or ICE server list exceeds its limit", ErrConfig)
	}
	seenOrigins := make(map[string]struct{}, len(c.AllowedOrigins))
	for _, origin := range c.AllowedOrigins {
		if len(origin) == 0 || len(origin) > 512 || !isASCIIPrintable(origin) {
			return fmt.Errorf("%w: origins must be bounded printable ASCII values", ErrConfig)
		}
		u, err := url.Parse(origin)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Hostname() == "" || strings.Contains(u.Hostname(), "*") || strings.HasSuffix(u.Host, ":") || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("%w: origins must be exact HTTP(S) origins", ErrConfig)
		}
		port := u.Port()
		if port == "80" && u.Scheme == "http" || port == "443" && u.Scheme == "https" {
			port = ""
		}
		key := strings.ToLower(u.Scheme+"://"+u.Hostname()) + ":" + port
		if _, exists := seenOrigins[key]; exists {
			return fmt.Errorf("%w: origins must be unique", ErrConfig)
		}
		seenOrigins[key] = struct{}{}
	}
	for _, server := range c.ICEServers {
		if len(server.URLs) == 0 || len(server.URLs) > 16 || (server.Username == "") != (server.Credential == "") || len(server.Username) > 1024 || len(server.Credential) > 4096 || !isControlFree(server.Username) || !isControlFree(server.Credential) {
			return fmt.Errorf("%w: ICE URL and credential settings are incomplete", ErrConfig)
		}
		for _, raw := range server.URLs {
			if len(raw) == 0 || len(raw) > 2048 || !isASCIIPrintable(raw) {
				return fmt.Errorf("%w: ICE URLs must be bounded printable ASCII values", ErrConfig)
			}
			u, err := url.Parse(raw)
			if err != nil || (u.Scheme != "stun" && u.Scheme != "stuns" && u.Scheme != "turn" && u.Scheme != "turns") || u.Opaque == "" && u.Host == "" || u.User != nil {
				return fmt.Errorf("%w: ICE URLs must use STUN/TURN schemes without embedded credentials", ErrConfig)
			}
		}
	}
	return nil
}

func isASCIIPrintable(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] < 0x21 || value[i] > 0x7e {
			return false
		}
	}
	return true
}

func isControlFree(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

type signalMessage struct {
	Version int    `json:"v"`
	Type    string `json:"type"`
	SDP     string `json:"sdp"`
}

// Server owns active signaling sessions and peer connections until Close completes.
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

// NewServer validates and snapshots config before creating the peer-capacity limiter.
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

// Handler returns a mountable HTTP handler for the signaling path. It checks the
// exact browser-origin allowlist and optional authorization callback before upgrading.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != s.config.Path {
			http.NotFound(w, r)
			return
		}
		origins := r.Header.Values("Origin")
		if len(origins) > 1 || len(origins) == 1 && !s.originAllowed(origins[0]) {
			http.Error(w, "origin not allowed", http.StatusForbidden)
			return
		}
		select {
		case s.slots <- struct{}{}:
		default:
			http.Error(w, "peer capacity reached", http.StatusServiceUnavailable)
			return
		}
		slotHeld := true
		defer func() {
			if slotHeld {
				<-s.slots
			}
		}()
		if s.config.Authorize != nil {
			if err := s.config.Authorize(r); err != nil {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		// The scheme, host, and port allowlist above is authoritative. Disable the library's
		// same-host fallback because approved browser origins may arrive through a reverse proxy.
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
		slotHeld = false
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
	fields, err := decodeUniqueObject(raw)
	if err != nil || len(fields) != 3 || fields["v"] == nil || fields["type"] == nil || fields["sdp"] == nil {
		_ = lease.conn.Close(websocket.StatusPolicyViolation, "invalid offer")
		return
	}
	var message signalMessage
	if err := json.Unmarshal(raw, &message); err != nil || message.Version != 1 || message.Type != "offer" || message.SDP == "" || len(message.SDP) > s.config.MaxSDPBytes {
		_ = lease.conn.Close(websocket.StatusPolicyViolation, "invalid offer")
		return
	}
	mediaSections := strings.Count(message.SDP, "\nm=")
	if strings.HasPrefix(message.SDP, "m=") {
		mediaSections++
	}
	if mediaSections > s.config.MaxMediaSections {
		_ = lease.conn.Close(websocket.StatusPolicyViolation, "media section limit exceeded")
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
	if len(response) > MaxSignalBytes {
		_ = lease.conn.Close(websocket.StatusMessageTooBig, "answer exceeds signaling limit")
		return
	}
	if err := lease.conn.Write(ctx, websocket.MessageText, response); err != nil {
		return
	}
	for {
		_, data, readErr := lease.conn.Read(context.Background())
		if readErr != nil {
			return
		}
		control, decodeErr := decodeUniqueObject(data)
		var version int
		var typ string
		if decodeErr != nil || len(control) != 2 || control["v"] == nil || control["type"] == nil || json.Unmarshal(control["v"], &version) != nil || json.Unmarshal(control["type"], &typ) != nil || version != 1 || typ != "close" {
			_ = lease.conn.Close(websocket.StatusPolicyViolation, "unsupported signaling message")
			return
		}
		return
	}
}

func decodeUniqueObject(raw []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return nil, ErrProtocol
	}
	fields := make(map[string]json.RawMessage, 3)
	for decoder.More() {
		token, tokenErr := decoder.Token()
		key, ok := token.(string)
		if tokenErr != nil || !ok {
			return nil, ErrProtocol
		}
		if _, duplicate := fields[key]; duplicate {
			return nil, ErrProtocol
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return nil, ErrProtocol
		}
		fields[key] = value
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') {
		return nil, ErrProtocol
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, ErrProtocol
	}
	return fields, nil
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
