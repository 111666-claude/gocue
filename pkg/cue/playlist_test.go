package cue

import (
	"encoding/binary"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/suite"
)

type PlaylistSuite struct {
	suite.Suite
}

func TestPlaylistSuite(t *testing.T) {
	suite.Run(t, &PlaylistSuite{})
}

const testSampleRate = 44100

// writeWAV renders samples with fn (time in seconds) as 16-bit mono PCM.
func (s *PlaylistSuite) writeWAV(name string, seconds float64, fn func(t float64) float64) string {
	s.T().Helper()
	path := filepath.Join(s.T().TempDir(), name)
	n := int(math.Round(seconds * testSampleRate))
	buf := make([]byte, 44+2*n)

	copy(buf[0:4], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:], uint32(36+2*n))
	copy(buf[8:12], "WAVE")
	copy(buf[12:16], "fmt ")
	binary.LittleEndian.PutUint32(buf[16:], 16)
	binary.LittleEndian.PutUint16(buf[20:], 1) // PCM
	binary.LittleEndian.PutUint16(buf[22:], 1) // mono
	binary.LittleEndian.PutUint32(buf[24:], testSampleRate)
	binary.LittleEndian.PutUint32(buf[28:], testSampleRate*2)
	binary.LittleEndian.PutUint16(buf[32:], 2)
	binary.LittleEndian.PutUint16(buf[34:], 16)
	copy(buf[36:40], "data")
	binary.LittleEndian.PutUint32(buf[40:], uint32(2*n))
	for i := 0; i < n; i++ {
		v := math.Max(-1, math.Min(1, fn(float64(i)/testSampleRate)))
		binary.LittleEndian.PutUint16(buf[44+2*i:], uint16(int16(math.Round(v*32767))))
	}
	s.Require().NoError(os.WriteFile(path, buf, 0o644))
	return path
}

// ladderWAV is a steady 440 Hz tone at dbfs, the fixed loudness steps the
// handover and gain tests compare against.
func (s *PlaylistSuite) ladderWAV(name string, seconds, dbfs float64) string {
	amp := math.Pow(10, dbfs/20)
	return s.writeWAV(name, seconds, func(t float64) float64 {
		return amp * math.Sin(2*math.Pi*440*t)
	})
}

// silentTailWAV is a tone that fades linearly into digital silence, so its
// cue-out lands after its overlay point and a real overlap exists.
func (s *PlaylistSuite) silentTailWAV(name string, toneSeconds, fadeSeconds, silenceSeconds float64) string {
	amp := math.Pow(10, -3.0/20)
	return s.writeWAV(name, toneSeconds+fadeSeconds+silenceSeconds, func(t float64) float64 {
		v := amp * math.Sin(2*math.Pi*440*t)
		switch {
		case t < toneSeconds:
			return v
		case t < toneSeconds+fadeSeconds:
			return v * (1 - (t-toneSeconds)/fadeSeconds)
		default:
			return 0
		}
	})
}

// peakyWAV is sparse full-scale impulses: a high true peak with a low
// integrated loudness, so the peak ceiling cannot be met at the loudness target.
func (s *PlaylistSuite) peakyWAV(name string, seconds float64) string {
	return s.writeWAV(name, seconds, func(t float64) float64 {
		if math.Mod(t, 0.5) < 1.0/testSampleRate {
			return 1
		}
		return 0
	})
}

// newTestCalculator returns a default calculator with a longer deadline, so
// generated fixtures cannot time out on a slow machine.
func newTestCalculator() *Calculator {
	calc := NewCalculator(nil)
	calc.executionTimeout = 60 * time.Second
	return calc
}

func (s *PlaylistSuite) planner() *PlaylistPlanner {
	s.T().Helper()
	return NewPlaylistPlanner(newTestCalculator(), nil)
}

