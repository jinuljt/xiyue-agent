package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDeviceAudioStopAndTranscribe(t *testing.T) {
	asr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/audio/transcriptions" {
			t.Errorf("unexpected ASR path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, `{"text":"模拟转写"}`)
	}))
	defer asr.Close()

	p, err := openPlatform(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	input := settingsInput(p.snapshot().Settings)
	input.ASR = ServiceInput{Provider: "sensevoice", Endpoint: asr.URL}
	if err = p.saveSettings(input); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	v := &voiceRuntime{playPCM: func(ctx context.Context, audio io.Reader) error {
		clip, err := io.ReadAll(audio)
		if err != nil || !bytes.Equal(clip, wakeTone) {
			t.Error("incorrect built-in cue", err)
		}
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}}
	h := (&api{platform: p, speechProbe: v}).routes()
	request := func(ctx context.Context, action, contentType string, body []byte) *httptest.ResponseRecorder {
		r := httptest.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1/api/device/audio/"+action, bytes.NewReader(body))
		r.Header.Set("Content-Type", contentType)
		r.Header.Set("X-Xiyue-Request", "console")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	jsonRequest := func(action, body string) *httptest.ResponseRecorder {
		return request(ctx, action, "application/json", []byte(body))
	}
	if w := jsonRequest("tone", `{"tone":"unknown"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown tone accepted: %d %s", w.Code, w.Body)
	}
	played := make(chan *httptest.ResponseRecorder, 1)
	go func() { played <- jsonRequest("tone", `{"tone":"cue"}`) }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("playback did not start")
	}
	if w := jsonRequest("speak", `{"text":"不抢占输出"}`); w.Code != http.StatusConflict {
		t.Fatalf("second playback did not detect occupancy: %d %s", w.Code, w.Body)
	}
	if w := request(ctx, "transcribe", "audio/wav", fixtureWAV()); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "模拟转写") {
		t.Fatalf("uploaded clip failed: %d %s", w.Code, w.Body)
	}
	if w := jsonRequest("stop", `{}`); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"cancel_requested":true`) {
		t.Fatalf("stop failed: %d %s", w.Code, w.Body)
	}
	select {
	case w := <-played:
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "interrupted") {
			t.Fatalf("playback did not report interruption: %d %s", w.Code, w.Body)
		}
	case <-ctx.Done():
		t.Fatal("playback did not stop")
	}
}

func TestConversationPlaysQuestionBeforeListeningAndBindsConfirmation(t *testing.T) {
	const question = "准备好开始计时了吗？"
	transcripts := []string{"继续下一步", "我还没准备好", "现在可以开始了", "退出吧"}
	var transcript atomic.Pointer[string]
	heardQuestion := false
	asrAndModel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/audio/transcriptions":
			_ = json.NewEncoder(w).Encode(map[string]string{"text": *transcript.Load()})
		case "/chat/completions":
			var request struct {
				Messages []voiceMessage `json:"messages"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
				return
			}
			messages := request.Messages
			if len(messages) < 2 || messages[len(messages)-2] != (voiceMessage{"assistant", question}) {
				t.Error("model did not receive the exact delivered question")
			}
			plan := `{"action":"speak","speech":"好，准备好了再告诉我。"}`
			if messages[len(messages)-1].Content == "现在可以开始了" {
				plan = `{"action":"next_step"}`
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": voiceMessage{Content: plan}}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer asrAndModel.Close()

	p, err := openPlatform(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	input := settingsInput(p.snapshot().Settings)
	input.ASR = ServiceInput{Provider: "sensevoice", Endpoint: asrAndModel.URL}
	input.LLM = ServiceInput{Provider: "custom", Endpoint: asrAndModel.URL, Model: "fixture-model"}
	if err = p.saveSettings(input); err != nil {
		t.Fatal(err)
	}
	p.queueSpeech = true
	now := time.Now().UTC()
	manifest := Manifest{ID: "audio-fixture", Name: "确认流程", Version: "1", Steps: []Step{
		{Kind: "confirm", Text: "请在准备后继续。"}, {Kind: "tone", Text: "alarm"},
		{Kind: "ask", Text: question}, {Kind: "timer", Text: "开始计时。", Seconds: 1800},
	}}
	if err = validateManifest(manifest); err != nil {
		t.Fatal(err)
	}
	if err = p.change(func(s *State) error {
		s.Abilities[manifest.ID] = Ability{Manifest: manifest, Enabled: true}
		_, err := startTask(s, manifest.ID, nil, now)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	capture := func(_ context.Context, _ func([]byte) error) ([]byte, error) {
		if len(transcripts) == 0 {
			return nil, errors.New("unexpected follow-up capture")
		}
		lastAnswer := transcripts[0]
		transcripts = transcripts[1:]
		transcript.Store(&lastAnswer)
		if lastAnswer == "我还没准备好" && !heardQuestion {
			t.Error("opened the microphone before finishing the question")
		}
		if lastAnswer == "现在可以开始了" && p.snapshot().Tasks[0].Status != "waiting" {
			t.Error("a decline advanced the task")
		}
		return fixtureWAV(), nil
	}
	var playback []string
	v := &voiceRuntime{follow: capture, speak: func(_ context.Context, text string) error {
		playback = append(playback, text)
		if text == question {
			heardQuestion = true
		}
		return nil
	}, playPCM: func(_ context.Context, audio io.Reader) error {
		if data, _ := io.ReadAll(audio); bytes.Equal(data, alarmTone) {
			playback = append(playback, "tone")
		}
		return nil
	}}
	if err = p.deliverOne(context.Background(), v, now); err != nil {
		t.Fatal(err)
	}
	p.runDeviceWake(context.Background(), v, capture, nil)
	want := []string{"请在准备后继续。", "继续。", "tone", question, "好，准备好了再告诉我。", "继续。", "开始计时。"}
	if task := p.snapshot().Tasks[0]; task.Status != "timer" || task.DueAt == nil || len(transcripts) != 0 || !reflect.DeepEqual(playback, want) {
		t.Fatalf("task=%+v remaining=%q playback=%q", task, transcripts, playback)
	}

	manifest.ID = "background-audio-fixture"
	manifest.Steps = manifest.Steps[2:]
	var taskID string
	now = time.Now().UTC()
	if err := p.change(func(s *State) error {
		s.Abilities[manifest.ID] = Ability{Manifest: manifest, Enabled: true}
		var err error
		taskID, err = startTask(s, manifest.ID, nil, now)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.deliverOne(context.Background(), v, now); err != nil {
		t.Fatal(err)
	}
	oldQuestion := activeQuestion(p.snapshot(), now)
	if oldQuestion == nil || len(playback) != len(want)+1 || playback[len(want)] != question {
		t.Fatal("background flow did not deliver a question and wait for a wake")
	}
	if err := p.change(func(s *State) error { return taskAction(s, taskID, "repeat", now) }); err != nil {
		t.Fatal(err)
	}
	reply, err := p.confirmQuestion(*oldQuestion, now)
	if err != nil || reply.taskID != "" || p.snapshot().Tasks[1].Status != "waiting" {
		t.Fatal("confirmation of a replaced question advanced the task", err)
	}
}
