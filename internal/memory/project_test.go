package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func gitRepo(t *testing.T, remote string) string {
	t.Helper()
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	cfg := "[core]\n\trepositoryformatversion = 0\n"
	if remote != "" {
		cfg += "[remote \"origin\"]\n\turl = " + remote + "\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n"
	}
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "config"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestNormalizeRemote(t *testing.T) {
	cases := map[string]string{
		"https://github.com/Owner/Repo.git":        "github.com/owner/repo",
		"https://user:tok@github.com/owner/repo":   "github.com/owner/repo",
		"git@github.com:owner/repo.git":            "github.com/owner/repo",
		"ssh://git@github.com:2222/owner/repo.git": "github.com/owner/repo",
		"https://gitlab.com/group/sub/repo.git/":   "gitlab.com/group/sub/repo",
		"github.com:owner/repo":                    "github.com/owner/repo",
		"/home/me/local/bare.git":                  "",
		"":                                         "",
		"https://github.com":                       "",
		"../relative":                              "",
	}
	for in, want := range cases {
		if got := NormalizeRemote(in); got != want {
			t.Errorf("NormalizeRemote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTheSameRepositoryIsTheSameProjectWhereverItIsCloned(t *testing.T) {
	a := gitRepo(t, "git@github.com:Owner/app.git")
	b := gitRepo(t, "https://github.com/owner/app")
	sa, na := ProjectScope(a)
	sb, _ := ProjectScope(b)
	if sa != "project:github.com/owner/app" || sa != sb || na != "app" {
		t.Errorf("a=%q b=%q name=%q", sa, sb, na)
	}
	if err := ValidateScope(sa); err != nil {
		t.Errorf("derived scope is not valid: %v", err)
	}
}

func TestASubdirectoryBelongsToItsRepository(t *testing.T) {
	root := gitRepo(t, "https://github.com/o/r.git")
	sub := filepath.Join(root, "internal", "deep")
	os.MkdirAll(sub, 0o755)
	want, _ := ProjectScope(root)
	if got, _ := ProjectScope(sub); got != want {
		t.Errorf("subdir scope %q != root scope %q", got, want)
	}
}

func TestWithoutARemoteTheDirectoryIsTheIdentityAndStaysDistinct(t *testing.T) {
	a, b := gitRepo(t, ""), gitRepo(t, "")
	sa, na := ProjectScope(a)
	sb, _ := ProjectScope(b)
	if !strings.HasPrefix(sa, "project:dir:") || sa == sb || na != filepath.Base(a) {
		t.Errorf("sa=%q sb=%q name=%q", sa, sb, na)
	}
	if again, _ := ProjectScope(a); again != sa {
		t.Error("identity is not stable")
	}
	plain := t.TempDir()
	if s, _ := ProjectScope(plain); !strings.HasPrefix(s, "project:dir:") {
		t.Errorf("non-git dir scope = %q", s)
	}
	if err := ValidateScope(sa); err != nil {
		t.Errorf("fallback scope invalid: %v", err)
	}
}

func TestAWorktreeSharesItsMainRepositorysIdentity(t *testing.T) {
	main := gitRepo(t, "https://github.com/o/r.git")
	gitdir := filepath.Join(main, ".git", "worktrees", "feat")
	os.MkdirAll(gitdir, 0o755)
	os.WriteFile(filepath.Join(gitdir, "commondir"), []byte("../..\n"), 0o644)
	wt := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(wt); err == nil {
		wt = resolved
	}
	os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+gitdir+"\n"), 0o644)
	want, _ := ProjectScope(main)
	if got, _ := ProjectScope(wt); got != want {
		t.Errorf("worktree scope %q != main scope %q", got, want)
	}
}

func TestSymlinkedDirectoriesResolveToTheSameProject(t *testing.T) {
	root := gitRepo(t, "https://github.com/o/r.git")
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(root, link); err != nil {
		t.Skip("symlinks unavailable")
	}
	a, _ := ProjectScope(root)
	b, _ := ProjectScope(link)
	if a != b {
		t.Errorf("%q != %q", a, b)
	}
}

func TestEmptyDirectoryMeansNoProject(t *testing.T) {
	if s, n := ProjectScope(""); s != "" || n != "" {
		t.Errorf("got %q %q", s, n)
	}
}
