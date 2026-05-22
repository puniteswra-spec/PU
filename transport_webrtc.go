//go:build windows

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
)

func ptrBool(b bool) *bool { return &b }

type webrtcTransport struct {
	pc         *webrtc.PeerConnection
	dc         *webrtc.DataChannel
	url        string
	priority   int
	mu         sync.Mutex
	recvCh     chan *WireMessage
	closed     chan struct{}
	ctx        context.Context
	cancel     context.CancelFunc
	sendSignal func(msgType int, data []byte) error
}

func NewWebRTCTransport(ctx context.Context, url string, priority int, sendSignal func(int, []byte) error) (*webrtcTransport, error) {
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
			{URLs: []string{"stun:stun1.l.google.com:19302"}},
		},
	}

	pc, err := webrtc.NewPeerConnection(config)
	if err != nil {
		return nil, fmt.Errorf("webrtc new peer: %w", err)
	}

	dc, err := pc.CreateDataChannel("punmonitor", &webrtc.DataChannelInit{
		Ordered: ptrBool(true),
	})
	if err != nil {
		pc.Close()
		return nil, fmt.Errorf("webrtc create data channel: %w", err)
	}

	tCtx, tCancel := context.WithCancel(ctx)
	t := &webrtcTransport{
		pc:         pc,
		dc:         dc,
		url:        url,
		priority:   priority,
		recvCh:     make(chan *WireMessage, 16),
		closed:     make(chan struct{}),
		ctx:        tCtx,
		cancel:     tCancel,
		sendSignal: sendSignal,
	}

	dc.OnOpen(func() {
		llog("info", "webrtc data channel opened: %s", url)
	})

	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		wm := &WireMessage{}
		if wm.Unmarshal(msg.Data) == nil {
			select {
			case t.recvCh <- wm:
			case <-t.closed:
			}
		}
	})

	dc.OnClose(func() {
		close(t.closed)
		t.cancel()
	})

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateDisconnected || state == webrtc.PeerConnectionStateFailed {
			close(t.closed)
			t.cancel()
		}
	})

	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate != nil && t.sendSignal != nil {
			init := candidate.ToJSON()
			if data, err := json.Marshal(init); err == nil {
				_ = t.sendSignal(MSG_WEBRTC_ICE, data)
			}
		}
	})

	// Create and send offer asynchronously
	t.sendOffer()

	go t.keepaliveLoop()

	return t, nil
}

func (t *webrtcTransport) sendOffer() {
	offer, err := t.pc.CreateOffer(nil)
	if err != nil {
		llog("error", "webrtc create offer: %v", err)
		return
	}
	if err := t.pc.SetLocalDescription(offer); err != nil {
		llog("error", "webrtc set local desc: %v", err)
		return
	}
	<-webrtc.GatheringCompletePromise(t.pc)
	localDesc := t.pc.LocalDescription()
	if localDesc == nil {
		llog("error", "webrtc no local description")
		return
	}
	data, err := json.Marshal(localDesc)
	if err != nil {
		llog("error", "webrtc marshal offer: %v", err)
		return
	}
	if t.sendSignal != nil {
		_ = t.sendSignal(MSG_WEBRTC_OFFER, data)
	}
}

func (t *webrtcTransport) HandleAnswer(sdp string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.pc == nil {
		return fmt.Errorf("webrtc: no peer connection")
	}
	var desc webrtc.SessionDescription
	if err := json.Unmarshal([]byte(sdp), &desc); err != nil {
		return fmt.Errorf("webrtc: invalid answer SDP: %w", err)
	}
	if err := t.pc.SetRemoteDescription(desc); err != nil {
		return fmt.Errorf("webrtc: set remote answer: %w", err)
	}
	return nil
}

