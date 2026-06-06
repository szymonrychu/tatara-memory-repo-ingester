//go:build integration

package main

import (
	"context"
	"net/http"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// Requires a running tatara-memory and:
//
//	TATARA_TEST_BASE_URL  e.g. http://localhost:8080
//	TATARA_TEST_REPO_ROOT a checked-out repo to ingest
func TestE2EIngest(t *testing.T) {
	base := os.Getenv("TATARA_TEST_BASE_URL")
	root := os.Getenv("TATARA_TEST_REPO_ROOT")
	if base == "" || root == "" {
		t.Skip("set TATARA_TEST_BASE_URL and TATARA_TEST_REPO_ROOT")
	}
	o := options{repoRoot: root, repoName: "e2e-fixture", baseURL: base}
	require.NoError(t, run(context.Background(), o, http.DefaultClient))

	resp, err := http.Get(base + "/code/entities?repo=e2e-fixture&limit=1")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
}
