package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
)

const (
	doubaoASREndpoint = "wss://openspeech.bytedance.com/api/v3/sauc/bigmodel_nostream"
	doubaoASRResource = "volc.seedasr.sauc.duration"
	doubaoFrameLimit  = 128 << 10
	doubaoPacketBytes = 16_000 * 2 / 5 // 200 ms of 16 kHz, 16-bit mono audio
)

func (p *Platform) transcribeDoubao(ctx context.Context, s State, wav []byte) (string, error) {
	cfg := s.Settings.ASR
	if cfg.Endpoint != "" || s.Sealed["service:asr"] == "" {
		return "", errors.New("请在控制台保存豆包语音 API Key 与资源 ID")
	}
	key, err := p.unseal("service:asr", s.Sealed["service:asr"])
	if err != nil {
		return "", err
	}
	resource := cfg.Model
	if resource == "" {
		resource = doubaoASRResource
	}
	return recognizeDoubao(ctx, doubaoASREndpoint, resource, key, wav)
}

func recognizeDoubao(parent context.Context, endpoint, resource, key string, wav []byte) (string, error) {
	ctx, cancel := context.WithTimeout(parent, 12*time.Second)
	defer cancel()
	var requestID [16]byte
	if _, err := rand.Read(requestID[:]); err != nil {
		return "", err
	}
	requestID[6] = (requestID[6] & 0x0f) | 0x40
	requestID[8] = (requestID[8] & 0x3f) | 0x80
	headers := http.Header{}
	headers.Set("X-Api-Key", key)
	headers.Set("X-Api-Resource-Id", resource)
	headers.Set("X-Api-Request-Id", fmt.Sprintf("%x-%x-%x-%x-%x", requestID[:4], requestID[4:6], requestID[6:8], requestID[8:10], requestID[10:]))
	conn, response, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{
		HTTPHeader: headers,
		HTTPClient: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	})
	if err != nil {
		if response != nil {
			return "", fmt.Errorf("豆包语音握手失败（HTTP %d）", response.StatusCode)
		}
		return "", errors.New("豆包语音连接失败或超时")
	}
	defer conn.CloseNow()
	conn.SetReadLimit(doubaoFrameLimit)
	config := []byte(`{"audio":{"format":"wav","codec":"raw","rate":16000,"bits":16,"channel":1},"request":{"model_name":"bigmodel","result_type":"full"}}`)
	encoder := newDoubaoEncoder()
	if err := conn.Write(ctx, websocket.MessageBinary, encoder.frame(0x10, 0x10, config)); err != nil {
		return "", errors.New("无法发送豆包语音配置或请求已超时")
	}
	sent := make(chan error, 1)
	go func() {
		remaining := wav
		for len(remaining) > 0 {
			size := min(len(remaining), doubaoPacketBytes)
			message := byte(0x20)
			if size == len(remaining) {
				message = 0x22 // last audio fragment, without a sequence number
			}
			if err := conn.Write(ctx, websocket.MessageBinary, encoder.frame(message, 0, remaining[:size])); err != nil {
				sent <- errors.New("无法发送豆包语音数据或请求已超时")
				cancel()
				return
			}
			remaining = remaining[size:]
		}
		sent <- nil
	}()
	for {
		kind, data, err := conn.Read(ctx)
		if err != nil {
			select {
			case sendErr := <-sent:
				if sendErr != nil {
					return "", sendErr
				}
			default:
			}
			return "", errors.New("豆包语音未返回最终识别结果")
		}
		if kind != websocket.MessageBinary {
			return "", errors.New("豆包语音响应格式无效")
		}
		text, final, err := parseDoubaoFrame(data)
		if err != nil {
			return "", err
		}
		if final {
			if err := <-sent; err != nil {
				return "", err
			}
			return text, nil
		}
	}
}

type doubaoEncoder struct {
	compressed bytes.Buffer
	writer     *gzip.Writer
}

func newDoubaoEncoder() *doubaoEncoder {
	e := &doubaoEncoder{}
	e.writer, _ = gzip.NewWriterLevel(&e.compressed, gzip.BestSpeed)
	return e
}

func (e *doubaoEncoder) frame(kind, serialization byte, payload []byte) []byte {
	e.compressed.Reset()
	e.writer.Reset(&e.compressed)
	_, _ = e.writer.Write(payload)
	_ = e.writer.Close()
	frame := []byte{0x11, kind, serialization | 1, 0}
	frame = binary.BigEndian.AppendUint32(frame, uint32(e.compressed.Len()))
	return append(frame, e.compressed.Bytes()...)
}

func parseDoubaoFrame(frame []byte) (text string, final bool, err error) {
	invalid := errors.New("豆包语音响应格式无效")
	if len(frame) < 8 || frame[0] != 0x11 {
		return "", false, invalid
	}
	body := frame[4:]
	switch frame[1] >> 4 {
	case 0xf:
		if len(body) < 8 {
			return "", false, invalid
		}
		return "", false, fmt.Errorf("豆包语音识别失败（错误码 %d）", binary.BigEndian.Uint32(body))
	case 9:
	default:
		return "", false, invalid
	}
	if frame[2]>>4 != 1 || frame[1]&0x0c != 0 {
		return "", false, invalid
	}
	if frame[1]&1 != 0 {
		if len(body) < 8 {
			return "", false, invalid
		}
		body = body[4:] // sequence, whose sign varies between protocol versions
	}
	if uint64(binary.BigEndian.Uint32(body)) != uint64(len(body)-4) {
		return "", false, invalid
	}
	body = body[4:]
	switch frame[2] & 0xf {
	case 0:
	case 1:
		z, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return "", false, invalid
		}
		defer z.Close()
		body, err = io.ReadAll(io.LimitReader(z, doubaoFrameLimit+1))
		if err != nil || len(body) > doubaoFrameLimit {
			return "", false, invalid
		}
	default:
		return "", false, invalid
	}
	var reply struct {
		Result struct {
			Text string
		}
	}
	if err := json.Unmarshal(body, &reply); err != nil {
		return "", false, invalid
	}
	if frame[1]&2 == 0 {
		return "", false, nil
	}
	text = strings.TrimSpace(reply.Result.Text)
	if text == "" || len(text) > 16000 {
		return "", false, errors.New("没有识别到可用文字")
	}
	return text, true, nil
}
