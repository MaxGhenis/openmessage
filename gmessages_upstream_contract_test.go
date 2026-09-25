package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"
)

const (
	gmessagesModule          = "go.mau.fi/mautrix-gmessages"
	minimumGMessagesTime     = "20260910090721"
	gmessagesAuthRetryCommit = "0b54a8fe65207f81d353ffe63f4d2549c2eb7976"
)

type moduleVersion struct {
	Path    string
	Version string
}

type moduleReplacement struct {
	Old moduleVersion
	New moduleVersion
}

type goModFile struct {
	Require []moduleVersion
	Replace []moduleReplacement
}

func directGMessagesRequirement(mod goModFile) (moduleVersion, error) {
	for _, replacement := range mod.Replace {
		if replacement.Old.Path == gmessagesModule {
			return moduleVersion{}, fmt.Errorf("%s must resolve directly from upstream, without any replacement", gmessagesModule)
		}
	}
	for _, requirement := range mod.Require {
		if requirement.Path == gmessagesModule {
			return requirement, nil
		}
	}
	return moduleVersion{}, fmt.Errorf("%s must remain a direct dependency", gmessagesModule)
}

func TestGMessagesUpstreamContract(t *testing.T) {
	cmd := exec.Command("go", "mod", "edit", "-json", "go.mod")
	cmd.Env = envWithGOWorkOff()
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("parse go.mod: %v\n%s", err, output)
	}
	var mod goModFile
	if err := json.Unmarshal(output, &mod); err != nil {
		t.Fatalf("decode go.mod: %v", err)
	}
	requirement, err := directGMessagesRequirement(mod)
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`[.-]([0-9]{14})-([0-9a-f]{12})$`).FindStringSubmatch(requirement.Version)
	if match == nil {
		t.Fatalf("%s must use an immutable upstream pseudo-version; found %q", gmessagesModule, requirement.Version)
	}
	pinnedTime, err := time.Parse("20060102150405", match[1])
	if err != nil {
		t.Fatalf("parse upstream commit timestamp: %v", err)
	}
	minimumTime, err := time.Parse("20060102150405", minimumGMessagesTime)
	if err != nil {
		t.Fatal(err)
	}
	if pinnedTime.Before(minimumTime) {
		t.Fatal("upstream dependency predates the current context-aware API baseline")
	}
	workflow, err := os.ReadFile(".github/workflows/gmessages-upstream.yml")
	if err != nil {
		t.Fatalf("read upstream dependency workflow: %v", err)
	}
	contract := make(map[string]string)
	for _, entry := range regexp.MustCompile(`(?m)^  ([A-Z_]+): "?([^"\n]+)"?$`).FindAllStringSubmatch(string(workflow), -1) {
		contract[entry[1]] = entry[2]
	}
	for key, want := range map[string]string{
		"UPSTREAM_REPOSITORY":       "mautrix/gmessages",
		"UPSTREAM_BRANCH":           "main",
		"UPSTREAM_MODULE":           gmessagesModule,
		"UPSTREAM_VERSION":          requirement.Version,
		"AUTH_REFRESH_RETRY_COMMIT": gmessagesAuthRetryCommit,
	} {
		if got := contract[key]; got != want {
			t.Errorf("upstream workflow %s = %q; want %q", key, got, want)
		}
	}
	commit := contract["UPSTREAM_COMMIT"]
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(commit) {
		t.Fatalf("workflow must record the full upstream commit; found %q", commit)
	}
	if !strings.HasPrefix(commit, match[2]) {
		t.Errorf("workflow commit %s does not match dependency version %s", commit, requirement.Version)
	}
}

func TestGMessagesRejectsDependencyReplacement(t *testing.T) {
	for _, tc := range []struct {
		name      string
		require   []moduleVersion
		replace   []moduleReplacement
		wantError bool
	}{
		{name: "direct", require: []moduleVersion{{Path: gmessagesModule, Version: "test-version"}}},
		{name: "missing", wantError: true},
		{name: "module replacement", require: []moduleVersion{{Path: gmessagesModule}}, replace: []moduleReplacement{{Old: moduleVersion{Path: gmessagesModule}, New: moduleVersion{Path: "example.invalid/replacement", Version: "v1.0.0"}}}, wantError: true},
		{name: "version-specific replacement", require: []moduleVersion{{Path: gmessagesModule}}, replace: []moduleReplacement{{Old: moduleVersion{Path: gmessagesModule, Version: "v1.0.0"}, New: moduleVersion{Path: gmessagesModule, Version: "v0.1.0"}}}, wantError: true},
		{name: "local replacement", require: []moduleVersion{{Path: gmessagesModule}}, replace: []moduleReplacement{{Old: moduleVersion{Path: gmessagesModule}, New: moduleVersion{Path: "./replacement"}}}, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := directGMessagesRequirement(goModFile{Require: tc.require, Replace: tc.replace})
			if (err != nil) != tc.wantError {
				t.Fatalf("error = %v; wantError = %v", err, tc.wantError)
			}
		})
	}
}

func envWithGOWorkOff() []string {
	env := os.Environ()
	for i := 0; i < len(env); {
		if strings.HasPrefix(env[i], "GOWORK=") {
			env = append(env[:i], env[i+1:]...)
			continue
		}
		i++
	}
	return append(env, "GOWORK=off")
}
