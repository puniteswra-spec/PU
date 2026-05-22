//go:build windows

package main

import (
	"sync"

	"github.com/gorilla/websocket"
)

type wsTransport struct {
	conn     *websocket.Conn
	url      string
	priority int
	mu       sync.Mutex
	recvCh   chan *WireMessage
	closed   chan struct{}
}

func NewWSTransport(conn *websocket.Conn, priority int, url string) Transport {
	t := &wsTransport{
		conn:     conn,
		url:      url,
		priority: priority,
		recvCh:   make(chan *WireMessage, 16),
		closed:   make(chan struct{}),
	}
	go t.readPump()
	return t
}

func (t *wsTransport) Name() string { return "ws" }

func (t *wsTransport) Priority() int { return t.priority }

func (t *wsTransport) Send(wm *WireMessage) error {
	if wm == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.conn.WriteMessage(websocket.BinaryMessage, wm.Marshal())
}

func (t *wsTransport) Recv() (*WireMessage, error) {
	select {
	case <-t.closed:
		return nil, websocket.ErrCloseSent
	case wm := <-t.recvCh:
		return wm, nil
	}
}

func (t *wsTransport) readPump() {
	defer close(t.closed)
	for {
		_, data, err := t.conn.ReadMessage()
		if err != nil {
			return
		}
		wm := &WireMessage{}
		if wm.Unmarshal(data) != nil {
			continue
		}
		select {
		case t.recvCh <- wm:
		case <-t.closed:
			return
		}
	}
}
