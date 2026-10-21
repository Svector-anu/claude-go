package claudeargs

import (
	"os/exec"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

const fixedID = "11111111-2222-4333-8444-555555555555"

func fixedGen() (string, error) { return fixedID, nil }

func TestNewSessionIDIsValidV4(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		id, err := NewSessionID()
		if err != nil || !IsUUID(id) || id[14] != '4' {
			t.Fatalf("bad id %q: %v", id, err)
		}
		if seen[id] {
			t.Fatal("duplicate id")
		}
		seen[id] = true
	}
}

func TestBuildFreshSessionInjectsID(t *testing.T) {
	cases := map[string][]string{
		"no args":         nil,
		"prompt":          {"fix the bug"},
		"flags":           {"--model", "opus", "--add-dir", "a", "b", "--verbose"},
		"flag and prompt": {"--permission-mode", "plan", "do it"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			p, err := Build(args, fixedGen)
			if err != nil {
				t.Fatal(err)
			}
			want := append([]string{"--session-id", fixedID}, args...)
			if !reflect.DeepEqual(p.Args, want) {
				t.Errorf("Args = %v, want %v", p.Args, want)
			}
			if p.SessionID != fixedID || p.HandoffBlock != "" || p.Passthrough {
				t.Errorf("plan = %+v", p)
			}
		})
	}
}

func TestBuildDoesNotMutateInput(t *testing.T) {
	in := []string{"hello"}
	if _, err := Build(in, fixedGen); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, []string{"hello"}) {
		t.Errorf("input mutated: %v", in)
	}
}

func TestBuildPassthrough(t *testing.T) {
	for _, args := range [][]string{
		{"--version"}, {"-v"}, {"--help"}, {"-p", "hi"}, {"--print", "hi"},
		{"mcp", "list"}, {"auth", "status"}, {"--model", "opus", "doctor"}, {"update"},
	} {
		p, err := Build(args, fixedGen)
		if err != nil {
			t.Fatal(err)
		}
		if !p.Passthrough || !reflect.DeepEqual(p.Args, args) || p.SessionID != "" {
			t.Errorf("%v: plan = %+v", args, p)
		}
	}
}

func TestBuildAdoptsExplicitSessionID(t *testing.T) {
	other := "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	for _, args := range [][]string{
		{"--session-id", other},
		{"--session-id=" + other},
		{"--resume", other},
		{"-r", other, "--model", "opus"},
		{"--resume=" + other},
	} {
		p, err := Build(args, fixedGen)
		if err != nil {
			t.Fatal(err)
		}
		if p.SessionID != other || p.HandoffBlock != "" || !reflect.DeepEqual(p.Args, args) {
			t.Errorf("%v: plan = %+v", args, p)
		}
	}
}

func TestBuildBlocksWhenSessionUnknown(t *testing.T) {
	cases := map[string][]string{
		"continue":       {"--continue"},
		"short continue": {"-c"},
		"resume picker":  {"--resume"},
		"resume by name": {"--resume", "my-session"},
		"fork":           {"--resume", "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", "--fork-session"},
		"bad session id": {"--session-id", "nope"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			p, err := Build(args, fixedGen)
			if err != nil {
				t.Fatal(err)
			}
			if p.SessionID != "" || p.HandoffBlock == "" {
				t.Errorf("plan = %+v", p)
			}
			if !reflect.DeepEqual(p.Args, args) {
				t.Errorf("args must be untouched when the session is unknown: %v", p.Args)
			}
		})
	}
}

func TestBuildBlocksUnrebuildableFlags(t *testing.T) {
	for _, flag := range []string{"--bg", "--tmux", "--worktree", "-w", "--no-session-persistence", "--teleport", "--from-pr"} {
		p, err := Build([]string{flag}, fixedGen)
		if err != nil {
			t.Fatal(err)
		}
		if p.HandoffBlock == "" || !strings.Contains(p.HandoffBlock, flag) {
			t.Errorf("%s: HandoffBlock = %q", flag, p.HandoffBlock)
		}
		if p.SessionID == "" {
			t.Errorf("%s: should still pin a session id", flag)
		}
	}
}

func TestBuildBlocksUnknownFlagWithPossibleValue(t *testing.T) {
	p, _ := Build([]string{"--brand-new", "value"}, fixedGen)
	if !strings.Contains(p.HandoffBlock, "--brand-new") {
		t.Errorf("HandoffBlock = %q", p.HandoffBlock)
	}
	p, _ = Build([]string{"--brand-new", "--verbose"}, fixedGen)
	if p.HandoffBlock != "" {
		t.Errorf("trailing unknown boolean flag should be fine: %q", p.HandoffBlock)
	}
	p, _ = Build([]string{"--brand-new=1"}, fixedGen)
	if p.HandoffBlock != "" {
		t.Errorf("inline value is unambiguous: %q", p.HandoffBlock)
	}
}

