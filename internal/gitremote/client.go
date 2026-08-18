// Package gitremote provides bounded, non-interactive git network operations.
// It deliberately has no dependency on AIRA's store or domain layers.
package gitremote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	CodeSSHUnavailable    = "E_GIT_SSH_UNAVAILABLE"
	CodeGHUnavailable     = "E_GIT_GH_UNAVAILABLE"
	CodeAuthFailed        = "E_GIT_AUTH_FAILED"
	CodeFallbackBlocked   = "E_GIT_FALLBACK_BLOCKED"
	CodeRemoteUnsupported = "E_GIT_REMOTE_UNSUPPORTED"
	CodeRemoteUnresolved  = "E_GIT_REMOTE_UNRESOLVED"
	CodeTimeout           = "E_GIT_TIMEOUT"
	CodeFailed            = "E_GIT_FAILED"
	CodeArgInvalid        = "E_GIT_ARG_INVALID"
)

const (
	defaultSSHConnectTimeout = 10 * time.Second
	defaultOpTimeout         = 120 * time.Second
	outputTailBytes          = 64 * 1024
	credentialHelper         = "!gh auth git-credential"
)

// Config controls the bounded authentication operation. Callers should set
// GhFallback explicitly; the app layer implements the absent-means-enabled
// configuration contract.
type Config struct {
	GhFallback        bool
	SSHConnectTimeout time.Duration
	OpTimeout         time.Duration
}

// Request is the closed git-network request vocabulary.
type Request struct {
	Verb       string
	Remote     string
	URL        string
	Refspecs   []string
	Dir        string
	LiveStdout io.Writer
	LiveStderr io.Writer
}

// Result reports the transport git actually addressed. Auth is "ssh",
// "https-gh", or "https" for a non-GitHub HTTPS endpoint.
type Result struct {
	Op         string `json:"op"`
	Auth       string `json:"auth"`
	Remote     string `json:"remote,omitempty"`
	URL        string `json:"url"`
	Host       string `json:"host"`
	FellBack   bool   `json:"fell_back"`
	ExitCode   int    `json:"exit_code"`
	StdoutTail string `json:"stdout_tail,omitempty"`
	StderrTail string `json:"stderr_tail,omitempty"`
}

// Error is the package's stable, structured failure sentinel.
type Error struct {
	StableCode string
	Message    string
	Details    map[string]any
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Message == "" {
		return e.StableCode
	}
	return e.StableCode + ": " + redact(e.Message)
}

// Code returns the stable cross-face error code.
func (e *Error) Code() string { return e.StableCode }

// Data returns a copy of the structured refusal/failure payload.
func (e *Error) Data() map[string]any {
	if e == nil || len(e.Details) == 0 {
		return nil
	}
	out := make(map[string]any, len(e.Details))
	for k, v := range e.Details {
		out[k] = v
	}
	return out
}

func opError(code, message string, details map[string]any) error {
	return &Error{StableCode: code, Message: redact(message), Details: redactDetails(details)}
}

func errorCode(err error) string {
	var target *Error
	if errors.As(err, &target) {
		return target.StableCode
	}
	return ""
}

type runRequest struct {
	Name       string
	Args       []string
	Env        []string
	Dir        string
	Stdin      string
	LiveStdout io.Writer
	LiveStderr io.Writer
}

type runResult struct {
	ExitCode        int
	Stdout          string
	Stderr          string
	Err             error
	StdoutTruncated bool
	StderrTruncated bool
}

type runFn func(context.Context, runRequest) runResult

// Client resolves, probes, authenticates, and executes a single operation.
type Client struct {
	config Config
	run    runFn
}

// New constructs a client backed by hardened real subprocess execution.
func New(config Config) *Client {
	if config.SSHConnectTimeout <= 0 {
		config.SSHConnectTimeout = defaultSSHConnectTimeout
	}
	if config.OpTimeout <= 0 {
		config.OpTimeout = defaultOpTimeout
	}
	return &Client{config: config, run: realRun}
}

func newWithRun(config Config, run runFn) *Client {
	c := New(config)
	c.run = run
	return c
}

type endpoint struct {
	Raw, Redacted, Scheme, Host, User, Owner, Repo string
	Candidate, HasUserinfo, HasPassword            bool
}

