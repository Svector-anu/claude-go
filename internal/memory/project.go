package memory

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// GlobalScope holds facts that apply everywhere (preferences, general knowledge).
const GlobalScope = "global"

const projectPrefix = "project:"

// ProjectScope derives a project's scope from its working directory without
// running any program. A git repository is identified by its normalised
// `origin` URL (so the same repository is the same project on every machine,
// whatever folder it was cloned into); anything else falls back to a hash of
// the directory, with the folder name as the display name.
func ProjectScope(cwd string) (scope, name string) {
	if cwd == "" {
		return "", ""
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return "", ""
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	root, gitDir := findGit(abs)
	if root != "" {
		if id := NormalizeRemote(originURL(gitDir)); id != "" {
			return projectPrefix + id, filepath.Base(id)
		}
		abs = root
	}
	sum := sha256.Sum256([]byte(abs))
	return projectPrefix + "dir:" + hex.EncodeToString(sum[:8]), filepath.Base(abs)
}

// findGit returns the repository root above dir and the directory holding its
// config. For a worktree or submodule `.git` is a file pointing at the real
// git directory; its `commondir` leads to the shared config.
func findGit(dir string) (root, gitDir string) {
	for d := dir; ; d = filepath.Dir(d) {
		dotGit := filepath.Join(d, ".git")
		fi, err := os.Stat(dotGit)
		if err == nil && fi.IsDir() {
			return d, dotGit
		}
		if err == nil && fi.Mode().IsRegular() {
			return d, resolveGitFile(d, dotGit)
		}
		if filepath.Dir(d) == d {
			return "", ""
		}
	}
}

func resolveGitFile(root, dotGit string) string {
	data, err := os.ReadFile(dotGit)
	if err != nil {
		return ""
	}
	target, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir:")
	if !ok {
		return ""
	}
	gitDir := strings.TrimSpace(target)
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(root, gitDir)
	}
	if common, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
		c := strings.TrimSpace(string(common))
		if !filepath.IsAbs(c) {
			c = filepath.Join(gitDir, c)
		}
		return filepath.Clean(c)
	}
	return gitDir
}

var remoteHeader = regexp.MustCompile(`^\[remote "origin"\]$`)

// originURL reads remote.origin.url from <gitDir>/config.
func originURL(gitDir string) string {
	if gitDir == "" {
		return ""
	}
	f, err := os.Open(filepath.Join(gitDir, "config"))
	if err != nil {
		return ""
	}
	defer f.Close()
	in := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "["):
			in = remoteHeader.MatchString(line)
		case in:
			if k, v, ok := strings.Cut(line, "="); ok && strings.TrimSpace(k) == "url" {
				return strings.TrimSpace(v)
			}
		}
	}
	return ""
}

var scpLike = regexp.MustCompile(`^(?:[^@/]+@)?([^:/]+):(.+)$`)

// NormalizeRemote turns a git remote URL into host/owner/repo, lower case,
// without credentials, scheme, port or ".git". It returns "" for anything it
// cannot make sense of.
func NormalizeRemote(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var host, path string
	if i := strings.Index(raw, "://"); i >= 0 {
		rest := raw[i+3:]
		hostPart, p, found := strings.Cut(rest, "/")
		if !found {
			return ""
		}
		if at := strings.LastIndex(hostPart, "@"); at >= 0 {
			hostPart = hostPart[at+1:]
		}
		if colon := strings.Index(hostPart, ":"); colon >= 0 {
			hostPart = hostPart[:colon]
		}
		host, path = hostPart, p
	} else if m := scpLike.FindStringSubmatch(raw); m != nil && !strings.HasPrefix(raw, "/") && !strings.HasPrefix(raw, ".") {
		host, path = m[1], m[2]
	} else {
		return ""
	}
	path = strings.Trim(strings.TrimSuffix(strings.Trim(path, "/"), ".git"), "/")
	if host == "" || path == "" {
		return ""
	}
	return strings.ToLower(host + "/" + path)
}
