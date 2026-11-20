package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	captureFrameSamples = 320 // 20 milliseconds at 16 kHz.
	captureLimitFrames  = 600
)

func captureOH2P(ctx context.Context, arecord string, threshold float64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 14*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, arecord, "--quiet", "-D", "noop", "-t", "raw", "-f", "S32_LE", "-r", "16000", "-c", "4", "-d", "12")
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		return nil, errors.New("无法启动音箱录音")
	}
	stopReading := context.AfterFunc(ctx, func() { _ = out.Close() })
	wav, captureErr := readOH2PClip(out, threshold)
	waitErr := cmd.Process.Kill()
	_ = cmd.Wait() // Early end-of-speech intentionally terminates arecord.
	stopReading()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if captureErr != nil {
		return nil, captureErr
	}
	if waitErr != nil && !errors.Is(waitErr, os.ErrProcessDone) {
		return nil, errors.New("音箱录音未能正常停止")
	}
	return wav, nil
}

func readOH2PClip(src io.Reader, threshold float64) ([]byte, error) {
	var frame [captureFrameSamples * 4 * 4]byte
	// Keep up to 200 ms before speech onset, and use only the first microphone.
	audio := make([]int32, 0, captureLimitFrames*captureFrameSamples)
	loud, quiet := 0, 0
	for n := 0; n < captureLimitFrames; n++ {
		if _, err := io.ReadFull(src, frame[:]); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, errors.New("音箱录音帧不完整")
		}
		if loud == 0 && len(audio) == captureFrameSamples*10 {
			copy(audio, audio[captureFrameSamples:])
			audio = audio[:len(audio)-captureFrameSamples]
		}
		var squares float64
		for i := 0; i < len(frame); i += 16 {
			sample := int32(binary.LittleEndian.Uint32(frame[i:]))
			squares += float64(sample) * float64(sample)
			audio = append(audio, sample)
		}
		if squares >= threshold*threshold*captureFrameSamples {
			loud++
			quiet = 0
		} else if loud > 0 {
			quiet++
		}
		if loud >= 8 && quiet >= 28 {
			break
		}
		if loud < 8 && quiet >= 28 {
			loud, quiet = 0, 0
			audio = audio[:0]
		}
	}
	if loud < 8 {
		return nil, errors.New("未检测到足够的说话声")
	}
	var peak float64
	for _, sample := range audio {
		peak = math.Max(peak, math.Abs(float64(sample)))
	}
	if peak == 0 {
		return nil, errors.New("音箱录音为空")
	}
	data := make([]byte, 44+len(audio)*2)
	copy(data[:4], "RIFF")
	binary.LittleEndian.PutUint32(data[4:], uint32(len(data)-8))
	copy(data[8:16], "WAVEfmt ")
	binary.LittleEndian.PutUint32(data[16:], 16)
	binary.LittleEndian.PutUint16(data[20:], 1)
	binary.LittleEndian.PutUint16(data[22:], 1)
	binary.LittleEndian.PutUint32(data[24:], 16000)
	binary.LittleEndian.PutUint32(data[28:], 32000)
	binary.LittleEndian.PutUint16(data[32:], 2)
	binary.LittleEndian.PutUint16(data[34:], 16)
	copy(data[36:40], "data")
	binary.LittleEndian.PutUint32(data[40:], uint32(len(data)-44))
	for i, sample := range audio {
		binary.LittleEndian.PutUint16(data[44+i*2:], uint16(int16(math.Round(float64(sample)*24000/peak))))
	}
	return data, nil
}

func oh2pListeningLED(on bool) {
	program := "/bin/shut_led"
	if on {
		program = "/bin/show_led"
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = exec.CommandContext(ctx, program, "3").Run()
}

func openWakeSocket(path string) (*net.UnixListener, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("唤醒套接字必须使用绝对路径")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("唤醒套接字应位于权限为 0700 的目录")
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if err = os.Chmod(path, 0600); err != nil {
		_ = listener.Close()
		return nil, err
	}
	return listener, nil
}

func (p *Platform) serveWakeSocket(ctx context.Context, listener net.Listener, v *voiceRuntime, capture func(context.Context) ([]byte, error), setLED func(bool)) {
	stop := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stop()
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		_ = conn.SetDeadline(time.Now().Add(time.Second))
		packet, err := bufio.NewReader(io.LimitReader(conn, 128)).ReadString('\n')
		if err == nil {
			switch strings.TrimSuffix(packet, "\n") {
			case "cancel":
				v.stopTurn()
			case "wake " + p.snapshot().Settings.WakeWord:
				// runVoiceTurn discards competing wake events while any turn owns the microphone.
				go func() {
					result, err := p.runVoiceTurn(ctx, v, capture, setLED)
					if err != nil || result.Stage != "completed" {
						return
					}
					_ = p.change(func(s *State) error {
						if s.Settings.Revision == result.Revision {
							s.Settings.DeviceSetupComplete = true
						}
						return nil
					})
				}()
			}
		}
		_ = conn.Close()
	}
}

func sendWakePacket(path, packet string) error {
	socket, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		return err
	}
	defer socket.Close()
	_ = socket.SetDeadline(time.Now().Add(time.Second))
	_, err = io.WriteString(socket, packet+"\n")
	return err
}