var fallbackSegment = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func parseEndpoint(raw string) (endpoint, error) {
	if raw == "" {
		return endpoint{}, opError(CodeRemoteUnresolved, "empty remote URL", nil)
	}
	for _, r := range raw {
		if r > 127 || r < 0x20 || r == 0x7f || r == ' ' || r == '\t' {
			return endpoint{}, opError(CodeArgInvalid, "remote URL must contain printable ASCII without whitespace", nil)
		}
	}
	ep := endpoint{Raw: raw, Redacted: redactURL(raw)}
	var path, port string
	if !strings.Contains(raw, "://") {
		at := strings.IndexByte(raw, '@')
		colon := strings.IndexByte(raw, ':')
		if at <= 0 || colon <= at+1 || strings.ContainsAny(raw[:colon], "/\\") {
			return endpoint{}, opError(CodeRemoteUnsupported, "remote is not a supported network URL", map[string]any{"url": ep.Redacted})
		}
		ep.Scheme, ep.User = "ssh", raw[:at]
		ep.Host, path = raw[at+1:colon], raw[colon+1:]
	} else {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return endpoint{}, opError(CodeRemoteUnsupported, "remote is not a supported network URL", map[string]any{"url": ep.Redacted})
		}
		switch strings.ToLower(u.Scheme) {
		case "ssh":
			ep.Scheme = "ssh"
		case "https":
			ep.Scheme = "https"
		default:
			return endpoint{}, opError(CodeRemoteUnsupported, "remote scheme is outside aira git", map[string]any{"url": ep.Redacted})
		}
		ep.Host = u.Hostname()
		port = u.Port()
		if strings.Contains(u.Host, ":") && port == "" && net.ParseIP(strings.Trim(u.Host, "[]")) == nil && strings.Count(u.Host, ":") > 1 {
			return endpoint{}, opError(CodeArgInvalid, "malformed remote authority", nil)
		}
		path = strings.TrimPrefix(u.EscapedPath(), "/")
		if u.User != nil {
			ep.HasUserinfo = true
			ep.User = u.User.Username()
			_, ep.HasPassword = u.User.Password()
		}
		if u.RawQuery != "" || u.Fragment != "" {
			path += "?"
		}
	}
	ep.Host = strings.ToLower(strings.TrimSuffix(ep.Host, "."))
	if ep.Host == "" {
		return endpoint{}, opError(CodeRemoteUnsupported, "remote host is empty", nil)
	}
	github := ep.Host == "github.com"
	defaultPort := port == "" || (ep.Scheme == "ssh" && port == "22") || (ep.Scheme == "https" && port == "443")
	identityOK := (ep.Scheme == "ssh" && ep.User == "git" && !ep.HasPassword) || (ep.Scheme == "https" && !ep.HasUserinfo)
	if github && defaultPort && identityOK {
		parts := strings.Split(path, "/")
		if len(parts) == 2 && parts[0] != "." && parts[0] != ".." && parts[1] != "." && parts[1] != ".." &&
			!strings.ContainsAny(path, "%?#") && fallbackSegment.MatchString(parts[0]) && fallbackSegment.MatchString(parts[1]) {
			repo := parts[1]
			if strings.HasSuffix(repo, ".git.git") {
				return ep, nil
			}
			if strings.HasSuffix(repo, ".git") {
				repo = strings.TrimSuffix(repo, ".git")
			}
			if repo != "" && repo != "." && repo != ".." && fallbackSegment.MatchString(repo) {
				ep.Candidate, ep.Owner, ep.Repo = true, parts[0], repo
			}
		}
	}
	return ep, nil
}

func canonicalHTTPS(ep endpoint) string {
	return "https://github.com/" + ep.Owner + "/" + ep.Repo + ".git"
}

func sshAuthFailureFor(exit int, stderr string, ep endpoint) bool {
	if exit == 0 {
		return false
	}
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.HasPrefix(line, "Permission denied (publickey") {
			return true
		}
		identity, rest, ok := strings.Cut(line, ": ")
		user, host, hasAt := strings.Cut(identity, "@")
		if ok && hasAt && user != "" && strings.EqualFold(strings.TrimSuffix(host, "."), ep.Host) && (ep.User == "" || user == ep.User) && strings.HasPrefix(rest, "Permission denied (publickey") {
			return true
		}
	}
	return false
}

func httpsAuthFailure(exit int, stderr string) bool {
	if exit == 0 {
		return false
	}
	for _, signature := range []string{"Invalid username or password", "Support for password authentication", "Authentication failed", "HTTP 401", "HTTP 403"} {
		if strings.Contains(stderr, signature) {
			return true
		}
	}
	return false
}

type rewriteRule struct {
	Key, Base, Prefix string
	Push              bool
}

func applyRewrite(raw string, rules []rewriteRule, push bool) string {
	best := -1
	replacement := ""
	if push {
		for _, rule := range rules {
			if rule.Push && strings.HasPrefix(raw, rule.Prefix) && len(rule.Prefix) > best {
				best, replacement = len(rule.Prefix), rule.Base
			}
		}
	}
	if best < 0 {
		for _, rule := range rules {
			if !rule.Push && strings.HasPrefix(raw, rule.Prefix) && len(rule.Prefix) > best {
				best, replacement = len(rule.Prefix), rule.Base
			}
		}
	}
	if best < 0 {
		return raw
	}
	return replacement + raw[best:]
}

var remoteNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

