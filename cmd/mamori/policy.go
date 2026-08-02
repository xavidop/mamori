// This file implements `mamori policy`: it reuses Extract (extract.go) to
// read a config struct's source: refs and emits a least-privilege access
// artifact for one of three targets (an AWS IAM policy document, a GCP
// Secret Manager access document, or an ExternalSecrets.io ExternalSecret
// manifest), derived entirely from the refs found -- never resolving
// anything (decision D1, see extract.go).
//
// Every IAM action, GCP role, and ExternalSecrets CRD field name this file
// emits was verified against the relevant service's own documentation
// before being hardcoded here; none of it is guessed.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/xavidop/mamori/cmd/mamori/internal/sourcetag"
)

var policyUsage = `usage: mamori policy [patterns...] [--type=Name] --format=<f> [--secret-schemes=list]

Policy reads Go source (via golang.org/x/tools/go/packages) and emits a
least-privilege access artifact derived from each source: tagged config
struct's refs. It never resolves anything (no network calls, no secret
managers contacted), and it never fabricates account IDs, project IDs, or
cluster resource names that no source: ref carries -- those show up as
clearly-marked placeholders (see below) for the operator to fill in.

  patterns   Go package patterns to load (default: the current directory,
             same as omitting a pattern to "go build"). Example: ./...
  --type     only consider refs from the struct type with this name
  --secret-schemes  comma-separated extra schemes to treat as secret-bearing,
             added to the built-in set. Use this for a custom provider, e.g.
             --secret-schemes=mysecrets,corp-kv
  --format   required; one of:
               aws-iam           an IAM policy document granting
                                  secretsmanager:GetSecretValue on every
                                  aws-sm:// ref and ssm:GetParameter /
                                  ssm:GetParameters on every aws-ps:// ref.
                                  The account ID and region are not part of
                                  a source: ref, so every ARN uses a "*"
                                  placeholder for both.
               gcp                a document listing roles/secretmanager.
                                  secretAccessor and the projects/<p>/
                                  secrets/<name> resource name for every
                                  gcp-sm:// ref. gcp-sm:// refs carry their
                                  project as the ref's own first path
                                  segment (gcp-sm://<project>/<secret>), so
                                  the real project is used; "PROJECT" is
                                  only a placeholder for a malformed ref
                                  missing that segment.
               external-secret    an external-secrets.io/v1
                                  ExternalSecret manifest with one
                                  spec.data entry per aws-sm://, aws-ps://,
                                  or gcp-sm:// ref. spec.secretStoreRef.name
                                  is always a placeholder: no source: ref
                                  names a Kubernetes SecretStore.

A format whose relevant refs are empty (no matching scheme found) still
emits a valid, empty artifact, plus a stderr note that nothing was found --
never a silent, misleadingly-complete-looking success.
`

// Supported --format values.
const (
	formatAWSIAM         = "aws-iam"
	formatGCP            = "gcp"
	formatExternalSecret = "external-secret"
)

// supportedPolicyFormats is both the set consulted by isSupportedPolicyFormat
// and the list printed on an unknown/missing --format, in a fixed,
// documented order.
var supportedPolicyFormats = []string{formatAWSIAM, formatGCP, formatExternalSecret}

// policyCmd is the mamori policy subcommand. Like explainCmd/schemaCmd, it
// writes to injected stdout/stderr writers (so tests never touch the real
// os.Stdout/os.Stderr) and returns the process exit code: 2 for a missing
// or unrecognized --format (a usage error, same code main.go's own unknown-
// subcommand path uses), 1 for a package-load or encoding error, 0 on
// success (including the "found nothing relevant" case, which is still a
// successful run that produced a valid, if empty, artifact -- see
// policyUsage).
func policyCmd(args []string, stdout, stderr io.Writer) int {
	if wantsHelp(args) {
		return writeHelp(stdout, policyUsage)
	}
	patterns, typeName, format, schemes, err := parsePolicyArgs(args)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		_, _ = fmt.Fprint(stderr, policyUsage)
		return 1
	}

	if !isSupportedPolicyFormat(format) {
		_, _ = fmt.Fprintf(stderr, "mamori policy: unsupported --format %q; supported formats: %s\n",
			format, strings.Join(supportedPolicyFormats, ", "))
		return 2
	}

	structs, err := Extract(patterns, typeName, schemes)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "mamori policy: %v\n", err)
		return 1
	}

	refs := collectPolicyRefs(structs)

	switch format {
	case formatAWSIAM:
		return writeAWSIAMPolicy(stdout, stderr, refs)
	case formatGCP:
		return writeGCPPolicy(stdout, stderr, refs)
	case formatExternalSecret:
		return writeExternalSecretManifest(stdout, stderr, refs)
	default:
		// Unreachable: isSupportedPolicyFormat already validated format
		// against exactly this switch's cases.
		panic("mamori policy: unreachable format " + format)
	}
}

