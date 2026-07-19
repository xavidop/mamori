package mamori

// The built-in providers (env: and file://) are registered automatically so they
// work with no extra import. The opt-in exec: provider lives in provider/exec and
// must be enabled explicitly with WithExecProvider for security reasons.
func init() {
	Register(envProvider{})
	Register(fileProvider{})
	Register(dotenvProvider{})
}
