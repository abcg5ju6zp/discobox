package sourcecache

import "testing"

func TestEverySpellingOfARemoteIsOneCache(t *testing.T) {
	const want = "github.com/Acme/widget"
	for _, remote := range []string{
		"https://github.com/Acme/widget",
		"https://github.com/Acme/widget.git",
		"https://GitHub.com/Acme/widget.git/",
		"https://user:token@github.com:443/Acme/widget.git",
		"http://github.com:80/Acme/widget",
		"ssh://git@github.com/Acme/widget.git",
		"ssh://git@github.com:22/Acme/widget.git",
		"git+ssh://git@github.com/Acme/widget.git",
		"git://github.com/Acme/widget.git",
		"git@github.com:Acme/widget.git",
		"github.com:Acme/widget",
		"  git@github.com:/Acme//widget.git/ ",
	} {
		if got := NormalizeRemote(remote); got != want {
			t.Errorf("NormalizeRemote(%q) = %q, want %q", remote, got, want)
		}
	}
}

func TestARemoteOnAnotherPortIsAnotherRemote(t *testing.T) {
	if got, want := NormalizeRemote("ssh://git@git.example.com:2222/team/app.git"), "git.example.com:2222/team/app"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// A path names a repository only on the machine it is a path on, so it is not
// a remote: two clients' /srv/git/app must not share a cache.
func TestAPathIsNotARemote(t *testing.T) {
	for _, remote := range []string{
		"",
		"/srv/git/app.git",
		"../app",
		"./app:odd",
		"file:///srv/git/app.git",
		`C:\src\app`,
		"C:/src/app",
		"https://github.com",
		"https://github.com/",
		"git@github.com:",
		"has space@host:path",
	} {
		if got := NormalizeRemote(remote); got != "" {
			t.Errorf("NormalizeRemote(%q) = %q, want none", remote, got)
		}
		if got := RemoteKey(remote); got != "" {
			t.Errorf("RemoteKey(%q) = %q, want none", remote, got)
		}
	}
}

func TestOnlyASandboxKeyIsForOneSandbox(t *testing.T) {
	if !ForOneSandbox(SandboxKey("sbx-1", "primary")) {
		t.Fatal("a sandbox key was not recognized")
	}
	for _, key := range []string{RemoteKey("https://github.com/sandbox/x"), DirectoryKey("abc"), ""} {
		if ForOneSandbox(key) {
			t.Errorf("%q was taken for one sandbox's cache", key)
		}
	}
	if DirectoryKey("") != "" {
		t.Fatal("no data key made a directory key")
	}
}
