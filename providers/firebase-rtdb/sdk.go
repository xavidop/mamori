package firebasertdb

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	firebase "firebase.google.com/go/v4"
	"firebase.google.com/go/v4/db"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// Realtime Database OAuth scopes required to read data and stream changes over
// the REST API.
const (
	scopeDatabase = "https://www.googleapis.com/auth/firebase.database"
	scopeEmail    = "https://www.googleapis.com/auth/userinfo.email"
)

// dbRefClient is the minimal subset of *db.Client the SDK backend uses. It exists
// so the SDK backend depends on a small surface (and remains straightforward to
// reason about).
type dbRefClient interface {
	NewRef(path string) *db.Ref
}

// sdkBackend is the live backend: it reads values with the Firebase Admin SDK and
// streams changes over the Realtime Database REST endpoint (Server-Sent Events)
// using an ADC bearer token.
type sdkBackend struct {
	client      dbRefClient
	dbURL       string
	tokenSource oauth2.TokenSource
	httpClient  *http.Client
}

// compile-time check that sdkBackend satisfies the provider's backend contract.
var _ backend = (*sdkBackend)(nil)

// newSDKBackend builds the live backend from Application Default Credentials and
// the configured (or FIREBASE_DATABASE_URL) database URL. It is the default
// Provider.newBackend and is invoked lazily on first use.
func newSDKBackend(ctx context.Context, dbURL, projectID string) (backend, error) {
	if dbURL == "" {
		dbURL = os.Getenv("FIREBASE_DATABASE_URL")
	}
	if dbURL == "" {
		return nil, errors.New("no database URL: set WithDatabaseURL or FIREBASE_DATABASE_URL")
	}

	conf := &firebase.Config{DatabaseURL: dbURL}
	if projectID != "" {
		conf.ProjectID = projectID
	}
	app, err := firebase.NewApp(ctx, conf)
	if err != nil {
		return nil, fmt.Errorf("init app: %w", err)
	}
	client, err := app.Database(ctx)
	if err != nil {
		return nil, fmt.Errorf("init database client: %w", err)
	}
	creds, err := google.FindDefaultCredentials(ctx, scopeDatabase, scopeEmail)
	if err != nil {
		return nil, fmt.Errorf("default credentials: %w", err)
	}

	return &sdkBackend{
		client:      client,
		dbURL:       strings.TrimRight(dbURL, "/"),
		tokenSource: creds.TokenSource,
		httpClient:  &http.Client{},
	}, nil
}

// Get reads the value at path with the Admin SDK, requesting the entry ETag for
// cheap change detection. A null / missing value is reported as (nil, "", nil).
func (b *sdkBackend) Get(ctx context.Context, path string) ([]byte, string, error) {
	var raw json.RawMessage
	etag, err := b.client.NewRef(path).GetWithETag(ctx, &raw)
	if err != nil {
		return nil, "", err
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil, "", nil
	}
	return raw, etag, nil
}

// Stream opens a Server-Sent-Events connection to <db-url>/<path>.json bound to
// ctx. Cancelling ctx aborts the in-flight request and unblocks Recv.
func (b *sdkBackend) Stream(ctx context.Context, path string) (changeStream, error) {
	url := b.dbURL + "/" + strings.TrimLeft(path, "/") + ".json"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")

	tok, err := b.tokenSource.Token()
	if err != nil {
		return nil, fmt.Errorf("stream token: %w", err)
	}
	tok.SetAuthHeader(req)

	resp, err := b.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("stream status %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}
	return &sseStream{resp: resp, dec: newSSEDecoder(resp.Body)}, nil
}

// sseStream adapts an HTTP Server-Sent-Events response to changeStream.
type sseStream struct {
	resp *http.Response
	dec  *sseDecoder
}

func (s *sseStream) Recv() (string, []byte, error) { return s.dec.next() }

func (s *sseStream) Close() error { return s.resp.Body.Close() }

// sseDecoder parses a Server-Sent-Events byte stream into discrete events. It is
// deliberately independent of net/http so the parsing logic is unit-testable
// against any io.Reader.
type sseDecoder struct {
	r *bufio.Reader
}

func newSSEDecoder(r io.Reader) *sseDecoder {
	return &sseDecoder{r: bufio.NewReader(r)}
}

// next reads and returns the next complete event (its "event" name and
// concatenated "data" payload). A blank line dispatches the accumulated event.
// It returns io.EOF at the end of the stream.
func (d *sseDecoder) next() (event string, data []byte, err error) {
	var (
		name      string
		payload   []byte
		haveField bool
	)
	for {
		line, err := d.r.ReadBytes('\n')
		if len(line) == 0 && err != nil {
			// EOF (or read error) with no trailing data.
			return "", nil, err
		}
		line = bytes.TrimRight(line, "\r\n")

		if len(line) == 0 {
			// Blank line: dispatch if we have accumulated an event, else keep
			// reading (leading/duplicate blank lines are ignored).
			if haveField {
				return name, payload, nil
			}
			if err != nil {
				return "", nil, err
			}
			continue
		}
		if line[0] == ':' {
			// Comment / heartbeat line; ignore.
			if err != nil {
				return "", nil, err
			}
			continue
		}

		field, value, _ := bytes.Cut(line, []byte(":"))
		value = bytes.TrimPrefix(value, []byte(" "))
		switch string(field) {
		case "event":
			name = string(value)
			haveField = true
		case "data":
			if payload != nil {
				payload = append(payload, '\n')
			}
			payload = append(payload, value...)
			haveField = true
		default:
			// "id", "retry", and unknown fields are ignored.
		}

		if err != nil {
			// Stream ended mid-event; dispatch what we have if anything.
			if haveField {
				return name, payload, nil
			}
			return "", nil, err
		}
	}
}
