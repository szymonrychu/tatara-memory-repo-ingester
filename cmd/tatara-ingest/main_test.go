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

// TestOIDCValidation_MissingIssuerFails verifies finding 5: when oidc-client-id
// is set but oidc-issuer is empty, realMain must return a clear config error
// before making any HTTP call (fail-fast, not a lazy oauth2 error on first push).
func TestOIDCValidation_MissingIssuerFails(t *testing.T) {
	// We test validateOIDCConfig directly since realMain wires env and flags.
	err := validateOIDCConfig("my-client-id", "", "my-secret")
	if err == nil {
		t.Fatal("expected error when oidc-client-id is set but oidc-issuer is empty")
	}
}

func TestOIDCValidation_MissingSecretFails(t *testing.T) {
	err := validateOIDCConfig("my-client-id", "https://auth.example.com", "")
	if err == nil {
		t.Fatal("expected error when oidc-client-id is set but oidc-client-secret is empty")
	}
}

func TestOIDCValidation_InvalidIssuerURLFails(t *testing.T) {
	err := validateOIDCConfig("my-client-id", "not-a-url", "my-secret")
	if err == nil {
		t.Fatal("expected error when oidc-issuer is not a valid URL")
	}
}

func TestOIDCValidation_ValidConfigPasses(t *testing.T) {
	err := validateOIDCConfig("my-client-id", "https://auth.example.com", "my-secret")
	if err != nil {
		t.Fatalf("unexpected error for valid OIDC config: %v", err)
	}
}

func TestOIDCValidation_NoClientIDSkips(t *testing.T) {
	// When client-id is empty, no validation is needed regardless of other fields.
	err := validateOIDCConfig("", "", "")
	if err != nil {
		t.Fatalf("unexpected error when oidc-client-id is empty: %v", err)
	}
}

// TestResolveOptions_GetenvWired verifies finding 5: resolveOptions must wire
// its getenv parameter into o.getenv so production code and tests share the same
// env source rather than leaving o.getenv nil in prod (which hides the source).
func TestResolveOptions_GetenvWired(t *testing.T) {
	called := false
	sentinel := func(key string) string {
		called = true
		return map[string]string{"REPO_ROOT": "/some/path"}[key]
	}
	o, err := resolveOptions([]string{}, sentinel)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if o.getenv == nil {
		t.Fatal("o.getenv must be wired from resolveOptions; was nil in production (finding 5)")
	}
	// Call it and verify it delegates to the injected function.
	_ = called // sentinel was called during flag parsing via envKey
	got := o.getenv("REPO_ROOT")
	if got != "/some/path" {
		t.Errorf("o.getenv(REPO_ROOT) = %q, want /some/path", got)
	}
}
