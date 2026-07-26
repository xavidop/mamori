package main

import (
	"bufio"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// liveFlags is the shared endpoint and credential flag set for the live
// commands (doctor, status), which are thin clients of a running process's
// admin endpoint. Every credential has a value form (convenient, but the
// value ends up in os.Args and process listings) and a file/stdin form (the
// recommended form: the secret never appears on the command line).
type liveFlags struct {
	endpoint string
	insecure bool

	bearer     string
	bearerFile string

	basic     string
	basicFile string

	clientCert string
	clientKey  string

	// stdin is read when a *File flag is set to "-". It defaults to
	// os.Stdin when nil; tests inject a strings.Reader instead so they
	// never touch the real standard input.
	stdin io.Reader
}

// register adds the live command flags to fs. The value-bearing forms
// (--bearer, --basic) exist for convenience; the file/stdin forms
// (--bearer-file, --basic-file, both accepting "-" for stdin) are what the
// documentation recommends, since they keep credentials out of os.Args.
func (f *liveFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.endpoint, "endpoint", "", "admin endpoint URL (https://host:port, http://host:port, or unix:///path/to.sock)")
	fs.BoolVar(&f.insecure, "insecure", false, "allow http:// (or skip TLS verification) instead of requiring TLS")

	fs.StringVar(&f.bearer, "bearer", "", "bearer token value (prefer --bearer-file; this form appears in os.Args)")
	fs.StringVar(&f.bearerFile, "bearer-file", "", "path to a file containing the bearer token, or - for stdin")

	fs.StringVar(&f.basic, "basic", "", "user:pass value (prefer --basic-file; this form appears in os.Args)")
	fs.StringVar(&f.basicFile, "basic-file", "", "path to a user:pass file, or - for stdin")

	fs.StringVar(&f.clientCert, "client-cert", "", "path to a client certificate (PEM) for mTLS")
	fs.StringVar(&f.clientKey, "client-key", "", "path to the client certificate's private key (PEM) for mTLS")
}

// resolveCredential turns the configured flags into an Authorization header
// value and/or TLS client certificates. It is valid for no credential to be
// configured at all (empty header, nil certs, nil error): a Unix domain
// socket authenticated by PeerCred, or an endpoint with NoAuth, needs none.
func (f *liveFlags) resolveCredential() (header string, tlsCerts []tls.Certificate, err error) {
	bearerSet := f.bearer != "" || f.bearerFile != ""
	basicSet := f.basic != "" || f.basicFile != ""
	if bearerSet && basicSet {
		return "", nil, errors.New("mamori: --bearer/--bearer-file and --basic/--basic-file are mutually exclusive")
	}

	switch {
	case bearerSet:
		token, err := f.resolveValue(f.bearer, f.bearerFile)
		if err != nil {
			return "", nil, fmt.Errorf("mamori: reading bearer token: %w", err)
		}
		header = "Bearer " + token
	case basicSet:
		userPass, err := f.resolveValue(f.basic, f.basicFile)
		if err != nil {
			return "", nil, fmt.Errorf("mamori: reading basic credentials: %w", err)
		}
		header = "Basic " + base64.StdEncoding.EncodeToString([]byte(userPass))
	}

	certSet := f.clientCert != ""
	keySet := f.clientKey != ""
	switch {
	case certSet && keySet:
		cert, err := tls.LoadX509KeyPair(f.clientCert, f.clientKey)
		if err != nil {
			return "", nil, fmt.Errorf("mamori: loading client certificate: %w", err)
		}
		tlsCerts = []tls.Certificate{cert}
	case certSet != keySet:
		return "", nil, errors.New("mamori: --client-cert and --client-key must both be set")
	}

	return header, tlsCerts, nil
}

// resolveValue returns the value form if set, otherwise reads the file form
// (the special path "-" reads from f.stdin, defaulting to os.Stdin), and
// trims trailing whitespace and newlines so a file saved with a trailing
// newline (the common case) does not end up embedded in the credential.
func (f *liveFlags) resolveValue(value, file string) (string, error) {
	if value != "" {
		return value, nil
	}
	var r io.Reader
	if file == "-" {
		r = f.stdin
		if r == nil {
			r = os.Stdin
		}
	} else {
		fh, err := os.Open(file)
		if err != nil {
			return "", err
		}
		defer fh.Close()
		r = fh
	}
	br := bufio.NewReader(r)
	data, err := io.ReadAll(br)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(data), " \t\r\n"), nil
}