func TestResumeArgs(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"drops injected id", []string{"--session-id", fixedID}, []string{"--resume", fixedID}},
		{"drops prompt keeps flags", []string{"--session-id", fixedID, "--model", "opus", "write tests", "--verbose"},
			[]string{"--resume", fixedID, "--model", "opus", "--verbose"}},
		{"keeps variadic values", []string{"--add-dir", "a", "b", "--session-id", fixedID},
			[]string{"--resume", fixedID, "--add-dir", "a", "b"}},
		{"replaces old resume", []string{"--resume", fixedID, "--permission-mode", "plan"},
			[]string{"--resume", fixedID, "--permission-mode", "plan"}},
		{"inline forms", []string{"--model=opus", "--session-id=" + fixedID},
			[]string{"--resume", fixedID, "--model=opus"}},
		{"drops after terminator", []string{"--verbose", "--", "-literal prompt"},
			[]string{"--resume", fixedID, "--verbose"}},
		{"no args", nil, []string{"--resume", fixedID}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ResumeArgs(c.in, fixedID); !reflect.DeepEqual(got, c.want) {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestResumeArgsRoundTripFromBuild(t *testing.T) {
	user := []string{"--model", "opus", "refactor the parser"}
	p, _ := Build(user, fixedGen)
	got := ResumeArgs(p.Args, p.SessionID)
	want := []string{"--resume", fixedID, "--model", "opus"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestFlagTableCoversInstalledClaude fails when the installed Claude Code has
// flags the arity table does not know, which would silently break argument rebuilding.
func TestFlagTableCoversInstalledClaude(t *testing.T) {
	path, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude not installed")
	}
	out, err := exec.Command(path, "--help").Output()
	if err != nil {
		t.Skipf("claude --help failed: %v", err)
	}
	options := string(out)
	if i := strings.Index(options, "Options:"); i >= 0 {
		options = options[i:]
	}
	if i := strings.Index(options, "\nCommands:"); i >= 0 {
		options = options[:i]
	}
	re := regexp.MustCompile(`(?m)^  ((?:-[A-Za-z], )?(?:--[A-Za-z][\w-]*)(?:, --[A-Za-z][\w-]*)?)`)
	var missing []string
	for _, m := range re.FindAllStringSubmatch(options, -1) {
		for _, name := range strings.Split(m[1], ", ") {
			if _, ok := flagArity[name]; !ok {
				missing = append(missing, name)
			}
		}
	}
	if len(missing) > 0 {
		t.Errorf("flags missing from flagArity: %v", missing)
	}
}

func TestResumeArgsWithPrompt(t *testing.T) {
	cases := []struct {
		name   string
		in     []string
		prompt string
		want   []string
	}{
		{"no flags", nil, "carry on", []string{"--resume", fixedID, "carry on"}},
		{"prompt goes before the user's flags", []string{"--session-id", fixedID, "--model", "opus"}, "carry on",
			[]string{"--resume", fixedID, "carry on", "--model", "opus"}},
		{"variadic flags cannot swallow it", []string{"--add-dir", "a", "b", "--session-id", fixedID}, "carry on",
			[]string{"--resume", fixedID, "carry on", "--add-dir", "a", "b"}},
		{"the original one-shot prompt is not resent", []string{"--session-id", fixedID, "fix the bug"}, "carry on",
			[]string{"--resume", fixedID, "carry on"}},
		{"empty prompt is plain resume", []string{"--session-id", fixedID}, "  ", []string{"--resume", fixedID}},
		{"a message starting with a dash is padded", nil, "--help", []string{"--resume", fixedID, " --help"}},
	}
	for _, c := range cases {
		if got := ResumeArgsWithPrompt(c.in, fixedID, c.prompt); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n got  %q\n want %q", c.name, got, c.want)
		}
	}
}

// The padded form must still be a positional argument to Claude's own parser.
func TestAPaddedPromptIsNotATokenThatLooksLikeAFlag(t *testing.T) {
	got := ResumeArgsWithPrompt(nil, fixedID, "--dangerously-skip-permissions")
	if strings.HasPrefix(got[2], "-") {
		t.Errorf("prompt %q can be parsed as a flag", got[2])
	}
}
