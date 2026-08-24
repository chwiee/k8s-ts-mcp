package agentcore

import (
	"regexp"
	"strings"
)

// execFormatErrorSignatures catches the container runtime's own error text
// for "the kernel couldn't execute this binary at all" — almost always an
// image built for the wrong CPU architecture. This is the ONLY reliable
// signal available: the Kubernetes status API frequently exposes no message
// at all for this failure (Terminated.Message empty, just an exit code —
// confirmed live testing an arm64 image on an amd64 kind node), so the real
// text only ever shows up in the container's own log output.
//
//   - "exec format error" / "exec user process caused" are the OCI
//     runtime's own wording for ENOEXEC — high confidence, always this
//     failure.
//   - "exec <path>: no such file or directory" is runc/containerd's
//     disguised form of the same ENOEXEC on some runtimes (observed live:
//     "exec /bin/sh: no such file or directory" for the arm64 case above) —
//     weaker signal on its own, since a genuinely missing binary (e.g. a
//     distroless image with no shell) produces the identical text. It's
//     only trusted here in combination with the caller's other evidence
//     (RestartCount > 0, no image-pull problem) — see refineExecFormatError.
var execFormatErrorSignatures = []*regexp.Regexp{
	regexp.MustCompile(`(?i)exec format error`),
	regexp.MustCompile(`(?i)exec user process caused`),
	regexp.MustCompile(`(?i)^exec [^\s:]+: no such file or directory`),
}

// looksLikeExecFormatError reports whether logs contain one of the known
// exec-format-error signatures on any line.
func looksLikeExecFormatError(logs string) bool {
	for _, line := range strings.Split(logs, "\n") {
		for _, sig := range execFormatErrorSignatures {
			if sig.MatchString(line) {
				return true
			}
		}
	}
	return false
}
