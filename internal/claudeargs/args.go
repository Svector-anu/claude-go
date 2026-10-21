// Package claudeargs understands just enough of Claude Code's command line to
// pin a session ID on launch and rebuild a safe `--resume` command for handoff.
//
// It never guesses: when an invocation cannot be interpreted with certainty,
// the plan says so and handoff is refused rather than risking the wrong session.
package claudeargs

import (
	"crypto/rand"
	"fmt"
	"regexp"
	"strings"
)

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// IsUUID reports whether s is a canonical UUID.
func IsUUID(s string) bool { return uuidPattern.MatchString(s) }

// NewSessionID returns a random version-4 UUID.
func NewSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate session id: %w", err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}

// Plan is how claude-go will launch Claude Code for the user's arguments.
type Plan struct {
	// Args are the exact arguments to execute (the user's, plus an injected --session-id when safe).
	Args []string
	// Passthrough means the invocation is not an interactive session (subcommand, --print, --help...).
	Passthrough bool
	// SessionID is the session this launch will use; empty when it cannot be known.
	SessionID string
	// HandoffBlock explains why handoff is unavailable; empty when it is allowed.
	HandoffBlock string
}

type item struct {
	name string   // flag name as written (before '='), or "" for a positional
	raw  []string // tokens this item occupies
	val  string   // first value, if any
}

type parsed struct {
	items      []item
	ambiguous  string // unknown flag followed by a token that may be its value
	subcommand bool
	terminated bool
}

func tokenize(args []string) parsed {
	var p parsed
	for i := 0; i < len(args); i++ {
		tok := args[i]
		if p.terminated || !strings.HasPrefix(tok, "-") || tok == "-" {
			if !p.terminated && subcommands[tok] {
				p.subcommand = true
			}
			p.items = append(p.items, item{raw: []string{tok}})
			continue
		}
		if tok == "--" {
			p.terminated = true
			p.items = append(p.items, item{name: "--", raw: []string{tok}})
			continue
		}
		name, inline, hasInline := strings.Cut(tok, "=")
		it := item{name: name, raw: []string{tok}}
		if hasInline {
			it.val = inline
			p.items = append(p.items, it)
			continue
		}
		kind, known := flagArity[name]
		next := func() (string, bool) {
			if i+1 < len(args) {
				return args[i+1], true
			}
			return "", false
		}
		take := func() {
			it.raw = append(it.raw, args[i+1])
			if it.val == "" {
				it.val = args[i+1]
			}
			i++
		}
		switch {
		case !known:
			if n, ok := next(); ok && !strings.HasPrefix(n, "-") && p.ambiguous == "" {
				p.ambiguous = name
			}
		case kind == arityValue:
			if _, ok := next(); ok {
				take()
			}
		case kind == arityOptional:
			if n, ok := next(); ok && !strings.HasPrefix(n, "-") {
				take()
			}
		case kind == arityVariadic:
			if _, ok := next(); ok {
				take()
				for n, ok := next(); ok && !strings.HasPrefix(n, "-"); n, ok = next() {
					take()
				}
			}
		}
		p.items = append(p.items, it)
	}
	return p
}

func (p parsed) find(names ...string) (item, bool) {
	for _, it := range p.items {
		for _, n := range names {
			if it.name == n {
				return it, true
			}
		}
	}
	return item{}, false
}

// Build decides how to launch Claude Code for args.
func Build(args []string, newID func() (string, error)) (Plan, error) {
	p := tokenize(args)
	if _, ok := p.find("--print", "-p", "--version", "-v", "--help", "-h"); ok || p.subcommand {
		return Plan{Args: args, Passthrough: true, HandoffBlock: "not an interactive session"}, nil
	}

	plan := Plan{Args: args}
	for _, it := range p.items {
		if reason, blocked := handoffBlockers[it.name]; blocked && plan.HandoffBlock == "" {
			plan.HandoffBlock = fmt.Sprintf("%s: %s", it.name, reason)
		}
	}
	if plan.HandoffBlock == "" && p.ambiguous != "" {
		plan.HandoffBlock = fmt.Sprintf("unrecognized flag %s may take a value; arguments cannot be rebuilt safely", p.ambiguous)
	}

	_, fork := p.find("--fork-session")
	sid, hasSID := p.find("--session-id")
	res, hasResume := p.find("--resume", "-r")
	_, hasContinue := p.find("--continue", "-c")

	switch {
	case hasSID:
		if IsUUID(sid.val) {
			plan.SessionID = strings.ToLower(sid.val)
		} else {
			plan.block("--session-id value is not a UUID")
		}
	case fork && (hasResume || hasContinue):
		plan.block("--fork-session creates a session id claude-go cannot know in advance")
	case hasResume && IsUUID(res.val):
		plan.SessionID = strings.ToLower(res.val)
	case hasResume || hasContinue:
		plan.block("session resumed by name, picker or --continue; pass --resume <uuid> to enable handoff")
	default:
		id, err := newID()
		if err != nil {
			return Plan{}, err
		}
		plan.SessionID = id
		plan.Args = append([]string{"--session-id", id}, args...)
	}
	if plan.SessionID == "" && plan.HandoffBlock == "" {
		plan.block("session id unknown")
	}
	return plan, nil
}

func (p *Plan) block(reason string) {
	if p.HandoffBlock == "" {
		p.HandoffBlock = reason
	}
}

// ResumeArgs rebuilds the user's arguments to resume sessionID: the one-shot
// prompt and every session-selection flag are dropped, everything else is kept.
// Only call it for a plan whose HandoffBlock is empty.
func ResumeArgs(args []string, sessionID string) []string {
	out := []string{"--resume", sessionID}
	dropped := map[string]bool{
		"--session-id": true, "--resume": true, "-r": true,
		"--continue": true, "-c": true, "--fork-session": true,
	}
	terminated := false
	for _, it := range tokenize(args).items {
		switch {
		case it.name == "--":
			terminated = true
		case terminated, it.name == "" || dropped[it.name]:
		default:
			out = append(out, it.raw...)
		}
	}
	return out
}

// ResumeArgsWithPrompt is ResumeArgs plus a message for Claude to act on as
// soon as the session is open. The message goes right after the session id:
// a prompt placed after a flag that takes several values (--add-dir, --tools…)
// would be swallowed as one of them. A message that starts with "-" is padded
// so it can never be read as a flag.
func ResumeArgsWithPrompt(args []string, sessionID, prompt string) []string {
	base := ResumeArgs(args, sessionID)
	if strings.TrimSpace(prompt) == "" {
		return base
	}
	if strings.HasPrefix(prompt, "-") {
		prompt = " " + prompt
	}
	out := make([]string, 0, len(base)+1)
	out = append(out, base[:2]...)
	out = append(out, prompt)
	return append(out, base[2:]...)
}
