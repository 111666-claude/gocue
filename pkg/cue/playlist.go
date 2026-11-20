package cue

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
)

const (
	// defaultPeakCeilingDBFS keeps a planned track's true peak at the EBU
	// recommended maximum when its gain would otherwise push it higher.
	defaultPeakCeilingDBFS = -1.0
	// targetEpsilon is the gain shortfall below which a track counts as having
	// reached the requested loudness target.
	targetEpsilon = 1e-6
)

// PlaylistOptions configures playlist planning. A nil value uses the
// calculator's target loudness as the overlap budget and -1 dBFS as the peak
// ceiling; a non-nil value is used verbatim.
type PlaylistOptions struct {
	// PeakCeilingDBFS is the highest true peak a planned track or an overlap
	// may reach.
	PeakCeilingDBFS float64
	// OverlapLoudnessLUFS is the loudness budget for the region where two
	// tracks play together.
	OverlapLoudnessLUFS float64
}

// PlaylistPlanner turns an ordered list of files into a playout timeline. It
// analyses each file with the calculator's single-file path, so cue points and
// loudness are the same values `gocue file` reports.
type PlaylistPlanner struct {
	calc            *Calculator
	peakCeiling     float64
	overlapLoudness float64
}

// NewPlaylistPlanner returns a planner that analyses files with calc. A nil
// calc uses the default calculator.
func NewPlaylistPlanner(calc *Calculator, opts *PlaylistOptions) *PlaylistPlanner {
	if calc == nil {
		calc = NewCalculator(nil)
	}
	if opts == nil {
		opts = &PlaylistOptions{
			PeakCeilingDBFS:     defaultPeakCeilingDBFS,
			OverlapLoudnessLUFS: calc.targetLoudness,
		}
	}
	return &PlaylistPlanner{
		calc:            calc,
		peakCeiling:     opts.PeakCeilingDBFS,
		overlapLoudness: opts.OverlapLoudnessLUFS,
	}
}

// PlaylistTrack is one successfully analysed track on the plan's timeline.
type PlaylistTrack struct {
	Index         int     // position in the input list
	Path          string  // input path as given
	Start         float64 // timeline position of cue-in, seconds
	End           float64 // timeline position of cue-out, seconds
	CueIn         float64 // seconds from the start of the file
	CueOut        float64 // seconds from the start of the file
	Duration      float64 // file duration, seconds
	PlayDuration  float64 // cue_out - cue_in, seconds
	OverlapIn     float64 // overlap with the previous track, seconds
	OverlapOut    float64 // overlap with the next track, seconds
	Loudness      float64 // integrated loudness before gain, LUFS
	TruePeakDB    float64 // true peak before gain, dBFS
	Gain          float64 // applied gain, dB
	GainShortfall float64 // gain the peak ceiling kept us from applying, dB
	TargetReached bool    // false when the peak ceiling capped the gain

	// crossStartNext is the single-file overlay point, kept only to size the
	// handover to the next track.
	crossStartNext float64
}

// Handover is the transition between two consecutive planned tracks. Both
// tracks are ducked by Duck while they play together, so Loudness and
// TruePeakDB are the bound the mix stays inside.
type Handover struct {
	FromIndex  int     // input index of the outgoing track
	ToIndex    int     // input index of the incoming track
	At         float64 // timeline position where the incoming track starts, seconds
	Duration   float64 // overlap length, seconds
	Duck       float64 // gain applied to both tracks during the overlap, dB
	Loudness   float64 // worst-case combined loudness after ducking, LUFS
	TruePeakDB float64 // worst-case combined true peak after ducking, dBFS
}

// PlanIssue describes a file that could not be planned or a plan condition
// that needs attention.
type PlanIssue struct {
	Index       int    `json:"index"`                  // input index; -1 for a problem with the list itself
	Path        string `json:"path"`                   // input path, empty for list-level problems
	Reason      string `json:"reason"`                 // machine-readable reason, e.g. "timeout"
	Message     string `json:"message"`                // human-readable context
	DuplicateOf *int   `json:"duplicate_of,omitempty"` // input index of the first copy
}

// Reasons reported in PlaylistPlan.Errors.
const (
	PlanReasonEmptyPlaylist   = "empty_playlist"
	PlanReasonDuplicate       = "duplicate"
	PlanReasonTimeout         = "timeout"
	PlanReasonBadMedia        = "bad_media"
	PlanReasonInvalidAnalysis = "invalid_analysis"
)

