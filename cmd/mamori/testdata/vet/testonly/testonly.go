// Package testonly holds no violation in its non-test file; the violation
// lives in the test file beside it, so this package proves that vet analyzes
// _test.go sources (matching what `go vet -vettool` does).
package testonly