// TestPlanVolumeLadder plans a fixed loudness ladder and checks that
// compensating gains put every track at the same peak, that handovers happen
// where the timeline says, and that the original single-file cue/loudness
// values are carried through unchanged.
func (s *PlaylistSuite) TestPlanVolumeLadder() {
	files := []string{
		s.ladderWAV("ladder_3.wav", 4, -3),
		s.ladderWAV("ladder_9.wav", 4, -9),
		s.ladderWAV("ladder_15.wav", 4, -15),
	}

	plan := s.planner().Plan(files)

	s.Require().Empty(plan.Errors)
	s.Require().Len(plan.Tracks, len(files))
	s.Require().Len(plan.Handovers, len(files)-1)
	s.Equal(-18.0, plan.TargetLoudness)

	// The fixed ladder has three distinct loudness steps; the gain must undo
	// them so every track reaches the same post-gain peak.
	peaks := make([]float64, len(plan.Tracks))
	for i, track := range plan.Tracks {
		peaks[i] = track.TruePeakDB + track.Gain
		s.True(track.TargetReached, "track %d must reach the target", i)
		s.InDelta(0.0, track.GainShortfall, 1e-9)
		s.InDelta(-18.0, track.Loudness+track.Gain, 0.05, "track %d post-gain loudness", i)
		if i > 0 {
			s.Greater(plan.Tracks[i-1].Loudness, track.Loudness, "ladder must step down in loudness")
		}
	}
	for i := 1; i < len(peaks); i++ {
		s.InDelta(peaks[0], peaks[i], 0.05, "ladder gain must land on a common peak")
	}

	// Each planned track carries the single-file analysis values unchanged.
	calc := newTestCalculator()
	for i, file := range files {
		res, err := calc.Calc(file)
		s.Require().NoError(err)
		s.Equal(res.CueIn, plan.Tracks[i].CueIn)
		s.Equal(res.CueOut, plan.Tracks[i].CueOut)
		s.Equal(res.Loudness, plan.Tracks[i].Loudness)
		s.Equal(res.TruePeakDb, plan.Tracks[i].TruePeakDB)
	}

	// A flat tone runs to its end, so its natural overlap is zero: handovers
	// land exactly on track boundaries and no track starts before the previous
	// one ends.
	for i, h := range plan.Handovers {
		s.Equal(plan.Tracks[i+1].Start, h.At)
		s.InDelta(plan.Tracks[i].End, h.At, 1e-9)
		s.InDelta(0.0, h.Duration, 0.15)
		// A butt-splice has nothing to duck, so it reports the louder track.
		s.Equal(0.0, h.Duck)
		s.InDelta(math.Max(plan.Tracks[i].Loudness+plan.Tracks[i].Gain, plan.Tracks[i+1].Loudness+plan.Tracks[i+1].Gain), h.Loudness, 1e-9)
	}
	s.InDelta(plan.Tracks[len(plan.Tracks)-1].End, plan.TotalDuration, 1e-9)
}

// TestPlanSilentTail checks a fade-out tail produces a real overlap, placed at
// the overlay point, with both tracks ducked so the overlap meets its budget.
func (s *PlaylistSuite) TestPlanSilentTail() {
	tail := s.silentTailWAV("silent_tail.wav", 3, 2, 1)
	next := s.ladderWAV("next.wav", 4, -6)

	plan := s.planner().Plan([]string{tail, next})

	s.Require().Empty(plan.Errors)
	s.Require().Len(plan.Tracks, 2)
	s.Require().Len(plan.Handovers, 1)

	h := plan.Handovers[0]
	s.Greater(h.Duration, 0.2, "a fade-out tail must create a real overlap")
	s.InDelta(plan.Tracks[0].End-h.Duration, h.At, 1e-9)
	s.InDelta(plan.Tracks[1].Start, h.At, 1e-9)
	s.InDelta(plan.Tracks[1].CueIn, 0.0, 1e-9)
	// Both tracks sit at the target before ducking, so the mix would be about
	// 3 dB hot; the duck brings it back to the budget.
	s.InDelta(-3.0103, h.Duck, 0.05)
	s.InDelta(plan.OverlapLoudness, h.Loudness, 1e-9)
	s.LessOrEqual(h.TruePeakDB, plan.PeakCeiling+1e-9)

	// The tail's cue-out is before the file ends, and the overlap is exactly
	// the tail's cue-out minus its single-file overlay point clamped to the
	// playable range.
	s.Less(plan.Tracks[0].CueOut, plan.Tracks[0].Duration)
	s.LessOrEqual(h.Duration, plan.Tracks[0].PlayDuration)
	s.LessOrEqual(h.Duration, plan.Tracks[1].PlayDuration)
}

// TestPlanSilentTailIsLast verifies a fading final track simply ends; nothing
// is invented after it.
func (s *PlaylistSuite) TestPlanSilentTailIsLast() {
	tail := s.silentTailWAV("silent_tail.wav", 3, 2, 1)

	plan := s.planner().Plan([]string{tail})

	s.Require().Empty(plan.Errors)
	s.Require().Len(plan.Tracks, 1)
	s.Empty(plan.Handovers)
	s.Equal(plan.Tracks[0].End, plan.TotalDuration)
	s.Equal(0.0, plan.Tracks[0].OverlapOut)
}

