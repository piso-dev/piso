package main

import (
	"testing"

	"piso/cli/internal/pisoconfig"
)

func TestPiVersionFromDockerfile(t *testing.T) {
	df := `# comment
ARG PISO_PI_VERSION=0.84.4
RUN npm install -g @earendil-works/pi-coding-agent@${PISO_PI_VERSION}
`
	if got := pisoconfig.PiVersionFromDockerfile(df); got != "0.84.4" {
		t.Fatalf("PiVersionFromDockerfile = %q, want 0.84.4", got)
	}
	if got := pisoconfig.PiVersionFromDockerfile("no version here"); got != "" {
		t.Fatalf("PiVersionFromDockerfile on absent = %q, want \"\"", got)
	}
}

func TestRewriteWorkerDockerfileARG(t *testing.T) {
	df := `ARG PISO_PI_VERSION=0.84.4
RUN npm install -g @earendil-works/pi-coding-agent@${PISO_PI_VERSION}
`
	got := pisoconfig.RewriteWorkerDockerfileARG(df, "0.85.0")
	if got != `ARG PISO_PI_VERSION=0.85.0
RUN npm install -g @earendil-works/pi-coding-agent@${PISO_PI_VERSION}
` {
		t.Fatalf("RewriteWorkerDockerfileARG = %q", got)
	}
	if pisoconfig.RewriteWorkerDockerfileARG(got, "0.85.0") != got {
		t.Fatalf("RewriteWorkerDockerfileARG not idempotent")
	}
}

func TestFmtInterval(t *testing.T) {
	cases := map[int]string{
		0: "0s", 30: "30s", 60: "1m", 125: "2m",
		3600: "1h0m", 3661: "1h1m",
	}
	for in, want := range cases {
		if got := fmtInterval(in); got != want {
			t.Fatalf("fmtInterval(%d) = %q, want %q", in, got, want)
		}
	}
}