func validateRequest(req *Request) error {
	req.Verb = strings.ToLower(strings.TrimSpace(req.Verb))
	switch req.Verb {
	case "clone", "fetch", "push", "ls-remote":
	default:
		return opError(CodeArgInvalid, fmt.Sprintf("unknown git operation %q", req.Verb), nil)
	}
	if req.Verb == "clone" {
		if req.URL == "" {
			return opError(CodeArgInvalid, "clone requires a URL", nil)
		}
		if req.Remote != "" || len(req.Refspecs) > 0 {
			return opError(CodeArgInvalid, "clone accepts only URL and optional dir", nil)
		}
		if strings.HasPrefix(req.URL, "-") || strings.HasPrefix(req.Dir, "-") || strings.ContainsAny(req.Dir, "\r\n\x00") {
			return opError(CodeArgInvalid, "clone arguments may not begin with '-'", nil)
		}
		return nil
	}
	if req.URL != "" || req.Dir != "" {
		return opError(CodeArgInvalid, "URL and dir are clone-only arguments", nil)
	}
	if req.Remote == "" {
		req.Remote = "origin"
	}
	if !remoteNamePattern.MatchString(req.Remote) || strings.Contains(req.Remote, "..") || strings.Contains(req.Remote, "=") {
		return opError(CodeArgInvalid, "invalid remote name", map[string]any{"remote": req.Remote})
	}
	for _, ref := range req.Refspecs {
		if ref == "" || strings.HasPrefix(ref, "-") || strings.ContainsAny(ref, "\r\n\x00") {
			return opError(CodeArgInvalid, "invalid refspec", nil)
		}
	}
	return nil
}

// Run performs one closed, bounded network operation.
func (c *Client) Run(parent context.Context, request Request) (*Result, error) {
	if err := validateRequest(&request); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(parent, c.config.OpTimeout)
	defer cancel()

	ep, rules, err := c.resolve(ctx, request)
	if err != nil {
		return nil, err
	}
	// Refuse an embedded PASSWORD wherever the URL reaches a child argv: the ssh path runs
	// a read-only probe (git ls-remote -- <url>), and clone passes the URL as an op arg —
	// both would expose the password to same-user process inspection. (An ssh URL password
	// is also unusable under BatchMode.) https fetch/push use the remote NAME for the
	// committed op and run no probe, so a configured credential is not exposed by AIRA and
	// is redacted in surfaced output — those keep the plan's run-as-is behaviour.
	if ep.HasPassword && (ep.Scheme == "ssh" || request.Verb == "clone") {
		return nil, opError(CodeArgInvalid, "remote URL must not embed a password; use gh or a git credential helper", nil)
	}
	// clone additionally passes the URL as an op arg, so refuse an https token-username
	// (bare userinfo) clone too. A bare ssh username (git@host) is not a credential.
	if request.Verb == "clone" && ep.Scheme == "https" && ep.HasUserinfo {
		return nil, opError(CodeArgInvalid, "clone URL must not embed a credential; use gh or a git credential helper", nil)
	}
	result := &Result{Op: request.Verb, Remote: request.Remote, URL: ep.Redacted, Host: ep.Host}
	if ep.Scheme == "ssh" {
		result.Auth = "ssh"
		if err := c.requireSSH(ctx); err != nil {
			return nil, err
		}
	} else {
		result.Auth = "https"
	}

	var pushDestinations []string
	if request.Verb == "push" {
		pushDestinations, err = c.pushDestinations(ctx, request.Remote)
		if err != nil {
			return nil, err
		}
		if len(pushDestinations) > 1 && ep.Scheme != "ssh" {
			return nil, multipleDestinationsError()
		}
	}
	if ep.Scheme == "https" {
		// A native github.com HTTPS op does not REQUIRE gh: a public repo works
		// anonymously. AIRA owns the github auth story, so ALWAYS clear inherited helpers
		// there (deterministic, no silent/hanging system helper); add gh OPPORTUNISTICALLY
		// when it can serve a credential, else run truly anonymously and let a private repo
		// fail honestly with an auth error. Non-github HTTPS keeps git's native credential
		// handling (honour a user's configured helper; the non-interactive env prevents hangs).
		clearHelpers, useGH := false, false
		if ep.Host == "github.com" && !ep.HasUserinfo {
			clearHelpers = true
			if c.requireGH(ctx) == nil {
				useGH = true
				result.Auth = "https-gh"
			}
		}
		opEndpoint := ep
		if request.Verb == "clone" {
			opEndpoint.Raw = request.URL
		}
		op := c.nativeOp(ctx, request, opEndpoint, clearHelpers, useGH)
		return finish(result, op, false, ep)
	}

	if request.Verb == "ls-remote" {
		op := c.nativeOp(ctx, request, ep, false, false)
		if op.ExitCode == 0 && op.Err == nil {
			return finish(result, op, false, ep)
		}
		if timedOut(ctx, op) {
			return nil, timeoutError(op)
		}
		if !sshAuthFailureFor(op.ExitCode, op.Stderr, ep) {
			return nil, failedError(ep, op, false)
		}
		if !ep.Candidate || !c.config.GhFallback {
			return nil, authError(ep, op)
		}
		return c.fallback(ctx, request, ep, rules, result)
	}

	probe := c.probe(ctx, ep, request.LiveStdout, request.LiveStderr)
	if probe.ExitCode != 0 || probe.Err != nil {
		if timedOut(ctx, probe) {
			return nil, timeoutError(probe)
		}
		if len(pushDestinations) > 1 && !sshAuthFailureFor(probe.ExitCode, probe.Stderr, ep) {
			return nil, multipleDestinationsError()
		}
		if !sshAuthFailureFor(probe.ExitCode, probe.Stderr, ep) {
			return nil, failedError(ep, probe, false)
		}
		if len(pushDestinations) > 1 {
			if ep.Candidate && c.config.GhFallback && destinationsDisagree(pushDestinations) {
				return nil, opError(CodeFallbackBlocked, "fallback push destinations disagree", map[string]any{"reason": "multiple-destinations"})
			}
			return nil, multipleDestinationsError()
		}
		if !ep.Candidate || !c.config.GhFallback {
			return nil, authError(ep, probe)
		}
		return c.fallback(ctx, request, ep, rules, result)
	}
	if len(pushDestinations) > 1 {
		return nil, multipleDestinationsError()
	}
	opEndpoint := ep
	if request.Verb == "clone" {
		opEndpoint.Raw = request.URL
	}
	op := c.nativeOp(ctx, request, opEndpoint, false, false)
	return finish(result, op, false, ep)
}

