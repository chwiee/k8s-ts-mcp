package agentcore

import "testing"

func TestLooksLikeExecFormatError(t *testing.T) {
	cases := []struct {
		name string
		logs string
		want bool
	}{
		{"direct kernel message", "exec format error", true},
		{"runc classic wording", "OCI runtime exec failed: exec user process caused: exec format error: unknown", true},
		{
			name: "disguised form seen live testing an arm64 image on an amd64 node",
			logs: "exec /bin/sh: no such file or directory",
			want: true,
		},
		{"signature buried among other log lines", "starting up...\nreading config...\nexec /entrypoint.sh: no such file or directory\n", true},
		{"unrelated crash", "panic: runtime error: index out of range [3] with length 2\n", false},
		{"genuinely missing file, not an exec at all", "open /etc/config.yaml: no such file or directory", false},
		{"empty logs", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := looksLikeExecFormatError(tc.logs); got != tc.want {
				t.Errorf("looksLikeExecFormatError(%q) = %v, want %v", tc.logs, got, tc.want)
			}
		})
	}
}
