package server

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/xavidop/mamori"
	"gopkg.in/yaml.v3"
)

// Binding is one operator-declared name-to-ref mapping the server resolves
// and serves under that name. Bindings are the ONLY unit a client can
// request over the wire protocol (a later task): a client sends a name, never
// a ref, so it can never make the server resolve a ref of the client's own
// choosing. Without that boundary a client could ask for file:///etc/shadow
// or its own exec: command and the server, holding every backend credential,
// would happily comply.
type Binding struct {
	// Name is what a client requests.
	Name string
	// Ref is the parsed reference this name resolves to.
	Ref mamori.Ref
}

// rawBinding is an unparsed (name, ref) pair recorded by Bind or BindFile,
// before mamori.ParseRef and the exec:/mamori: scheme gates have run.
type rawBinding struct {
	name string
	ref  string
}

// bindingsFile is the YAML shape BindFile reads: a flat map of binding name
// to ref string, under a top-level `bindings:` key. Extra top-level keys are
// ignored (yaml.v3's default), which leaves room for later file-level
// metadata (e.g. a schema version) without breaking existing files.
type bindingsFile struct {
	Bindings map[string]string `yaml:"bindings"`
}

// loadBindFile reads and parses the YAML file at path, returning its
// bindings sorted by name so that a BindFile's contribution to the final
// table has a deterministic order, independent of Go's randomized map
// iteration.
func loadBindFile(path string) ([]rawBinding, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("mamori/server: bind file %q: %w", path, err)
	}
	var bf bindingsFile
	if err := yaml.Unmarshal(data, &bf); err != nil {
		return nil, fmt.Errorf("mamori/server: bind file %q: %w", path, err)
	}

	names := make([]string, 0, len(bf.Bindings))
	for name := range bf.Bindings {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]rawBinding, 0, len(names))
	for _, name := range names {
		out = append(out, rawBinding{name: name, ref: bf.Bindings[name]})
	}
	return out, nil
}

// resolveBindings merges every Bind declaration (direct, in the order given
// to New) with every BindFile's declarations (in the order the BindFile
// options were given, each file's own entries sorted by name), then parses
// and gates each one.
//
// This is called once from New, after every Option in the caller's list has
// already applied to the Server. That ordering matters: exec:/mamori: scheme
// gating depends on allowExec/allowChaining, which may be set by an Option
// appearing anywhere in the list, including AFTER the Bind call it is meant
// to permit. If Bind validated its ref eagerly, at the moment its own Option
// closure ran, then
//
//	New(Bind("x", "exec:date"), AllowExec())
//
// would fail (AllowExec hasn't run yet) while
//
//	New(AllowExec(), Bind("x", "exec:date"))
//
// would succeed, purely because of option order - a footgun with no security
// benefit. Deferring all validation to this single post-options pass makes
// Bind/BindFile order-independent with respect to the allow-opts.
//
// A repeated binding name is rejected outright rather than silently letting
// a later declaration shadow an earlier one: last-write-wins would let one
// Bind (or one BindFile entry) quietly change what an existing name resolves
// to, without the conflict ever being visible.
func resolveBindings(direct []rawBinding, files []string, allowExec, allowChaining bool) (map[string]Binding, error) {
	all := make([]rawBinding, 0, len(direct))
	all = append(all, direct...)
	for _, f := range files {
		fb, err := loadBindFile(f)
		if err != nil {
			return nil, err
		}
		all = append(all, fb...)
	}

	out := make(map[string]Binding, len(all))
	for _, rb := range all {
		if _, exists := out[rb.name]; exists {
			return nil, fmt.Errorf("mamori/server: duplicate binding name %q", rb.name)
		}

		ref, err := mamori.ParseRef(rb.ref)
		if err != nil {
			return nil, fmt.Errorf("mamori/server: binding %q: %w", rb.name, err)
		}

		// Canonicalize the scheme ONCE, here, right after parsing, so every
		// downstream consumer of this Binding - the exec:/mamori: gate below
		// AND resolve.go's s.providers[b.Ref.Scheme] lookup - agrees on the
		// same value. URI schemes are case-insensitive per RFC 3986, and
		// ParseRef preserves whatever case the operator wrote, so without this
		// a binding like "EXEC:echo hi" could pass the gate (which used to
		// lowercase only its own local comparison) yet fail provider lookup
		// (which compared ref.Scheme verbatim, case-sensitively): a binding
		// the gate accepted would then resolve to a confusing "no provider
		// registered" error at runtime instead of matching the "exec"
		// provider WithProvider registered. Storing the lowercased scheme
		// back onto ref (not just using a lowercased value locally in the
		// switch below) makes that impossible: the Binding's Ref, and
		// therefore whatever mamori.WatchRef/the provider ultimately sees, IS
		// the canonical scheme. ref.Raw is left untouched (ParseRef sets it
		// to the original tag and Ref.String prefers it), so diagnostics and
		// error messages still show exactly what the operator wrote.
		ref.Scheme = strings.ToLower(ref.Scheme)

		// exec: runs an arbitrary command on the server's host; mamori: chains
		// to another config server and can form a cycle. Both are opt-in only
		// (see AllowExec/AllowChaining in server.go), rejected here by default
		// so that binding one in by accident fails loudly at construction time
		// rather than becoming reachable to every authorized consumer.
		//
		// ref.Scheme is already canonicalized (lowercased) above, so this
		// switch can compare it directly: "EXEC:" or "Mamori:" cannot slip
		// past unmatched. An unmatched case here means no gate fires at all,
		// which would let the ref through New with neither an error nor any
		// record that it needed AllowExec/AllowChaining, only to fail later -
		// obscurely, at resolution time, with no obvious link back to the
		// missing allow-opt. Failing loudly here, at construction, is the
		// whole point of these gates existing.
		switch ref.Scheme {
		case "exec":
			if !allowExec {
				return nil, fmt.Errorf("mamori/server: binding %q: exec: scheme is remote command execution and is rejected unless AllowExec() is set", rb.name)
			}
		case "mamori":
			if !allowChaining {
				return nil, fmt.Errorf("mamori/server: binding %q: mamori: scheme chains to another config server and is rejected unless AllowChaining() is set", rb.name)
			}
		}

		out[rb.name] = Binding{Name: rb.name, Ref: ref}
	}
	return out, nil
}
