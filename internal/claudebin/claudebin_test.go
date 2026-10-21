package claudebin

import (
	"context"
	"errors"
	"testing"
)

type fakeRunner struct {
	out []byte
	err error
}

func (f fakeRunner) Output(context.Context, []string, string, ...string) ([]byte, error) {
	return f.out, f.err
}

func TestLocate(t *testing.T) {
	look := func(name string) (string, error) {
		if name == "claude" || name == "/pinned/claude" {
			return "/resolved/" + name, nil
		}
		return "", errors.New("missing")
	}
	if got, err := Locate("", look); err != nil || got != "/resolved/claude" {
		t.Errorf("PATH lookup: got %q, %v", got, err)
	}
	if got, err := Locate("/pinned/claude", look); err != nil || got != "/resolved//pinned/claude" {
		t.Errorf("configured: got %q, %v", got, err)
	}
	if _, err := Locate("/nope", look); !errors.Is(err, ErrNotFound) {
		t.Errorf("configured miss: %v", err)
	}
	if _, err := Locate("", func(string) (string, error) { return "", errors.New("x") }); !errors.Is(err, ErrNotFound) {
		t.Errorf("PATH miss: %v", err)
	}
}

func TestVersion(t *testing.T) {
	v, err := Version(context.Background(), fakeRunner{out: []byte("2.1.304 (Claude Code)\n")}, "claude")
	if err != nil || v != "2.1.304 (Claude Code)" {
		t.Errorf("got %q, %v", v, err)
	}
	if _, err := Version(context.Background(), fakeRunner{err: errors.New("boom")}, "claude"); err == nil {
		t.Error("expected runner error")
	}
	if _, err := Version(context.Background(), fakeRunner{out: []byte("  \n")}, "claude"); err == nil {
		t.Error("expected empty-output error")
	}
}

func TestLoggedIn(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name    string
		r       fakeRunner
		want    bool
		wantErr bool
	}{
		{"logged in", fakeRunner{out: []byte(`{"loggedIn":true}`)}, true, false},
		{"logged out", fakeRunner{out: []byte(`{"loggedIn":false,"authMethod":"none"}`)}, false, false},
		{"logged out with nonzero exit", fakeRunner{out: []byte(`{"loggedIn":false}`), err: errors.New("exit 1")}, false, false},
		{"garbage", fakeRunner{out: []byte("hello")}, false, true},
		{"exec failure", fakeRunner{err: errors.New("boom")}, false, true},
		{"missing field", fakeRunner{out: []byte(`{}`)}, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := LoggedIn(ctx, c.r, nil, "claude")
			if got != c.want || (err != nil) != c.wantErr {
				t.Errorf("got %v, %v", got, err)
			}
		})
	}
}
