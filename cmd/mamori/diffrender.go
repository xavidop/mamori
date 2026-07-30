// This file renders a Diff (diffmodel.go) in the three formats `mamori diff`
// supports. Rendering is kept separate from computation so every format can
// be tested against a Diff literal, with no files and no JSON round trip.
//
// All three formats share privilegeLines, so the privilege section cannot
// drift between the text a human reads in a terminal and the markdown a
// reviewer reads in a pull request.
package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
)

// Policy format names accepted by --policy-format. The empty string means the
// scheme-neutral default.
const (
	policyFormatAWSIAM         = "aws-iam"
	policyFormatGCP            = "gcp"
	policyFormatExternalSecret = "external-secret"
)

// renderDiffText writes the default human-readable report: one section per
// changed struct, then the privilege delta.
func renderDiffText(w io.Writer, d Diff, policyFormat string) {
	if d.Empty() {
		_, _ = fmt.Fprintln(w, "no configuration surface changes")
		return
	}

	for i, sd := range d.Structs {
		if i > 0 {
			_, _ = fmt.Fprintln(w)
		}
		switch sd.Kind {
		case ChangeAdded:
			_, _ = fmt.Fprintf(w, "+ %s.%s (new config struct)\n", sd.Package, sd.TypeName)
			continue
		case ChangeRemoved:
			_, _ = fmt.Fprintf(w, "- %s.%s (config struct removed)\n", sd.Package, sd.TypeName)
			continue
		}

		_, _ = fmt.Fprintf(w, "%s.%s\n", sd.Package, sd.TypeName)
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		for _, fd := range sd.Fields {
			switch fd.Kind {
			case ChangeAdded:
				_, _ = fmt.Fprintf(tw, "  + %s\t%s\t%s\n", fd.Path, fieldType(fd.Field), fieldChain(fd.Field))
			case ChangeRemoved:
				_, _ = fmt.Fprintf(tw, "  - %s\t%s\t%s\n", fd.Path, fieldType(fd.Field), fieldChain(fd.Field))
			default:
				_, _ = fmt.Fprintf(tw, "  ~ %s\t\t\n", fd.Path)
				for _, a := range fd.Attrs {
					_, _ = fmt.Fprintf(tw, "      %s\t%s -> %s\t\n", a.Name, blank(a.Base), blank(a.Head))
				}
				for _, r := range fd.Refs {
					_, _ = fmt.Fprintf(tw, "      %s\t%s\t\n", chainMarker(r), r.Ref)
				}
			}
			if fd.BecameSensitive {
				_, _ = fmt.Fprintf(tw, "      !\tnow reads secret material\t\n")
			}
		}
		_ = tw.Flush()
	}

	lines := privilegeLines(d.Privilege, policyFormat)
	if len(lines) == 0 {
		return
	}
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "privilege delta")
	for _, l := range lines {
		_, _ = fmt.Fprintf(w, "  %s\n", l)
	}
}

// chainMarker renders a RefChange's verdict as a short prefix.
func chainMarker(r RefChange) string {
	switch r.Kind {
	case ChangeAdded:
		return "chain +"
	case ChangeRemoved:
		return "chain -"
	default:
		return fmt.Sprintf("chain ~ (position %d -> %d)", r.BasePos, r.HeadPos)
	}
}

// blank renders an empty attribute value visibly, so "" to "fail" does not
// print as a line that appears to start with an arrow.
func blank(s string) string {
	if s == "" {
		return `""`
	}
	return s
}

func fieldType(f *Field) string {
	if f == nil {
		return ""
	}
	return f.GoType
}

func fieldChain(f *Field) string {
	if f == nil {
		return ""
	}
	return strings.Join(f.Refs, ", ")
}

// privilegeLines renders the privilege delta as one line per path. With an
// empty policyFormat it is scheme neutral, which works for every provider and
// presumes no cloud. With a policyFormat it additionally renders the concrete
// grant for the schemes that format knows how to address, reusing policy.go's
// own ARN and resource-name helpers so the identifiers match what
// `mamori policy` would emit for the same refs.
//
// A scheme the chosen format cannot address still gets its neutral line: a
// change to a vault:// or k8s-secret:// ref must never vanish from the report
// just because no IAM vocabulary exists for it.
func privilegeLines(d PrivilegeDelta, policyFormat string) []string {
	var out []string
	out = append(out, privilegeSide(d.Added, "+", policyFormat)...)
	out = append(out, privilegeSide(d.Removed, "-", policyFormat)...)
	return out
}

func privilegeSide(byScheme map[string][]string, marker, policyFormat string) []string {
	if len(byScheme) == 0 {
		return nil
	}
	schemes := make([]string, 0, len(byScheme))
	for s := range byScheme {
		schemes = append(schemes, s)
	}
	sort.Strings(schemes)

	var out []string
	for _, scheme := range schemes {
		for _, path := range byScheme[scheme] {
			line := fmt.Sprintf("%s %s  %s", marker, scheme, path)
			if grant := concreteGrant(scheme, path, policyFormat); grant != "" {
				line += "  " + grant
			}
			out = append(out, line)
		}
	}
	return out
}

// concreteGrant renders the artifact-specific grant for one path, or "" when
// the chosen format has no vocabulary for that scheme.
func concreteGrant(scheme, path, policyFormat string) string {
	switch policyFormat {
	case policyFormatAWSIAM:
		switch scheme {
		case awsSMScheme:
			return "secretsmanager:GetSecretValue on " + awsSecretARN(path)
		case awsPSScheme:
			return "ssm:GetParameter on " + awsParameterARN(path)
		}
	case policyFormatGCP:
		if scheme == gcpSMScheme {
			return gcpSecretAccessorRole + " on " + gcpResourceName(path)
		}
	case policyFormatExternalSecret:
		switch scheme {
		case awsSMScheme, awsPSScheme, gcpSMScheme:
			return "ExternalSecret remoteRef.key " + path
		}
	}
	return ""
}