// isSupportedPolicyFormat reports whether format is one of
// supportedPolicyFormats. An empty format (the flag was never given at all)
// is unsupported too, so a bare "mamori policy" gets the same helpful
// "here are the formats" message as "mamori policy --format=bogus", rather
// than a bare "missing flag" error with no further guidance.
func isSupportedPolicyFormat(format string) bool {
	for _, f := range supportedPolicyFormats {
		if f == format {
			return true
		}
	}
	return false
}

// parsePolicyArgs splits args into package patterns and the --type/--format
// flags. It scans by recognized flag shape rather than using flag.FlagSet,
// so patterns and flags may appear in either order, matching
// parseExplainArgs/parseSchemaArgs (explain.go, schema.go).
// The returned schemes is nil unless --secret-schemes was given (see
// secretschemes.go), so the common case keeps using the built-in set.
func parsePolicyArgs(args []string) (patterns []string, typeName, format string, schemes sourcetag.SchemeSet, err error) {
	var extra string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if value, consumed, matchErr := matchSecretSchemes("policy", args, i); matchErr != nil {
			return nil, "", "", nil, matchErr
		} else if consumed > 0 {
			extra = value
			i += consumed - 1
			continue
		}
		switch {
		case a == "--type" || a == "-type":
			i++
			if i >= len(args) {
				return nil, "", "", nil, fmt.Errorf("mamori policy: %s requires a value", a)
			}
			typeName = args[i]
		case strings.HasPrefix(a, "--type="):
			typeName = strings.TrimPrefix(a, "--type=")
		case strings.HasPrefix(a, "-type="):
			typeName = strings.TrimPrefix(a, "-type=")
		case a == "--format" || a == "-format":
			i++
			if i >= len(args) {
				return nil, "", "", nil, fmt.Errorf("mamori policy: %s requires a value", a)
			}
			format = args[i]
		case strings.HasPrefix(a, "--format="):
			format = strings.TrimPrefix(a, "--format=")
		case strings.HasPrefix(a, "-format="):
			format = strings.TrimPrefix(a, "-format=")
		case strings.HasPrefix(a, "-"):
			return nil, "", "", nil, fmt.Errorf("mamori policy: unknown flag %q", a)
		default:
			patterns = append(patterns, a)
		}
	}
	schemes, err = secretSchemeSet("policy", extra)
	if err != nil {
		return nil, "", "", nil, err
	}
	return patterns, typeName, format, schemes, nil
}

// policyRefs buckets every ref collected from a set of StructInfo by
// scheme, deduplicated and sorted for deterministic output. Only three
// buckets are ever read downstream (aws-sm, aws-ps, gcp-sm -- one per
// scheme this command knows how to turn into a resource identifier); every
// other scheme (env, file, exec, vault, azure-kv, ...) is collected here
// too but simply never consulted by any of the three format writers below.
// Extending policy.go to a fourth scheme is a matter of adding another
// bucket read, not touching collectPolicyRefs itself.
type policyRefs map[string][]string

// collectPolicyRefs walks every field of every matched struct and buckets
// each of its refs (Field.Refs, the already-chain-split list -- a field's
// entire fallback chain contributes, not just its first entry) by scheme,
// via sourcetag.SchemeOf. A ref with no parseable scheme is skipped (it
// cannot be routed to any artifact).
func collectPolicyRefs(structs []StructInfo) policyRefs {
	byScheme := policyRefs{}
	for _, s := range structs {
		for _, f := range s.Fields {
			for _, ref := range f.Refs {
				scheme, ok := sourcetag.SchemeOf(ref)
				if !ok {
					continue
				}
				byScheme[scheme] = append(byScheme[scheme], refPath(ref))
			}
		}
	}
	for scheme, paths := range byScheme {
		byScheme[scheme] = sortedUniqueStrings(paths)
	}
	return byScheme
}

