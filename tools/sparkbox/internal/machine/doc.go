// Package machine is the platform-neutral vocabulary for the nested linux VM a
// darwin host provisions its gateway into. It knows nothing about Apple's
// `container` CLI (that lives in machine/appcontainer) and nothing about
// sparkbox's provisioning steps (those live in internal/hostsetup), so a second
// backend — Virtualization.framework directly, say — implements Driver without
// touching either.
//
// # The transport contract, as measured on Apple Container 1.1.0
//
// Everything in exec.go exists because of one property of `container machine
// run`, established empirically on a live machine and reproduced six ways:
//
//	IT DOES NOT EXEC YOUR ARGV. It joins every argv element with single spaces
//	and evaluates the result as `/bin/bash -c "<joined>"` inside the machine.
//
// Proof, verbatim from that machine: `-- 'echo SHELL0=$0 BASHVER=$BASH_VERSION
// LOGIN=$-'` answered `SHELL0=/bin/bash BASHVER=5.2.21(1)-release LOGIN=hBc`
// (the `c` is `bash -c`); `-- /bin/echo 'A;/bin/echo B'` ran BOTH commands;
// `-- /bin/echo '/etc/hostn*'` glob-expanded; `-- sh -c 'sleep 1'` ran
// `sh -c sleep 1` and printed "sleep: missing operand". A Go caller writing the
// obvious exec.Command(...,"sh","-c",script) therefore ships a mangled fragment
// — and the fragment frequently still exits 0.
//
// Three more measured properties shape this package:
//
//   - STDIN IS SILENTLY DISCARDED WITHOUT -i. `printf X | container machine run
//     -n m -- cat` produced no output and exited 0. That is F7's exact shape:
//     a green result over work that never happened. It is the single reason the
//     receipt protocol below exists.
//   - A STDIN PAYLOAD AT OR ABOVE ~192 KiB DEADLOCKS. 64 KiB and 128 KiB
//     round-trip byte-exact; 192 KiB and 1 MiB hang with the guest never seeing
//     EOF, and the hang does not respond to a SIGTERM aimed at the pipeline. So
//     context cancellation is NOT a defence — the payload is capped instead.
//   - `container machine run` BOOTS A STOPPED MACHINE ("booting the container
//     machine if necessary"; its miss path literally reads "failed to boot
//     container machine"). A probe that boots what it measures is not a probe,
//     so state questions go through Inspect, never Exec.
//
// Two further facts a maintainer needs: `container machine inspect`, `stop` and
// `logs` all take the id as an OPTIONAL argument and fall back to the DEFAULT
// machine, so every call here passes it explicitly and machine names are
// validated non-empty; and `container image ls` is broken on at least one real
// target Mac today (a single bad content blob takes the whole listing down, in
// table AND --format json), so image existence is asked with `image inspect`.
package machine