// PlanReasonTargetUnreachable is reported in PlaylistPlan.Warnings when the
// peak ceiling keeps a track below the requested loudness.
const PlanReasonTargetUnreachable = "target_unreachable"

// PlaylistPlan is an ordered playout timeline. All times are seconds from the
// start of the programme; loudness and gain values are plain dB numbers and are
// unit-suffixed only when marshalled.
type PlaylistPlan struct {
	TargetLoudness  float64
	PeakCeiling     float64
	OverlapLoudness float64
	TotalDuration   float64
	Tracks          []PlaylistTrack
	Handovers       []Handover
	Errors          []PlanIssue
	Warnings        []PlanIssue
}

// Plan analyses paths in order and returns the timeline. A file that cannot be
// analysed is reported in Errors and left out of the timeline; the remaining
// tracks still chain together. Plan always returns a usable plan, including for
// an empty list or when every file fails.
func (p *PlaylistPlanner) Plan(paths []string) *PlaylistPlan {
	plan := &PlaylistPlan{
		TargetLoudness:  p.calc.targetLoudness,
		PeakCeiling:     p.peakCeiling,
		OverlapLoudness: p.overlapLoudness,
		Tracks:          []PlaylistTrack{},
		Handovers:       []Handover{},
		Errors:          []PlanIssue{},
		Warnings:        []PlanIssue{},
	}
	if len(paths) == 0 {
		plan.Errors = append(plan.Errors, PlanIssue{
			Index:   -1,
			Reason:  PlanReasonEmptyPlaylist,
			Message: "no input files",
		})
		return plan
	}

	seen := make(map[string]int, len(paths))
	for i, path := range paths {
		key := canonicalPath(path)
		if first, ok := seen[key]; ok {
			firstIndex := first
			plan.Errors = append(plan.Errors, PlanIssue{
				Index:       i,
				Path:        path,
				Reason:      PlanReasonDuplicate,
				Message:     fmt.Sprintf("duplicate of track %d", first),
				DuplicateOf: &firstIndex,
			})
			continue
		}
		seen[key] = i

		res, err := p.calc.Calc(path)
		if err != nil {
			plan.Errors = append(plan.Errors, planError(i, path, err))
			continue
		}
		track, err := p.newTrack(i, path, res)
		if err != nil {
			plan.Errors = append(plan.Errors, PlanIssue{
				Index:   i,
				Path:    path,
				Reason:  PlanReasonInvalidAnalysis,
				Message: err.Error(),
			})
			continue
		}
		if !track.TargetReached {
			plan.Warnings = append(plan.Warnings, PlanIssue{
				Index:  i,
				Path:   path,
				Reason: PlanReasonTargetUnreachable,
				Message: fmt.Sprintf("target %.3f LUFS unreachable: peak ceiling %.3f dBFS allows %.3f dB of gain, %.3f dB needed",
					plan.TargetLoudness, p.peakCeiling, track.Gain, track.Gain+track.GainShortfall),
			})
		}
		plan.Tracks = append(plan.Tracks, track)
	}

	p.layout(plan)
	return plan
}

// newTrack derives a track's gain and initial timeline fields from one
// analysis result.
func (p *PlaylistPlanner) newTrack(index int, path string, res *Result) (PlaylistTrack, error) {
	if err := validateAnalysis(res); err != nil {
		return PlaylistTrack{}, err
	}

	rawGain := p.calc.targetLoudness - res.Loudness
	peakCap := p.peakCeiling - res.TruePeakDb
	gain := math.Min(rawGain, peakCap)
	shortfall := math.Max(0, rawGain-gain)

	return PlaylistTrack{
		Index:          index,
		Path:           path,
		CueIn:          res.CueIn,
		CueOut:         res.CueOut,
		Duration:       res.Duration,
		PlayDuration:   res.CueOut - res.CueIn,
		Loudness:       res.Loudness,
		TruePeakDB:     res.TruePeakDb,
		Gain:           gain,
		GainShortfall:  shortfall,
		TargetReached:  shortfall <= targetEpsilon,
		crossStartNext: res.CrossStartNext,
	}, nil
}

