package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
)

const (
	doubaoTTSEndpoint  = "https://openspeech.bytedance.com/api/v3/tts/unidirectional"
	doubaoTTSResource  = "seed-tts-2.0"
	doubaoDefaultVoice = "zh_female_vv_uranus_bigtts"
)

type pcmPlayer func(context.Context, io.Reader) error

func playALSA(ctx context.Context, device string, audio io.Reader) error {
	cmd := exec.CommandContext(ctx, "aplay", "-q", "-D", device, "-t", "raw", "-f", "S16_LE", "-r", "24000", "-c", "1")
	cmd.Stdin = audio
	if err := cmd.Run(); err != nil {
		return errors.New("音箱 PCM 播放失败或超时")
	}
	return nil
}

func ttsReady(v *voiceRuntime, s State) bool {
	cfg := s.Settings.TTS
	return v != nil && (cfg.Provider == "system" ||
		(cfg.Provider == "doubao" && v.playPCM != nil && cfg.Model != "" && cfg.Endpoint == "" && cfg.KeyConfigured))
}

func (p *Platform) playConfiguredSpeech(ctx context.Context, v *voiceRuntime, s State, text string) error {
	if !ttsReady(v, s) {
		return errors.New("请配置系统语音或可用的豆包语音合成服务")
	}
	if s.Settings.TTS.Provider == "system" {
		return v.speak(ctx, text)
	}
	key, err := p.unseal("service:tts", s.Sealed["service:tts"])
	if err != nil {
		return err
	}
	return doubaoSpeech(ctx, doubaoTTSEndpoint, s.Settings.TTS.Model, key, text, v.playPCM)
}

func doubaoSpeech(ctx context.Context, endpoint, speaker, key, text string, play pcmPlayer) error {
	text = strings.TrimSpace(text)
	if len(text) == 0 || len(text) > 4096 {
		return errors.New("播报文字为空或过长")
	}
	body, err := json.Marshal(map[string]any{"req_params": map[string]any{
		"text": text, "speaker": speaker, "audio_params": map[string]any{"format": "pcm", "sample_rate": 24000},
	}})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return errors.New("豆包语音合成地址无效")
	}
	req.Header, err = doubaoHeaders(key, doubaoTTSResource)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("豆包语音合成连接失败或超时")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("豆包语音合成请求失败（HTTP %d）", resp.StatusCode)
	}
	const maxStream = 12 << 20
	limited := &io.LimitedReader{R: resp.Body, N: maxStream + 1}
	decoder := json.NewDecoder(limited)
	var pipe *io.PipeWriter
	var played chan error
	var total int
	var result error
	for {
		var chunk struct {
			Code *int
			Data []byte
		}
		if err = decoder.Decode(&chunk); err != nil {
			if err != io.EOF || limited.N == 0 {
				result = errors.New("豆包语音合成音频不完整或超出限制")
			}
			break
		}
		if chunk.Code == nil {
			result = errors.New("豆包语音合成响应格式无效")
			break
		}
		if *chunk.Code != 0 {
			result = fmt.Errorf("豆包语音合成失败（错误码 %d）", *chunk.Code)
			break
		}
		if len(chunk.Data) == 0 {
			continue
		}
		total += len(chunk.Data)
		if pipe == nil {
			var reader *io.PipeReader
			reader, pipe = io.Pipe()
			played = make(chan error, 1)
			go func() {
				err := play(ctx, reader)
				reader.CloseWithError(err)
				played <- err
			}()
		}
		if _, err = pipe.Write(chunk.Data); err != nil {
			result = errors.New("音箱未完成音频播放")
			break
		}
	}
	if pipe == nil {
		if result != nil {
			return result
		}
		return errors.New("豆包语音合成未返回音频")
	}
	if total%2 != 0 && result == nil {
		result = errors.New("豆包语音合成音频不完整")
	}
	pipe.CloseWithError(result)
	playErr := <-played
	if result != nil {
		return result
	}
	return playErr
}
