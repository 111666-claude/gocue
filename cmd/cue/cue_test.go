package cue

import (
	"bytes"
	"strings"
	"testing"
)

// TestValidateRanges checks the per-invocation flag validation.
func TestValidateRanges(t *testing.T) {
	valid := &options{target: -18, silence: -42, overlay: -8, longtail: 15, extra: -12, drop: 40, blankskip: 0}
	if err := valid.validateRanges(); err != nil {
		t.Fatalf("expected valid options, got error: %v", err)
	}

	tests := []struct {
		name string
		o    *options
	}{
		{"target too high", &options{target: 5}},
		{"silence too low", &options{silence: -200}},
		{"longtail too high", &options{longtail: 100}},
		{"drop too high", &options{drop: 150}},
		{"blankskip negative", &options{blankskip: -1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.o.validateRanges(); err == nil {
				t.Fatalf("expected validation error for %s", tc.name)
			}
		})
	}
}

// TestNewRootCmdIsolatedState verifies the §4.1 fix: each newRootCmd() has its
// own flag state, so parsing flags on one command does not leak into another.
func TestNewRootCmdIsolatedState(t *testing.T) {
	c1 := newRootCmd()
	if err := c1.ParseFlags([]string{"--target", "-20"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if v, _ := c1.Flags().GetFloat64("target"); v != -20.0 {
		t.Fatalf("c1 target: expected -20, got %v", v)
	}

	c2 := newRootCmd()
	if v, _ := c2.Flags().GetFloat64("target"); v != -18.0 {
		t.Fatalf("c2 target: expected default -18, got %v (state leaked between commands)", v)
	}
}

// TestRunInvalidFlagReturnsError confirms validateRanges is wired into RunE and
// that a bad flag fails fast (before any ffprobe/ffmpeg invocation).
func TestRunInvalidFlagReturnsError(t *testing.T) {
	c := newRootCmd()
	c.SetArgs([]string{"--target", "10", "nonexistent.wav"})
	if err := c.Execute(); err == nil {
		t.Fatal("expected an error for out-of-range --target")
	}
}

// TestPrintFlagsGoesToStderr ensures --print_flags never pollutes stdout JSON.
func TestPrintFlagsGoesToStderr(t *testing.T) {
	c := newRootCmd()
	var stdout, stderr bytes.Buffer
	c.SetOut(&stdout)
	c.SetErr(&stderr)
	// Missing file fails after flag logging; validation still passes.
	c.SetArgs([]string{"--print_flags", "nonexistent.wav"})
	_ = c.Execute()

	if !strings.Contains(stderr.String(), "Flag: target") {
		t.Fatalf("expected flag dump on stderr, got %q", stderr.String())
	}
	if strings.Contains(stdout.String(), "Flag:") {
		t.Fatalf("print_flags leaked to stdout: %q", stdout.String())
	}
}

// TestPlaylistCmdEmptyPlan confirms an empty playlist is a successful run with
// a plan on stdout that says why it has no tracks, and that nothing else
// reaches stdout.
func TestPlaylistCmdEmptyPlan(t *testing.T) {
	c := newRootCmd()
	var stdout, stderr bytes.Buffer
	c.SetOut(&stdout)
	c.SetErr(&stderr)
	c.SetArgs([]string{"playlist"})

	if err := c.Execute(); err != nil {
		t.Fatalf("empty playlist must not fail the command: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected one JSON line on stdout, got %q", stdout.String())
	}
	if !strings.Contains(lines[0], `"reason":"empty_playlist"`) {
		t.Fatalf("plan must report an empty playlist, got %s", lines[0])
	}
	if !strings.Contains(lines[0], `"tracks":[]`) {
		t.Fatalf("plan must carry an empty tracks array, got %s", lines[0])
	}
	if !strings.Contains(stderr.String(), "empty_playlist") {
		t.Fatalf("expected the reason on stderr, got %q", stderr.String())
	}
	if strings.Contains(stdout.String(), "empty_playlist:") {
		t.Fatalf("diagnostics leaked to stdout: %q", stdout.String())
	}
}

// TestPlaylistCmdFlagValidation checks the plan-only limits are rejected before
// any file is touched, and that the shared analysis flags keep their ranges.
func TestPlaylistCmdFlagValidation(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"peak ceiling above 0 dBFS", []string{"playlist", "--peak_ceiling", "1.0"}},
		{"overlap loudness above 0 LUFS", []string{"playlist", "--overlap_lufs", "3.0"}},
		{"overlap loudness below -23 LUFS", []string{"playlist", "--overlap_lufs", "-30.0"}},
		{"target out of range", []string{"playlist", "--target", "10"}},
		{"blankskip negative", []string{"playlist", "--blankskip", "-1"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newRootCmd()
			c.SetOut(&bytes.Buffer{})
			c.SetErr(&bytes.Buffer{})
			c.SetArgs(tc.args)
			if err := c.Execute(); err == nil {
				t.Fatalf("expected a validation error for %s", tc.name)
			}
		})
	}
}

// TestPlaylistCmdFollowsTarget checks that changing only --target moves the
// overlap budget with it instead of leaving the mix at the old level.
func TestPlaylistCmdFollowsTarget(t *testing.T) {
	c := newRootCmd()
	var stdout bytes.Buffer
	c.SetOut(&stdout)
	c.SetErr(&bytes.Buffer{})
	c.SetArgs([]string{"playlist", "--target", "-14"})
	if err := c.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(stdout.String(), `"target_lufs":"-14.000 LUFS","peak_ceiling":"-1.000 dBFS","overlap_lufs":"-14.000 LUFS"`) {
		t.Fatalf("overlap budget must follow --target, got %s", stdout.String())
	}
}

// TestPlaylistCmdReportsBadMedia confirms failed files are in the plan and that
// the command still succeeds, since a partial plan is the useful result.
func TestPlaylistCmdReportsBadMedia(t *testing.T) {
	c := newRootCmd()
	var stdout, stderr bytes.Buffer
	c.SetOut(&stdout)
	c.SetErr(&stderr)
	c.SetArgs([]string{"playlist", "does_not_exist.wav"})

	if err := c.Execute(); err != nil {
		t.Fatalf("a failed file must be reported in the plan, not as a command error: %v", err)
	}
	if !strings.Contains(stdout.String(), `"reason":"bad_media"`) {
		t.Fatalf("plan must carry the failure, got %s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "bad_media:") {
		t.Fatalf("failure reason must go to stderr, got %q", stderr.String())
	}
}

// TestSingleFileOutputUnchanged pins the single-file command's arguments now
// that a subcommand shares the root: still exactly one file, and the playlist
// command is a real subcommand rather than a flag.
func TestSingleFileOutputUnchanged(t *testing.T) {
	root := newRootCmd()

	found, _, err := root.Find([]string{"playlist"})
	if err != nil {
		t.Fatalf("find playlist: %v", err)
	}
	if found == root || found.Name() != "playlist" {
		t.Fatal("playlist must resolve to its own subcommand")
	}

	if err := root.Args(root, []string{"a.wav"}); err != nil {
		t.Fatalf("root must keep accepting exactly one file: %v", err)
	}
	if err := root.Args(root, []string{"a.wav", "b.wav"}); err == nil {
		t.Fatal("root must reject more than one file; use the playlist subcommand")
	}
}