// refPath extracts the path component of a hierarchical source ref
// (scheme://path[#key][?opts]) -- the same slice mamori's ref.go ParseRef
// would assign to Ref.Path. This is duplicated here by hand rather than
// calling mamori's Ref/ParseRef, for the same reason internal/sourcetag's
// own SplitChain/SchemeOf duplicate ref.go's chain-split/scheme rules
// instead of calling core: extracting one path substring needs none of
// core's decode/validate machinery, so there is nothing to gain by routing
// through it here. Every scheme this file routes on (aws-sm, aws-ps, gcp-sm)
// is hierarchical (scheme://...); refPath is never called on an opaque ref
// (env:, exec:), since collectPolicyRefs buckets by scheme first and only
// aws-iam/gcp/external-secret's writers (which only ever read the
// aws-sm/aws-ps/gcp-sm buckets) ever call it.
func refPath(ref string) string {
	scheme, ok := sourcetag.SchemeOf(ref)
	if !ok {
		return ""
	}
	rest := strings.TrimPrefix(strings.TrimSpace(ref), scheme+":")
	rest = strings.TrimPrefix(rest, "//")
	if i := strings.IndexAny(rest, "#?"); i >= 0 {
		rest = rest[:i]
	}
	return rest
}

// sortedUniqueStrings sorts in and removes adjacent duplicates in place,
// returning the deduplicated prefix. Used to make every policyRefs bucket
// (and so every emitted artifact's resource list) deterministic regardless
// of which field or how many chained refs contributed the same path twice.
func sortedUniqueStrings(in []string) []string {
	sort.Strings(in)
	out := in[:0]
	var prev string
	for i, v := range in {
		if i == 0 || v != prev {
			out = append(out, v)
		}
		prev = v
	}
	return out
}

// --- aws-iam ---
//
// Action names verified against the AWS Service Authorization Reference:
// secretsmanager:GetSecretValue is
// the action that reads a secret's value; ssm:GetParameter and
// ssm:GetParameters are the actions that read one or more SSM Parameter
// Store parameters, respectively. None of the three is invented.
const (
	awsSecretsManagerGetSecretValue = "secretsmanager:GetSecretValue"
	awsSSMGetParameter              = "ssm:GetParameter"
	awsSSMGetParameters             = "ssm:GetParameters"
)

// iamPolicyDocument is an AWS IAM JSON policy document
// ({"Version":"2012-10-17","Statement":[...]}, the exact top-level shape
// AWS's policy grammar requires).
type iamPolicyDocument struct {
	Version   string         `json:"Version"`
	Statement []iamStatement `json:"Statement"`
}

// iamStatement is one IAM policy statement. Action and Resource are always
// emitted as arrays (never a bare string), even with a single element:
// both spellings are valid IAM syntax, and using the array form
// unconditionally keeps the shape uniform regardless of how many refs a
// given scheme contributed.
type iamStatement struct {
	Sid      string   `json:"Sid,omitempty"`
	Effect   string   `json:"Effect"`
	Action   []string `json:"Action"`
	Resource []string `json:"Resource"`
}

// awsSecretARN builds a Secrets Manager secret ARN from a ref path (e.g.
// "prod/api"). "*:*" stands in for the account ID and region: neither is
// part of an aws-sm:// ref (see providers/aws/sm.go's Resolve -- both come
// from ambient AWS config at resolve time, never from the ref), so there is
// nothing truthful to put there instead. The "*" is deliberately visible
// and greppable, not silently omitted or invented.
func awsSecretARN(path string) string {
	return "arn:aws:secretsmanager:*:*:secret:" + path
}

// awsParameterARN builds an SSM parameter ARN from a ref path (e.g.
// "prod/app/config"), for the same "*:*" account/region reason as
// awsSecretARN. A leading "/" is trimmed first: AWS's own parameter ARN
// format is "arn:...:parameter" + the parameter name (which itself may
// start with "/" for a hierarchical parameter), so concatenating a
// name that already starts with "/" after the literal "parameter/" would
// double the slash.
func awsParameterARN(path string) string {
	return "arn:aws:ssm:*:*:parameter/" + strings.TrimPrefix(path, "/")
}