// TestPlanTargetUnreachable verifies a peaky track is gain-capped at the peak
// ceiling and reported as a warning instead of silently missing the target.
func (s *PlaylistSuite) TestPlanTargetUnreachable() {
	plan := s.planner().Plan([]string{s.peakyWAV("peaky.wav", 3)})

	s.Require().Empty(plan.Errors)
	s.Require().Len(plan.Tracks, 1)
	track := plan.Tracks[0]
	s.False(track.TargetReached)
	s.Positive(track.GainShortfall)
	s.InDelta(plan.PeakCeiling, track.TruePeakDB+track.Gain, 1e-3)
	s.Less(track.Loudness+track.Gain, plan.TargetLoudness)

	s.Require().Len(plan.Warnings, 1)
	s.Equal(PlanReasonTargetUnreachable, plan.Warnings[0].Reason)
	s.Equal(0, plan.Warnings[0].Index)
}

// TestPlanErrors covers list-level and per-file failures: the plan still comes
// back, failed files carry context, and the remaining tracks chain together.
func (s *PlaylistSuite) TestPlanErrors() {
	good := s.ladderWAV("good.wav", 2, -6)
	other := s.ladderWAV("other.wav", 2, -12)
	notMedia := filepath.Join(s.T().TempDir(), "not_media.txt")
	s.Require().NoError(os.WriteFile(notMedia, []byte("not audio"), 0o644))

	calc := newTestCalculator()
	planner := NewPlaylistPlanner(calc, nil)

	s.Run("empty list reports the list problem and no timeline", func() {
		plan := planner.Plan(nil)
		s.Empty(plan.Tracks)
		s.Empty(plan.Handovers)
		s.Require().Len(plan.Errors, 1)
		s.Equal(-1, plan.Errors[0].Index)
		s.Equal(PlanReasonEmptyPlaylist, plan.Errors[0].Reason)
	})

	s.Run("duplicate file is skipped and names its original", func() {
		plan := planner.Plan([]string{good, good, other})
		s.Require().Len(plan.Tracks, 2)
		s.Require().Len(plan.Errors, 1)
		s.Equal(PlanReasonDuplicate, plan.Errors[0].Reason)
		s.Equal(1, plan.Errors[0].Index)
		s.Require().NotNil(plan.Errors[0].DuplicateOf)
		s.Equal(0, *plan.Errors[0].DuplicateOf)
		s.Len(plan.Handovers, 1)
		// the surviving tracks still form one continuous timeline
		s.InDelta(plan.Tracks[0].End-plan.Tracks[0].OverlapOut, plan.Tracks[1].Start, 1e-9)
	})

	s.Run("unreadable media is reported with its path and the rest still plan", func() {
		plan := planner.Plan([]string{notMedia, good})
		s.Require().Len(plan.Tracks, 1)
		s.Require().Len(plan.Errors, 1)
		s.Equal(PlanReasonBadMedia, plan.Errors[0].Reason)
		s.Equal(0, plan.Errors[0].Index)
		s.Equal(notMedia, plan.Errors[0].Path)
		s.NotEmpty(plan.Errors[0].Message)
		s.Equal(0.0, plan.Tracks[0].Start, "the first surviving track starts the timeline")
	})

	s.Run("timeout is reported as a timeout", func() {
		timed := NewPlaylistPlanner(
			NewCalculator(&CalculatorOptions{ExecutionTimeout: 1}),
			&PlaylistOptions{PeakCeilingDBFS: -1, OverlapLoudnessLUFS: -18},
		)
		plan := timed.Plan([]string{good})
		s.Empty(plan.Tracks)
		s.Require().Len(plan.Errors, 1)
		s.Equal(PlanReasonTimeout, plan.Errors[0].Reason)
		s.Contains(plan.Errors[0].Message, "timed out")
	})
}

// TestPlanAcrossFailure checks a failed middle track does not leave a stale
// handover pointing at a gap.
func (s *PlaylistSuite) TestPlanAcrossFailure() {
	first := s.ladderWAV("first.wav", 2, -6)
	miss := filepath.Join(s.T().TempDir(), "missing.wav")
	last := s.ladderWAV("last.wav", 2, -12)

	plan := s.planner().Plan([]string{first, miss, last})

	s.Require().Len(plan.Tracks, 2)
	s.Require().Len(plan.Handovers, 1)
	s.Equal(0, plan.Handovers[0].FromIndex)
	s.Equal(2, plan.Handovers[0].ToIndex)
	s.Require().Len(plan.Errors, 1)
	s.Equal(PlanReasonBadMedia, plan.Errors[0].Reason)
}

