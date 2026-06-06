package analyze

// Default returns the registry with all built-in analyzers in precedence order.
// Helm is registered before docs so chart YAML is not swallowed by the doc match.
// crossRepoPrefix is forwarded to the Go analyzer for requires symbol emission.
func Default(crossRepoPrefix string) *Registry {
	r := NewRegistry()
	r.Register(NewGo(crossRepoPrefix))
	r.Register(NewPython())
	r.Register(NewJavaScript())
	r.Register(NewTerraform())
	r.Register(NewHelm())
	r.Register(NewDocs())
	return r
}