func finish(result *Result, op runResult, fellBack bool, ep endpoint) (*Result, error) {
	result.FellBack = fellBack
	result.ExitCode = op.ExitCode
	result.StdoutTail = tail(redact(op.Stdout), outputTailBytes)
	result.StderrTail = tail(redact(op.Stderr), outputTailBytes)
	if timedOut(context.Background(), op) {
		return nil, timeoutError(op)
	}
	if op.ExitCode == 0 && op.Err == nil {
		return result, nil
	}
	ep.Redacted = result.URL
	if result.Auth == "ssh" && sshAuthFailureFor(op.ExitCode, op.Stderr, ep) {
		return nil, authError(ep, op)
	}
	if result.Auth != "ssh" && httpsAuthFailure(op.ExitCode, op.Stderr) {
		return nil, authError(ep, op)
	}
	return nil, failedError(ep, op, false)
}

func timedOut(ctx context.Context, result runResult) bool {
	return errors.Is(result.Err, context.DeadlineExceeded) || errors.Is(result.Err, context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded)
}

func timeoutError(result runResult) error {
	return opError(CodeTimeout, "git operation exceeded its deadline", failureDetails(result, nil))
}

func authError(ep endpoint, result runResult) error {
	return opError(CodeAuthFailed, "git authentication failed", failureDetails(result, &ep))
}

func failedError(ep endpoint, result runResult, resolution bool) error {
	code := CodeFailed
	if resolution {
		code = CodeRemoteUnresolved
	}
	if result.Err != nil && strings.Contains(strings.ToLower(result.Err.Error()), "executable file not found") && ep.Scheme == "ssh" {
		code = CodeSSHUnavailable
	}
	return opError(code, "git operation failed: "+redact(result.Stderr), failureDetails(result, &ep))
}

func failureDetails(result runResult, ep *endpoint) map[string]any {
	d := map[string]any{"exit_code": result.ExitCode, "stdout_tail": tail(redact(result.Stdout), outputTailBytes), "stderr_tail": tail(redact(result.Stderr), outputTailBytes)}
	if ep != nil {
		d["url"], d["host"] = ep.Redacted, ep.Host
	}
	return d
}

func (c *Client) command(ctx context.Context, request runRequest) runResult {
	request.Env = commandEnv(request.Env, c.config.SSHConnectTimeout)
	return c.run(ctx, request)
}

func (c *Client) probe(ctx context.Context, ep endpoint, _, _ io.Writer) runResult {
	env := rewriteFreeEnv(commandEnv(nil, c.config.SSHConnectTimeout))
	return c.run(ctx, runRequest{Name: "git", Args: []string{"ls-remote", "--heads", "--", ep.Raw}, Env: env, Dir: "/"})
}

// nativeOp runs the committed operation. clearHelpers injects an empty credential.helper
// first (dropping any inherited/system helper — determinism + no silent/hanging helper);
// useGH additionally adds the gh helper. github.com HTTPS always clears (AIRA owns that
// auth story: gh or truly anonymous); other hosts keep git's native credential handling.
func (c *Client) nativeOp(ctx context.Context, req Request, ep endpoint, clearHelpers, useGH bool) runResult {
	args := make([]string, 0, 12)
	if clearHelpers {
		args = append(args, "-c", "credential.helper=")
	}
	if useGH {
		args = append(args, "-c", "credential.helper="+credentialHelper)
	}
	args = append(args, req.Verb, "--")
	if req.Verb == "clone" {
		args = append(args, ep.Raw)
		if req.Dir != "" {
			args = append(args, req.Dir)
		}
	} else {
		args = append(args, req.Remote)
		args = append(args, req.Refspecs...)
	}
	return c.command(ctx, runRequest{Name: "git", Args: args, LiveStdout: req.LiveStdout, LiveStderr: req.LiveStderr})
}