// validateAnalysis rejects results a timeline cannot be built from, with the
// offending values in the message.
func validateAnalysis(res *Result) error {
	if res == nil {
		return errors.New("analysis returned no result")
	}
	// An ordered list, so a result with several bad fields always names the
	// same one.
	for _, f := range []struct {
		name  string
		value float64
	}{
		{"duration", res.Duration},
		{"cue_in", res.CueIn},
		{"cue_out", res.CueOut},
		{"cross_start_next", res.CrossStartNext},
		{"loudness", res.Loudness},
		{"true_peak_db", res.TruePeakDb},
	} {
		if math.IsNaN(f.value) || math.IsInf(f.value, 0) {
			return fmt.Errorf("%s is not finite: %v", f.name, f.value)
		}
	}
	switch {
	case res.Duration <= 0:
		return fmt.Errorf("duration is not positive: %.3f", res.Duration)
	case res.CueIn < 0:
		return fmt.Errorf("cue_in is negative: %.3f", res.CueIn)
	case res.CueOut <= res.CueIn:
		return fmt.Errorf("cue_out %.3f is not after cue_in %.3f", res.CueOut, res.CueIn)
	}
	return nil
}

// layout computes overlaps, timeline positions and handover bounds in place.
func (p *PlaylistPlanner) layout(plan *PlaylistPlan) {
	tracks := plan.Tracks
	for i := 0; i+1 < len(tracks); i++ {
		overlap := overlapFor(
			tracks[i].PlayDuration, tracks[i].OverlapIn, tracks[i+1].PlayDuration,
			tracks[i].CueOut, tracks[i].crossStartNext,
		)
		tracks[i].OverlapOut = overlap
		tracks[i+1].OverlapIn = overlap
	}

	start := 0.0
	for i := range tracks {
		tracks[i].Start = start
		tracks[i].End = start + tracks[i].PlayDuration
		start = tracks[i].End - tracks[i].OverlapOut
	}
	if len(tracks) > 0 {
		plan.TotalDuration = tracks[len(tracks)-1].End
	}

	for i := 0; i+1 < len(tracks); i++ {
		from, to := tracks[i], tracks[i+1]
		plan.Handovers = append(plan.Handovers, p.handover(from, to))
	}
}

// handover returns the transition from the outgoing to the incoming track,
// ducking both by whatever is needed to meet the overlap loudness and peak
// limits.
func (p *PlaylistPlanner) handover(from, to PlaylistTrack) Handover {
	combinedLoudness := sumLoudness(
		from.Loudness+from.Gain,
		to.Loudness+to.Gain,
	)
	combinedPeak := sumPeakDB(
		from.TruePeakDB+from.Gain,
		to.TruePeakDB+to.Gain,
	)
	duck := math.Min(0, math.Min(
		p.overlapLoudness-combinedLoudness,
		p.peakCeiling-combinedPeak,
	))
	// A butt-splice has no overlap for a duck to act on, so report the mix it
	// actually plays instead of numbers that never happen.
	if from.OverlapOut <= 0 {
		return Handover{
			FromIndex:  from.Index,
			ToIndex:    to.Index,
			At:         to.Start,
			Loudness:   math.Max(from.Loudness+from.Gain, to.Loudness+to.Gain),
			TruePeakDB: math.Max(from.TruePeakDB+from.Gain, to.TruePeakDB+to.Gain),
		}
	}
	return Handover{
		FromIndex:  from.Index,
		ToIndex:    to.Index,
		At:         to.Start,
		Duration:   from.OverlapOut,
		Duck:       duck,
		Loudness:   combinedLoudness + duck,
		TruePeakDB: combinedPeak + duck,
	}
}

// overlapFor returns the overlap between an outgoing track and its successor.
// The natural overlap is cue_out minus the overlay point, clamped so it is
// never negative, never longer than either track can play, and never overlaps
// the outgoing track's own incoming transition (which would put three tracks
// on the timeline at once).
func overlapFor(playDuration, overlapIn, nextPlayDuration, cueOut, crossStartNext float64) float64 {
	natural := cueOut - crossStartNext
	maxOverlap := math.Min(playDuration-overlapIn, nextPlayDuration)
	return math.Min(math.Max(natural, 0), math.Max(maxOverlap, 0))
}

// sumLoudness returns the loudness of two uncorrelated signals played
// together.
func sumLoudness(a, b float64) float64 {
	return 10 * math.Log10(math.Pow(10, a/10)+math.Pow(10, b/10))
}

// sumPeakDB returns the worst-case (coherent) peak of two signals played
// together.
func sumPeakDB(a, b float64) float64 {
	return 20 * math.Log10(math.Pow(10, a/20)+math.Pow(10, b/20))
}

func canonicalPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}

func planError(index int, path string, err error) PlanIssue {
	issue := PlanIssue{Index: index, Path: path, Reason: PlanReasonBadMedia, Message: err.Error()}
	var timeout TimeoutError
	if errors.As(err, &timeout) {
		issue.Reason = PlanReasonTimeout
	}
	return issue
}

