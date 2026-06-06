package analyze_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-memory-repo-ingester/internal/analyze"
)

func TestDefaultRegistryPrecedence(t *testing.T) {
	reg := analyze.Default("github.com/szymonrychu/")
	g := reg.Group([]string{
		"main.go", "app.py", "web.js", "main.tf",
		"mychart/templates/deployment.yaml", "README.md",
	})
	require.Equal(t, []string{"main.go"}, g["go"])
	require.Equal(t, []string{"app.py"}, g["python"])
	require.Equal(t, []string{"web.js"}, g["javascript"])
	require.Equal(t, []string{"main.tf"}, g["terraform"])
	require.Equal(t, []string{"mychart/templates/deployment.yaml"}, g["helm"])
	require.Equal(t, []string{"README.md"}, g["docs"])
}
