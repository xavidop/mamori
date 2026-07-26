// This file holds the one definition of the --secret-schemes flag shared by
// every command that decides whether a field is sensitive: explain, schema,
// policy, vet, and doctor --compare.
//
// They share it because they must agree. All of them answer the same
// question, "does this source ref carry a secret", from the same built-in
// scheme set (internal/sourcetag), and an operator who extends that set for
// one command would be badly served if another quietly kept the old answer:
// `mamori explain` calling a field non-sensitive while `mamori vet` flags it
// is worse than either answer alone.
package main

import (
	"fmt"
	"strings"

	"github.com/xavidop/mamori/cmd/mamori/internal/sourcetag"
)

// Each command spells the flag out in its own usage text rather than sharing
// one block here: the surrounding help is indented differently per command,
// and doctor has to say the flag only affects --compare. What must not drift
// is the BEHAVIOUR, and that is shared, below.

// matchSecretSchemes recognizes --secret-schemes at args[i] in any of the four
// spellings the static commands accept (one or two leading dashes, and either
// "=value" or a following argument), and reports how many arguments it
// consumed. consumed is 0 when args[i] is some other argument entirely, which
// is how a caller's switch knows to fall through to its own cases.
//
// cmd names the subcommand for the error message ("schema", "policy", ...),
// so the caller does not have to reformat it.
func matchSecretSchemes(cmd string, args []string, i int) (value string, consumed int, err error) {
	a := args[i]
	switch {
	case a == "--secret-schemes" || a == "-secret-schemes":
		if i+1 >= len(args) {
			return "", 0, fmt.Errorf("mamori %s: %s requires a value", cmd, a)
		}
		return args[i+1], 2, nil
	case strings.HasPrefix(a, "--secret-schemes="):
		return strings.TrimPrefix(a, "--secret-schemes="), 1, nil
	case strings.HasPrefix(a, "-secret-schemes="):
		return strings.TrimPrefix(a, "-secret-schemes="), 1, nil
	}
	return "", 0, nil
}

// secretSchemeSet turns a --secret-schemes value into the SchemeSet to hand
// Extract. An empty list returns a nil set, which Extract reads as "use the
// built-in set", so the common case of the flag being absent keeps using the
// shared package-level set instead of building a fresh map per call.
//
// An unparseable entry is an error rather than a silent omission: a check
// that quietly covers less than the operator asked for is worse than one that
// fails loudly. See sourcetag.ParseSchemeList.
func secretSchemeSet(cmd, list string) (sourcetag.SchemeSet, error) {
	if list == "" {
		return nil, nil
	}
	parsed, err := sourcetag.ParseSchemeList(list)
	if err != nil {
		return nil, fmt.Errorf("mamori %s: --secret-schemes: %w", cmd, err)
	}
	set := sourcetag.DefaultSecretSchemes()
	set.Add(parsed...)
	return set, nil
}
