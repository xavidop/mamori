package secret_test

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"

	"github.com/xavidop/mamori/secret"
)

// String redacts itself everywhere a value is normally printed. Nothing short
// of an explicit Reveal produces the plaintext, so an accidental log line or
// error message cannot leak a credential.
func ExampleString() {
	pw := secret.NewString("hunter2")

	fmt.Println(pw)          // Stringer
	fmt.Printf("%v\n", pw)   // default verb
	fmt.Printf("%s\n", pw)   // string verb
	fmt.Printf("%q\n", pw)   // quoted
	fmt.Println(pw.Reveal()) // the one explicit, greppable escape hatch

	// Output:
	// [REDACTED]
	// [REDACTED]
	// [REDACTED]
	// "[REDACTED]"
	// hunter2
}

// Marshaling a struct that carries a String emits the redaction, not the
// secret, so a config dump or an API response is safe by construction.
func ExampleString_MarshalJSON() {
	type Config struct {
		User     string        `json:"user"`
		Password secret.String `json:"password"`
	}

	out, err := json.Marshal(Config{User: "svc-api", Password: secret.NewString("hunter2")})
	if err != nil {
		fmt.Println("marshal failed:", err)
		return
	}
	fmt.Println(string(out))

	// Output:
	// {"user":"svc-api","password":"[REDACTED]"}
}

// String implements slog.LogValuer, so structured logging redacts it too, even
// when it is passed straight to a log call.
func ExampleString_LogValue() {
	// A handler with time and level stripped, so the example output is stable.
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey || a.Key == slog.LevelKey {
				return slog.Attr{}
			}
			return a
		},
	}))

	logger.Info("connecting", "user", "svc-api", "password", secret.NewString("hunter2"))

	// Output:
	// msg=connecting user=svc-api password=[REDACTED]
}

// Reveal is deliberately the only way out. Keeping it explicit makes every
// place a secret becomes plaintext a one-line grep away in code review.
func ExampleString_Reveal() {
	token := secret.NewString("sk-live-abc123")

	// Pass the plaintext at the exact point it is needed, not before.
	authHeader := "Bearer " + token.Reveal()

	fmt.Println("stored: ", token)
	fmt.Println("on wire:", authHeader)

	// Output:
	// stored:  [REDACTED]
	// on wire: Bearer sk-live-abc123
}

// Bytes is the same guarantee for binary secrets such as a private key or a
// certificate, and Zero scrubs the backing memory when the value is done.
func ExampleBytes() {
	key := secret.NewBytes([]byte("-----BEGIN PRIVATE KEY-----"))

	fmt.Println(key)
	fmt.Println(len(key.Reveal()), "bytes revealed")

	// Output:
	// [REDACTED]
	// 27 bytes revealed
}
