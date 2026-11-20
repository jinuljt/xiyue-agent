package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/coder/websocket"
)

func TestDoubaoSentenceRecognitionOverWebSocket(t *testing.T) {
	wav := append(fixtureWAV(), make([]byte, 12000)...)
	binary.LittleEndian.PutUint32(wav[4:], uint32(len(wav)-8))
	binary.LittleEndian.PutUint32(wav[40:], uint32(len(wav)-44))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := r.Header.Get("X-Api-Request-Id")
		if r.Header.Get("X-Api-Key") != "synthetic-key" || r.Header.Get("X-Api-Resource-Id") != doubaoASRResource ||
			!regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-4[a-f0-9]{3}-[89ab][a-f0-9]{3}-[a-f0-9]{12}$`).MatchString(requestID) {
			t.Error("incorrect authentication or request ID")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		var audio []byte
		for index, expected := range []byte{0x10, 0x20, 0x22} {
			kind, frame, err := conn.Read(r.Context())
			if err != nil || kind != websocket.MessageBinary || len(frame) < 8 || frame[0] != 0x11 ||
				frame[1] != expected || int(binary.BigEndian.Uint32(frame[4:8])) != len(frame)-8 {
				t.Errorf("bad wire frame %d: %v", index, err)
				return
			}
			compressed, err := gzip.NewReader(bytes.NewReader(frame[8:]))
			if err != nil {
				t.Error(err)
				return
			}
			data, err := io.ReadAll(compressed)
			compressed.Close()
			if err != nil {
				t.Error(err)
				return
			}
			if index == 0 {
				var config struct {
					Audio struct {
						Format string
						Rate   int
					}
					Request struct {
						Model string `json:"model_name"`
					}
				}
				if err := json.Unmarshal(data, &config); err != nil || config.Audio.Format != "wav" ||
					config.Audio.Rate != 16000 || config.Request.Model != "bigmodel" {
					t.Error("unexpected speech configuration")
					return
				}
			} else {
				audio = append(audio, data...)
			}
		}
		if !bytes.Equal(audio, wav) {
			t.Error("speech payload changed")
			return
		}
		partial := []byte(`{"result":{"text":"部分识别"}}`)
		early := binary.BigEndian.AppendUint32([]byte{0x11, 0x91, 0x10, 0}, 1)
		early = binary.BigEndian.AppendUint32(early, uint32(len(partial)))
		if err := conn.Write(r.Context(), websocket.MessageBinary, append(early, partial...)); err != nil {
			t.Error(err)
			return
		}
		encoded := newDoubaoEncoder().frame(0x93, 0x10, []byte(`{"result":{"text":"合成的识别结果"}}`))
		final := append(binary.BigEndian.AppendUint32(encoded[:4:4], ^uint32(1)), encoded[4:]...)
		if err := conn.Write(r.Context(), websocket.MessageBinary, final); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	reply, err := recognizeDoubao(context.Background(), "ws"+strings.TrimPrefix(server.URL, "http"), doubaoASRResource, "synthetic-key", wav)
	if err != nil || reply != "合成的识别结果" {
		t.Fatalf("unexpected result: %q, %v", reply, err)
	}
}

func TestDoubaoRejectsBadFramesAndRedirects(t *testing.T) {
	for _, malformed := range [][]byte{{0x11, 0x93}, {0x11, 0x92, 0x10, 0, 0, 0, 0, 5},
		{0x11, 0x93, 0x10, 0, 0, 0, 0, 0}} {
		if _, _, err := parseDoubaoFrame(malformed); err == nil {
			t.Fatal("malformed response was accepted")
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirected" {
			t.Error("redirect forwarded the speech credential")
			return
		}
		http.Redirect(w, r, "/redirected", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	if _, err := recognizeDoubao(context.Background(), "ws"+strings.TrimPrefix(server.URL, "http"), doubaoASRResource, "synthetic-key", nil); err == nil ||
		strings.Contains(err.Error(), "synthetic-key") {
		t.Fatal("redirect should fail without revealing the credential")
	}
}
