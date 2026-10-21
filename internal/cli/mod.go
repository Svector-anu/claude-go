package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/Svector-anu/claude-go/integrations/claude-code"
	"github.com/Svector-anu/claude-go/internal/fsx"
	"github.com/Svector-anu/claude-go/internal/modinstall"
)

// installMod puts the optional mod in claude-go's own state directory. Claude
// Code's settings and installed plugins are not touched; claude-go loads the
// mod into the sessions it launches.
func (s *setup) installMod(string) {
	plugin, err := claudecode.Plugin()
	if err != nil {
		s.row(symWarn, "Companion", "not installed: %v", err)
		return
	}
	version, err := modinstall.Install(s.layout.Root, plugin)
	if err != nil {
		s.row(symWarn, "Companion", "not installed: %v", err)
		return
	}
	_ = version
	s.row(symOK, "Companion", "/accounts · /switch · /flow inside Claude")
}

func (s *setup) uninstallMod() error {
	removed, err := modinstall.Remove(s.layout.Root)
	switch {
	case err != nil:
		s.row(symFail, "Companion", "not removed: %v", err)
	case removed:
		s.row(symOK, "Companion", "removed")
	}
	return err
}

func (a *app) describeMod() string {
	have, ok, err := modinstall.Version(a.layout.Root)
	switch {
	case err != nil:
		return "unknown (" + err.Error() + ")"
	case !ok:
		return "not installed (optional: claude-go setup --mod)"
	}
	if plugin, perr := claudecode.Plugin(); perr == nil {
		if bundled, berr := modinstall.BundledVersion(plugin); berr == nil && bundled != have {
			return "v" + have + " installed; v" + bundled + " is bundled and loads on the next run"
		}
	}
	return "v" + have + " installed; loads in sessions started by claude-go"
}

// syncMod keeps an installed mod in step with the binary. It is silent: the mod
// is optional, and a failure here must never get in the way of starting Claude.
func (a *app) syncMod() (pluginDir string) {
	plugin, err := claudecode.Plugin()
	if err != nil {
		return ""
	}
	_, _ = modinstall.Sync(a.layout.Root, plugin) // best effort; status reports a stale mod
	if _, ok, _ := modinstall.Version(a.layout.Root); !ok {
		return ""
	}
	if binary, on := a.memoryEnabled(); on && binary != "" {
		if _, err := os.Stat(binary); err == nil {
			_, _ = modinstall.WriteMCP(a.layout.Root, binary) // an upgrade replaces the folder; put the declaration back
		}
	}
	return modinstall.Dir(a.layout.Root)
}

// memoryMarker records that the user turned the memory on, and which binary
// serves it. It lives beside the mod, not inside it, so a mod upgrade (which
// replaces the whole folder) cannot lose the choice.
func (a *app) memoryMarker() string { return filepath.Join(a.layout.Root, "memory.enabled") }

func (a *app) memoryEnabled() (binary string, on bool) {
	data, err := os.ReadFile(a.memoryMarker())
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(data)), true
}

// enableMemory declares the memory MCP server in the mod and remembers the choice.
func (a *app) enableMemory(binary string) error {
	if _, err := modinstall.WriteMCP(a.layout.Root, binary); err != nil {
		return err
	}
	return fsx.WriteFileAtomic(a.memoryMarker(), []byte(binary+"\n"), 0o600)
}

func (a *app) disableMemory() (removed bool, err error) {
	err = os.Remove(a.memoryMarker())
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func (s *setup) installMemory(binary string) {
	if binary == "" {
		s.row(symWarn, "Memory", "not enabled: no claude-go executable path")
		return
	}
	if _, ok, _ := modinstall.Version(s.layout.Root); !ok {
		s.installMod("")
	}
	if err := s.enableMemory(binary); err != nil {
		s.row(symWarn, "Memory", "not enabled: %v", err)
		return
	}
	dir, err := s.memoryDir()
	if err != nil {
		dir = "~/.claude-go/memory"
	}
	s.row(symOK, "Memory", "Claude can save and recall project notes (%s)", s.tilde(dir))
}

func (a *app) describeMemory() string {
	if _, on := a.memoryEnabled(); !on {
		return "off (optional: claude-go setup --memory)"
	}
	dir, err := a.memoryDir()
	if err != nil {
		return "on, but " + err.Error()
	}
	store, err := a.memoryStore()
	if err != nil {
		return "on, but " + err.Error()
	}
	facts, err := store.Facts()
	if err != nil {
		return "on, but " + err.Error()
	}
	state := ""
	if store.Cipher != nil {
		state = ", encrypted"
	}
	locked, _ := store.Locked()
	if locked > 0 {
		state += fmt.Sprintf(", %d unreadable (no key)", locked)
	}
	return fmt.Sprintf("on, %d note(s) in %s%s", len(facts), dir, state)
}
