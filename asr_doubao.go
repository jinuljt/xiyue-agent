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
	"regexp"
	"strings"
	"time"

	"github.com/coder/websocket"
)

const (
	doubaoASREndpoint = "wss://openspeech.bytedance.com/api/v3/sauc/bigmodel_async"
	doubaoASRResource = "volc.seedasr.sauc.duration"
	doubaoFrameLimit  = 128 << 10
	doubaoPacketBytes = 16_000 * 2 * 160 / 1000
)

type doubaoAudioSource func(context.Context, func([]byte) error) error

func transcribeDoubao(ctx context.Context, cfg ServiceConfig, key string, source doubaoAudioSource) (string, error) {
	if cfg.Endpoint != "" || key == "" {
		return "", errors.New("请填写豆包语音 API Key 和资源 ID")
	}
	return recognizeDoubao(ctx, doubaoASREndpoint, doubaoASRResource, key, source)
}

func doubaoHeaders(key, resource string) (http.Header, error) {
	var requestID [16]byte
	if _, err := rand.Read(requestID[:]); err != nil {
		return nil, err
	}
	requestID[6] = (requestID[6] & 0x0f) | 0x40
	requestID[8] = (requestID[8] & 0x3f) | 0x80
	headers := http.Header{}
	headers.Set("X-Api-Key", key)
	headers.Set("X-Api-Resource-Id", resource)
	headers.Set("X-Api-Request-Id", fmt.Sprintf("%x-%x-%x-%x-%x", requestID[:4], requestID[4:6], requestID[6:8], requestID[8:10], requestID[10:]))
	return headers, nil
}

func recognizeDoubao(parent context.Context, endpoint, resource, key string, source doubaoAudioSource) (string, error) {
	ctx, cancel := context.WithTimeout(parent, 25*time.Second)
	defer cancel()
	headers, err := doubaoHeaders(key, resource)
	if err != nil {
		return "", err
	}
	chunks := make(chan []byte, 32)
	captured := make(chan error, 1)
	captureDone := make(chan struct{})
	defer func() {
		cancel()
		<-captureDone
	}()
	go func() {
		defer close(captureDone)
		defer close(chunks)
		captured <- source(ctx, func(pcm []byte) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			select {
			case chunks <- bytes.Clone(pcm):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	conn, response, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{
		HTTPHeader: headers,
		HTTPClient: &http.Client{
			Timeout:       5 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	})
	if err != nil {
		if response != nil {
			return "", doubaoHandshakeError(response)
		}
		return "", errors.New("豆包语音连接失败或超时")
	}
	defer conn.CloseNow()
	conn.SetReadLimit(doubaoFrameLimit)
	config := []byte(`{"audio":{"format":"pcm","codec":"raw","rate":16000,"bits":16,"channel":1},"request":{"model_name":"bigmodel","result_type":"full"}}`)
	encoder := newDoubaoEncoder()
	if err := conn.Write(ctx, websocket.MessageBinary, encoder.frame(0x10, 0x10, config)); err != nil {
		return "", errors.New("无法发送豆包语音配置或请求已超时")
	}
	sent := make(chan error, 1)
	go func() {
		buffer := make([]byte, 0, doubaoPacketBytes)
		write := func(kind byte, audio []byte) error {
			if err := conn.Write(ctx, websocket.MessageBinary, encoder.frame(kind, 0, audio)); err != nil {
				return errors.New("无法发送豆包语音数据或请求已超时")
			}
			return nil
		}
		for chunk := range chunks {
			buffer = append(buffer, chunk...)
			for len(buffer) > doubaoPacketBytes {
				if err := write(0x20, buffer[:doubaoPacketBytes]); err != nil {
					sent <- err
					cancel()
					return
				}
				buffer = buffer[doubaoPacketBytes:]
			}
		}
		err := <-captured
		if err == nil {
			if len(buffer) == 0 {
				err = errors.New("没有录到可识别的语音")
			} else {
				err = write(0x22, buffer)
			}
		}
		sent <- err
		if err != nil {
			cancel()
		}
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
			select {
			case err := <-sent:
				if err != nil {
					return "", err
				}
				return text, nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
	}
}

var doubaoLogID = regexp.MustCompile(`^[a-zA-Z0-9_-]{8,80}$`)

func doubaoHandshakeError(response *http.Response) error {
	message := fmt.Sprintf("豆包语音握手失败（HTTP %d）", response.StatusCode)
	if response.Body != nil {
		defer response.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
		detail := strings.ToLower(string(body))
		switch {
		case strings.Contains(detail, "grant") || strings.Contains(detail, "permission denied"):
			message += "，当前项目未获所选识别资源的调用权限"
		case strings.Contains(detail, "invalid api key") || response.StatusCode == http.StatusUnauthorized:
			message += "，请确认豆包语音 API Key 仍然有效"
		case response.StatusCode == http.StatusForbidden:
			message += "，请核对 API Key 所属项目是否已开通流式语音识别 2.0 小时版"
		}
	}
	if id := response.Header.Get("X-Tt-Logid"); doubaoLogID.MatchString(id) {
		message += "（火山请求日志 ID " + id + "）"
	}
	return errors.New(message)
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