type playlistDTO struct {
	TargetLoudness  string             `json:"target_lufs"`
	PeakCeiling     string             `json:"peak_ceiling"`
	OverlapLoudness string             `json:"overlap_lufs"`
	TotalDuration   float64            `json:"total_duration"`
	Tracks          []playlistTrackDTO `json:"tracks"`
	Handovers       []handoverDTO      `json:"handovers"`
	Errors          []PlanIssue        `json:"errors"`
	Warnings        []PlanIssue        `json:"warnings"`
}

type playlistTrackDTO struct {
	Index         int     `json:"index"`
	Path          string  `json:"path"`
	Start         float64 `json:"start"`
	End           float64 `json:"end"`
	CueIn         float64 `json:"cue_in"`
	CueOut        float64 `json:"cue_out"`
	Duration      float64 `json:"duration"`
	PlayDuration  float64 `json:"play_duration"`
	OverlapIn     float64 `json:"overlap_in"`
	OverlapOut    float64 `json:"overlap_out"`
	Loudness      string  `json:"loudness"`
	TruePeakDB    string  `json:"true_peak_db"`
	Gain          string  `json:"gain"`
	GainShortfall string  `json:"gain_shortfall"`
	TargetReached bool    `json:"target_reached"`
}

type handoverDTO struct {
	FromIndex  int     `json:"from_index"`
	ToIndex    int     `json:"to_index"`
	At         float64 `json:"at"`
	Duration   float64 `json:"duration"`
	Duck       string  `json:"duck"`
	Loudness   string  `json:"loudness"`
	TruePeakDB string  `json:"true_peak_db"`
}

func (p *PlaylistPlan) dto() playlistDTO {
	d := playlistDTO{
		TargetLoudness:  fmt.Sprintf("%.3f LUFS", p.TargetLoudness),
		PeakCeiling:     fmt.Sprintf("%.3f dBFS", p.PeakCeiling),
		OverlapLoudness: fmt.Sprintf("%.3f LUFS", p.OverlapLoudness),
		TotalDuration:   p.TotalDuration,
		Tracks:          make([]playlistTrackDTO, 0, len(p.Tracks)),
		Handovers:       make([]handoverDTO, 0, len(p.Handovers)),
		Errors:          p.Errors,
		Warnings:        p.Warnings,
	}
	if d.Errors == nil {
		d.Errors = []PlanIssue{}
	}
	if d.Warnings == nil {
		d.Warnings = []PlanIssue{}
	}
	for _, t := range p.Tracks {
		d.Tracks = append(d.Tracks, playlistTrackDTO{
			Index:         t.Index,
			Path:          t.Path,
			Start:         t.Start,
			End:           t.End,
			CueIn:         t.CueIn,
			CueOut:        t.CueOut,
			Duration:      t.Duration,
			PlayDuration:  t.PlayDuration,
			OverlapIn:     t.OverlapIn,
			OverlapOut:    t.OverlapOut,
			Loudness:      fmt.Sprintf("%.3f LUFS", t.Loudness),
			TruePeakDB:    fmt.Sprintf("%.3f dBFS", t.TruePeakDB),
			Gain:          fmt.Sprintf("%.3f dB", t.Gain),
			GainShortfall: fmt.Sprintf("%.3f dB", t.GainShortfall),
			TargetReached: t.TargetReached,
		})
	}
	for _, h := range p.Handovers {
		d.Handovers = append(d.Handovers, handoverDTO{
			FromIndex:  h.FromIndex,
			ToIndex:    h.ToIndex,
			At:         h.At,
			Duration:   h.Duration,
			Duck:       fmt.Sprintf("%.3f dB", h.Duck),
			Loudness:   fmt.Sprintf("%.3f LUFS", h.Loudness),
			TruePeakDB: fmt.Sprintf("%.3f dBFS", h.TruePeakDB),
		})
	}
	return d
}

// MarshalJSON returns the plan with unit-suffixed loudness and gain fields, as
// the tool's other JSON output does.
func (p *PlaylistPlan) MarshalJSON() ([]byte, error) {
	return json.Marshal(p.dto())
}

// MarshalNiceJSON returns the plan pretty-printed with two-space indentation.
func (p *PlaylistPlan) MarshalNiceJSON() ([]byte, error) {
	return json.MarshalIndent(p.dto(), "", "  ")
}
