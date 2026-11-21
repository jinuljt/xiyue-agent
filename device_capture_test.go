package main

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
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
