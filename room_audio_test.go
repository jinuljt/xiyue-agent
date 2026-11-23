package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRoomAudioLifecycleAndPriority(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	header := fixtureWAV()[:44]
	binary.LittleEndian.PutUint32(header[4:8], 0)
	binary.LittleEndian.PutUint32(header[40:44], 0)
	// A quiet 16 kHz mono signal. It should still reach the player.
	quiet := bytes.Repeat([]byte{100, 0}, 320)
	var connections atomic.Int32
	const username, password = "demo-user", "demo-camera-password"
	drop := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/audio.wav" {
			http.NotFound(w, r)
			return
		}
		user, pass, ok := r.BasicAuth()
		if !ok || user != username || pass != password {
			w.Header().Set("WWW-Authenticate", `Basic realm="camera"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		number := connections.Add(1)
		_, _ = w.Write(header)
		_, _ = w.Write(quiet)
		w.(http.Flusher).Flush()
		if number == 2 {
			select {
			case <-drop:
			case <-r.Context().Done():
			}
			return
		}
		<-r.Context().Done()
	}))
	defer server.Close()

	p, err := openPlatform(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var playing atomic.Int32
	played := make(chan int16, 8)
	var v *voiceRuntime
	var alarms atomic.Int32
	v = &voiceRuntime{playPCM: func(ctx context.Context, r io.Reader) error {
		if n := playing.Add(1); n != 1 {
			t.Error("two players used the output simultaneously")
		}
		defer playing.Add(-1)
		var packet [960]byte
		if _, err := io.ReadFull(r, packet[:]); err != nil {
			return err
		}
		if bytes.Equal(packet[:], alarmTone[:len(packet)]) {
			alarms.Add(1)
			v.stopAlarm()
			return ctx.Err()
		}
		played <- int16(binary.LittleEndian.Uint16(packet[:]))
		_, err := io.Copy(io.Discard, r)
		return err
	}}
	room := newRoomAudio(ctx, v)
	// Honor production validation and use the test source without touching the LAN.
	room.client.Transport.(*http.Transport).DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	h := (&api{platform: p, room: room}).routes()
	secured := (&api{platform: p, room: room, admin: &adminGate{host: "127.0.0.1:18765"}}).routes()
	unauthenticated := httptest.NewRecorder()
	secured.ServeHTTP(unauthenticated, httptest.NewRequest("GET", "https://127.0.0.1:18765/api/room-audio", nil))
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatal("room audio configuration is visible without a management session")
	}
	request := func(method, path, body string, status int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "http://127.0.0.1/api/room-audio"+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Xiyue-Request", "console")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("%s %s: got %d %s want %d", method, path, w.Code, w.Body, status)
		}
		return w
	}
	awaitPlayback := func() {
		t.Helper()
		select {
		case sample := <-played:
			if sample != 70 {
				t.Fatalf("quiet sample was clipped or lost: %d", sample)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no audio reached the player")
		}
	}
	fixtureAddress := "http://" + netip.AddrFrom4([4]byte{10, 0, 0, 1}).String() + "/audio.wav"
	source := `{"source":"` + fixtureAddress + `","volume":70}`
	credentials := source[:len(source)-1] + `,"username":"` + username + `","password":"` + password + `"}`
	keepPassword := source[:len(source)-1] + `,"username":"` + username + `"}`
	request("PUT", "", `{"source":"http://127.0.0.1/audio.wav","volume":70}`, 400)
	request("PUT", "", `{"source":"http://169.254.169.254/audio.wav","volume":70}`, 400)
	denied := request("POST", "/verify", source, 400)
	if !strings.Contains(denied.Body.String(), "核对用户名和密码") {
		t.Fatal("missing authentication error")
	}
	probe := request("POST", "/verify", credentials, 200)
	if !strings.Contains(probe.Body.String(), `"sample_rate":16000`) {
		t.Fatal(probe.Body.String())
	}
	if saved := request("PUT", "", credentials, 200); strings.Contains(saved.Body.String(), password) {
		t.Fatal("password echoed by API")
	}
	request("PUT", "", strings.Replace(keepPassword, "/audio.wav", "/new/audio.wav", 1), 400)
	request("PUT", "", keepPassword, 200)
	saved, err := os.ReadFile(p.path)
	if err != nil || bytes.Contains(saved, []byte(password)) {
		t.Fatal("password not encrypted on disk")
	}
	request("POST", "/start", "{}", 200)
	// Always stop before closing the test HTTP server, including on assertions.
	defer request("POST", "/stop", "{}", 200)
	awaitPlayback()
	current := request("GET", "", "", 200)
	if !strings.Contains(current.Body.String(), `"stage":"playing"`) || !strings.Contains(current.Body.String(), `"last_audio_at"`) ||
		!strings.Contains(current.Body.String(), `"password_configured":true`) || strings.Contains(current.Body.String(), password) {
		t.Fatal(current.Body.String())
	}
	request("PUT", "", source, 409)
	close(drop)
	awaitPlayback()

	foregroundCtx, foregroundCancel := context.WithTimeout(ctx, 3*time.Second)
	defer foregroundCancel()
	_, finish, err := v.startPlayback(foregroundCtx)
	if err != nil {
		t.Fatal(err)
	}
	if playing.Load() != 0 {
		t.Error("foreground playback did not wait for the room player to stop")
	}
	finish()
	awaitPlayback()
	p.queueSpeech = true
	now := time.Now().UTC()
	if err := p.change(func(s *State) error {
		emit(s, &Task{ID: "room-audio-alarm", AbilityID: quickTimer.ID}, "speech", "计时结束。", now)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.deliverOne(foregroundCtx, v, now); err != nil || alarms.Load() != 1 {
		t.Fatalf("alarm did not interrupt the room player: %v", err)
	}
	awaitPlayback()
	stopped := request("POST", "/stop", "{}", 200)
	var status roomAudioStatus
	if err = json.Unmarshal(stopped.Body.Bytes(), &status); err != nil || status.Running || status.Stage != "stopped" || playing.Load() != 0 {
		t.Fatalf("playback did not stop: %s", stopped.Body)
	}
	reopened, err := openPlatform(p.path)
	if err != nil {
		t.Fatal(err)
	}
	if config := reopened.roomConfig(); config.Source != fixtureAddress || config.Volume != 70 {
		t.Fatalf("config was not restored: %+v", config)
	}
	draft := strings.Replace(keepPassword, `"volume":70`, `"volume":35,"name":"temporary"`, 1)
	started := request("POST", "/start", `{"config":`+draft+`}`, 200)
	if !strings.Contains(started.Body.String(), `"name":"temporary"`) {
		t.Fatal("draft name not in playback status")
	}
	select {
	case sample := <-played:
		if sample != 35 {
			t.Fatalf("draft volume produced sample %d", sample)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("draft did not start playback")
	}
	request("POST", "/stop", "{}", 200)
	if config := p.roomConfig(); config.Name != "" || config.Volume != 70 {
		t.Fatal("starting playback changed saved configuration")
	}
}
