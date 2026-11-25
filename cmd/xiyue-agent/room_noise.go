package main

import (
	"encoding/binary"
	"math"
)

// roomNoiseReducer tracks a background estimate for three broad bands at 24 kHz.
// The residual floor keeps quiet events audible and avoids a hard noise gate.
type roomNoiseReducer struct {
	low, upper       float64
	floor, min, gain [3]float64
	window           int
	estimated        bool
}

func (r *roomNoiseReducer) reduce(pcm []byte) (power float64, peak int, signal bool) {
	count := len(pcm) / 2
	if count == 0 {
		return 0, 0, false
	}
	var bands [roomSamplesPerFrame][3]float64
	var energy [3]float64
	for i := range count {
		x := float64(int16(binary.LittleEndian.Uint16(pcm[i*2:])))
		// Parallel one-pole low-passes near 150 Hz and 3.2 kHz.
		r.low += 0.039 * (x - r.low)
		r.upper += 0.567 * (x - r.upper)
		bands[i] = [3]float64{r.low, r.upper - r.low, x - r.upper}
		for b, weight := range bands[i] {
			energy[b] += weight * weight
		}
	}
	previous := r.gain
	for b := range energy {
		p := energy[b] / float64(count)
		if r.window == 0 || p < r.min[b] {
			r.min[b] = p
		}
		if !r.estimated {
			previous[b], r.gain[b] = 1, 1
			continue
		}
		floor := r.floor[b]
		signal = signal || p > max(26*26, 2.5*floor)
		target := math.Sqrt(max(0.16, 1-1.25*floor/max(1, p)))
		// Open promptly for speech and close gently across pauses.
		rate := 0.18
		if target > r.gain[b] {
			rate = 0.7
		}
		r.gain[b] += rate * (target - r.gain[b])
	}
	r.window++
	if r.window == 25 {
		for b, p := range r.min {
			if r.estimated {
				// Follow a falling floor quickly and a rising one slowly.
				p = min(p, max(16, 1.2*r.floor[b]))
			}
			r.floor[b] = p
		}
		r.estimated = true
		r.window = 0
	}
	for i := range count {
		mix := 0.0
		progress := float64(i+1) / float64(count)
		for b, sample := range bands[i] {
			mix += sample * (previous[b] + progress*(r.gain[b]-previous[b]))
		}
		out := int(math.Round(max(-32768, min(32767, mix))))
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(int16(out)))
		power += float64(out * out)
		peak = max(peak, out, -out)
	}
	return power / float64(count), peak, signal
}