func (c *Client) resolve(ctx context.Context, req Request) (endpoint, []rewriteRule, error) {
	raw := req.URL
	explicitPushURL := false
	if req.Verb != "clone" {
		if req.Verb == "push" {
			values, absent, err := c.configValues(ctx, "remote."+req.Remote+".pushurl")
			if err != nil {
				return endpoint{}, nil, err
			}
			if !absent {
				explicitPushURL = true
				if len(values) != 1 {
					return endpoint{}, nil, opError(CodeRemoteUnsupported, "push remote has multiple destinations", map[string]any{"reason": "multiple-destinations"})
				}
				if values[0] == "" {
					return endpoint{}, nil, opError(CodeRemoteUnresolved, "push URL is empty", map[string]any{"remote": req.Remote})
				}
				raw = values[0]
			}
		}
		if raw == "" {
			values, absent, err := c.configValues(ctx, "remote."+req.Remote+".url")
			if err != nil {
				return endpoint{}, nil, err
			}
			if absent || len(values) == 0 || values[0] == "" {
				return endpoint{}, nil, opError(CodeRemoteUnresolved, "remote URL is not configured", map[string]any{"remote": req.Remote})
			}
			raw = values[0]
		}
	}
	rules, err := c.rewriteRules(ctx, req.Verb == "clone")
	if err != nil {
		return endpoint{}, nil, err
	}
	// Apply git's URL rewriting exactly once. pushInsteadOf is honoured only for a push
	// that uses remote.<name>.url — git ignores pushInsteadOf when an explicit pushurl is
	// set — but ordinary insteadOf STILL applies to an explicit pushurl, so we must not
	// skip rewriting entirely.
	effective := applyRewrite(raw, rules, req.Verb == "push" && !explicitPushURL)
	ep, err := parseEndpoint(effective)
	if err != nil {
		return endpoint{}, nil, err
	}
	return ep, rules, nil
}

func (c *Client) configValues(ctx context.Context, key string) ([]string, bool, error) {
	r := c.command(ctx, runRequest{Name: "git", Args: []string{"config", "--null", "--get-all", key}})
	if timedOut(ctx, r) {
		return nil, false, timeoutError(r)
	}
	if r.ExitCode == 1 && r.Err == nil {
		return nil, true, nil
	}
	if r.ExitCode != 0 || r.Err != nil {
		return nil, false, opError(CodeRemoteUnresolved, "could not read remote config: "+r.Stderr, failureDetails(r, nil))
	}
	if r.StdoutTruncated {
		return nil, false, opError(CodeRemoteUnresolved, "remote config output exceeded the capture bound", nil)
	}
	return splitValues(r.Stdout), false, nil
}

func splitValues(output string) []string {
	if output == "" {
		return nil
	}
	if strings.ContainsRune(output, '\x00') {
		parts := strings.Split(strings.TrimSuffix(output, "\x00"), "\x00")
		return parts
	}
	lines := strings.Split(strings.TrimSuffix(output, "\n"), "\n")
	return lines
}

func (c *Client) rewriteRules(ctx context.Context, globalOnly bool) ([]rewriteRule, error) {
	request := runRequest{Name: "git", Args: []string{"config", "--null", "--get-regexp", `^url\..*\.(insteadof|pushinsteadof)$`}}
	if globalOnly {
		request.Dir = "/"
	}
	r := c.command(ctx, request)
	if timedOut(ctx, r) {
		return nil, timeoutError(r)
	}
	if r.ExitCode == 1 && r.Err == nil {
		return nil, nil
	}
	if r.ExitCode != 0 || r.Err != nil {
		return nil, opError(CodeRemoteUnresolved, "could not enumerate URL rewrite config: "+r.Stderr, failureDetails(r, nil))
	}
	if r.StdoutTruncated {
		return nil, opError(CodeRemoteUnresolved, "URL rewrite config output exceeded the capture bound", nil)
	}
	return parseRules(r.Stdout), nil
}

func parseRules(output string) []rewriteRule {
	entries := splitValues(output)
	rules := make([]rewriteRule, 0, len(entries))
	for _, entry := range entries {
		key, value, ok := strings.Cut(entry, "\n")
		if !ok {
			key, value, ok = strings.Cut(entry, " ")
		}
		if !ok {
			continue
		}
		lower := strings.ToLower(key)
		push := strings.HasSuffix(lower, ".pushinsteadof")
		suffix := ".insteadof"
		if push {
			suffix = ".pushinsteadof"
		}
		if !strings.HasPrefix(lower, "url.") || !strings.HasSuffix(lower, suffix) {
			continue
		}
		base := key[len("url.") : len(key)-len(suffix)]
		if value != "" {
			rules = append(rules, rewriteRule{Key: key, Base: base, Prefix: value, Push: push})
		}
	}
	return rules
}