// TestOverlapFor covers the bounds that keep a handover from going negative,
// outlasting a short track, or putting three tracks on the timeline at once.
func (s *PlaylistSuite) TestOverlapFor() {
	tests := []struct {
		name           string
		playDuration   float64
		overlapIn      float64
		nextPlay       float64
		cueOut         float64
		crossStartNext float64
		want           float64
	}{
		{name: "natural overlap is used when it fits", playDuration: 100, nextPlay: 100, cueOut: 95, crossStartNext: 92, want: 3},
		{name: "a negative overlap is clamped to zero", playDuration: 100, nextPlay: 100, cueOut: 92, crossStartNext: 95, want: 0},
		{name: "a short next track caps the overlap", playDuration: 100, nextPlay: 1.5, cueOut: 95, crossStartNext: 92, want: 1.5},
		{
			name:           "a short outgoing track caps the overlap",
			playDuration:   2,
			nextPlay:       100,
			cueOut:         0.5,
			crossStartNext: 0,
			want:           0.5,
		},
		{
			name:           "the incoming overlap is subtracted so three tracks never coincide",
			playDuration:   10,
			overlapIn:      4,
			nextPlay:       100,
			cueOut:         10,
			crossStartNext: 0,
			want:           6,
		},
		{
			name:           "a track fully consumed by its incoming overlap hands over nothing",
			playDuration:   10,
			overlapIn:      10,
			nextPlay:       100,
			cueOut:         10,
			crossStartNext: 0,
			want:           0,
		},
	}

	for _, tc := range tests {
		s.Run(tc.name, func() {
			got := overlapFor(tc.playDuration, tc.overlapIn, tc.nextPlay, tc.cueOut, tc.crossStartNext)
			s.InDelta(tc.want, got, 1e-9)
			s.GreaterOrEqual(got, 0.0)
		})
	}
}

// TestHandoverDucking covers the two independent limits: loudness and true peak.
func (s *PlaylistSuite) TestHandoverDucking() {
	planner := NewPlaylistPlanner(NewCalculator(nil), &PlaylistOptions{
		PeakCeilingDBFS:     -1.0,
		OverlapLoudnessLUFS: -18.0,
	})
	from := PlaylistTrack{Loudness: -18, Gain: 0, TruePeakDB: -10, PlayDuration: 100, OverlapIn: 0, OverlapOut: 2}
	to := PlaylistTrack{Loudness: -18, Gain: 0, TruePeakDB: -10, OverlapOut: 0}

	tests := []struct {
		name     string
		fromPeak float64
		toPeak   float64
		wantDuck float64
		wantLoud float64
		wantPeak float64
	}{
		{
			name:     "peaks well below the ceiling let the loudness limit set the duck",
			fromPeak: -60, toPeak: -10,
			wantDuck: -3.0103, wantLoud: -18, wantPeak: -12.9829,
		},
		{
			name:     "coherent peaks at the ceiling force a deeper duck than loudness does",
			fromPeak: 0, toPeak: -10,
			wantDuck: -3.3866, wantLoud: -18.3763, wantPeak: -1,
		},
		{
			name:     "the louder of the two limits always wins",
			fromPeak: -10, toPeak: -10,
			wantDuck: -3.0103, wantLoud: -18, wantPeak: -6.9897,
		},
	}
	for _, tc := range tests {
		s.Run(tc.name, func() {
			from := from
			from.TruePeakDB = tc.fromPeak
			to := to
			to.TruePeakDB = tc.toPeak
			h := planner.handover(from, to)
			s.InDelta(tc.wantDuck, h.Duck, 1e-3)
			s.LessOrEqual(h.Loudness, planner.overlapLoudness+1e-9)
			s.LessOrEqual(h.TruePeakDB, planner.peakCeiling+1e-9)
			s.InDelta(tc.wantLoud, h.Loudness, 1e-3)
			s.InDelta(tc.wantPeak, h.TruePeakDB, 1e-3)
			s.Equal(2.0, h.Duration)
		})
	}
}

