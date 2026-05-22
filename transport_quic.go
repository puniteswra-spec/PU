package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
)

type quicTransport struct {
	conn     *quic.Conn
	stream   *quic.Stream
	url      string
	priority int
	mu       sync.Mutex
	recvCh   chan *WireMessage
	closed   chan struct{}
	ctx      context.Context
	cancel   context.CancelFunc
}

func NewQUICTransport(ctx context.Context, addr string, priority int, url string) (Transport, error) {
	tlsConf := &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{"punmonitor"},
	}

	quicConn, err := quic.DialAddr(ctx, addr, tlsConf, &quic.Config{
		MaxIncomingStreams: 1,
		KeepAlivePeriod:    5 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("quic dial %s: %w", addr, err)
	}

	stream, err := quicConn.OpenStreamSync(ctx)
	if err != nil {
		quicConn.CloseWithError(0, "stream open failed")
		return nil, fmt.Errorf("quic stream open: %w", err)
	}

	tCtx, tCancel := context.WithCancel(ctx)
	t := &quicTransport{
		conn:     quicConn,
		stream:   stream,
		url:      url,
		priority: priority,
		recvCh:   make(chan *WireMessage, 16),
		closed:   make(chan struct{}),
		ctx:      tCtx,
		cancel:   tCancel,
	}
	go t.readPump()
	return t, nil
}

func (t *quicTransport) Name() string { return "quic" }

func (t *quicTransport) Priority() int { return t.priority }

func (t *quicTransport) Send(wm *WireMessage) error {
	if wm == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	data := wm.Marshal()
	// Write length prefix (4 bytes) + payload
	length := uint32(len(data))
	lenBytes := []byte{byte(length >> 24), byte(length >> 16), byte(length >> 8), byte(length)}
	_, err := t.stream.Write(append(lenBytes, data...))
	return err
}

func (t *quicTransport) Recv() (*WireMessage, error) {
	select {
	case <-t.closed:
		return nil, fmt.Errorf("quic transport closed")
	case wm := <-t.recvCh:
		return wm, nil
	}
}

func (t *quicTransport) readPump() {
	defer close(t.closed)
	defer t.cancel()
	lenBuf := make([]byte, 4)
	for {
		select {
		case <-t.ctx.Done():
			return
		default:
		}
		// Read 4-byte length prefix
		n, err := t.stream.Read(lenBuf)
		if err != nil || n < 4 {
			return
		}
		length := uint32(lenBuf[0])<<24 | uint32(lenBuf[1])<<16 | uint32(lenBuf[2])<<8 | uint32(lenBuf[3])
		if length == 0 || length > 65536 {
			return
		}
		// Read payload
		payload := make([]byte, length)
		totalRead := 0
		for totalRead < int(length) {
			n, err = t.stream.Read(payload[totalRead:])
			if err != nil {
				return
			}
			totalRead += n
		}
		wm := &WireMessage{}
		if wm.Unmarshal(payload) != nil {
			continue
		}
		select {
		case t.recvCh <- wm:
		case <-t.closed:
			return
		}
	}
}

func (t *quicTransport) Close() error {
	t.cancel()
	t.stream.CancelRead(0)
	return t.conn.CloseWithError(0, "closed")
}

// QUICTunnelServer provides a QUIC endpoint for agents to connect to
type QUICTunnelServer struct {
	listener  quic.EarlyListener
	onConnect func(Transport, string)
	mu        sync.Mutex
	clients   map[string]Transport
}

func NewQUICTunnelServer(addr string, onConnect func(Transport, string)) (*QUICTunnelServer, error) {
	tlsConf := &tls.Config{
		Certificates: []tls.Certificate{},
		NextProtos:   []string{"punmonitor"},
	}
	listener, err := quic.ListenAddrEarly(addr, tlsConf, &quic.Config{
		MaxIncomingStreams: 100,
		KeepAlivePeriod:    5 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("quic listen: %w", err)
	}
	return &QUICTunnelServer{
		listener:  *listener,
		onConnect: onConnect,
		clients:   make(map[string]Transport),
	}, nil
}

func (s *QUICTunnelServer) Start(ctx context.Context) {
	go func() {
		for {
			conn, err := s.listener.Accept(ctx)
			if err != nil {
				return
			}
			go s.handleClient(conn)
		}
	}()
}

func (s *QUICTunnelServer) handleClient(conn *quic.Conn) {
	defer conn.CloseWithError(0, "done")
	for {
		stream, err := conn.AcceptStream(context.Background())
		if err != nil {
			return
		}
		go s.handleStream(stream, conn.RemoteAddr().String())
	}
}

func (s *QUICTunnelServer) handleStream(stream *quic.Stream, remoteAddr string) {
	defer stream.Close()
	tCtx, tCancel := context.WithCancel(context.Background())
	t := &quicTransport{
		conn:     nil, // server-side doesn't need conn reference
		stream:   stream,
		url:      remoteAddr,
		priority: 5,
		recvCh:   make(chan *WireMessage, 16),
		closed:   make(chan struct{}),
		ctx:      tCtx,
		cancel:   tCancel,
	}
	go t.readPump()

	s.mu.Lock()
	s.clients[remoteAddr] = t
	s.mu.Unlock()

	if s.onConnect != nil {
		s.onConnect(t, remoteAddr)
	}

	<-t.closed
	s.mu.Lock()
	delete(s.clients, remoteAddr)
	s.mu.Unlock()
}

func (s *QUICTunnelServer) Stop() {
	s.listener.Close()
}

// QUIC server-side message handling for relay
func (s *QUICTunnelServer) Broadcast(msg *WireMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, client := range s.clients {
		_ = client.Send(msg)
	}
}

// QUIC agent-side control command handling
func handleQUICControl(t Transport, cmd map[string]interface{}) {
	switch cmd["type"] {
	case "mouse_move":
	case "mouse_click":
	case "key_press":
	case "set_quality":
	case "set_fps":
		if q, ok := cmd["quality"].(float64); ok {
			// quality adjustment would go through encoder
			_ = q
		}
	case "ping":
		resp, _ := json.Marshal(map[string]interface{}{"type": "pong"})
		t.Send(&WireMessage{Type: MSG_HEARTBEAT, Data: resp})
	}
}