func (t *webrtcTransport) HandleICE(candidateJSON string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.pc == nil {
		return fmt.Errorf("webrtc: no peer connection")
	}
	var candidate webrtc.ICECandidateInit
	if err := json.Unmarshal([]byte(candidateJSON), &candidate); err != nil {
		return fmt.Errorf("webrtc: invalid candidate JSON: %w", err)
	}
	return t.pc.AddICECandidate(candidate)
}

func (t *webrtcTransport) Name() string { return "webrtc" }

func (t *webrtcTransport) Priority() int { return t.priority }

func (t *webrtcTransport) Send(wm *WireMessage) error {
	if wm == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.dc == nil || t.dc.ReadyState() != webrtc.DataChannelStateOpen {
		return fmt.Errorf("webrtc data channel not open")
	}
	data := wm.Marshal()
	return t.dc.Send(data)
}

func (t *webrtcTransport) Recv() (*WireMessage, error) {
	select {
	case <-t.closed:
		return nil, fmt.Errorf("webrtc transport closed")
	case wm := <-t.recvCh:
		return wm, nil
	}
}

func (t *webrtcTransport) keepaliveLoop() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-t.ctx.Done():
			return
		case <-ticker.C:
			if t.dc != nil && t.dc.ReadyState() == webrtc.DataChannelStateOpen {
				_ = t.dc.Send([]byte(`{"type":"webrtc_ping"}`))
			}
		}
	}
}

func (t *webrtcTransport) Close() error {
	t.cancel()
	if t.pc != nil {
		return t.pc.Close()
	}
	return nil
}

// WebRTCSignalHandler handles WebRTC signaling on the server side
type WebRTCSignalHandler struct {
	pcMap   map[string]*webrtc.PeerConnection
	dcMap   map[string]*webrtc.DataChannel
	conns   map[string]*websocket.Conn
	onData  func(agentID string, msg *WireMessage)
	mu      sync.RWMutex
}

func NewWebRTCSignalHandler(onData func(agentID string, msg *WireMessage)) *WebRTCSignalHandler {
	return &WebRTCSignalHandler{
		pcMap:  make(map[string]*webrtc.PeerConnection),
		dcMap:  make(map[string]*webrtc.DataChannel),
		conns:  make(map[string]*websocket.Conn),
		onData: onData,
	}
}

func (h *WebRTCSignalHandler) SetConn(agentID string, conn *websocket.Conn) {
	h.mu.Lock()
	h.conns[agentID] = conn
	h.mu.Unlock()
}

func (h *WebRTCSignalHandler) RemoveConn(agentID string) {
	h.mu.Lock()
	delete(h.conns, agentID)
	h.mu.Unlock()
}

func (h *WebRTCSignalHandler) getConn(agentID string) *websocket.Conn {
	h.mu.RLock()
	conn := h.conns[agentID]
	h.mu.RUnlock()
	return conn
}

