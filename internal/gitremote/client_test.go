package gitremote

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// verifies: strict structural URL classification and GitHub fallback grammar.
func TestParseEndpointStrictGrammar(t *testing.T) {
	tests := []struct {
		name, raw, scheme, host, owner, repo string
		candidate, unsupported, invalid      bool
	}{
		{name: "scp", raw: "git@github.com:owner/repo.git", scheme: "ssh", host: "github.com", owner: "owner", repo: "repo", candidate: true},
		{name: "ssh URL", raw: "ssh://git@github.com/owner/repo", scheme: "ssh", host: "github.com", owner: "owner", repo: "repo", candidate: true},
		{name: "https", raw: "https://github.com/owner/repo.git", scheme: "https", host: "github.com", owner: "owner", repo: "repo", candidate: true},
		{name: "case and trailing dot", raw: "ssh://git@GitHub.com./owner/repo", scheme: "ssh", host: "github.com", owner: "owner", repo: "repo", candidate: true},
		{name: "suffix spoof", raw: "git@github.com.evil.example:owner/repo", scheme: "ssh", host: "github.com.evil.example"},
		{name: "dash spoof", raw: "git@github.com-x:owner/repo", scheme: "ssh", host: "github.com-x"},
		{name: "prefix spoof", raw: "git@evilgithub.com:owner/repo", scheme: "ssh", host: "evilgithub.com"},
		{name: "authority spoof", raw: "ssh://github.com@evil/owner/repo", scheme: "ssh", host: "evil"},
		{name: "punycode", raw: "git@xn--github-9za.com:owner/repo", scheme: "ssh", host: "xn--github-9za.com"},
		{name: "non git user", raw: "ssh://mark@github.com/owner/repo", scheme: "ssh", host: "github.com"},
		{name: "nondefault ssh port", raw: "ssh://git@github.com:2222/owner/repo", scheme: "ssh", host: "github.com"},
		{name: "default ssh port", raw: "ssh://git@github.com:22/owner/repo", scheme: "ssh", host: "github.com", owner: "owner", repo: "repo", candidate: true},
		{name: "default https port", raw: "https://github.com:443/owner/repo", scheme: "https", host: "github.com", owner: "owner", repo: "repo", candidate: true},
		{name: "userinfo", raw: "https://user:token@github.com/owner/repo", scheme: "https", host: "github.com"},
		{name: "ssh password userinfo", raw: "ssh://git:password@github.com/owner/repo", scheme: "ssh", host: "github.com"},
		{name: "gist host", raw: "https://gist.github.com/owner/repo", scheme: "https", host: "gist.github.com"},
		{name: "extra segment", raw: "git@github.com:owner/repo/extra", scheme: "ssh", host: "github.com"},
		{name: "duplicate git suffix", raw: "git@github.com:owner/repo.git.git", scheme: "ssh", host: "github.com"},
		{name: "dot segment", raw: "git@github.com:owner/../repo", scheme: "ssh", host: "github.com"},
		{name: "percent", raw: "https://github.com/owner/re%70o", scheme: "https", host: "github.com"},
		{name: "query", raw: "https://github.com/owner/repo?q=x", scheme: "https", host: "github.com"},
		{name: "fragment", raw: "https://github.com/owner/repo#x", scheme: "https", host: "github.com"},
		{name: "duplicate slash", raw: "https://github.com/owner//repo", scheme: "https", host: "github.com"},
		{name: "trailing slash", raw: "https://github.com/owner/repo/", scheme: "https", host: "github.com"},
		{name: "git scheme", raw: "git://github.com/owner/repo", unsupported: true},
		{name: "http", raw: "http://github.com/owner/repo", unsupported: true},
		{name: "file", raw: "file:///repo", unsupported: true},
		{name: "local", raw: "../repo", unsupported: true},
		{name: "control", raw: "git@github.com:owner/repo\n", invalid: true},
		{name: "unicode", raw: "git@githüb.com:owner/repo", invalid: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ep, err := parseEndpoint(tt.raw)
			if tt.invalid {
				if errorCode(err) != CodeArgInvalid {
					t.Fatalf("error=%v", err)
				}
				return
			}
			if tt.unsupported {
				if errorCode(err) != CodeRemoteUnsupported {
					t.Fatalf("error=%v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if ep.Scheme != tt.scheme || ep.Host != tt.host || ep.Owner != tt.owner || ep.Repo != tt.repo || ep.Candidate != tt.candidate {
				t.Fatalf("endpoint=%+v", ep)
			}
			if strings.Contains(ep.Redacted, "token") || strings.Contains(redact(tt.raw), "token") {
				t.Fatalf("secret surfaced endpoint=%q redact=%q", ep.Redacted, redact(tt.raw))
			}
		})
	}
}

// verifies: SSH auth classification is positive, publickey-anchored, host-scoped, and accepts git's wrapped exit 128.
// Exercises sshAuthFailureFor — the SAME function the Run decision flow uses (not a look-alike).
func TestSSHAuthClassifierAtGitExit128(t *testing.T) {
	ep := endpoint{Host: "github.com", User: "git"}
	positive := []string{
		"git@github.com: Permission denied (publickey).",
		"Permission denied (publickey,password).",
		"Permission denied (publickey).\nfatal: Could not read from remote repository.",
	}
	for _, stderr := range positive {
		if !sshAuthFailureFor(128, stderr, ep) {
			t.Errorf("did not classify %q", stderr)
		}
		if sshAuthFailureFor(0, stderr, ep) {
			t.Errorf("classified successful process %q", stderr)
		}
	}
	negative := []string{
		"Host key verification failed",
		"Connection timed out",
		"Could not resolve hostname github.com",
		"Repository not found",
		"Could not read from remote repository",
		"remote: Invalid username or password",
		"remote: Support for password authentication was removed",
		"fatal: Authentication failed",
		"git@github.com: Permission denied",
		"remote: Permission denied (publickey).",
		"warning Permission denied (publickey).",
		// host-scoped: a publickey rejection attributed to a DIFFERENT host must not be
		// read as our endpoint's auth failure (the unscoped look-alike would accept it).
		"git@evil.example: Permission denied (publickey).",
	}
	for _, stderr := range negative {
		if sshAuthFailureFor(128, stderr, ep) {
			t.Errorf("false positive %q", stderr)
		}
	}
}

// verifies: git URL rewriting is longest-prefix, push-preferred, case-sensitive, and single-pass.
func TestApplyRewriteExactlyOnce(t *testing.T) {
	rules := []rewriteRule{
		{Base: "ssh://git@github.com/", Prefix: "gh:", Push: false},
		{Base: "ssh://git@github.com/team/", Prefix: "gh:team/", Push: false},
		{Base: "https://github.com/", Prefix: "ssh://git@github.com/", Push: false},
		{Base: "ssh://push@github.com/", Prefix: "gh:", Push: true},
	}
	if got := applyRewrite("gh:team/repo", rules, false); got != "ssh://git@github.com/team/repo" {
		t.Fatalf("longest got %q", got)
	}
	if got := applyRewrite("gh:owner/repo", rules, false); got != "ssh://git@github.com/owner/repo" {
		t.Fatalf("single pass got %q", got)
	}
	if got := applyRewrite("gh:owner/repo", rules, true); got != "ssh://push@github.com/owner/repo" {
		t.Fatalf("push precedence got %q", got)
	}
	if got := applyRewrite("GH:owner/repo", rules, false); got != "GH:owner/repo" {
		t.Fatalf("case sensitivity got %q", got)
	}
	if got := applyRewrite("other", rules, false); got != "other" {
		t.Fatalf("passthrough got %q", got)
	}
}

// verifies: user-controlled names cannot become options or config subsection escapes.
func TestValidateRequestRejectsOptionInjection(t *testing.T) {
	tests := []Request{
		{Verb: "pull", Remote: "origin"},
		{Verb: "fetch", Remote: "-o"},
		{Verb: "fetch", Remote: "bad=name"},
		{Verb: "fetch", Remote: "bad..name"},
		{Verb: "fetch", Remote: "bad\nname"},
		{Verb: "push", Remote: "origin", Refspecs: []string{"--force"}},
	}
	for _, req := range tests {
		if err := validateRequest(&req); errorCode(err) != CodeArgInvalid {
			t.Errorf("request=%+v error=%v", req, err)
		}
	}
}

type fakeExec struct {
	rawURL             string
	pushURL            string
	pushAll            string
	rules              string
	rulesResult        *runResult
	urlResult          *runResult
	remoteConfig       string
	remoteConfigResult *runResult
	remotes            string
	probe              runResult
	op                 runResult
	ghStatus           runResult
	ghCredential       runResult
	calls              []runRequest
	opCalls            int
	sshCheck           *runResult
}

func goodFake(raw string) *fakeExec {
	return &fakeExec{
		rawURL: raw, pushAll: raw + "\n", probe: runResult{}, op: runResult{},
		ghStatus: runResult{}, ghCredential: runResult{Stdout: "username=x\npassword=secret\n"},
	}
}

func (f *fakeExec) run(_ context.Context, req runRequest) runResult {
	f.calls = append(f.calls, req)
	joined := strings.Join(req.Args, "\x00")
	switch {
	case req.Name == "ssh" && reflect.DeepEqual(req.Args, []string{"-V"}):
		if f.sshCheck != nil {
			return *f.sshCheck
		}
		return runResult{}
	case req.Name == "gh" && strings.Contains(joined, "status"):
		return f.ghStatus
	case req.Name == "gh" && strings.Contains(joined, "git-credential"):
		return f.ghCredential
	case req.Name == "git" && strings.Contains(joined, "remote\x00get-url\x00--push\x00--all"):
		return runResult{Stdout: f.pushAll}
	case req.Name == "git" && reflect.DeepEqual(req.Args, []string{"remote"}):
		return runResult{Stdout: f.remotes}
	case req.Name == "git" && strings.Contains(joined, "--get-all\x00remote."):
		if strings.HasSuffix(joined, ".pushurl") {
			if f.pushURL == "" {
				return runResult{ExitCode: 1}
			}
			return runResult{Stdout: f.pushURL + "\x00"}
		}
		if f.urlResult != nil {
			return *f.urlResult
		}
		return runResult{Stdout: f.rawURL + "\x00"}
	case req.Name == "git" && strings.Contains(joined, "^url\\..*"):
		if f.rulesResult != nil {
			return *f.rulesResult
		}
		if f.rules == "" {
			return runResult{ExitCode: 1}
		}
		return runResult{Stdout: f.rules}
	case req.Name == "git" && strings.Contains(joined, "^remote\\."):
		if f.remoteConfigResult != nil {
			return *f.remoteConfigResult
		}
		if f.remoteConfig == "" {
			return runResult{ExitCode: 1}
		}
		return runResult{Stdout: f.remoteConfig}
	case req.Name == "git" && len(req.Args) > 0 && req.Args[0] == "ls-remote" && contains(req.Args, "--heads"):
		return f.probe
	case req.Name == "git":
		f.opCalls++
		return f.op
	default:
		return runResult{ExitCode: -1, Err: errors.New("unexpected command")}
	}
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func codeOf(t *testing.T, err error) string {
	t.Helper()
	var opErr *Error
	if !errors.As(err, &opErr) {
		t.Fatalf("error type=%T value=%v", err, err)
	}
	return opErr.Code()
}

// verifies: every §4 transport row chooses a distinct, honest path.
func TestTransportSelectionTable(t *testing.T) {
	tests := []struct {
		name, raw, wantAuth, wantCode string
		probe                         runResult
		fallback                      bool
	}{
		{name: "github ssh native", raw: "git@github.com:o/r.git", probe: runResult{}, wantAuth: "ssh"},
		{name: "github ssh fallback", raw: "git@github.com:o/r.git", probe: runResult{ExitCode: 128, Stderr: "Permission denied (publickey)."}, wantAuth: "https-gh", fallback: true},
		{name: "other ssh auth fail", raw: "git@example.com:o/r.git", probe: runResult{ExitCode: 128, Stderr: "Permission denied (publickey)."}, wantCode: CodeAuthFailed},
		{name: "github https gh", raw: "https://github.com/o/r.git", wantAuth: "https-gh"},
		{name: "other https", raw: "https://example.com/o/r.git", wantAuth: "https"},
		{name: "local rejected", raw: "../repo", wantCode: CodeRemoteUnsupported},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := goodFake(tt.raw)
			fake.probe = tt.probe
			fake.remoteConfig = "remote.origin.fetch\n+refs/heads/*:refs/remotes/origin/*\x00"
			client := newWithRun(Config{GhFallback: true, OpTimeout: time.Second}, fake.run)
			got, err := client.Run(context.Background(), Request{Verb: "fetch", Remote: "origin"})
			if tt.wantCode != "" {
				if codeOf(t, err) != tt.wantCode {
					t.Fatalf("error=%v", err)
				}
				return
			}
			if err != nil || got.Auth != tt.wantAuth || got.FellBack != tt.fallback || got.URL == "" || got.Host == "" {
				t.Fatalf("result=%+v error=%v", got, err)
			}
		})
	}
}

