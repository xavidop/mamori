// Package tools is the root of the repository's own tooling, a module that is
// never released. The commands live in their own directories (release/).
//
// This package also keeps `go build ./...` from writing a binary: with a
// single main package to build, go build would write it next to the
// directory of the same name.
package tools
