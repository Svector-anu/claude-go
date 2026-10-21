package claudeargs

type arity int

const (
	arityNone     arity = iota // boolean flag
	arityValue                 // <value>: consumes the next token
	arityOptional              // [value]: consumes the next token unless it starts with '-'
	arityVariadic              // <values...>: consumes following tokens until one starts with '-'
)

// flagArity mirrors `claude --help` for Claude Code 2.1.304. Short aliases are
// listed under their own key. TestFlagTableCoversInstalledClaude fails when a
// newer Claude Code adds a flag this table does not know.
var flagArity = map[string]arity{
	"--allow-dangerously-skip-permissions": arityNone,
	"--ax-screen-reader":                   arityNone,
	"--bg":                                 arityNone,
	"--background":                         arityNone,
	"--bare":                               arityNone,
	"--brief":                              arityNone,
	"--chrome":                             arityNone,
	"--continue":                           arityNone,
	"-c":                                   arityNone,
	"--dangerously-skip-permissions":       arityNone,
	"--desktop":                            arityNone,
	"--disable-slash-commands":             arityNone,
	"--exclude-dynamic-system-prompt-sections": arityNone,
	"--fork-session":             arityNone,
	"--forward-subagent-text":    arityNone,
	"--help":                     arityNone,
	"-h":                         arityNone,
	"--ide":                      arityNone,
	"--include-hook-events":      arityNone,
	"--include-partial-messages": arityNone,
	"--no-chrome":                arityNone,
	"--no-session-persistence":   arityNone,
	"--print":                    arityNone,
	"-p":                         arityNone,
	"--replay-user-messages":     arityNone,
	"--restricted":               arityNone,
	"--safe-mode":                arityNone,
	"--strict-mcp-config":        arityNone,
	"--tmux":                     arityNone,
	"--verbose":                  arityNone,
	"--version":                  arityNone,
	"-v":                         arityNone,

	"--agent":                              arityValue,
	"--agents":                             arityValue,
	"--append-system-prompt":               arityValue,
	"--autocompact":                        arityValue,
	"--debug-file":                         arityValue,
	"--effort":                             arityValue,
	"--environment":                        arityValue,
	"--fallback-model":                     arityValue,
	"--input-format":                       arityValue,
	"--json-schema":                        arityValue,
	"--max-budget-usd":                     arityValue,
	"--model":                              arityValue,
	"--name":                               arityValue,
	"-n":                                   arityValue,
	"--output-format":                      arityValue,
	"--permission-mode":                    arityValue,
	"--permission-prompts":                 arityValue,
	"--plugin-dir":                         arityValue,
	"--plugin-url":                         arityValue,
	"--remote-control-session-name-prefix": arityValue,
	"--session-id":                         arityValue,
	"--setting-sources":                    arityValue,
	"--settings":                           arityValue,
	"--system-prompt":                      arityValue,
	"--system-prompt-snapshot":             arityValue,

	"--add-dir":          arityVariadic,
	"--allowedTools":     arityVariadic,
	"--allowed-tools":    arityVariadic,
	"--disallowedTools":  arityVariadic,
	"--disallowed-tools": arityVariadic,
	"--betas":            arityVariadic,
	"--file":             arityVariadic,
	"--mcp-config":       arityVariadic,
	"--tools":            arityVariadic,

	"--cloud":              arityOptional,
	"--debug":              arityOptional,
	"-d":                   arityOptional,
	"--from-pr":            arityOptional,
	"--prompt-suggestions": arityOptional,
	"--remote-control":     arityOptional,
	"--resume":             arityOptional,
	"-r":                   arityOptional,
	"--teleport":           arityOptional,
	"--worktree":           arityOptional,
	"-w":                   arityOptional,
}

// subcommands are Claude Code's non-session commands. Seeing one anywhere in
// argv makes the whole invocation a passthrough, which fails safe.
var subcommands = map[string]bool{
	"agents": true, "attach": true, "auth": true, "auto-mode": true, "doctor": true,
	"gateway": true, "import": true, "install": true, "logs": true, "mcp": true,
	"plugin": true, "plugins": true, "purge": true, "respawn": true, "rm": true,
	"setup-token": true, "stop": true, "kill": true, "update": true, "upgrade": true,
}

// handoffBlockers cannot be restarted by replaying the same flags.
var handoffBlockers = map[string]string{
	"--bg":                     "background sessions are managed by Claude Code, not a child process",
	"--background":             "background sessions are managed by Claude Code, not a child process",
	"--tmux":                   "tmux sessions outlive the child process",
	"--worktree":               "relaunching would create another worktree",
	"-w":                       "relaunching would create another worktree",
	"--cloud":                  "cloud sessions are not local transcripts",
	"--teleport":               "teleported sessions are not local transcripts",
	"--from-pr":                "relaunching would re-resolve the pull request",
	"--remote-control":         "remote-control sessions are bound to the running process",
	"--desktop":                "the session runs in the desktop app",
	"--no-session-persistence": "the session writes no transcript to resume",
}
