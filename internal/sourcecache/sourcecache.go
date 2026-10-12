// Package sourcecache names the pool cache a sandbox's Git source is
// delivered into (ADR 26-10-10-545 §2). The control plane forms the key when
// a sandbox is created and the pool agent keeps one bare repository per key,
// so both read it through here.
//
// A key is one of three kinds, and the kind is its first path element:
//
//   - remote/<host>/<path> — the source's remote, normalized, so every user
//     of a project and every spelling of one repository share a cache;
//   - directory/<key> — a source with no remote, by its client's host
//     identity and directory;
//   - sandbox/<sandboxID>/<slug> — a source with neither, which shares with
//     nothing and is removed with its sandbox.
package sourcecache

import (
	"net/url"
	"path"
	"strings"
)

const (
	remotePrefix    = "remote/"
	directoryPrefix = "directory/"
	sandboxPrefix   = "sandbox/"
)

// How the control plane addresses a pool's cache when it forwards a client's
// request. The route a client uses names a sandbox and a source slug (ADR
// 0058 §3); which cache that is, and whose request it is, are the control
// plane's to say, so a pool needs no record of a sandbox to take a delivery
// for it (ADR 26-10-10-545 §3).
const (
	// PoolOriginRoute is the pool agent's route for a source's namespace of
	// its cache: a delivery into it, and the pin that precedes one.
	PoolOriginRoute = "git-cache-origins"
	// PoolRemoteRoute is the pool agent's route for what a cache remembers
	// of its remote, fetch-only.
	PoolRemoteRoute = "git-cache-remotes"
	// PinSuffix follows "<slug>.git" on the origin routes for the one request
	// there that is not git's own: the client names the refs of a delivery
	// and the object each points at, and the pool creates those its cache
	// already holds.
	PinSuffix = "/pin"
	// KeyHeader carries the cache's key.
	KeyHeader = "X-Discobox-Git-Cache"
	// CallerHeader carries the id of the sandbox making the request, when one
	// is: a discobox delivering the source of a discobox it created, which a
	// pool holds to what it sends and what its own sources hold (§7).
	CallerHeader = "X-Discobox-Git-Cache-Caller"
)

// RemoteKey is the key of the cache for remote, or empty when remote does not
// name a repository on a network: a path on the client's machine means
// nothing on another, and is not a remote here.
func RemoteKey(remote string) string {
	normalized := NormalizeRemote(remote)
	if normalized == "" {
		return ""
	}
	return remotePrefix + normalized
}

// DirectoryKey is the key of the cache for a source with no remote, by the
// key that already names its pool-local data (originkey.Of). Empty in, empty
// out: a client with no host identity has no such key.
func DirectoryKey(dataKey string) string {
	if dataKey == "" {
		return ""
	}
	return directoryPrefix + dataKey
}

// SandboxKey is the key of a cache made for one source of one sandbox.
func SandboxKey(sandboxID, slug string) string {
	return sandboxPrefix + sandboxID + "/" + slug
}

// ForOneSandbox reports whether key names a cache made for one sandbox's
// source alone, which nothing outlives.
func ForOneSandbox(key string) bool {
	return strings.HasPrefix(key, sandboxPrefix)
}

// defaultPorts are the ports a remote's scheme implies, which a URL may spell
// out or leave off.
var defaultPorts = map[string]string{"https": "443", "http": "80", "ssh": "22", "git": "9418"}

// NormalizeRemote reduces a remote's URL to its host and path — lower-cased
// host, no scheme, user, default port, trailing slash or ".git" — so the
// HTTPS and SSH spellings of one repository are one remote. It is empty for
// anything that is not a network remote.
func NormalizeRemote(remote string) string {
	remote = strings.TrimSpace(remote)
	if remote == "" || strings.ContainsAny(remote, " \t\r\n\x00") {
		return ""
	}
	host, port, repository := "", "", ""
	if strings.Contains(remote, "://") {
		parsed, err := url.Parse(remote)
		if err != nil {
			return ""
		}
		scheme := strings.ToLower(parsed.Scheme)
		scheme = strings.TrimPrefix(strings.TrimSuffix(scheme, "+ssh"), "ssh+")
		if scheme == "" {
			scheme = "ssh"
		}
		implied, ok := defaultPorts[scheme]
		if !ok {
			return ""
		}
		host, port, repository = parsed.Hostname(), parsed.Port(), parsed.Path
		if port == implied {
			port = ""
		}
	} else {
		// git's scp-like form, [user@]host:path. A colon after the first
		// slash is part of a path, and a one-letter host is a Windows drive.
		colon := strings.IndexByte(remote, ':')
		if colon <= 0 || strings.ContainsAny(remote[:colon], `/\`) {
			return ""
		}
		host, repository = remote[:colon], remote[colon+1:]
		if at := strings.LastIndexByte(host, '@'); at >= 0 {
			host = host[at+1:]
		}
		if len(host) < 2 || strings.HasPrefix(repository, `\`) {
			return ""
		}
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	repository = strings.TrimSuffix(strings.Trim(path.Clean("/"+repository), "/"), ".git")
	repository = strings.TrimRight(repository, "/")
	if host == "" || repository == "" || repository == "." {
		return ""
	}
	if port != "" {
		host += ":" + port
	}
	return host + "/" + repository
}