// TestValidateAnalysis keeps corrupt analysis results out of the timeline.
func (s *PlaylistSuite) TestValidateAnalysis() {
	valid := &Result{Duration: 10, CueIn: 0, CueOut: 9, Loudness: -18, TruePeakDb: -3, CrossStartNext: 8}
	tests := []struct {
		name    string
		res     *Result
		wantErr string
	}{
		{name: "valid result passes", res: valid},
		{name: "missing result is rejected", res: nil, wantErr: "no result"},
		{name: "non-positive duration is rejected", res: &Result{Duration: 0}, wantErr: "duration"},
		{name: "negative cue-in is rejected", res: &Result{Duration: 10, CueIn: -1, CueOut: 9}, wantErr: "cue_in"},
		{name: "cue-out at cue-in is rejected", res: &Result{Duration: 10, CueIn: 4, CueOut: 4}, wantErr: "cue_out"},
		{name: "infinite loudness is rejected", res: &Result{Duration: 10, CueOut: 9, Loudness: math.Inf(-1)}, wantErr: "loudness"},
		{name: "NaN peak is rejected", res: &Result{Duration: 10, CueOut: 9, TruePeakDb: math.NaN()}, wantErr: "true_peak_db"},
	}
	for _, tc := range tests {
		s.Run(tc.name, func() {
			err := validateAnalysis(tc.res)
			if tc.wantErr == "" {
				s.NoError(err)
				return
			}
			s.Require().Error(err)
			s.Contains(err.Error(), tc.wantErr)
		})
	}
}

// TestPlanJSON locks the wire form: unit suffixes on loudness/gain, plain
// numbers on the timeline, and arrays that are always present.
func (s *PlaylistSuite) TestPlanJSON() {
	plan := &PlaylistPlan{
		TargetLoudness:  -18,
		PeakCeiling:     -1,
		OverlapLoudness: -18,
		TotalDuration:   7.5,
		Tracks: []PlaylistTrack{{
			Index: 0, Path: "a.wav", Start: 0, End: 4, CueIn: 0.1, CueOut: 4,
			Duration: 4.1, PlayDuration: 3.9, OverlapOut: 0.5,
			Loudness: -14.2, TruePeakDB: -0.4, Gain: -3.8, TargetReached: true,
		}},
		Handovers: []Handover{{
			FromIndex: 0, ToIndex: 1, At: 3.5, Duration: 0.5,
			Duck: -3, Loudness: -18, TruePeakDB: -5,
		}},
	}

	b, err := plan.MarshalJSON()
	s.Require().NoError(err)

	var m map[string]any
	s.Require().NoError(json.Unmarshal(b, &m))
	s.Equal("-18.000 LUFS", m["target_lufs"])
	s.Equal("-1.000 dBFS", m["peak_ceiling"])
	s.Equal("-18.000 LUFS", m["overlap_lufs"])
	s.Equal(7.5, m["total_duration"])
	s.Equal([]any{}, m["errors"])
	s.Equal([]any{}, m["warnings"])

	track := m["tracks"].([]any)[0].(map[string]any)
	s.Equal("-14.200 LUFS", track["loudness"])
	s.Equal("-0.400 dBFS", track["true_peak_db"])
	s.Equal("-3.800 dB", track["gain"])
	s.Equal(0.1, track["cue_in"])
	s.Equal(true, track["target_reached"])

	handover := m["handovers"].([]any)[0].(map[string]any)
	s.Equal("-3.000 dB", handover["duck"])
	s.Equal(3.5, handover["at"])

	nice, err := plan.MarshalNiceJSON()
	s.Require().NoError(err)
	s.Contains(string(nice), "\n  \"target_lufs\"")
}

// TestPlanEmptyJSON checks an empty plan still serializes with empty arrays
// rather than nulls, so consumers need no nil checks.
func (s *PlaylistSuite) TestPlanEmptyJSON() {
	plan := s.planner().Plan(nil)

	b, err := plan.MarshalJSON()
	s.Require().NoError(err)

	var m map[string]any
	s.Require().NoError(json.Unmarshal(b, &m))
	s.Equal([]any{}, m["tracks"])
	s.Equal([]any{}, m["handovers"])
	s.Equal([]any{}, m["warnings"])
	errs := m["errors"].([]any)
	s.Require().Len(errs, 1)
	s.Equal(PlanReasonEmptyPlaylist, errs[0].(map[string]any)["reason"])
}

// TestPlanReasonForTimeout pins the classification helper on both error kinds.
func (s *PlaylistSuite) TestPlanReasonForTimeout() {
	s.Run("timeout error is classified", func() {
		issue := planError(2, "a.wav", TimeoutError{After: 20})
		s.Equal(PlanReasonTimeout, issue.Reason)
		s.Equal(2, issue.Index)
	})
	s.Run("other failures are bad media", func() {
		issue := planError(0, "a.wav", os.ErrNotExist)
		s.Equal(PlanReasonBadMedia, issue.Reason)
	})
}
