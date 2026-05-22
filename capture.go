package main

import (
	"bytes"
	"context"
	"image"
	"image/jpeg"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kbinani/screenshot"
)

type CaptureEncoder struct {
	mu      sync.Mutex
	quality int
}

func NewCaptureEncoder() *CaptureEncoder {
	return &CaptureEncoder{quality: 70}
}

func (ce *CaptureEncoder) SetQuality(q float64) {
	ce.mu.Lock()
	defer ce.mu.Unlock()
	if q <= 1 {
		ce.quality = int(q * 100)
	} else {
		ce.quality = int(q)
	}
	if ce.quality < 20 {
		ce.quality = 20
	}
	if ce.quality > 95 {
		ce.quality = 95
	}
}

func (ce *CaptureEncoder) Encode(img image.Image) ([]byte, error) {
	ce.mu.Lock()
	q := ce.quality
	ce.mu.Unlock()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: q}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

type CaptureLoop struct {
	encoder   *CaptureEncoder
	bandwidth *BandwidthMonitor
	pool      *TransportPool
	localCh   chan<- *WireMessage
	ctx       context.Context
	cancel    context.CancelFunc
	fps       atomic.Uint64 // stored as fps * 100
	tier      CaptureTier
	mu        sync.Mutex
}

func NewCaptureLoop(encoder *CaptureEncoder, bandwidth *BandwidthMonitor, pool *TransportPool, localCh chan<- *WireMessage) *CaptureLoop {
	ctx, cancel := context.WithCancel(context.Background())
	cl := &CaptureLoop{
		encoder:   encoder,
		bandwidth: bandwidth,
		pool:      pool,
		localCh:   localCh,
		ctx:       ctx,
		cancel:    cancel,
		tier:      CaptureTierAuto,
	}
	cl.fps.Store(uint64(15 * 100))
	go cl.run()
	return cl
}

func (cl *CaptureLoop) SetFPS(fps float64) {
	if fps < MIN_FPS {
		fps = MIN_FPS
	}
	if fps > 30 {
		fps = 30
	}
	cl.fps.Store(uint64(fps * 100))
}

func (cl *CaptureLoop) ForceTier(t CaptureTier) {
	cl.mu.Lock()
	cl.tier = t
	cl.mu.Unlock()
}

func (cl *CaptureLoop) Stop() {
	cl.cancel()
}

func (cl *CaptureLoop) run() {
	var lastFrame []byte
	for {
		select {
		case <-cl.ctx.Done():
			return
		default:
		}
		fps := float64(cl.fps.Load()) / 100.0
		if fps < MIN_FPS {
			fps = MIN_FPS
		}
		interval := time.Duration(float64(time.Second) / fps)

		n := screenshot.NumActiveDisplays()
		if n == 0 {
			time.Sleep(interval)
			continue
		}
		bounds := screenshot.GetDisplayBounds(0)
		cl.mu.Lock()
		tier := cl.tier
		cl.mu.Unlock()
		if tier == CaptureTierLow {
			bounds = scaleBounds(bounds, 0.5)
		}

		img, err := screenshot.CaptureRect(bounds)
		if err != nil {
			time.Sleep(interval)
			continue
		}
		data, err := cl.encoder.Encode(img)
		if err != nil || len(data) == 0 {
			time.Sleep(interval)
			continue
		}

		msgType := MSG_FRAME_DELTA
		if lastFrame == nil || len(data) > len(lastFrame)*3/4 {
			msgType = MSG_FRAME_KEY
		}
		lastFrame = data
		wm := &WireMessage{Type: msgType, Data: data}

		if cl.localCh != nil {
			select {
			case cl.localCh <- wm:
			default:
			}
		}
		if cl.pool != nil {
			if t := cl.pool.GetBest(); t != nil {
				_ = t.Send(wm)
				if cl.bandwidth != nil {
					cl.bandwidth.RecordBytes(len(data))
					delay := cl.bandwidth.GetThrottleDelay()
					if delay > 0 {
						time.Sleep(delay)
					}
				}
			}
		}

		atomic.AddUint64(&framesCaptured, 1)
		time.Sleep(interval)
	}
}

func scaleBounds(b image.Rectangle, factor float64) image.Rectangle {
	w := int(float64(b.Dx()) * factor)
	h := int(float64(b.Dy()) * factor)
	if w < 64 {
		w = 64
	}
	if h < 64 {
		h = 64
	}
	return image.Rect(b.Min.X, b.Min.Y, b.Min.X+w, b.Min.Y+h)
}

var framesCaptured uint64