func (c *Client) pushDestinations(ctx context.Context, remote string) ([]string, error) {
	r := c.command(ctx, runRequest{Name: "git", Args: []string{"remote", "get-url", "--push", "--all", remote}})
	if timedOut(ctx, r) {
		return nil, timeoutError(r)
	}
	if r.ExitCode != 0 || r.Err != nil {
		return nil, opError(CodeRemoteUnresolved, "could not resolve push destination: "+r.Stderr, failureDetails(r, nil))
	}
	if r.StdoutTruncated {
		return nil, opError(CodeRemoteUnresolved, "push destination output exceeded the capture bound", nil)
	}
	values := splitValues(strings.ReplaceAll(r.Stdout, "\n", "\x00"))
	if len(values) == 0 {
		return nil, opError(CodeRemoteUnresolved, "push remote has no destination", map[string]any{"remote": remote})
	}
	return values, nil
}

func multipleDestinationsError() error {
	return opError(CodeRemoteUnsupported, "push remote has multiple destinations", map[string]any{"reason": "multiple-destinations"})
}

func destinationsDisagree(values []string) bool {
	want := ""
	for _, value := range values {
		ep, err := parseEndpoint(value)
		if err != nil || ep.Owner == "" || ep.Repo == "" {
			return true
		}
		identity := strings.ToLower(ep.Owner + "/" + ep.Repo)
		if want == "" {
			want = identity
		} else if identity != want {
			return true
		}
	}
	return false
}

func (c *Client) requireGH(ctx context.Context) error {
	status := c.command(ctx, runRequest{Name: "gh", Args: []string{"auth", "status", "--hostname", "github.com"}})
	if timedOut(ctx, status) {
		return timeoutError(status)
	}
	if status.ExitCode != 0 || status.Err != nil {
		return opError(CodeGHUnavailable, "gh is not authenticated for github.com", failureDetails(status, nil))
	}
	credential := c.command(ctx, runRequest{Name: "gh", Args: []string{"auth", "git-credential", "get"}, Stdin: "protocol=https\nhost=github.com\n\n"})
	if timedOut(ctx, credential) {
		return timeoutError(credential)
	}
	if credential.ExitCode != 0 || credential.Err != nil || !hasPassword(credential.Stdout) {
		return opError(CodeGHUnavailable, "gh did not provide a github.com credential", failureDetails(credential, nil))
	}
	if credential.StdoutTruncated {
		return opError(CodeGHUnavailable, "gh credential output exceeded the capture bound", nil)
	}
	return nil
}

func (c *Client) requireSSH(ctx context.Context) error {
	check := c.command(ctx, runRequest{Name: "ssh", Args: []string{"-V"}})
	if timedOut(ctx, check) {
		return timeoutError(check)
	}
	if check.ExitCode != 0 || check.Err != nil {
		return opError(CodeSSHUnavailable, "ssh is unavailable", failureDetails(check, nil))
	}
	return nil
}

func hasPassword(output string) bool {
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "password=") && len(line) > len("password=") {
			return true
		}
	}
	return false
}

func (c *Client) fallback(ctx context.Context, req Request, ep endpoint, rules []rewriteRule, result *Result) (*Result, error) {
	canonical := canonicalHTTPS(ep)
	// All deterministic fallback validation runs BEFORE gh credential access (Sol build
	// review): rewrite-safety, remote-config allow-list, synthetic-name, and the
	// explicit-refspec requirement. The transport fields on `result` are set only once
	// everything has passed, immediately before launching the fallback child, so a
	// FALLBACK_BLOCKED can never leave a "https-gh" transport claim on a run that never ran.
	for _, rule := range rules {
		if rule.Prefix != "" && strings.HasPrefix(canonical, rule.Prefix) {
			return nil, opError(CodeFallbackBlocked, "fallback URL would be rewritten by "+rule.Key, map[string]any{"reason": "insteadof-rewrite", "rule": rule.Key, "prefix": rule.Prefix})
		}
	}
	if req.Verb == "clone" {
		if err := c.requireGH(ctx); err != nil {
			return nil, err
		}
		result.Auth, result.URL, result.Host, result.FellBack = "https-gh", canonical, "github.com", true
		fallbackEP := endpoint{Raw: canonical, Redacted: canonical, Scheme: "https", Host: "github.com"}
		op := c.nativeOp(ctx, req, fallbackEP, true, true)
		return finish(result, op, true, fallbackEP)
	}
	settings, err := c.remoteSettings(ctx, req.Remote)
	if err != nil {
		return nil, err
	}
	if req.Verb == "push" && len(req.Refspecs) == 0 && !hasSetting(settings, "push") {
		return nil, opError(CodeFallbackBlocked, "fallback push requires an explicit refspec; use HEAD:<branch>", map[string]any{"reason": "explicit-refspec-required"})
	}
	synthetic, err := c.syntheticName(ctx)
	if err != nil {
		return nil, err
	}
	if err := c.requireGH(ctx); err != nil {
		return nil, err
	}
	result.Auth, result.URL, result.Host, result.FellBack = "https-gh", canonical, "github.com", true
	args := []string{"-c", "remote." + synthetic + ".url=" + canonical}
	for _, setting := range settings {
		if setting.key == "push" && len(req.Refspecs) > 0 {
			continue
		}
		args = append(args, "-c", "remote."+synthetic+"."+setting.key+"="+setting.value)
	}
	args = append(args, "-c", "credential.helper=", "-c", "credential.helper="+credentialHelper, req.Verb, "--", synthetic)
	args = append(args, req.Refspecs...)
	op := c.command(ctx, runRequest{Name: "git", Args: args, LiveStdout: req.LiveStdout, LiveStderr: req.LiveStderr})
	return finish(result, op, true, endpoint{Scheme: "https", Host: "github.com", Redacted: canonical})
}

