package testonly

import "testing"

// TestConfig declares a config struct in a test file that stores a
// secret-bearing source in a plain string. mamori vet must report it, the
// same as `go vet -vettool=mamori` does.
func TestConfig(t *testing.T) {
	type Config struct {
		TestSecret string `source:"vault://kv/test#password"`
	}
	_ = Config{}
}