// writeAWSIAMPolicy emits the aws-iam artifact: one statement (Sid
// "SecretsManagerGetSecretValue") for every aws-sm:// ref found, one
// statement (Sid "SSMGetParameter") for every aws-ps:// ref found. A
// scheme that contributed no refs contributes no statement at all (an
// empty Resource list is not emitted for it); if neither scheme
// contributed anything, Statement is an empty (but valid) array and a
// stderr note says so.
func writeAWSIAMPolicy(stdout, stderr io.Writer, refs policyRefs) int {
	doc := iamPolicyDocument{Version: "2012-10-17", Statement: []iamStatement{}}

	if secrets := refs[awsSMScheme]; len(secrets) > 0 {
		resources := make([]string, len(secrets))
		for i, p := range secrets {
			resources[i] = awsSecretARN(p)
		}
		doc.Statement = append(doc.Statement, iamStatement{
			Sid:      "SecretsManagerGetSecretValue",
			Effect:   "Allow",
			Action:   []string{awsSecretsManagerGetSecretValue},
			Resource: resources,
		})
	}

	if params := refs[awsPSScheme]; len(params) > 0 {
		resources := make([]string, len(params))
		for i, p := range params {
			resources[i] = awsParameterARN(p)
		}
		doc.Statement = append(doc.Statement, iamStatement{
			Sid:      "SSMGetParameter",
			Effect:   "Allow",
			Action:   []string{awsSSMGetParameter, awsSSMGetParameters},
			Resource: resources,
		})
	}

	if len(doc.Statement) == 0 {
		_, _ = fmt.Fprintf(stderr, "mamori policy: no %s:// or %s:// refs found for --format=%s; emitted an empty policy document\n",
			awsSMScheme, awsPSScheme, formatAWSIAM)
	}

	return encodePolicyJSON(stdout, stderr, doc)
}

// --- gcp ---
//
// gcpSecretAccessorRole was verified against Google Cloud's Secret Manager
// access control documentation: roles/secretmanager.
// secretAccessor is the predefined role that grants
// secretmanager.versions.access, the permission that reads a secret
// version's payload. It is not invented.
const gcpSecretAccessorRole = "roles/secretmanager.secretAccessor"

// gcpProjectPlaceholder is used only when a gcp-sm:// ref's path does not
// split into <project>/<secret> (a malformed ref): the normal case uses
// the real project recovered from the ref itself (see gcpResourceName).
const gcpProjectPlaceholder = "PROJECT"

// gcpPolicyDocument lists the one role every gcp-sm:// ref needs and the
// resource names it should be bound to. It is not a literal GCP IAM
// policy/binding: a real GCP resource-level IAM binding also needs a
// principal (member) to bind the role to, which no source: ref ever
// carries, so emitting something that merely looks like a ready-to-apply
// GCP policy would misrepresent it. This is a smaller, honest summary an
// operator turns into one `gcloud secrets add-iam-policy-binding` (or one
// Terraform google_secret_manager_secret_iam_member) per resource.
type gcpPolicyDocument struct {
	Role      string   `json:"role"`
	Resources []string `json:"resources"`
}

// gcpResourceName builds a "projects/<project>/secrets/<secret>" resource
// name from a gcp-sm:// ref's path. providers/gcp/gcp.go's own Resolve
// parses the path the same way (project, secret, ok :=
// strings.Cut(ref.Path, "/")): the ref's grammar is
// gcp-sm://<project>/<secret>, so the project is genuinely present in the
// ref, not a placeholder, in the normal (well-formed) case.
func gcpResourceName(path string) string {
	project, secret, ok := strings.Cut(path, "/")
	if !ok || project == "" || secret == "" {
		return "projects/" + gcpProjectPlaceholder + "/secrets/" + path
	}
	return "projects/" + project + "/secrets/" + secret
}

// writeGCPPolicy emits the gcp artifact: gcpSecretAccessorRole plus the
// resource name of every gcp-sm:// ref found. An empty Resources list (no
// gcp-sm:// refs at all) is still a valid document, plus a stderr note.
func writeGCPPolicy(stdout, stderr io.Writer, refs policyRefs) int {
	paths := refs[gcpSMScheme]
	resources := make([]string, len(paths))
	for i, p := range paths {
		resources[i] = gcpResourceName(p)
	}
	sort.Strings(resources)

	if len(resources) == 0 {
		_, _ = fmt.Fprintf(stderr, "mamori policy: no %s:// refs found for --format=%s; emitted an empty policy document\n",
			gcpSMScheme, formatGCP)
	}

	doc := gcpPolicyDocument{Role: gcpSecretAccessorRole, Resources: resources}
	return encodePolicyJSON(stdout, stderr, doc)
}