func (h *WebRTCSignalHandler) HandleOffer(agentID string, sdp string) (string, error) {
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
		},
	}

	pc, err := webrtc.NewPeerConnection(config)
	if err != nil {
		return "", fmt.Errorf("webrtc new peer: %w", err)
	}

	dc, err := pc.CreateDataChannel("punmonitor", &webrtc.DataChannelInit{
		Ordered: ptrBool(true),
	})
	if err != nil {
		pc.Close()
		return "", fmt.Errorf("webrtc create data channel: %w", err)
	}

	dc.OnOpen(func() {
		llog("info", "webrtc data channel opened for agent %s", agentID)
	})

	dc.OnClose(func() {
		h.mu.Lock()
		delete(h.pcMap, agentID)
		delete(h.dcMap, agentID)
		h.mu.Unlock()
	})

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateDisconnected || state == webrtc.PeerConnectionStateClosed {
			h.mu.Lock()
			delete(h.pcMap, agentID)
			delete(h.dcMap, agentID)
			h.mu.Unlock()
		}
	})

	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}
		init := candidate.ToJSON()
		data, err := json.Marshal(init)
		if err != nil {
			return
		}
		// Send ICE candidate to agent via stored WebSocket connection
		h.mu.RLock()
		conn := h.conns[agentID]
		h.mu.RUnlock()
		if conn != nil {
			wm := &WireMessage{Type: MSG_WEBRTC_ICE, Data: data}
			_ = conn.WriteMessage(websocket.BinaryMessage, wm.Marshal())
		}
	})

	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		wm := &WireMessage{}
		if wm.Unmarshal(msg.Data) == nil {
			if h.onData != nil {
				h.onData(agentID, wm)
			}
		}
	})

	h.mu.Lock()
	h.pcMap[agentID] = pc
	h.dcMap[agentID] = dc
	h.mu.Unlock()

	// Set remote description (agent's offer)
	var offer webrtc.SessionDescription
	if err := json.Unmarshal([]byte(sdp), &offer); err != nil {
		pc.Close()
		h.mu.Lock()
		delete(h.pcMap, agentID)
		delete(h.dcMap, agentID)
		h.mu.Unlock()
		return "", fmt.Errorf("webrtc: invalid offer JSON: %w", err)
	}

	if err := pc.SetRemoteDescription(offer); err != nil {
		pc.Close()
		h.mu.Lock()
		delete(h.pcMap, agentID)
		delete(h.dcMap, agentID)
		h.mu.Unlock()
		return "", fmt.Errorf("webrtc: set remote offer: %w", err)
	}

	// Create answer
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		pc.Close()
		h.mu.Lock()
		delete(h.pcMap, agentID)
		delete(h.dcMap, agentID)
		h.mu.Unlock()
		return "", fmt.Errorf("webrtc: create answer: %w", err)
	}

	if err := pc.SetLocalDescription(answer); err != nil {
		pc.Close()
		h.mu.Lock()
		delete(h.pcMap, agentID)
		delete(h.dcMap, agentID)
		h.mu.Unlock()
		return "", fmt.Errorf("webrtc: set local answer: %w", err)
	}

	<-webrtc.GatheringCompletePromise(pc)
	localDesc := pc.LocalDescription()
	if localDesc == nil {
		pc.Close()
		h.mu.Lock()
		delete(h.pcMap, agentID)
		delete(h.dcMap, agentID)
		h.mu.Unlock()
		return "", fmt.Errorf("webrtc: no local description")
	}

	return localDesc.SDP, nil
}

func (h *WebRTCSignalHandler) HandleICECandidate(agentID string, candidateJSON string) error {
	h.mu.RLock()
	pc, ok := h.pcMap[agentID]
	h.mu.RUnlock()
	if !ok {
		return fmt.Errorf("webrtc: no peer connection for %s", agentID)
	}

	var candidate webrtc.ICECandidateInit
	if err := json.Unmarshal([]byte(candidateJSON), &candidate); err != nil {
		return fmt.Errorf("webrtc: invalid candidate JSON: %w", err)
	}

	return pc.AddICECandidate(candidate)
}

func (h *WebRTCSignalHandler) SendData(agentID string, data []byte) error {
	h.mu.RLock()
	dc, ok := h.dcMap[agentID]
	h.mu.RUnlock()
	if !ok {
		return fmt.Errorf("webrtc: no data channel for %s", agentID)
	}

	if dc.ReadyState() != webrtc.DataChannelStateOpen {
		return fmt.Errorf("webrtc: data channel not open for %s", agentID)
	}

	return dc.Send(data)
}

func (h *WebRTCSignalHandler) Close(agentID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if pc, ok := h.pcMap[agentID]; ok {
		pc.Close()
		delete(h.pcMap, agentID)
	}
	if _, ok := h.dcMap[agentID]; ok {
		delete(h.dcMap, agentID)
	}
}

func (h *WebRTCSignalHandler) CloseAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for agentID, pc := range h.pcMap {
		pc.Close()
		delete(h.pcMap, agentID)
	}
	for agentID := range h.dcMap {
		delete(h.dcMap, agentID)
	}
}
