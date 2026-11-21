package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"strconv"
	"time"
)

var wakeTone = tonePCM(90*time.Millisecond, 880)
var alarmTone = tonePCM(300*time.Millisecond, 720)

func tonePCM(duration time.Duration, frequency float64) []byte {
	const rate = 24000
	const fade = rate / 150
	samples := int(duration * rate / time.Second)
	data := make([]byte, samples*2)
	for i := range samples {
		edge := float64(min(fade, i, samples-1-i)) / fade
		value := 6800 * edge * math.Sin(2*math.Pi*frequency*float64(i)/rate)
		binary.LittleEndian.PutUint16(data[i*2:], uint16(int16(value)))
	}
	return data
}

func (v *voiceRuntime) playTone(ctx context.Context, clip []byte) error {
	if v.playPCM == nil {
		return errors.New("本地提示音不可用")
	}
	return v.playPCM(ctx, bytes.NewReader(clip))
}

func (v *voiceRuntime) ring(ctx context.Context, interval time.Duration) error {
	for ctx.Err() == nil {
		if err := v.playTone(ctx, alarmTone); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}
	}
	return nil
}

func readAlarmButton(src io.Reader, stopAlarm func() bool) {
	header := strconv.IntSize / 4
	event := make([]byte, header+8)
	for {
		if _, err := io.ReadFull(src, event); err != nil {
			return
		}
		if binary.NativeEndian.Uint16(event[header:]) == 1 && binary.NativeEndian.Uint32(event[header+4:]) == 1 {
			stopAlarm()
		}
	}
}
