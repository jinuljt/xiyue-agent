package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDoubaoPlaybackStreamsBeforeSynthesisFinishes(t *testing.T) {
	firstPlayed := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Params struct {
				Text    string
				Speaker string
				Audio   struct {
					Format string
					Rate   int `json:"sample_rate"`
				} `json:"audio_params"`
			} `json:"req_params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Params.Text != "稍等一下" ||
			request.Params.Speaker != doubaoDefaultVoice || request.Params.Audio.Format != "pcm" ||
			request.Params.Audio.Rate != 24000 || r.Header.Get("X-Api-Key") != "synthetic-key" ||
			r.Header.Get("X-Api-Resource-Id") != doubaoTTSResource {
			http.Error(w, "unexpected request", 400)
			t.Error("incorrect synthesis parameters")
			return
		}
		_, _ = io.WriteString(w, "{\"code\":0,\"data\":\"AQACAA==\"}\n")
		w.(http.Flusher).Flush()
		select {
		case <-firstPlayed:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, "{\"code\":0,\"data\":\"AwAEAA==\"}\n")
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	player := func(_ context.Context, audio io.Reader) error {
		first := make([]byte, 4)
		if _, err := io.ReadFull(audio, first); err != nil {
			return err
		}
		close(firstPlayed)
		last, err := io.ReadAll(audio)
		if err != nil {
			return err
		}
		if !bytes.Equal(append(first, last...), []byte{1, 0, 2, 0, 3, 0, 4, 0}) {
			t.Error("playback PCM differs from synthesized bytes")
		}
		return nil
	}
	if err := doubaoSpeech(ctx, server.URL, doubaoDefaultVoice, "synthetic-key", "稍等一下", player); err != nil {
		t.Fatal(err)
	}
}

func TestDoubaoSynthesisErrorsDoNotStartPlayback(t *testing.T) {
	for _, body := range []string{`{"code":45000000,"message":"contains sensitive request context"}`,
		`{"data":"AQACAA=="}`, `{"code":0}`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, body)
		}))
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err := doubaoSpeech(ctx, server.URL, doubaoDefaultVoice, "synthetic-key", "测试", func(context.Context, io.Reader) error {
			t.Error("playback started without valid PCM")
			return nil
		})
		cancel()
		server.Close()
		if err == nil || bytes.Contains([]byte(err.Error()), []byte("sensitive")) {
			t.Fatal("invalid synthesis response should fail without forwarding provider body")
		}
	}
}