type remoteSetting struct{ key, value string }

var allowedRemoteSettings = map[string]bool{
	"fetch": true, "tagopt": true, "prune": true, "prunetags": true,
	"partialclonefilter": true, "promisor": true, "push": true,
	"url": true, "pushurl": true,
}

func (c *Client) remoteSettings(ctx context.Context, remote string) ([]remoteSetting, error) {
	pattern := "^remote\\." + regexp.QuoteMeta(remote) + "\\..*$"
	r := c.command(ctx, runRequest{Name: "git", Args: []string{"config", "--null", "--get-regexp", pattern}})
	if timedOut(ctx, r) {
		return nil, timeoutError(r)
	}
	if r.ExitCode == 1 && r.Err == nil {
		return nil, nil
	}
	if r.ExitCode != 0 || r.Err != nil {
		return nil, opError(CodeFallbackBlocked, "could not enumerate effective remote config", map[string]any{"reason": "unsupported-remote-config", "stderr_tail": tail(r.Stderr, outputTailBytes)})
	}
	if r.StdoutTruncated {
		return nil, opError(CodeFallbackBlocked, "effective remote config output exceeded the capture bound", map[string]any{"reason": "unsupported-remote-config"})
	}
	prefix := strings.ToLower("remote." + remote + ".")
	var settings []remoteSetting
	for _, entry := range splitValues(r.Stdout) {
		key, value, ok := strings.Cut(entry, "\n")
		if !ok {
			key, value, ok = strings.Cut(entry, " ")
		}
		if !ok || !strings.HasPrefix(strings.ToLower(key), prefix) {
			continue
		}
		name := key[len(prefix):]
		lower := strings.ToLower(name)
		if !allowedRemoteSettings[lower] {
			return nil, opError(CodeFallbackBlocked, "unsupported remote config key "+key, map[string]any{"reason": "unsupported-remote-config", "key": key})
		}
		if lower != "url" && lower != "pushurl" {
			settings = append(settings, remoteSetting{key: name, value: value})
		}
	}
	return settings, nil
}

func hasSetting(settings []remoteSetting, key string) bool {
	for _, setting := range settings {
		if strings.EqualFold(setting.key, key) {
			return true
		}
	}
	return false
}

func (c *Client) syntheticName(ctx context.Context) (string, error) {
	r := c.command(ctx, runRequest{Name: "git", Args: []string{"remote"}})
	if timedOut(ctx, r) {
		return "", timeoutError(r)
	}
	if r.ExitCode != 0 || r.Err != nil {
		return "", opError(CodeFallbackBlocked, "could not enumerate remotes", map[string]any{"reason": "unsupported-remote-config"})
	}
	if r.StdoutTruncated {
		return "", opError(CodeFallbackBlocked, "remote enumeration output exceeded the capture bound", map[string]any{"reason": "unsupported-remote-config"})
	}
	existing := map[string]bool{}
	for _, name := range strings.Fields(r.Stdout) {
		existing[name] = true
	}
	for i := 0; i < 1000; i++ {
		name := "aira-fallback"
		if i > 0 {
			name += "-" + strconv.Itoa(i)
		}
		if !existing[name] {
			return name, nil
		}
	}
	return "", opError(CodeFallbackBlocked, "no collision-free synthetic remote name", map[string]any{"reason": "unsupported-remote-config"})
}

func commandEnv(extra []string, connect time.Duration) []string {
	env := scrubEnv(os.Environ(), false)
	env = append(env,
		"GIT_TERMINAL_PROMPT=0",
		"GCM_INTERACTIVE=never",
		"GIT_SSH_COMMAND=ssh -o BatchMode=yes -o ConnectTimeout="+strconv.Itoa(max(1, int(connect/time.Second))),
	)
	env = append(env, extra...)
	return env
}