// --- external-secret ---
//
// esAPIVersion/esKind were verified against the ExternalSecrets CRD:
// external-secrets.io/v1 / ExternalSecret is the current stable apiVersion+kind
// for the ExternalSecret custom resource (v1 became stable in ExternalSecrets
// Operator v0.17.0, superseding the now-deprecated v1beta1).
// spec.secretStoreRef.{name,kind}, spec.target.name, spec.data[].secretKey, and
// spec.data[].remoteRef.key are likewise real, verified field names on that CRD;
// no field name here is guessed.
const (
	esAPIVersion = "external-secrets.io/v1"
	esKind       = "ExternalSecret"

	// esManifestName is used for both metadata.name and spec.target.name:
	// a suggested, not-fake, default resource name (unlike
	// esSecretStorePlaceholder below, this is a reasonable name an
	// operator can keep rather than something that must be replaced).
	esManifestName = "mamori-managed-secrets"

	// esSecretStorePlaceholder is spec.secretStoreRef.name: no source: ref
	// ever names a Kubernetes SecretStore/ClusterSecretStore, so this is
	// always a placeholder, deliberately spelled to be unmistakable and
	// greppable rather than a name that could pass for a real one.
	esSecretStorePlaceholder = "REPLACE_ME_SECRET_STORE"
	esSecretStoreKind        = "SecretStore"
)

// esDataEntry is one spec.data[] entry: a Kubernetes Secret key
// (secretKey) mapped to the key used to look the value up in the external
// provider (remoteRef.key).
type esDataEntry struct {
	secretKey string
	remoteKey string
}

// writeExternalSecretManifest emits the external-secret artifact: one
// spec.data entry per aws-sm://, aws-ps://, or gcp-sm:// ref found (in that
// fixed order, matching writeAWSIAMPolicy's statement order), or an empty
// (but valid) spec.data list plus a stderr note if none of the three
// contributed anything.
func writeExternalSecretManifest(stdout, stderr io.Writer, refs policyRefs) int {
	var entries []esDataEntry
	seen := map[string]int{}

	add := func(scheme, remoteKey string) {
		base := sanitizeSecretKey(scheme + "-" + basename(remoteKey))
		key := base
		if n := seen[base]; n > 0 {
			key = fmt.Sprintf("%s-%d", base, n+1)
		}
		seen[base]++
		entries = append(entries, esDataEntry{secretKey: key, remoteKey: remoteKey})
	}

	for _, p := range refs[awsSMScheme] {
		add(awsSMScheme, p)
	}
	for _, p := range refs[awsPSScheme] {
		add(awsPSScheme, p)
	}
	for _, p := range refs[gcpSMScheme] {
		// The GCP provider's SecretStore config scopes to a project
		// separately (spec.provider.gcpsm.projectID); remoteRef.key is
		// just the secret ID, not "<project>/<secret>" (see
		// gcpResourceName's doc comment for the same project/secret
		// split, used here for the ID half instead of the resource name).
		_, secret, ok := strings.Cut(p, "/")
		if !ok {
			secret = p
		}
		add(gcpSMScheme, secret)
	}

	if len(entries) == 0 {
		_, _ = fmt.Fprintf(stderr, "mamori policy: no relevant refs found for --format=%s; emitted an empty manifest\n", formatExternalSecret)
	}

	_, _ = fmt.Fprint(stdout, renderExternalSecretYAML(entries))
	return 0
}

// basename returns the last "/"-separated segment of path, or path itself
// if it has no "/". Used to derive a short, readable secretKey from a
// longer ref path (e.g. "prod/app/config" -> "config").
func basename(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[i+1:]
	}
	return path
}

