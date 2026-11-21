package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOH2PCaptureEndsOnSilenceAndExcludesPlaybackReference(t *testing.T) {
	raw := make([]byte, 60*captureFrameSamples*16)
	for n := 0; n < 60*captureFrameSamples; n++ {
		binary.LittleEndian.PutUint32(raw[n*16+12:], 2_000_000_000)
	}
	if _, err := readOH2PClip(bytes.NewReader(raw), 1_100_000, nil); err == nil {
		t.Fatal("playback reference was incorrectly treated as speech")
	}
	for n := 10 * captureFrameSamples; n < 20*captureFrameSamples; n++ {
		binary.LittleEndian.PutUint32(raw[n*16:], 2_000_000)
	}
	wav, err := readOH2PClip(bytes.NewReader(raw), 1_100_000, nil)
	if err != nil || !validVoiceWAV(wav) {
		t.Fatalf("capture failed: %v", err)
	}
	const audibleFrames = 10
	const retainedPreroll = 9
	if samples := (len(wav) - 44) / 2; samples != (retainedPreroll+audibleFrames+28)*captureFrameSamples {
		t.Fatalf("expected bounded pre-roll and silence cutoff, got %d samples", samples)
	}
	if n := binary.LittleEndian.Uint16(wav[44+retainedPreroll*captureFrameSamples*2:]); n != 24000 {
		t.Fatalf("wrong channel or normalization: %d", n)
	}
	if _, err := readOH2PClip(strings.NewReader("truncated"), 1_100_000, nil); err == nil {
		t.Fatal("truncated native audio must fail")
	}
	reader := bytes.NewReader(raw)
	var pcm []byte
	firstFrame := 0
	wav, err = readOH2PClip(reader, 1_100_000, func(chunk []byte) error {
		if len(pcm) == 0 {
			firstFrame = (len(raw) - reader.Len()) / (captureFrameSamples * 16)
		}
		pcm = append(pcm, chunk...)
		return nil
	})
	if err != nil || wav != nil || firstFrame != 18 || len(pcm) != (retainedPreroll+audibleFrames+28)*captureFrameSamples*2 {
		t.Fatalf("unexpected streaming capture: first frame %d, PCM bytes %d, error %v", firstFrame, len(pcm), err)
	}
	if n := binary.LittleEndian.Uint16(pcm[retainedPreroll*captureFrameSamples*2:]); n != 2_000_000>>9 {
		t.Fatalf("wrong stream channel or gain: %d", n)
	}
}

func TestDeviceWakeRoutesCountdownLocally(t *testing.T) {
	dir := t.TempDir()
	detector := filepath.Join(dir, "detector")
	if err := os.WriteFile(detector, []byte("#!/bin/sh\nprintf '%s\\n' 'app components started' 'gate armed: local_kws: unrelated' 'gate armed: local_kws: 你好喜悦'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	asr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/transcriptions" {
			t.Errorf("unexpected recognition path %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"text":"你好喜悦，倒计时一秒钟。"}`)
	}))
	defer asr.Close()
	p, err := openPlatform(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	input := settingsInput(p.snapshot().Settings)
	input.WakeWord = "你好喜悦"
	input.ASR = ServiceInput{Provider: "sensevoice", Endpoint: asr.URL + "/v1"}
	if err := p.saveSettings(input); err != nil {
		t.Fatal(err)
	}
	spoken := make(chan string, 2)
	v := &voiceRuntime{speak: func(_ context.Context, text string) error {
		spoken <- text
		return nil
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	p.runWakeDetector(ctx, detector, "unused-keyword-file", v, func(context.Context, func([]byte) error) ([]byte, error) {
		return fixtureWAV(), nil
	}, nil)
	select {
	case answer := <-spoken:
		if answer != "好的，已设定一秒钟。" {
			t.Fatal("did not confirm a local countdown")
		}
	case <-ctx.Done():
		t.Fatal("wake event did not complete a voice turn")
	}
	state := p.snapshot()
	if len(state.Usage) != 0 || len(state.Schedules) != 1 || state.Schedules[0].AbilityID != quickTimer.ID {
		t.Fatal("countdown must schedule locally without a model call")
	}
}
