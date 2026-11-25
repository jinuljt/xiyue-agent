package main

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
)

type roomWAV struct {
	pcm            io.Reader
	rate, channels int
}

var errRoomWAV = errors.New("需要 8–48 kHz、单声道或双声道的 16 位 PCM WAV 音频")

func openRoomWAV(src io.Reader) (roomWAV, error) {
	var start [12]byte
	if _, err := io.ReadFull(src, start[:]); err != nil || string(start[:4]) != "RIFF" || string(start[8:]) != "WAVE" {
		return roomWAV{}, errRoomWAV
	}
	var wav roomWAV
	remaining := int64(64 << 10)
	for remaining >= 8 {
		var chunk [8]byte
		if _, err := io.ReadFull(src, chunk[:]); err != nil {
			break
		}
		remaining -= 8
		size := int64(binary.LittleEndian.Uint32(chunk[4:]))
		if string(chunk[:4]) == "data" {
			if wav.rate == 0 {
				break
			}
			// Live WAV encoders can use zero or a large placeholder size.
			wav.pcm = src
			if size != 0 {
				wav.pcm = io.LimitReader(src, size)
			}
			return wav, nil
		}
		padded := size + size%2
		if padded > remaining {
			break
		}
		if string(chunk[:4]) == "fmt " {
			var format [16]byte
			if size < int64(len(format)) || wav.rate != 0 {
				break
			}
			if _, err := io.ReadFull(src, format[:]); err != nil {
				break
			}
			wav.channels = int(binary.LittleEndian.Uint16(format[2:]))
			wav.rate = int(binary.LittleEndian.Uint32(format[4:]))
			if (wav.channels != 1 && wav.channels != 2) || wav.rate < 8000 || wav.rate > 48000 ||
				binary.LittleEndian.Uint16(format[:]) != 1 || binary.LittleEndian.Uint16(format[14:]) != 16 ||
				int(binary.LittleEndian.Uint16(format[12:])) != wav.channels*2 {
				break
			}
			padded -= int64(len(format))
			remaining -= int64(len(format))
		}
		if _, err := io.CopyN(io.Discard, src, padded); err != nil {
			break
		}
		remaining -= padded
	}
	return roomWAV{}, errRoomWAV
}

const roomSamplesPerFrame = 480 // 20 ms at the existing 24 kHz player rate.

type roomFrame struct {
	data [roomSamplesPerFrame * 2]byte
	size int
}

func (wav roomWAV) stream(ctx context.Context, c roomAudioConfig, frames chan roomFrame, onAudio func(int)) error {
	defer close(frames)
	block := wav.channels * 2
	input := make([]byte, ((wav.rate+49)/50)*block)
	remainder := 0
	gain, levelPower := 1.0, 0.0
	enhanceVoice := c.enhancesVoice()
	autoGain := c.AutoGain || enhanceVoice
	var noise roomNoiseReducer
	for ctx.Err() == nil {
		expected := (wav.rate + remainder) / 50
		remainder = (wav.rate + remainder) % 50
		n, err := io.ReadFull(wav.pcm, input[:expected*block])
		if samples := n / block; samples > 0 {
			var frame roomFrame
			output := roomSamplesPerFrame * samples / expected
			frame.size = output * 2
			var power float64
			peak := 0
			for i := range output {
				offset := i * samples / output * block
				sample := int(int16(binary.LittleEndian.Uint16(input[offset:])))
				if wav.channels == 2 {
					sample = (sample + int(int16(binary.LittleEndian.Uint16(input[offset+2:])))) / 2
				}
				power += float64(sample * sample)
				peak = max(peak, sample, -sample)
				binary.LittleEndian.PutUint16(frame.data[i*2:], uint16(int16(sample)))
			}
			if output > 0 {
				power /= float64(output)
			}
			levelPower += 0.08 * (power - levelPower)
			level := int(math.Round(10 * math.Log10(max(1.0, levelPower)/(32768.0*32768.0))))
			signal := true
			if enhanceVoice {
				power, peak, signal = noise.reduce(frame.data[:frame.size])
			}
			volume := float64(c.Volume) / 100
			if autoGain {
				// Aim for -18 dBFS RMS, cap boost at +24 dB and ceiling at -6 dBFS.
				// Near-silence relaxes gain instead of pulling the noise floor up.
				target := 1.0
				if signal && power >= 26*26 {
					target = min(16, 4125/math.Sqrt(power))
				}
				rate := 0.04
				if target < gain {
					rate = 0.25
				}
				gain += rate * (target - gain)
				volume = min(gain*volume, 16384/float64(max(1, peak)))
			}
			for i := range output {
				sample := int16(binary.LittleEndian.Uint16(frame.data[i*2:]))
				adjusted := int(float64(sample) * volume)
				if !autoGain {
					adjusted = int(sample) * c.Volume / 100
				}
				binary.LittleEndian.PutUint16(frame.data[i*2:], uint16(int16(adjusted)))
			}
			onAudio(level)
			select {
			case frames <- frame:
				// Keep the newest frame when playback cannot keep up.
			default:
				select {
				case <-frames:
				default:
				}
				select {
				case frames <- frame:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}
		if err != nil {
			return err
		}
	}
	return ctx.Err()
}

type roomPCMReader struct {
	ctx     context.Context
	frames  <-chan roomFrame
	pending roomFrame
	offset  int
	onPlay  func()
}

func (r *roomPCMReader) Read(buf []byte) (int, error) {
	if r.ctx.Err() != nil {
		return 0, r.ctx.Err()
	}
	if r.offset == r.pending.size {
		select {
		case <-r.ctx.Done():
			return 0, r.ctx.Err()
		case frame, ok := <-r.frames:
			if !ok {
				return 0, io.EOF
			}
			r.pending, r.offset = frame, 0
			r.onPlay()
		}
	}
	n := copy(buf, r.pending.data[r.offset:r.pending.size])
	r.offset += n
	return n, nil
}
