package firebasertdb

import (
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
	"github.com/xavidop/mamori/providers/httpcore"
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
	// sse bounds the decoder every stream is read through. Its zero value
	// selects httpcore's one-megabyte defaults; WithMaxFrameBytes replaces it.
	sse httpcore.SSEConfig
}

// compile-time check that sdkBackend satisfies the provider's backend contract.
var _ backend = (*sdkBackend)(nil)

// newSDKBackend builds the live backend from Application Default Credentials and
// the configured (or FIREBASE_DATABASE_URL) database URL. It is the default
// Provider.newBackend and is invoked lazily on first use.
func newSDKBackend(ctx context.Context, dbURL, projectID string, sse httpcore.SSEConfig) (backend, error) {
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
		sse:         sse,
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
	// b.sse bounds the read. Its zero value selects httpcore's one-megabyte
	// ceilings on a single line and on one frame's accumulated data, and
	// WithMaxFrameBytes moves both for a caller who watches a larger node.
	//
	// This provider's own decoder had NEITHER bound: it read a line with
	// bufio.Reader.ReadBytes, which grows until a newline arrives and nothing
	// obliges a Realtime Database endpoint (or anything impersonating one) to
	// ever send one, and it appended data: lines into one payload with no total.
	// Either shape was an unbounded allocation driven entirely by the far end of
	// the socket.
	return &sseStream{s: httpcore.NewSSEStream(ctx, resp, b.sse)}, nil
}

// sseStream adapts httpcore's bounded SSE stream to changeStream.
//
// The adapter exists rather than changeStream being redefined in httpcore's
// terms because changeStream is what the in-memory test fake implements too, and
// that fake has no HTTP response to hand out.
type sseStream struct{ s *httpcore.SSEStream }

func (s *sseStream) Recv() (string, []byte, error) {
	ev, err := s.s.Next()
	return ev.Name, ev.Data, err
}

func (s *sseStream) Close() error { return s.s.Close() }
