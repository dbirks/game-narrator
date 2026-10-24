package dolphin

import (
	"context"
	"testing"
)

// TestProbeContract pins the display-free probe's argv/env (issue #62): the
// binary core-dumps on a bare --version without a display server, so any
// change that drops the offscreen platform or rewrites the argv restores the
// core dump on the doctor row and here.
func TestProbeContract(t *testing.T) {
	args := ProbeArgs()
	if len(args) != 1 || args[0] != "--version" {
		t.Fatalf("ProbeArgs() = %v, want exactly [--version]", args)
	}
	env := ProbeEnv()
	found := false
	for _, e := range env {
		if e == "QT_QPA_PLATFORM=offscreen" {
			found = true
		}
	}
	if !found {
		t.Fatalf("ProbeEnv() = %v, must include QT_QPA_PLATFORM=offscreen", env)
	}
}

func TestHeadlessVersionReturnsFirstNonEmptyLine(t *testing.T) {
	var gotArgs, gotEnv []string
	got, err := HeadlessVersion(context.Background(), "/usr/bin/dolphin-emu", func(_ context.Context, name string, args []string, env []string) (string, error) {
		if name != "/usr/bin/dolphin-emu" {
			t.Fatalf("binary = %q", name)
		}
		gotArgs, gotEnv = args, env
		return "\n\nDolphin 2609\n", nil
	})
	if err != nil {
		t.Fatalf("HeadlessVersion: %v", err)
	}
	if got != "Dolphin 2609" {
		t.Fatalf("version = %q, want %q", got, "Dolphin 2609")
	}
	if len(gotArgs) != 1 || gotArgs[0] != "--version" || gotEnv[0] != "QT_QPA_PLATFORM=offscreen" {
		t.Fatalf("argv/env = %v %v, want the ProbeArgs/ProbeEnv contract", gotArgs, gotEnv)
	}
}

func TestHeadlessVersionEmptyOutputIsAnError(t *testing.T) {
	_, err := HeadlessVersion(context.Background(), "dolphin-emu", func(_ context.Context, _ string, _ []string, _ []string) (string, error) {
		return "   \n\n", nil
	})
	if err != errProbeOutput {
		t.Fatalf("err = %v, want errProbeOutput", err)
	}
}
