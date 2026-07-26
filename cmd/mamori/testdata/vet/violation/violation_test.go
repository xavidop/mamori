package violation

import "testing"

// TestConfigDeclared exists so this package has a test variant, which is what
// makes packages.Config{Tests: true} load the package more than once. The vet
// fixture asserts the non-test violation above is still reported exactly once.
func TestConfigDeclared(t *testing.T) {
	if (Config{}).DBPassword != "" {
		t.Fatal("zero Config should have an empty DBPassword")
	}
}
