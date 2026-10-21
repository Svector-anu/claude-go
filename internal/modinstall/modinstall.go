// Package modinstall puts the optional Claude Code mod where claude-go can load it.
//
// The mod is extracted into claude-go's own state directory and loaded into
// the sessions claude-go launches through CLAUDE_CODE_PLUGIN_DIRS. Nothing in
// the user's Claude configuration (settings.json, installed plugins) is read
// or changed, so installing and removing it are fully reversible.
package modinstall

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Name is the mod's plugin name (reserved prefixes such as "claude-" are refused by Claude Code).
const Name = "account-flow"

// EnvPluginDirs is the variable Claude Code reads for extra plugin folders.
const EnvPluginDirs = "CLAUDE_CODE_PLUGIN_DIRS"

// Dir is where the mod lives under the claude-go state directory.
func Dir(stateRoot string) string { return filepath.Join(stateRoot, "mod", Name) }

// shipped reports whether a file of the embedded plugin is part of the install
// (tests and engine-generated type files are not).
func shipped(p string) bool {
	for _, part := range strings.Split(p, "/") {
		if part == "tests" || part == "types" && strings.HasPrefix(p, ".claude-plugin/") {
			return false
		}
	}
	return !strings.HasSuffix(p, ".test.ts") && !strings.HasSuffix(p, ".test.tsx")
}

type manifest struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Version returns the installed mod's version, or ok=false when it is absent.
func Version(stateRoot string) (version string, ok bool, err error) {
	data, err := os.ReadFile(filepath.Join(Dir(stateRoot), ".claude-plugin", "plugin.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil || m.Name != Name {
		return "", false, fmt.Errorf("%s does not look like the claude-go mod", Dir(stateRoot))
	}
	return m.Version, true, nil
}

// Install extracts plugin into the state directory, replacing any previous
// version. It returns the installed version.
func Install(stateRoot string, plugin fs.FS) (string, error) {
	dest := Dir(stateRoot)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", fmt.Errorf("create %s: %w", filepath.Dir(dest), err)
	}
	staging, err := os.MkdirTemp(filepath.Dir(dest), "."+Name+".new-")
	if err != nil {
		return "", fmt.Errorf("stage the mod: %w", err)
	}
	defer os.RemoveAll(staging)

	err = fs.WalkDir(plugin, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == "." || !shipped(p) {
			if d != nil && d.IsDir() && p != "." && !shipped(p) {
				return fs.SkipDir
			}
			return nil
		}
		target := filepath.Join(staging, filepath.FromSlash(p))
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := fs.ReadFile(plugin, path.Clean(p))
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		return "", fmt.Errorf("extract the mod: %w", err)
	}
	m, err := os.ReadFile(filepath.Join(staging, ".claude-plugin", "plugin.json"))
	if err != nil {
		return "", fmt.Errorf("the embedded mod has no manifest: %w", err)
	}
	var mf manifest
	if err := json.Unmarshal(m, &mf); err != nil || mf.Name != Name {
		return "", errors.New("the embedded mod has an unexpected manifest")
	}
	if err := os.RemoveAll(dest); err != nil {
		return "", fmt.Errorf("remove the previous mod: %w", err)
	}
	if err := os.Rename(staging, dest); err != nil {
		return "", fmt.Errorf("activate the mod: %w", err)
	}
	return mf.Version, nil
}

// Remove deletes the installed mod. It refuses anything that is not the mod.
func Remove(stateRoot string) (removed bool, err error) {
	if _, ok, err := Version(stateRoot); err != nil || !ok {
		return false, err
	}
	if err := os.RemoveAll(Dir(stateRoot)); err != nil {
		return false, fmt.Errorf("remove the mod: %w", err)
	}
	_ = os.Remove(filepath.Dir(Dir(stateRoot))) // the mod/ folder, only if now empty
	return true, nil
}

// MergePluginDirs returns the value for CLAUDE_CODE_PLUGIN_DIRS with dir added
// to what is already set, without duplicating it.
func MergePluginDirs(existing, dir string) string {
	if existing == "" {
		return dir
	}
	for _, p := range filepath.SplitList(existing) {
		if filepath.Clean(p) == filepath.Clean(dir) {
			return existing
		}
	}
	return existing + string(os.PathListSeparator) + dir
}

// BundledVersion returns the version of the mod embedded in this binary.
func BundledVersion(plugin fs.FS) (string, error) {
	data, err := fs.ReadFile(plugin, ".claude-plugin/plugin.json")
	if err != nil {
		return "", fmt.Errorf("the embedded mod has no manifest: %w", err)
	}
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil || m.Name != Name {
		return "", errors.New("the embedded mod has an unexpected manifest")
	}
	return m.Version, nil
}

// Sync brings an installed mod up to the version bundled with this binary. It
// does nothing when the mod is not installed (the user has not opted in) or is
// already current.
func Sync(stateRoot string, plugin fs.FS) (updated bool, err error) {
	have, ok, err := Version(stateRoot)
	if err != nil || !ok {
		return false, err
	}
	want, err := BundledVersion(plugin)
	if err != nil || want == have {
		return false, err
	}
	_, err = Install(stateRoot, plugin)
	return err == nil, err
}

// MCPFile is the plugin's MCP server declaration. It is generated at install
// time because it must name the absolute path of the claude-go binary.
const MCPFile = ".mcp.json"

// mcpServerName is the key inside the file; Claude shows it as plugin:account-flow:memory.
const mcpServerName = "memory"

type mcpFile struct {
	Servers map[string]mcpServer `json:"mcpServers"`
}

type mcpServer struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

// WriteMCP declares the memory MCP server in the installed mod, pointing at
// binary. It reports whether the file changed.
func WriteMCP(stateRoot, binary string) (bool, error) {
	if _, ok, err := Version(stateRoot); err != nil || !ok {
		if err != nil {
			return false, err
		}
		return false, errors.New("the claude-go mod is not installed")
	}
	data, err := json.MarshalIndent(mcpFile{Servers: map[string]mcpServer{mcpServerName: {Command: binary, Args: []string{"mcp-memory"}}}}, "", "  ")
	if err != nil {
		return false, fmt.Errorf("encode %s: %w", MCPFile, err)
	}
	data = append(data, '\n')
	path := filepath.Join(Dir(stateRoot), MCPFile)
	if have, err := os.ReadFile(path); err == nil && string(have) == string(data) {
		return false, nil
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return false, fmt.Errorf("write %s: %w", MCPFile, err)
	}
	return true, nil
}

// MCPBinary returns the binary the installed MCP declaration runs.
func MCPBinary(stateRoot string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(Dir(stateRoot), MCPFile))
	if err != nil {
		return "", false
	}
	var f mcpFile
	if json.Unmarshal(data, &f) != nil {
		return "", false
	}
	s, ok := f.Servers[mcpServerName]
	return s.Command, ok && s.Command != ""
}