// sanitizeSecretKey restricts s to the character set Kubernetes allows in a
// Secret's data key ([-._a-zA-Z0-9]+), replacing every other rune with '-'.
// The inputs this is ever called with (a fixed scheme name plus a
// basename'd ref path) are already close to safe, but ref paths are
// provider-specific text this command does not control, so the sanitizer
// is unconditional rather than assumed unnecessary.
func sanitizeSecretKey(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// renderExternalSecretYAML hand-writes the ExternalSecret manifest as YAML.
// cmd/mamori's allowed dependency set (see this file's own top comment) does
// not include a YAML library, so this is a small,
// deliberately narrow writer: fixed 2-space indentation, a fixed key order
// matching the struct this mirrors, and yamlScalar (below) for the only two
// values that come from ref-derived text (secretKey, remoteRef.key) rather
// than a fixed constant.
func renderExternalSecretYAML(entries []esDataEntry) string {
	var b strings.Builder
	fmt.Fprintf(&b, "apiVersion: %s\n", esAPIVersion)
	fmt.Fprintf(&b, "kind: %s\n", esKind)
	fmt.Fprintf(&b, "metadata:\n")
	fmt.Fprintf(&b, "  name: %s\n", esManifestName)
	fmt.Fprintf(&b, "spec:\n")
	fmt.Fprintf(&b, "  secretStoreRef:\n")
	fmt.Fprintf(&b, "    name: %s\n", esSecretStorePlaceholder)
	fmt.Fprintf(&b, "    kind: %s\n", esSecretStoreKind)
	fmt.Fprintf(&b, "  target:\n")
	fmt.Fprintf(&b, "    name: %s\n", esManifestName)

	if len(entries) == 0 {
		fmt.Fprintf(&b, "  data: []\n")
		return b.String()
	}

	fmt.Fprintf(&b, "  data:\n")
	for _, e := range entries {
		fmt.Fprintf(&b, "    - secretKey: %s\n", yamlScalar(e.secretKey))
		fmt.Fprintf(&b, "      remoteRef:\n")
		fmt.Fprintf(&b, "        key: %s\n", yamlScalar(e.remoteKey))
	}
	return b.String()
}

// yamlScalar renders s as a YAML scalar: bare (unquoted) when s is safe to
// write that way, double-quoted and escaped otherwise. This hand-written
// subset is deliberately narrow rather than a general YAML emitter -- the
// only strings ever passed through it are k8s Secret keys (already
// restricted to [-._a-zA-Z0-9] by sanitizeSecretKey) and ref paths
// (arbitrary provider-specific text, hence still routed through this
// quoting decision rather than assumed safe).
func yamlScalar(s string) string {
	if s != "" && yamlSafeBare(s) {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		if r == '"' || r == '\\' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}

// yamlSafeBare reports whether s can be written as an unquoted YAML plain
// scalar: no leading/trailing whitespace, does not start with a character
// YAML reserves as a block/flow indicator, contains neither ':' nor '#'
// (either can turn a plain scalar into a mapping key or a comment
// depending on surrounding whitespace; this quotes defensively any time
// either appears at all rather than replicating YAML's full,
// context-sensitive indicator rules), and is not spelled like one of
// YAML 1.1's implicit null/bool literals (which a plain "yes"/"off"/etc.
// value would otherwise be silently reinterpreted as).
func yamlSafeBare(s string) bool {
	if strings.TrimSpace(s) != s {
		return false
	}
	switch s[0] {
	case '-', '?', ':', ',', '[', ']', '{', '}', '#', '&', '*', '!', '|', '>', '\'', '"', '%', '@', '`':
		return false
	}
	if strings.ContainsAny(s, ":#") {
		return false
	}
	switch strings.ToLower(s) {
	case "null", "~", "true", "false", "yes", "no", "on", "off":
		return false
	}
	return true
}

// Scheme name constants shared by every writer above, matching the literal
// scheme tokens providers/aws (sm.go's schemeSM, ps.go's schemePS) and
// providers/gcp (gcp.go's scheme) register.
const (
	awsSMScheme = "aws-sm"
	awsPSScheme = "aws-ps"
	gcpSMScheme = "gcp-sm"
)

// encodePolicyJSON marshals v (an iamPolicyDocument or gcpPolicyDocument)
// as indented JSON to stdout, matching writeSchema's (schema.go) approach.
// It returns 1 (and writes to stderr) only on an encoding failure, which
// should not happen in practice since both document types are plain
// marshalable data.
func encodePolicyJSON(stdout, stderr io.Writer, v any) int {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "mamori policy: encoding JSON: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "%s\n", b)
	return 0
}
