package ghapp

// Reading one file out of a repository.
//
// Everything else in this package hands a credential to somebody: MintToken
// returns a token, RepositoryID spends one internally but exists to serve a
// mint. This is the one verb that answers with *data*, and that is the whole
// point of it. `ctlops.GitHubApp` deliberately does not include MintToken
// (ops.go: "the discipline that keeps a token out of a log is much easier to
// hold when there is no token in the room"), so the environment builder — which
// wants to seed itself from a repository's `.sparkbox/setup.sh` before any VM
// exists to do the reading — needs a door that returns bytes and never a
// credential. The token minted below is scoped to one repository, read-only,
// and does not outlive this function's stack frame.
//
// Two things about the shape are deliberate.
//
// A missing file is the COMMON case, not a failure: most repositories have no
// `.sparkbox/setup.sh`, and `env build` asks every attached checkout in turn. So
// it gets its own sentinel, it costs one request, and nothing here logs about
// it.
//
// The read is bounded at maxFileBytes. This is somebody else's data arriving
// over somebody else's network, and the first caller feeds the result to `bash`
// — a truncated shell script is not a smaller shell script, it is a *different*
// one, whose last command may be half an `rm`. So an oversized file is refused
// whole rather than cut down to size.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
)

// ErrNoSuchFile means the repository was reachable and there is no such path at
// that ref. It is the answer to a question, not a fault: the caller asking
// "does this checkout carry a setup script?" gets it far more often than not.
//
// GitHub answers 404 both for a path that does not exist and for a repository
// this installation can no longer see, and the contents endpoint gives no way
// to tell them apart. The second is already impossible to reach here — the
// installation was resolved for this very repository a moment earlier, and a
// mint scoped to it would have failed first with ErrNotInstalled — so folding
// the pair into "no such file" costs nothing a caller could have acted on.
var ErrNoSuchFile = errors.New("that repository has no such file")

const contentsPath = "/repos/%s/%s/contents/%s"

const (
	// rawMediaType makes the response body the file itself. The alternative,
	// the default JSON envelope, would arrive base64-encoded with newlines in
	// it and would have to be decoded before it could be bounded — which means
	// buffering an unbounded body first, then discovering it was too big. Raw
	// lets io.LimitReader do the bounding on the wire, where it belongs.
	rawMediaType = "application/vnd.github.raw"

	// maxFileBytes bounds one read. A setup script is a page or two; 64 KiB is
	// two orders of magnitude of headroom and still small enough that a
	// repository full of large files cannot spend a gateway's memory through
	// this door.
	maxFileBytes = 64 << 10
)

// ReadFile returns the contents of owner/name's file at path, as of ref.
//
// ref may be empty, which means the repository's default branch. That is left
// to GitHub rather than resolved here: guessing "main" is wrong on every
// repository that never renamed master, and asking for the default branch by
// name would cost an extra round trip to learn a fact the contents endpoint
// already applies for free.
//
// The caller has already decided that this installation may be used on this
// caller's behalf (App.Authorize); this verb, like MintToken, does not re-decide
// it. What it does enforce is the scope of the credential it spends: one
// repository, `contents: read`, and nothing else.
//
// Errors: ErrNoSuchFile when the path is not there, ErrForbidden when GitHub
// refuses the App, ErrNotInstalled when the installation no longer covers the
// repository, ErrUpstream when github.com does not answer. A path that is a
// directory, a symlink or a submodule, and a file over maxFileBytes, are plain
// errors — both are misconfigurations with exactly one sensible fix, and
// neither is worth a sentinel a caller would only ever print.
func (a *App) ReadFile(ctx context.Context, inst Installation, owner, name, ref, path string) ([]byte, error) {
	slug, err := checkSlug(owner, name)
	if err != nil {
		return nil, err
	}
	if err := checkFilePath(path); err != nil {
		return nil, err
	}
	if err := checkRef(ref); err != nil {
		return nil, err
	}

	// Narrowed against what the App actually holds for the same reason
	// metadata/repos.go narrows: GitHub refuses a request naming a permission
	// the installation lacks outright rather than trimming it. With a single
	// permission asked for the intersection can only be the whole thing or
	// nothing, and `contents` is restored after it because a mint that fails
	// saying "the installation does not cover contents" is a better answer than
	// MintToken's local refusal of an empty permission map — which would read
	// as a bug in this file rather than a permission to grant on github.com.
	perms := inst.Narrow(map[string]string{"contents": PermRead})
	perms["contents"] = PermRead

	tok, err := a.MintToken(ctx, inst, []string{name}, perms)
	if err != nil {
		return nil, err
	}

	// The op string is what every error below is decorated with, so it names
	// the repository and the path and stops there: no ref-derived secrets, no
	// credential, and above all none of the bytes that come back.
	op := fmt.Sprintf("reading %s from %s", path, slug)
	target := fmt.Sprintf(contentsPath, url.PathEscape(owner), url.PathEscape(name), escapeFilePath(path))
	if ref != "" {
		target += "?" + url.Values{"ref": []string{ref}}.Encode()
	}

	body, ctype, err := a.getRaw(ctx, target, "Bearer "+tok.Token, op)
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.status == http.StatusNotFound {
			// Deliberately does not wrap ae: this is the expected answer for
			// most repositories, and an error carrying github's "Not Found"
			// prose reads like a fault in a log that only wanted a fact.
			return nil, fmt.Errorf("%w: %s has no %s%s", ErrNoSuchFile, slug, path, atRef(ref))
		}
		return nil, err
	}
	// A directory, a symlink and a submodule have no raw form, so GitHub
	// abandons the media type and answers with the JSON envelope instead. That
	// content type is the signal, and it is the only one available without
	// spending a second request on the object form. A refusal is the right
	// outcome either way: the caller asked for a file's bytes, and what came
	// back is not a file.
	if isJSON(ctype) {
		return nil, fmt.Errorf("%s: %s%s is not a regular file — a directory, symlink or submodule has no contents to read",
			op, path, atRef(ref))
	}
	if len(body) > maxFileBytes {
		return nil, fmt.Errorf("%s: that file is larger than %d bytes, which is more than this reads — a truncated script is a different script, so it is refused whole",
			op, maxFileBytes)
	}
	return body, nil
}