func TestHTTPSUserinfoIsHonouredWithoutGHAndNeverSurfaced(t *testing.T) {
	fake := goodFake("https://user:token@github.com/owner/repo.git")
	client := newWithRun(Config{GhFallback: true, OpTimeout: time.Second}, fake.run)
	result, err := client.Run(context.Background(), Request{Verb: "fetch"})
	if err != nil || result.Auth != "https" || strings.Contains(result.URL, "token") {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	for _, call := range fake.calls {
		if call.Name == "gh" {
			t.Fatalf("caller credential was silently swapped: %#v", call.Args)
		}
	}
}

func TestHTTPSApplicationAuthFailureHasAuthCode(t *testing.T) {
	fake := goodFake("https://example.com/owner/repo.git")
	fake.op = runResult{ExitCode: 128, Stderr: "fatal: Authentication failed"}
	client := newWithRun(Config{GhFallback: true, OpTimeout: time.Second}, fake.run)
	_, err := client.Run(context.Background(), Request{Verb: "fetch"})
	if codeOf(t, err) != CodeAuthFailed || fake.opCalls != 1 {
		t.Fatalf("error=%v opCalls=%d", err, fake.opCalls)
	}
}

// verifies: a native github.com HTTPS op does NOT require gh — a public repo runs when
// gh is absent (never a false E_GIT_GH_UNAVAILABLE) AND is TRULY anonymous: inherited
// credential helpers are cleared, the gh helper is not used.
func TestNativeGitHubHTTPSRunsAnonymouslyWhenGHAbsent(t *testing.T) {
	fake := goodFake("https://github.com/owner/repo.git")
	fake.ghStatus = runResult{ExitCode: 1} // gh not authenticated / absent
	client := newWithRun(Config{GhFallback: true, OpTimeout: time.Second}, fake.run)
	result, err := client.Run(context.Background(), Request{Verb: "fetch"})
	if err != nil || result.Auth != "https" || fake.opCalls != 1 {
		t.Fatalf("public op should run anonymously: result=%+v error=%v opCalls=%d", result, err, fake.opCalls)
	}
	sawOp := false
	for _, call := range fake.calls {
		if call.Name != "git" || !contains(call.Args, "fetch") {
			continue
		}
		sawOp = true
		if !contains(call.Args, "credential.helper=") {
			t.Fatalf("anonymous github op did not clear inherited helpers: %#v", call.Args)
		}
		if contains(call.Args, "credential.helper="+credentialHelper) {
			t.Fatalf("gh helper injected despite gh being unavailable: %#v", call.Args)
		}
	}
	if !sawOp {
		t.Fatal("no fetch op observed")
	}
}

// verifies: a bare (colon-free) or uppercase-scheme HTTPS userinfo credential is redacted;
// redaction must not depend on a colon, a token-shape heuristic, or scheme casing.
func TestBareHTTPSUserinfoIsRedacted(t *testing.T) {
	for _, raw := range []string{
		"https://plaincredential@github.com/owner/repo.git",
		"HTTPS://plaincredential@github.com/owner/repo.git",
	} {
		fake := goodFake(raw)
		client := newWithRun(Config{GhFallback: true, OpTimeout: time.Second}, fake.run)
		result, err := client.Run(context.Background(), Request{Verb: "fetch"})
		if err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
		if result.Auth != "https" || strings.Contains(result.URL, "plaincredential") {
			t.Fatalf("bare userinfo leaked in reported URL for %q: %q", raw, result.URL)
		}
	}
}

// verifies: a credential-bearing clone URL is refused (it would be exposed in child argv),
// while a bare ssh username (git@host, not a credential) clones normally.
func TestCloneCredentialBearingURLIsRefused(t *testing.T) {
	for _, raw := range []string{
		"https://token@github.com/owner/repo.git",
		"https://user:secret@github.com/owner/repo.git",
		"ssh://user:pass@github.com/owner/repo.git",
	} {
		fake := goodFake("")
		client := newWithRun(Config{GhFallback: true, OpTimeout: time.Second}, fake.run)
		_, err := client.Run(context.Background(), Request{Verb: "clone", URL: raw, Dir: "dst"})
		if codeOf(t, err) != CodeArgInvalid || fake.opCalls != 0 {
			t.Errorf("clone %q: err=%v opCalls=%d (want ARG_INVALID, op never runs)", raw, err, fake.opCalls)
		}
	}
	fake := goodFake("")
	client := newWithRun(Config{GhFallback: true, OpTimeout: time.Second}, fake.run)
	if _, err := client.Run(context.Background(), Request{Verb: "clone", URL: "ssh://git@github.com/owner/repo.git", Dir: "dst"}); err != nil {
		t.Fatalf("bare ssh username clone was wrongly refused: %v", err)
	}
}

// verifies: an embedded password in a configured ssh remote is refused BEFORE the probe,
// so the read-only `git ls-remote -- <url>` probe argv never carries the password.
func TestSSHPasswordRemoteRefusedBeforeProbe(t *testing.T) {
	for _, verb := range []string{"fetch", "push", "ls-remote"} {
		fake := goodFake("ssh://user:s3cr3t@example.com/owner/repo.git")
		req := Request{Verb: verb, Remote: "origin"}
		if verb == "push" {
			req.Refspecs = []string{"HEAD:main"}
		}
		client := newWithRun(Config{GhFallback: true, OpTimeout: time.Second}, fake.run)
		_, err := client.Run(context.Background(), req)
		if codeOf(t, err) != CodeArgInvalid {
			t.Errorf("%s: err=%v (want ARG_INVALID)", verb, err)
		}
		for _, call := range fake.calls {
			if contains(call.Args, "--heads") || strings.Contains(strings.Join(call.Args, " "), "s3cr3t") {
				t.Errorf("%s: password reached a child argv (probe ran before rejection): %#v", verb, call.Args)
			}
		}
	}
}

// verifies: an explicit pushurl ignores pushInsteadOf (git's rule) but ORDINARY insteadOf
// still applies — probe/report must use the insteadOf-rewritten endpoint, not the raw pushurl.
func TestExplicitPushURLStillHonoursOrdinaryInsteadOf(t *testing.T) {
	fake := goodFake("git@github.com:owner/fetch.git")
	fake.pushURL = "git@rewrite-me.example:owner/push.git"
	fake.pushAll = fake.pushURL + "\n"
	fake.rules = "url.ssh://git@moved.example/.insteadof\ngit@rewrite-me.example:\x00"
	client := newWithRun(Config{GhFallback: true, OpTimeout: time.Second}, fake.run)
	result, err := client.Run(context.Background(), Request{Verb: "push", Refspecs: []string{"HEAD:main"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Host != "moved.example" || !strings.HasPrefix(result.URL, "ssh://git@moved.example/") {
		t.Fatalf("ordinary insteadOf was not applied to the explicit pushurl: %+v", result)
	}
	for _, call := range fake.calls {
		if contains(call.Args, "--heads") && !contains(call.Args, "ssh://git@moved.example/owner/push.git") {
			t.Fatalf("probe did not use the insteadOf-rewritten pushurl: %#v", call.Args)
		}
	}
}

// verifies: URL/rewrite resolution failures fail CLOSED (never silently "no rewrites" / "no url"),
// so an unseen rule cannot redirect the committed op. The op must never run.
func TestResolutionConfigErrorsFailClosed(t *testing.T) {
	t.Run("rewrite enumeration error", func(t *testing.T) {
		fake := goodFake("git@github.com:owner/repo.git")
		fake.rulesResult = &runResult{ExitCode: 2, Stderr: "fatal: bad config"}
		client := newWithRun(Config{GhFallback: true, OpTimeout: time.Second}, fake.run)
		if _, err := client.Run(context.Background(), Request{Verb: "fetch"}); err == nil || fake.opCalls != 0 {
			t.Fatalf("error=%v opCalls=%d (must fail closed, op never runs)", err, fake.opCalls)
		}
	})
	t.Run("rewrite enumeration truncated", func(t *testing.T) {
		fake := goodFake("git@github.com:owner/repo.git")
		fake.rulesResult = &runResult{Stdout: "url.x.insteadof\ny", StdoutTruncated: true}
		client := newWithRun(Config{GhFallback: true, OpTimeout: time.Second}, fake.run)
		if _, err := client.Run(context.Background(), Request{Verb: "fetch"}); err == nil || fake.opCalls != 0 {
			t.Fatalf("error=%v opCalls=%d (truncation must fail closed)", err, fake.opCalls)
		}
	})
	t.Run("url read error", func(t *testing.T) {
		fake := goodFake("")
		fake.urlResult = &runResult{ExitCode: 2, Stderr: "fatal: bad config"}
		client := newWithRun(Config{GhFallback: true, OpTimeout: time.Second}, fake.run)
		if _, err := client.Run(context.Background(), Request{Verb: "fetch", Remote: "origin"}); err == nil || fake.opCalls != 0 {
			t.Fatalf("error=%v opCalls=%d (url read error must fail closed)", err, fake.opCalls)
		}
	})
}

// verifies: fallback targets a fresh one-URL synthetic remote and copies only known semantics.
func TestSyntheticRemoteFallbackArgvAndAllowList(t *testing.T) {
	fake := goodFake("git@github.com:owner/repo.git")
	fake.probe = runResult{ExitCode: 128, Stderr: "git@github.com: Permission denied (publickey)."}
	fake.remoteConfig = strings.Join([]string{
		"remote.origin.url\ngit@github.com:owner/repo.git",
		"remote.origin.url\ngit@github.com:owner/second.git",
		"remote.origin.fetch\n+refs/heads/*:refs/remotes/origin/*",
		"remote.origin.fetch\n+refs/pull/*:refs/remotes/origin/pr/*",
		"remote.origin.tagOpt\n--no-tags",
		"remote.origin.prune\ntrue",
		"remote.origin.pushurl\ngit@github.com:owner/repo.git",
	}, "\x00") + "\x00"
	client := newWithRun(Config{GhFallback: true, OpTimeout: time.Second}, fake.run)
	result, err := client.Run(context.Background(), Request{Verb: "fetch", Remote: "origin"})
	if err != nil || result.Auth != "https-gh" {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	var op runRequest
	for _, call := range fake.calls {
		if call.Name == "git" && contains(call.Args, "aira-fallback") {
			op = call
		}
		if len(call.Args) > 1 && call.Args[0] == "config" && !contains(call.Args, "--get-all") && !contains(call.Args, "--get-regexp") {
			t.Fatalf("git config write appeared: %#v", call.Args)
		}
	}
	joined := strings.Join(op.Args, "\n")
	for _, wanted := range []string{
		"remote.aira-fallback.url=https://github.com/owner/repo.git",
		"remote.aira-fallback.fetch=+refs/heads/*:refs/remotes/origin/*",
		"remote.aira-fallback.fetch=+refs/pull/*:refs/remotes/origin/pr/*",
		"remote.aira-fallback.tagOpt=--no-tags", "remote.aira-fallback.prune=true",
		"credential.helper=", "credential.helper=" + credentialHelper,
	} {
		if !strings.Contains(joined, wanted) {
			t.Errorf("missing %q in %#v", wanted, op.Args)
		}
	}
	if strings.Count(joined, "remote.aira-fallback.url=") != 1 || strings.Contains(joined, "remote.origin.url=") || contains(op.Args, "https://github.com/owner/repo.git") {
		t.Fatalf("fallback did not use exactly one synthetic URL: %#v", op.Args)
	}
	// url and pushurl are recognised for validation but must NEVER be copied onto the
	// synthetic remote (else the synthetic remote gains a second/original destination).
	if strings.Contains(joined, "remote.aira-fallback.pushurl=") || strings.Contains(joined, "remote.aira-fallback.url=git@") {
		t.Fatalf("fallback copied url/pushurl onto the synthetic remote: %#v", op.Args)
	}
}

func TestSyntheticRemoteNameAvoidsExistingRemote(t *testing.T) {
	fake := goodFake("git@github.com:owner/repo.git")
	fake.probe = runResult{ExitCode: 128, Stderr: "Permission denied (publickey)."}
	fake.remotes = "aira-fallback\norigin\n"
	client := newWithRun(Config{GhFallback: true, OpTimeout: time.Second}, fake.run)
	if _, err := client.Run(context.Background(), Request{Verb: "fetch"}); err != nil {
		t.Fatal(err)
	}
	for _, call := range fake.calls {
		if contains(call.Args, "aira-fallback-1") {
			return
		}
	}
	t.Fatal("fallback did not choose a collision-free synthetic remote")
}

func TestNativeAndFallbackFetchPreserveSameRefAndTagSettings(t *testing.T) {
	remoteConfig := strings.Join([]string{
		"remote.origin.fetch\n+refs/heads/*:refs/remotes/origin/*",
		"remote.origin.fetch\n+refs/tags/release:refs/tags/release",
		"remote.origin.tagOpt\n--tags", "remote.origin.prune\ntrue",
	}, "\x00") + "\x00"
	fake := goodFake("git@github.com:owner/repo.git")
	fake.probe = runResult{ExitCode: 128, Stderr: "Permission denied (publickey)."}
	fake.remoteConfig = remoteConfig
	client := newWithRun(Config{GhFallback: true, OpTimeout: time.Second}, fake.run)
	if _, err := client.Run(context.Background(), Request{Verb: "fetch"}); err != nil {
		t.Fatal(err)
	}
	var fallback []string
	for _, call := range fake.calls {
		if contains(call.Args, "aira-fallback") {
			fallback = call.Args
		}
	}
	for _, semantic := range []string{
		"remote.aira-fallback.fetch=+refs/heads/*:refs/remotes/origin/*",
		"remote.aira-fallback.fetch=+refs/tags/release:refs/tags/release",
		"remote.aira-fallback.tagOpt=--tags", "remote.aira-fallback.prune=true",
	} {
		if !contains(fallback, semantic) {
			t.Fatalf("fallback lost native semantic %q: %#v", semantic, fallback)
		}
	}
}

func TestFallbackUnknownRemoteConfigFailsClosed(t *testing.T) {
	fake := goodFake("git@github.com:owner/repo.git")
	fake.probe = runResult{ExitCode: 128, Stderr: "Permission denied (publickey)."}
	fake.remoteConfig = "remote.origin.fetch\n+refs/heads/*:refs/remotes/origin/*\x00remote.origin.futureBehavior\nenabled\x00"
	client := newWithRun(Config{GhFallback: true, OpTimeout: time.Second}, fake.run)
	_, err := client.Run(context.Background(), Request{Verb: "fetch"})
	if codeOf(t, err) != CodeFallbackBlocked {
		t.Fatal(err)
	}
	var opErr *Error
	errors.As(err, &opErr)
	if opErr.Data()["reason"] != "unsupported-remote-config" || fake.opCalls != 0 {
		t.Fatalf("error=%+v opCalls=%d", opErr, fake.opCalls)
	}
}

func TestFallbackRemoteConfigEnumerationErrorFailsClosed(t *testing.T) {
	fake := goodFake("git@github.com:owner/repo.git")
	fake.probe = runResult{ExitCode: 128, Stderr: "Permission denied (publickey)."}
	fake.remoteConfigResult = &runResult{ExitCode: 2, Stderr: "bad include"}
	client := newWithRun(Config{GhFallback: true, OpTimeout: time.Second}, fake.run)
	_, err := client.Run(context.Background(), Request{Verb: "fetch"})
	if codeOf(t, err) != CodeFallbackBlocked {
		t.Fatal(err)
	}
	var opErr *Error
	errors.As(err, &opErr)
	if opErr.Data()["reason"] != "unsupported-remote-config" || fake.opCalls != 0 {
		t.Fatalf("error=%+v opCalls=%d", opErr, fake.opCalls)
	}
}

func TestFallbackRemoteConfigEnumerationTruncationFailsClosed(t *testing.T) {
	fake := goodFake("git@github.com:owner/repo.git")
	fake.probe = runResult{ExitCode: 128, Stderr: "Permission denied (publickey)."}
	fake.remoteConfigResult = &runResult{Stdout: "remote.origin.fetch\nvalue", StdoutTruncated: true}
	client := newWithRun(Config{GhFallback: true, OpTimeout: time.Second}, fake.run)
	_, err := client.Run(context.Background(), Request{Verb: "fetch"})
	if codeOf(t, err) != CodeFallbackBlocked {
		t.Fatal(err)
	}
	var opErr *Error
	errors.As(err, &opErr)
	if opErr.Data()["reason"] != "unsupported-remote-config" || fake.opCalls != 0 {
		t.Fatalf("error=%+v opCalls=%d", opErr, fake.opCalls)
	}
}

// verifies: the native push path refuses several destinations before probe or mutation.
func TestPushSingleDestinationGuardOnNativePath(t *testing.T) {
	fake := goodFake("git@github.com:owner/repo.git")
	fake.pushAll = "git@github.com:owner/repo.git\nhttps://mirror.example/owner/repo.git\n"
	client := newWithRun(Config{GhFallback: true, OpTimeout: time.Second}, fake.run)
	_, err := client.Run(context.Background(), Request{Verb: "push", Remote: "origin", Refspecs: []string{"HEAD:main"}})
	if codeOf(t, err) != CodeRemoteUnsupported {
		t.Fatal(err)
	}
	var opErr *Error
	errors.As(err, &opErr)
	if opErr.Data()["reason"] != "multiple-destinations" || fake.opCalls != 0 {
		t.Fatalf("error=%+v opCalls=%d", opErr, fake.opCalls)
	}
}

func TestFallbackPushDisagreeingDestinationsIsBlocked(t *testing.T) {
	fake := goodFake("git@github.com:owner/repo.git")
	fake.pushAll = "git@github.com:owner/repo.git\ngit@github.com:other/different.git\n"
	fake.probe = runResult{ExitCode: 128, Stderr: "Permission denied (publickey)."}
	client := newWithRun(Config{GhFallback: true, OpTimeout: time.Second}, fake.run)
	_, err := client.Run(context.Background(), Request{Verb: "push", Remote: "origin", Refspecs: []string{"HEAD:main"}})
	if codeOf(t, err) != CodeFallbackBlocked {
		t.Fatal(err)
	}
	var opErr *Error
	errors.As(err, &opErr)
	if opErr.Data()["reason"] != "multiple-destinations" || fake.opCalls != 0 {
		t.Fatalf("error=%+v opCalls=%d", opErr, fake.opCalls)
	}
}

// verifies: fallback push never guesses push.default and configured/explicit refspecs remain exact.
func TestFallbackPushRefspecContract(t *testing.T) {
	for _, tc := range []struct {
		name       string
		request    []string
		configured string
		wantCode   string
		want       string
	}{
		{name: "explicit", request: []string{"HEAD:main"}, want: "HEAD:main"},
		{name: "configured", configured: "remote.origin.push\nHEAD:refs/heads/configured\x00", want: "remote.aira-fallback.push=HEAD:refs/heads/configured"},
		{name: "push default refused", wantCode: CodeFallbackBlocked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := goodFake("git@github.com:owner/repo.git")
			fake.probe = runResult{ExitCode: 128, Stderr: "Permission denied (publickey)."}
			fake.remoteConfig = tc.configured
			client := newWithRun(Config{GhFallback: true, OpTimeout: time.Second}, fake.run)
			_, err := client.Run(context.Background(), Request{Verb: "push", Remote: "origin", Refspecs: tc.request})
			if tc.wantCode != "" {
				if codeOf(t, err) != tc.wantCode {
					t.Fatal(err)
				}
				var opErr *Error
				errors.As(err, &opErr)
				if opErr.Data()["reason"] != "explicit-refspec-required" || fake.opCalls != 0 {
					t.Fatalf("error=%+v opCalls=%d", opErr, fake.opCalls)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			joined := ""
			for _, call := range fake.calls {
				if contains(call.Args, "push") && contains(call.Args, "aira-fallback") {
					joined = strings.Join(call.Args, "\n")
				}
			}
			if !strings.Contains(joined, tc.want) {
				t.Fatalf("missing %q in %q", tc.want, joined)
			}
		})
	}
}

// verifies: clone fallback uses the canonical URL directly, never a synthetic remote.
func TestCloneFallbackSpecialCase(t *testing.T) {
	fake := goodFake("")
	fake.probe = runResult{ExitCode: 128, Stderr: "Permission denied (publickey)."}
	client := newWithRun(Config{GhFallback: true, OpTimeout: time.Second}, fake.run)
	result, err := client.Run(context.Background(), Request{Verb: "clone", URL: "git@github.com:owner/repo.git", Dir: "dst"})
	if err != nil || result.URL != "https://github.com/owner/repo.git" {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	var argv []string
	for _, call := range fake.calls {
		if contains(call.Args, "clone") {
			argv = call.Args
		}
	}
	if !contains(argv, "https://github.com/owner/repo.git") || !contains(argv, "dst") || contains(argv, "aira-fallback") {
		t.Fatalf("clone argv=%#v", argv)
	}
	for _, call := range fake.calls {
		if contains(call.Args, "^remote\\.") {
			t.Fatalf("clone enumerated repo remote config: %#v", call.Args)
		}
		if contains(call.Args, "^url\\..*") && call.Dir != "/" {
			t.Fatalf("clone rewrite enumeration used repo cwd: %#v dir=%q", call.Args, call.Dir)
		}
	}
}

func TestRewriteSafetyBlocksAlwaysSSHRule(t *testing.T) {
	fake := goodFake("git@github.com:owner/repo.git")
	fake.probe = runResult{ExitCode: 128, Stderr: "Permission denied (publickey)."}
	fake.rules = "url.git@github.com:.insteadof\nhttps://github.com/\x00"
	client := newWithRun(Config{GhFallback: true, OpTimeout: time.Second}, fake.run)
	_, err := client.Run(context.Background(), Request{Verb: "fetch"})
	if codeOf(t, err) != CodeFallbackBlocked {
		t.Fatal(err)
	}
	var opErr *Error
	errors.As(err, &opErr)
	if opErr.Data()["reason"] != "insteadof-rewrite" || fake.opCalls != 0 {
		t.Fatalf("error=%+v opCalls=%d", opErr, fake.opCalls)
	}
}

func TestRewriteSafetyNonmatchingRuleAllowsFallback(t *testing.T) {
	fake := goodFake("git@github.com:owner/repo.git")
	fake.probe = runResult{ExitCode: 128, Stderr: "Permission denied (publickey)."}
	fake.rules = "url.ssh://elsewhere/.insteadof\nhttps://elsewhere.example/\x00"
	client := newWithRun(Config{GhFallback: true, OpTimeout: time.Second}, fake.run)
	result, err := client.Run(context.Background(), Request{Verb: "fetch"})
	if err != nil || result.Auth != "https-gh" || !result.FellBack {
		t.Fatalf("result=%+v error=%v", result, err)
	}
}

// verifies: push resolves its explicit pushurl verbatim and probes that endpoint, never the fetch URL.
func TestPushProbesExplicitPushURLWithoutRewrite(t *testing.T) {
	fake := goodFake("git@github.com:owner/fetch.git")
	fake.pushURL = "git@other.example:owner/push.git"
	fake.pushAll = fake.pushURL + "\n"
	fake.rules = "url.ssh://rewritten/.pushinsteadof\ngit@other.example:\x00"
	client := newWithRun(Config{GhFallback: true, OpTimeout: time.Second}, fake.run)
	result, err := client.Run(context.Background(), Request{Verb: "push", Refspecs: []string{"HEAD:main"}})
	if err != nil || result.URL != fake.pushURL || result.Host != "other.example" {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	for _, call := range fake.calls {
		if contains(call.Args, "--heads") && !contains(call.Args, fake.pushURL) {
			t.Fatalf("probe=%#v", call.Args)
		}
	}
}

func TestResolvedProbeAndReportedEndpointAgreeWithoutChainedReapplication(t *testing.T) {
	fake := goodFake("gh:owner/repo")
	fake.rules = strings.Join([]string{
		"url.ssh://git@other.example/.insteadof\ngh:",
		"url.https://must-not-chain.example/.insteadof\nssh://git@other.example/",
	}, "\x00") + "\x00"
	client := newWithRun(Config{GhFallback: true, OpTimeout: time.Second}, fake.run)
	result, err := client.Run(context.Background(), Request{Verb: "fetch"})
	if err != nil {
		t.Fatal(err)
	}
	want := "ssh://git@other.example/owner/repo"
	if result.URL != want || result.Host != "other.example" || result.Auth != "ssh" {
		t.Fatalf("result=%+v", result)
	}
	for _, call := range fake.calls {
		if contains(call.Args, "--heads") {
			if !contains(call.Args, want) || contains(call.Args, "https://must-not-chain.example/owner/repo") {
				t.Fatalf("probe=%#v", call.Args)
			}
		}
	}
}

func TestCloneChainedRewriteUsesRawForCommitAndEffectiveForProbeReport(t *testing.T) {
	fake := goodFake("")
	fake.rules = strings.Join([]string{
		"url.ssh://git@github.com/.insteadof\ngh:",
		"url.https://must-not-chain.example/.insteadof\nssh://git@github.com/",
	}, "\x00") + "\x00"
	client := newWithRun(Config{GhFallback: true, OpTimeout: time.Second}, fake.run)
	result, err := client.Run(context.Background(), Request{Verb: "clone", URL: "gh:owner/repo", Dir: "dst"})
	if err != nil {
		t.Fatal(err)
	}
	want := "ssh://git@github.com/owner/repo"
	if result.URL != want {
		t.Fatalf("result=%+v", result)
	}
	for _, call := range fake.calls {
		if contains(call.Args, "--heads") && !contains(call.Args, want) {
			t.Fatalf("probe=%#v", call.Args)
		}
		if contains(call.Args, "clone") && (!contains(call.Args, "gh:owner/repo") || contains(call.Args, want)) {
			t.Fatalf("commit=%#v", call.Args)
		}
	}
}

// verifies: an auth race after a passing probe is classified but never retried.
func TestPostProbeAuthFailureIsNotRetried(t *testing.T) {
	fake := goodFake("git@github.com:owner/repo.git")
	fake.op = runResult{ExitCode: 128, Stderr: "Permission denied (publickey)."}
	client := newWithRun(Config{GhFallback: true, OpTimeout: time.Second}, fake.run)
	_, err := client.Run(context.Background(), Request{Verb: "push", Refspecs: []string{"HEAD:main"}})
	if codeOf(t, err) != CodeAuthFailed || fake.opCalls != 1 {
		t.Fatalf("error=%v opCalls=%d", err, fake.opCalls)
	}
}

func TestGHChecksAreScopedAndRequireCredential(t *testing.T) {
	for _, tc := range []struct {
		name               string
		status, credential runResult
	}{
		{name: "github status fails", status: runResult{ExitCode: 1}, credential: runResult{Stdout: "password=x\n"}},
		{name: "no password", status: runResult{}, credential: runResult{Stdout: "username=x\n"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := goodFake("git@github.com:owner/repo.git")
			fake.probe = runResult{ExitCode: 128, Stderr: "Permission denied (publickey)."}
			fake.ghStatus, fake.ghCredential = tc.status, tc.credential
			client := newWithRun(Config{GhFallback: true, OpTimeout: time.Second}, fake.run)
			_, err := client.Run(context.Background(), Request{Verb: "fetch"})
			if codeOf(t, err) != CodeGHUnavailable || fake.opCalls != 0 {
				t.Fatalf("error=%v opCalls=%d", err, fake.opCalls)
			}
			for _, call := range fake.calls {
				if call.Name == "gh" && contains(call.Args, "status") && !contains(call.Args, "github.com") {
					t.Fatalf("unscoped gh call %#v", call.Args)
				}
			}
		})
	}
}

// verifies: every child is non-interactive and the explicit probe cannot inherit rewrite injection.
func TestEveryChildEnvironmentIsNonInteractiveAndProbeIsRewriteFree(t *testing.T) {
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "url.git@evil:.insteadOf")
	t.Setenv("GIT_CONFIG_VALUE_0", "git@github.com:")
	t.Setenv("GIT_CONFIG_PARAMETERS", "'url.git@evil:.insteadOf'='git@github.com:'")
	fake := goodFake("git@github.com:owner/repo.git")
	client := newWithRun(Config{GhFallback: true, SSHConnectTimeout: 7 * time.Second, OpTimeout: time.Second}, fake.run)
	if _, err := client.Run(context.Background(), Request{Verb: "fetch"}); err != nil {
		t.Fatal(err)
	}
	for _, call := range fake.calls {
		env := strings.Join(call.Env, "\n")
		for _, wanted := range []string{"GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never", "BatchMode=yes", "ConnectTimeout=7"} {
			if !strings.Contains(env, wanted) {
				t.Errorf("%s %#v missing %s", call.Name, call.Args, wanted)
			}
		}
		if contains(call.Args, "--heads") {
			if call.Dir != "/" || !strings.Contains(env, "GIT_CONFIG_GLOBAL=/dev/null") || !strings.Contains(env, "GIT_CONFIG_SYSTEM=/dev/null") || strings.Contains(env, "GIT_CONFIG_COUNT=") || strings.Contains(env, "GIT_CONFIG_KEY_0=") || strings.Contains(env, "GIT_CONFIG_VALUE_0=") || strings.Contains(env, "GIT_CONFIG_PARAMETERS=") {
				t.Fatalf("probe was not rewrite-free: dir=%q env=%s", call.Dir, env)
			}
		}
	}
}

func TestNoFallbackMatrix(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		enabled   bool
		probe     runResult
		gh        runResult
		code      string
	}{
		{name: "gh absent", raw: "git@github.com:o/r.git", enabled: true, probe: runResult{ExitCode: 128, Stderr: "Permission denied (publickey)."}, gh: runResult{ExitCode: 1}, code: CodeGHUnavailable},
		{name: "disabled", raw: "git@github.com:o/r.git", probe: runResult{ExitCode: 128, Stderr: "Permission denied (publickey)."}, code: CodeAuthFailed},
		{name: "non github", raw: "git@example.com:o/r.git", enabled: true, probe: runResult{ExitCode: 128, Stderr: "Permission denied (publickey)."}, code: CodeAuthFailed},
		{name: "non auth", raw: "git@github.com:o/r.git", enabled: true, probe: runResult{ExitCode: 128, Stderr: "Host key verification failed"}, code: CodeFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := goodFake(tc.raw)
			fake.probe, fake.ghStatus = tc.probe, tc.gh
			client := newWithRun(Config{GhFallback: tc.enabled, OpTimeout: time.Second}, fake.run)
			_, err := client.Run(context.Background(), Request{Verb: "push", Refspecs: []string{"HEAD:main"}})
			if codeOf(t, err) != tc.code || fake.opCalls != 0 {
				t.Fatalf("error=%v opCalls=%d", err, fake.opCalls)
			}
		})
	}
}

func TestMissingSSHHasDistinctCodeBeforeProbe(t *testing.T) {
	fake := goodFake("git@github.com:o/r.git")
	fake.sshCheck = &runResult{ExitCode: -1, Err: errors.New("executable file not found")}
	client := newWithRun(Config{GhFallback: true, OpTimeout: time.Second}, fake.run)
	_, err := client.Run(context.Background(), Request{Verb: "fetch"})
	if codeOf(t, err) != CodeSSHUnavailable || fake.opCalls != 0 {
		t.Fatalf("error=%v opCalls=%d", err, fake.opCalls)
	}
}

func TestRedactURLScrubsTokensOnEveryPath(t *testing.T) {
	const token = "ghp_super_secret_token_zzz"
	cases := []struct{ name, raw, want string }{
		{"token-in-https-path", "https://github.com/" + token + "/repo.git", "https://github.com/***/repo.git"},
		{"scp-token-userinfo", token + "@github.com:owner/repo.git", "***@github.com:owner/repo.git"},
		{"token-in-ssh-path", "ssh://git@github.com/" + token + "/repo", "ssh://git@github.com/***/repo"},
		{"pat-in-path", "https://github.com/github_pat_" + strings.Repeat("A", 20) + "/repo.git", "https://github.com/***/repo.git"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactURL(tc.raw)
			if strings.Contains(got, token) || strings.Contains(got, "github_pat_") {
				t.Fatalf("token survived: %q -> %q", tc.raw, got)
			}
			if got != tc.want {
				t.Fatalf("RedactURL(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
	// A bare ssh username is not a credential and must survive redaction.
	if got := RedactURL("git@github.com:owner/repo.git"); got != "git@github.com:owner/repo.git" {
		t.Fatalf("bare ssh username over-redacted: %q", got)
	}
}
