// Package claudejson carries a user's own first-run decisions from their
// default Claude Code state file into a claude-go profile's.
//
// A profile starts with an empty state file, so its first interactive launch
// would run Claude's onboarding wizard and ask again about every folder the
// user already trusted, which blocks an automatic handoff. Seed copies only
// those decisions, only where the profile has none, and never anything that
// identifies an account or a machine.
package claudejson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"claude-go/internal/fsx"
)

// topLevel are the flags copied when the profile lacks them. Nothing about
// identity (oauthAccount, userID, machineID), usage or caches is on this list.
var topLevel = []string{"hasCompletedOnboarding", "lastOnboardingVersion", "hasSeenTasksHint"}

// projectFlags are the per-folder decisions copied (only when true in the source).
var projectFlags = []string{"hasTrustDialogAccepted", "hasCompletedProjectOnboarding"}

type object = map[string]json.RawMessage

func readObject(path string) (object, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return object{}, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read %s: %w", path, err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return object{}, true, nil
	}
	var o object
	if err := json.Unmarshal(data, &o); err != nil || o == nil {
		return nil, true, fmt.Errorf("%s is not a JSON object; claude-go will not modify it", path)
	}
	return o, true, nil
}

func isTrue(raw json.RawMessage) bool { return string(bytes.TrimSpace(raw)) == "true" }

// Seed adds to dst the onboarding and folder-trust decisions recorded in src.
// It reports whether dst changed. A missing src, or a dst that is being written
// by Claude right now (its lock exists), is left alone.
func Seed(src, dst string) (bool, error) {
	from, ok, err := readObject(src)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	if _, err := os.Stat(dst + ".lock"); err == nil {
		return false, nil
	}
	to, dstExists, err := readObject(dst)
	if err != nil {
		return false, err
	}

	changed := false
	for _, k := range topLevel {
		if v, has := from[k]; has {
			if _, already := to[k]; !already {
				to[k], changed = v, true
			}
		}
	}

	var srcProjects, dstProjects map[string]json.RawMessage
	if raw, has := from["projects"]; has {
		_ = json.Unmarshal(raw, &srcProjects) // an unreadable map simply contributes nothing
	}
	if raw, has := to["projects"]; has {
		if err := json.Unmarshal(raw, &dstProjects); err != nil {
			return false, fmt.Errorf("%s: \"projects\" is not an object; claude-go will not modify it", dst)
		}
	}
	for path, entryRaw := range srcProjects {
		var entry object
		if json.Unmarshal(entryRaw, &entry) != nil {
			continue
		}
		var target object
		if raw, has := dstProjects[path]; has {
			if json.Unmarshal(raw, &target) != nil {
				continue
			}
		}
		for _, flag := range projectFlags {
			if v, has := entry[flag]; has && isTrue(v) && !isTrue(target[flag]) {
				if target == nil {
					target = object{}
				}
				target[flag] = v
				changed = true
				if dstProjects == nil {
					dstProjects = map[string]json.RawMessage{}
				}
				enc, err := json.Marshal(target)
				if err != nil {
					return false, fmt.Errorf("encode project %s: %w", path, err)
				}
				dstProjects[path] = enc
			}
		}
	}
	if !changed {
		return false, nil
	}
	if dstProjects != nil {
		enc, err := json.Marshal(dstProjects)
		if err != nil {
			return false, fmt.Errorf("encode projects: %w", err)
		}
		to["projects"] = enc
	}
	return true, writeObject(dst, to, dstExists)
}

// TrustFolder records in dst that folder is trusted, unless it already is.
// Claude never saves trust for a home folder, and in a profile the user's own
// ~/.claude/settings.json would be read there as that folder's project
// settings, so the trust dialog would open on every handoff started from home.
// The caller names a folder only when the settings that dialog warns about are
// the user's own. It reports whether dst changed; a missing or busy dst is
// left alone like Seed's.
func TrustFolder(dst, folder string) (bool, error) {
	if _, err := os.Stat(dst + ".lock"); err == nil {
		return false, nil
	}
	to, dstExists, err := readObject(dst)
	if err != nil {
		return false, err
	}
	var projects map[string]json.RawMessage
	if raw, has := to["projects"]; has {
		if err := json.Unmarshal(raw, &projects); err != nil {
			return false, fmt.Errorf("%s: \"projects\" is not an object; claude-go will not modify it", dst)
		}
	}
	var entry object
	if raw, has := projects[folder]; has {
		if err := json.Unmarshal(raw, &entry); err != nil {
			return false, fmt.Errorf("%s: the entry for %s is not an object; claude-go will not modify it", dst, folder)
		}
	}
	if isTrue(entry["hasTrustDialogAccepted"]) {
		return false, nil
	}
	if entry == nil {
		entry = object{}
	}
	entry["hasTrustDialogAccepted"] = json.RawMessage("true")
	enc, err := json.Marshal(entry)
	if err != nil {
		return false, fmt.Errorf("encode project %s: %w", folder, err)
	}
	if projects == nil {
		projects = map[string]json.RawMessage{}
	}
	projects[folder] = enc
	all, err := json.Marshal(projects)
	if err != nil {
		return false, fmt.Errorf("encode projects: %w", err)
	}
	to["projects"] = all
	return true, writeObject(dst, to, dstExists)
}

func writeObject(dst string, o object, dstExists bool) error {
	out, err := json.MarshalIndent(o, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", dst, err)
	}
	mode := os.FileMode(0o600)
	if fi, err := os.Stat(dst); err == nil && dstExists {
		mode = fi.Mode().Perm()
	}
	return fsx.WriteFileAtomic(dst, append(out, '\n'), mode)
}
