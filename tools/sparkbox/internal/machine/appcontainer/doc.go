// Package appcontainer implements machine.Driver over Apple's `container` CLI.
//
// It is the ONLY code in the tree that knows the word "container": every
// provisioning step talks to machine.Driver, so a second backend (a direct
// Virtualization.framework driver, say) is a sibling package rather than a
// rewrite.
//
// # What was measured, and where the answers come from
//
// Everything below was established on a live Mac running `container CLI version
// 1.1.0 (build: release, commit: 5973b9c)`, macOS 26.5.2, Apple M4 Max. The
// captured outputs are in testdata/ verbatim, so the parsers are tested against
// what the CLI actually prints rather than against what it ought to.
//
//	structured output    machine list --format json, machine inspect (always
//	                     JSON, no flag), container inspect, image inspect,
//	                     system version --format json, system status --format json.
//	                     The TABLE forms are not renamings of the JSON: the table
//	                     column is STATE where the JSON key is "status", and the
//	                     default machine is "*" in the table and "default":true in
//	                     the JSON. Never parse a table.
//	exit codes           0 success; 1 the operation failed; 64 (EX_USAGE) an
//	                     unknown subcommand or flag. 64 is therefore free
//	                     version-skew detection with no version string to parse,
//	                     and it maps to machine.ErrUnsupported everywhere.
//	image existence      `image inspect <ref>` (exit 0/1) — NOT `image ls`.
//	                     `image ls` is broken on the target Mac today: one bad
//	                     content blob fails the whole listing in table AND
//	                     --format json ("Error: content with digest sha256:…",
//	                     exit 1) while `image inspect` and `image ls -q` keep
//	                     working. A port reaching for the "structured option"
//	                     fails on the machine it was written for.
//	the id is mandatory  machine inspect / stop / logs take it as an OPTIONAL
//	                     argument and fall back to the DEFAULT machine. Every
//	                     call here passes it.
//	the container id     `container cp` and `container inspect` accept ONLY the
//	                     containerId (`<name>-<n>`), never the machine name, and
//	                     it changes across boots — so it is read fresh from
//	                     `machine inspect` at every use, never cached.
//	the exec argv        `container machine run -i --root --name <n> -- bash -s`
//	                     is a FIXED LITERAL. -i is mandatory (without it stdin is
//	                     silently discarded and the call exits 0 having run
//	                     nothing); -t is never passed (it allocates a pty even
//	                     over a pipe, merging the streams and rewriting LF as
//	                     CRLF); --root is always passed (the default identity is
//	                     the mapped host user with an EMPTY $USER).
package appcontainer
