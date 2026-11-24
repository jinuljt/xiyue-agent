package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"unicode"
)

const deepseekTTSEndpoint = searchOrigin + "/v1/tools/tts"

var deepseekTTSClient = &http.Client{Transport: networkClient.Transport, CheckRedirect: networkClient.CheckRedirect}

func deepseekCharacters(text string) int64 {
	var count int64
	for _, r := range strings.TrimSpace(text) {
		count++
		if unicode.Is(unicode.Han, r) {
			count++
		}
	}
	return count
}

func deepseekSpeech(ctx context.Context, endpoint, voice, key, text string, play pcmPlayer) error {
	text = strings.TrimSpace(text)
	if text == "" || len(text) > 4096 {
		return errors.New("播报文字为空或过长")
	}
	body, err := json.Marshal(map[string]string{"text": text, "voice_id": voice, "format": "pcm"})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return errors.New("DeepSeek 语音合成地址无效")
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := deepseekTTSClient.Do(req)
	if err != nil {
		return errors.New("DeepSeek 语音合成连接失败或超时")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("DeepSeek 语音合成失败（HTTP %d）", resp.StatusCode)
	}
	format, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if format == "application/json" || strings.HasPrefix(format, "text/") || format == "audio/wav" || format == "audio/mpeg" || format == "audio/ogg" {
		return errors.New("DeepSeek 语音合成未返回 PCM 音频")
	}
	const maxAudio = 12 << 20
	reader := &io.LimitedReader{R: resp.Body, N: maxAudio + 1}
	if err := play(ctx, reader); err != nil {
		return err
	}
	n := maxAudio + 1 - reader.N
	if n == 0 || n > maxAudio || n%2 != 0 || (resp.ContentLength >= 0 && n != resp.ContentLength) {
		return errors.New("DeepSeek 语音合成音频为空、不完整或过长")
	}
	return nil
}