// getRaw issues one GET and returns the body, bounded, with its content type.
//
// It is App.do's twin rather than a branch inside it: do exists to decode
// github's JSON and its whole contract is "give me a struct", while this one's
// contract is "give me bytes and tell me nothing you were not asked". The two
// share every header, the timeout, and statusError, which is the part that
// mattered — the mapping from a status code to this package's sentinels stays
// in one place.
//
// auth is the complete Authorization header value, passed in for the same
// reason do takes it that way: this function never chooses a credential and so
// has none to name in an error.
func (a *App) getRaw(ctx context.Context, path, auth, op string) ([]byte, string, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.baseURL+path, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Accept", rawMediaType)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := a.hc.Do(req)
	if err != nil {
		// A *url.Error prints the method and the URL and nothing else — no
		// headers — so a transport failure cannot carry the token into a log.
		return nil, "", fmt.Errorf("%w: %s: %v", ErrUpstream, op, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode/100 != 2 {
		return nil, "", statusError(resp, op)
	}
	// One byte past the cap, so that "exactly at the limit" and "too large"
	// are distinguishable without trusting a Content-Length the peer wrote.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFileBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("%w: %s: could not read github's answer: %v", ErrUpstream, op, err)
	}
	return body, resp.Header.Get("Content-Type"), nil
}

// isJSON reports whether a Content-Type is github's JSON envelope, ignoring the
// charset parameter it usually carries. A body served under the raw media type
// is text/plain or an octet stream; JSON here means the raw form did not apply.
func isJSON(ctype string) bool {
	mt, _, err := mime.ParseMediaType(ctype)
	if err != nil {
		return false
	}
	mt = strings.ToLower(mt)
	return mt == "application/json" || strings.HasSuffix(mt, "+json")
}

// atRef renders the ref for a message, and nothing when it is the default
// branch — "…has no .sparkbox/setup.sh" is a complete sentence already.
func atRef(ref string) string {
	if ref == "" {
		return ""
	}
	return " at " + ref
}

// checkFilePath validates a path before it becomes URL path segments. The
// escaping below would already make an injected "../" inert, but a path that
// cannot mean what the caller intended is worth refusing where it is written
// rather than resolving to somebody else's file two layers down.
func checkFilePath(path string) error {
	if path == "" {
		return errors.New("no file path was given to read")
	}
	if len(path) > 512 {
		return fmt.Errorf("%q is too long to be a path in a repository", clip(path))
	}
	if strings.HasPrefix(path, "/") || strings.HasSuffix(path, "/") {
		return fmt.Errorf("%q is not a path inside a repository", clip(path))
	}
	for _, seg := range strings.Split(path, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("%q is not a path inside a repository", clip(path))
		}
	}
	if strings.ContainsFunc(path, isControl) {
		return errors.New("that file path carries control characters")
	}
	return nil
}

// checkRef bounds the thing that becomes the `ref` query parameter. It is not a
// full git ref grammar — a ref may be a branch, a tag or a bare commit sha, and
// this host is not the authority on which of those exists. It refuses the two
// classes that could only be a mistake: whitespace or control characters, and a
// length no ref has.
func checkRef(ref string) error {
	if ref == "" {
		return nil
	}
	if len(ref) > 255 {
		return fmt.Errorf("%q is too long to be a git ref", clip(ref))
	}
	if strings.ContainsFunc(ref, func(r rune) bool { return isControl(r) || r == ' ' }) {
		return fmt.Errorf("%q is not a git ref", clip(ref))
	}
	return nil
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f }

// escapeFilePath escapes each segment and rejoins on "/", because the slashes
// are structure and everything between them is a name that may not be.
func escapeFilePath(path string) string {
	segs := strings.Split(path, "/")
	for i, seg := range segs {
		segs[i] = url.PathEscape(seg)
	}
	return strings.Join(segs, "/")
}

// clip bounds a caller's own string on its way into an error, so that a
// pathological argument cannot write a pathological log line.
func clip(s string) string {
	if len(s) <= 64 {
		return s
	}
	return s[:64] + "…"
}
