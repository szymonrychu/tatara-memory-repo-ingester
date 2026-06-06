package main

import (
	"testing"
)

func noEnv(_ string) string { return "" }

func envWith(m map[string]string) func(string) string {
	return func(key string) string { return m[key] }
}

func TestResolveOptions_EnvOnly(t *testing.T) {
	o, err := resolveOptions([]string{}, envWith(map[string]string{
		"REPO_ROOT": "/some/path",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if o.repoRoot != "/some/path" {
		t.Errorf("repoRoot: got %q, want %q", o.repoRoot, "/some/path")
	}
	if o.repoName != "path" {
		t.Errorf("repoName: got %q, want %q (basename fallback)", o.repoName, "path")
	}
}

func TestResolveOptions_MissingRepoRoot(t *testing.T) {
	_, err := resolveOptions([]string{}, noEnv)
	if err != errMissingRepoRoot {
		t.Errorf("got %v, want errMissingRepoRoot", err)
	}
}

func TestResolveOptions_FlagOverridesEnv(t *testing.T) {
	o, err := resolveOptions([]string{"--repo-root=/x"}, envWith(map[string]string{
		"REPO_ROOT": "/env-path",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if o.repoRoot != "/x" {
		t.Errorf("repoRoot: got %q, want /x (flag should override env)", o.repoRoot)
	}
}

func TestResolveOptions_ExplicitRepoName(t *testing.T) {
	o, err := resolveOptions([]string{"--repo-root=/a/b", "--repo-name=custom"}, noEnv)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if o.repoName != "custom" {
		t.Errorf("repoName: got %q, want custom", o.repoName)
	}
}
