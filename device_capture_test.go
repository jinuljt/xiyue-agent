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
	if _, err := readOH2PClip(bytes.NewReader(raw), 1_100_000); err == nil {
		t.Fatal("playback reference was incorrectly treated as speech")
	}
	for n := 10 * captureFrameSamples; n < 20*captureFrameSamples; n++ {
		binary.LittleEndian.PutUint32(raw[n*16:], 2_000_000)
	}
	wav, err := readOH2PClip(bytes.NewReader(raw), 1_100_000)
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
	if _, err := readOH2PClip(strings.NewReader("truncated"), 1_100_000); err == nil {
		t.Fatal("truncated native audio must fail")
	}
}
