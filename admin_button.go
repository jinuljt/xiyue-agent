package main

import (
	"context"
	"encoding/binary"
	"io"
	"os"
	"strconv"
	"time"
)

func serveAdminButton(ctx context.Context, device *os.File, code uint16, g *adminGate) {
	stop := context.AfterFunc(ctx, func() { _ = device.Close() })
	defer stop()
	defer device.Close()
	defer func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.buttonReady = false
		if g.pending != nil && g.pending.method == "button" {
			g.clearDeviceLogin()
		}
	}()
	readAdminButton(device, code, g.confirmButtonLogin)
}

func readAdminButton(src io.Reader, code uint16, confirm func(time.Time)) {
	// input_event carries two native longs followed by type, code and value.
	header := strconv.IntSize / 4
	event := make([]byte, header+8)
	var pressed, first time.Time
	var dropped bool
	for {
		if _, err := io.ReadFull(src, event); err != nil {
			return
		}
		data := event[header:]
		eventType, eventCode := binary.NativeEndian.Uint16(data), binary.NativeEndian.Uint16(data[2:])
		if eventType == 0 {
			if eventCode == 3 {
				dropped = true
				pressed, first = time.Time{}, time.Time{}
			}
			if eventCode == 0 {
				dropped = false
			}
		}
		if dropped || eventType != 1 || eventCode != code {
			continue
		}
		// Default evdev timestamps are wall-clock values. Reject buffered or invalid events.
		seconds := int64(binary.NativeEndian.Uint32(event))
		micros := int64(binary.NativeEndian.Uint32(event[4:]))
		if header == 16 {
			seconds = int64(binary.NativeEndian.Uint64(event))
			micros = int64(binary.NativeEndian.Uint64(event[8:]))
		}
		now := time.Unix(seconds, micros*1000)
		age := time.Since(now)
		if micros < 0 || micros >= 1_000_000 || age < 0 || age > 2*time.Second {
			first, pressed = time.Time{}, time.Time{}
			continue
		}
		switch binary.NativeEndian.Uint32(data[4:]) {
		case 1:
			pressed = now
		case 0:
			if pressed.IsZero() || now.Sub(pressed) > 500*time.Millisecond {
				first, pressed = time.Time{}, time.Time{}
				continue
			}
			if !first.IsZero() && now.Sub(first) <= time.Second {
				confirm(first)
				first = time.Time{}
			} else {
				first = pressed
			}
			pressed = time.Time{}
		}
	}
}