// rewriteFreeEnv suppresses only config-rewrite injection (GIT_CONFIG_*) so the
// probe cannot reapply insteadOf; it must PRESERVE the non-interactive vars that
// commandEnv already set (GIT_TERMINAL_PROMPT/GCM_INTERACTIVE/GIT_SSH_COMMAND),
// hence it does not call scrubEnv's interactive strip.
func rewriteFreeEnv(env []string) []string {
	out := make([]string, 0, len(env)+2)
	for _, item := range env {
		key, _, _ := strings.Cut(item, "=")
		if key == "GIT_CONFIG_COUNT" || key == "GIT_CONFIG_PARAMETERS" || key == "GIT_CONFIG_GLOBAL" || key == "GIT_CONFIG_SYSTEM" || strings.HasPrefix(key, "GIT_CONFIG_KEY_") || strings.HasPrefix(key, "GIT_CONFIG_VALUE_") {
			continue
		}
		out = append(out, item)
	}
	return append(out, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
}

func scrubEnv(env []string, rewrite bool) []string {
	out := make([]string, 0, len(env))
	for _, item := range env {
		key, _, _ := strings.Cut(item, "=")
		if key == "GIT_TERMINAL_PROMPT" || key == "GCM_INTERACTIVE" || key == "GIT_SSH_COMMAND" {
			continue
		}
		if rewrite && (key == "GIT_CONFIG_COUNT" || key == "GIT_CONFIG_PARAMETERS" || key == "GIT_CONFIG_GLOBAL" || key == "GIT_CONFIG_SYSTEM" || strings.HasPrefix(key, "GIT_CONFIG_KEY_") || strings.HasPrefix(key, "GIT_CONFIG_VALUE_")) {
			continue
		}
		out = append(out, item)
	}
	return out
}

var (
	// HTTPS userinfo is a potential credential (bare token or user:token) → redact ALL
	// of it. For other schemes (ssh) redact only credential-bearing userinfo (user:secret@)
	// so a bare ssh username (ssh://git@host) stays in the honest URL.
	httpsUserinfoPattern = regexp.MustCompile(`(?i)(https?://)[^/@\s]+@`)
	otherUserinfoPattern = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^/@\s]*:[^/@\s]*@`)
	// Length/format-aware so real tokens (ghX_ + a long unbroken run; a long
	// github_pat_ body) are scrubbed while short lookalikes such as an org or
	// repo named "ghp_tools" are not over-redacted.
	tokenPattern = regexp.MustCompile(`(?i)(github_pat_[A-Za-z0-9_]{20,}|gh[pousr]_[A-Za-z0-9]{16,}|Bearer[ \t]+[A-Za-z0-9._~+/=-]+|password=[^\s]+)`)
)

// RedactURL removes credential-bearing URL components while retaining enough
// endpoint identity for provenance and diagnostics.
func RedactURL(value string) string {
	trimmed := strings.TrimSpace(value)
	if parsed, err := url.Parse(trimmed); err == nil && parsed.Scheme != "" && parsed.Host != "" {
		if strings.EqualFold(parsed.Scheme, "http") || strings.EqualFold(parsed.Scheme, "https") || (parsed.User != nil && strings.Contains(parsed.User.String(), ":")) {
			parsed.User = nil
		}
		parsed.RawQuery = ""
		parsed.ForceQuery = false
		parsed.Fragment = ""
		// A token can hide in a path segment (…/ghp_x/repo) that structural
		// redaction leaves untouched; scrub every serialised return path.
		return scrubTokens(parsed.String())
	}
	trimmed = httpsUserinfoPattern.ReplaceAllString(trimmed, `${1}***@`)
	trimmed = otherUserinfoPattern.ReplaceAllString(trimmed, `${1}***@`)
	if at := strings.IndexByte(trimmed, '@'); at > 0 && strings.Contains(trimmed[:at], ":") && !strings.Contains(trimmed[:at], "/") {
		trimmed = "***" + trimmed[at:]
	}
	if cut := strings.IndexAny(trimmed, "?#"); cut >= 0 {
		trimmed = trimmed[:cut]
	}
	// SCP-style git@host:path and any residual bare token (no userinfo colon)
	// are only caught by the token scrub below.
	return scrubTokens(trimmed)
}

func scrubTokens(value string) string { return tokenPattern.ReplaceAllString(value, "***") }

func redactURL(value string) string { return RedactURL(value) }
func redact(value string) string    { return scrubTokens(redactURL(value)) }

func redactDetails(details map[string]any) map[string]any {
	if details == nil {
		return nil
	}
	out := make(map[string]any, len(details))
	for key, value := range details {
		if s, ok := value.(string); ok {
			out[key] = redact(s)
		} else {
			out[key] = value
		}
	}
	return out
}

func tail(value string, cap int) string {
	if len(value) <= cap {
		return value
	}
	return value[len(value)-cap:]
}
