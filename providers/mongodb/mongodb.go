// Package mongodb implements a mamori provider that resolves configuration and
// secret values from documents stored in MongoDB.
//
// The scheme is "mongodb" and the ref grammar is:
//
//	mongodb://<collection>/<docid>[#field][?key=<field>]
//
// Per the mamori grammar the optional #field fragment precedes the optional ?opts
// query.
//
//   - <collection> - the MongoDB collection to look in.
//
//   - <docid>      - identifies the document. By default the document whose _id
//     equals <docid> is selected. When _id is a valid 24-character hex ObjectID
//     the value is matched as an ObjectID, otherwise it is matched as-is.
//
//   - #field       - optional. Selects a single field from the matched document.
//     Scalars are stringified; objects and arrays are returned as their JSON
//     encoding, identically to every other mamori provider (mamori.SelectKey).
//
//   - ?key=<field> - optional. Selects the document by an arbitrary field instead
//     of _id, i.e. the document where <field> == <docid>.
//
//     DBPassword string `source:"mongodb://secrets/app-db#password"`
//     APIKey     string `source:"mongodb://secrets/prod#apiKey?key=name"`
//
// The whole document (relaxed MongoDB Extended JSON, re-encoded to deterministic
// plain JSON) becomes Value.Bytes when no #field is given. Value.Version is the
// document's "version" field when present, otherwise mamori.VersionHash over the
// document JSON, giving cheap change detection. Resolved values are not marked
// Sensitive; wrap a field in secret.String for redaction.
//
// The provider implements mamori.WatchableProvider using MongoDB change streams:
// it opens a change stream on the collection scoped to the target document and
// re-reads and emits an Update on every update/replace. Change streams require
// the server to be a replica set (or sharded cluster); against a standalone
// mongod Watch returns an error and mamori falls back to polling.
package mongodb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/xavidop/mamori"
)

// scheme is the URL scheme this provider handles.
const scheme = "mongodb"

// watchCloseTimeout bounds the best-effort change-stream close after the watch
// context is cancelled, so the watch goroutine can never block on shutdown.
const watchCloseTimeout = 5 * time.Second

// backend is the minimal subset of MongoDB behaviour the provider depends on.
// The production implementation (mongoBackend) wraps a *mongo.Client; tests
// inject an in-memory fake implementing the same shape, so the conformance kit
// and unit tests run without a live MongoDB.
type backend interface {
	// FindDoc returns the single document in collection selected by keyField ==
	// keyValue (keyField "" selects by _id) as a bson.M. A missing document
	// returns an error satisfying errors.Is(err, mamori.ErrNotFound).
	FindDoc(ctx context.Context, collection, keyField, keyValue string) (bson.M, error)
	// WatchDoc opens a change stream on collection scoped to the target document
	// and returns a cursor that reports one change per relevant event. The caller
	// re-reads via FindDoc on each event, so the cursor only needs to signal that
	// something changed.
	WatchDoc(ctx context.Context, collection, keyField, keyValue string) (changeCursor, error)
}

// changeCursor is the minimal subset of *mongo.ChangeStream the watch loop uses.
// The real *mongo.ChangeStream satisfies it directly; the fake reproduces its
// blocking Next / Close semantics.
type changeCursor interface {
	// Next blocks until the next event, returning false when ctx is cancelled or
	// the stream ends.
	Next(ctx context.Context) bool
	// Err returns the terminal error, if any, after Next returns false.
	Err() error
	// Close releases the server-side cursor.
	Close(ctx context.Context) error
}

// Provider resolves mongodb:// refs against a MongoDB database. It is safe for
// concurrent use. The client is built lazily on first use from MONGODB_URI (or
// WithURI) unless a client is injected via WithClient.
type Provider struct {
	uri      string
	database string
	client   *mongo.Client

	mu sync.Mutex
	be backend // resolved backend (injected or lazily built)
}

// Option configures a Provider.
type Option func(*Provider)

// WithURI sets the MongoDB connection string (default: MONGODB_URI).
func WithURI(uri string) Option {
	return func(p *Provider) { p.uri = uri }
}

// WithDatabase sets the database name the provider reads collections from. It is
// required (there is no default) unless a backend is otherwise fully configured.
func WithDatabase(database string) Option {
	return func(p *Provider) { p.database = database }
}

// WithClient injects a pre-configured *mongo.Client, bypassing lazy connection.
// Use it when you build the client yourself (custom TLS, auth, pool settings).
// WithDatabase is still required to select the database.
func WithClient(c *mongo.Client) Option {
	return func(p *Provider) {
		if c != nil {
			p.client = c
		}
	}
}

// withBackend injects a bare backend. Unexported: used by tests to supply an
// in-memory fake.
func withBackend(b backend) Option {
	return func(p *Provider) { p.be = b }
}

// New constructs a MongoDB provider. The client is connected lazily on first
// Resolve/Watch, so New never fails and never contacts MongoDB.
func New(opts ...Option) *Provider {
	p := &Provider{}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// init registers a lazily-initialized provider so `import _` wiring works from
// ambient configuration (MONGODB_URI). Users who need explicit config call
// mamori.WithProvider(mongodb.New(mongodb.WithDatabase("app"), ...)).
func init() { mamori.Register(New()) }

// Scheme returns "mongodb".
func (p *Provider) Scheme() string { return scheme }

// resolveBackend returns the backend, building it lazily on first use from the
// injected client or MONGODB_URI. Concurrent callers share one backend.
func (p *Provider) resolveBackend(ctx context.Context) (backend, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.be != nil {
		return p.be, nil
	}
	if p.database == "" {
		return nil, fmt.Errorf("mongodb: no database configured; use WithDatabase")
	}
	if p.client != nil {
		p.be = &mongoBackend{client: p.client, db: p.database}
		return p.be, nil
	}
	uri := p.uri
	if uri == "" {
		uri = os.Getenv("MONGODB_URI")
	}
	if uri == "" {
		return nil, fmt.Errorf("mongodb: no connection URI; set MONGODB_URI or use WithURI")
	}
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		return nil, fmt.Errorf("mongodb: connect: %w", err)
	}
	p.be = &mongoBackend{client: client, db: p.database}
	return p.be, nil
}

// Resolve fetches the current value for ref from MongoDB. A missing document
// yields an error satisfying errors.Is(err, mamori.ErrNotFound).
func (p *Provider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	if err := ctx.Err(); err != nil {
		return mamori.Value{}, err
	}
	coll, docID, err := parsePath(ref.Path)
	if err != nil {
		return mamori.Value{}, err
	}
	be, err := p.resolveBackend(ctx)
	if err != nil {
		return mamori.Value{}, err
	}
	doc, err := be.FindDoc(ctx, coll, ref.Opt("key"), docID)
	if err != nil {
		return mamori.Value{}, err
	}
	return valueFor(doc, ref)
}

// Watch implements mamori.WatchableProvider using a MongoDB change stream. It
// emits the current value as a baseline, then re-reads and emits a fresh Update
// each time the target document changes. The channel is closed when ctx is
// cancelled; the goroutine never leaks because Next is bound to ctx and the
// cursor is always closed on exit.
func (p *Provider) Watch(ctx context.Context, ref mamori.Ref) (<-chan mamori.Update, error) {
	coll, docID, err := parsePath(ref.Path)
	if err != nil {
		return nil, err
	}
	keyField := ref.Opt("key")
	be, err := p.resolveBackend(ctx)
	if err != nil {
		return nil, err
	}
	cur, err := be.WatchDoc(ctx, coll, keyField, docID)
	if err != nil {
		return nil, err
	}

	ch := make(chan mamori.Update, 1)
	go func() {
		defer close(ch)
		defer func() {
			closeCtx, cancel := context.WithTimeout(context.Background(), watchCloseTimeout)
			defer cancel()
			_ = cur.Close(closeCtx)
		}()

		emit := func(u mamori.Update) bool {
			select {
			case ch <- u:
				return true
			case <-ctx.Done():
				return false
			}
		}

		// Emit the current value as a baseline.
		doc, err := be.FindDoc(ctx, coll, keyField, docID)
		if !emit(toUpdate(doc, err, ref)) {
			return
		}

		for {
			if ctx.Err() != nil {
				return
			}
			if !cur.Next(ctx) {
				if err := cur.Err(); err != nil && ctx.Err() == nil {
					emit(mamori.Update{Err: fmt.Errorf("mongodb: change stream %q: %w", coll, err)})
				}
				return
			}
			doc, err := be.FindDoc(ctx, coll, keyField, docID)
			if !emit(toUpdate(doc, err, ref)) {
				return
			}
		}
	}()
	return ch, nil
}

// toUpdate turns a re-read document (or its error) into a watch Update. A
// not-found error (e.g. the document was deleted) is carried on the Update.
func toUpdate(doc bson.M, err error, ref mamori.Ref) mamori.Update {
	if err != nil {
		return mamori.Update{Err: err}
	}
	v, err := valueFor(doc, ref)
	return mamori.Update{Value: v, Err: err}
}

// parsePath splits a mongodb ref path "<collection>/<docid>" into its parts.
func parsePath(path string) (collection, docID string, err error) {
	collection, docID, ok := strings.Cut(path, "/")
	if !ok || collection == "" || docID == "" {
		return "", "", fmt.Errorf("mongodb: ref path %q must be <collection>/<docid>", path)
	}
	return collection, docID, nil
}

// valueFor converts a MongoDB document into a mamori.Value, applying #field
// selection when requested.
func valueFor(doc bson.M, ref mamori.Ref) (mamori.Value, error) {
	m, full, err := normalizeDoc(doc)
	if err != nil {
		return mamori.Value{}, err
	}
	version := versionString(m["version"])
	if version == "" {
		version = mamori.VersionHash(full)
	}
	out := full
	if ref.Key != "" {
		sel, err := mamori.SelectKey(full, ref.Key)
		if err != nil {
			return mamori.Value{}, err
		}
		out = sel
	}
	return mamori.Value{Bytes: out, Version: version, Sensitive: false}, nil
}

// normalizeDoc encodes a MongoDB document to deterministic, plain JSON. It first
// marshals to relaxed Extended JSON (so BSON types like ObjectID and dates get a
// stable JSON representation), then round-trips through encoding/json so map keys
// are sorted - giving a byte-stable payload for VersionHash change detection. The
// parsed map is returned alongside so the "version" field can be read without a
// second parse.
func normalizeDoc(doc bson.M) (map[string]any, []byte, error) {
	ext, err := bson.MarshalExtJSON(doc, false, false)
	if err != nil {
		return nil, nil, fmt.Errorf("mongodb: encode document: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(ext, &m); err != nil {
		return nil, nil, fmt.Errorf("mongodb: decode document: %w", err)
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, nil, fmt.Errorf("mongodb: re-encode document: %w", err)
	}
	return m, b, nil
}

// versionString renders a document's "version" field into a Value.Version string.
// It returns "" when the field is absent, so the caller falls back to VersionHash.
func versionString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

// --- production backend --------------------------------------------------------

// mongoBackend is the live backend, wrapping a *mongo.Client bound to a database.
type mongoBackend struct {
	client *mongo.Client
	db     string
}

var (
	_ backend      = (*mongoBackend)(nil)
	_ changeCursor = (*mongo.ChangeStream)(nil)
)

func (b *mongoBackend) collection(name string) *mongo.Collection {
	return b.client.Database(b.db).Collection(name)
}

// FindDoc looks up one document, mapping mongo.ErrNoDocuments to mamori.ErrNotFound.
func (b *mongoBackend) FindDoc(ctx context.Context, coll, keyField, keyValue string) (bson.M, error) {
	var doc bson.M
	err := b.collection(coll).FindOne(ctx, docFilter(keyField, keyValue)).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, fmt.Errorf("mongodb: document %q not found in collection %q: %w", keyValue, coll, mamori.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("mongodb: find in %q: %w", coll, err)
	}
	return doc, nil
}

// WatchDoc opens a change stream scoped to the target document, requesting the
// full post-image so custom-key matching works.
func (b *mongoBackend) WatchDoc(ctx context.Context, coll, keyField, keyValue string) (changeCursor, error) {
	opts := options.ChangeStream().SetFullDocument(options.UpdateLookup)
	cs, err := b.collection(coll).Watch(ctx, watchPipeline(keyField, keyValue), opts)
	if err != nil {
		return nil, fmt.Errorf("mongodb: open change stream on %q: %w", coll, err)
	}
	return cs, nil
}

// docFilter builds the find filter for the selected document.
func docFilter(keyField, keyValue string) bson.M {
	if keyField == "" {
		return bson.M{"_id": idValue(keyValue)}
	}
	return bson.M{keyField: keyValue}
}

// watchPipeline builds a change-stream $match stage scoped to the target
// document. When selecting by _id it filters on documentKey._id (present on every
// event); when selecting by a custom field it filters on the looked-up
// fullDocument field.
func watchPipeline(keyField, keyValue string) mongo.Pipeline {
	var match bson.D
	if keyField == "" {
		match = bson.D{{Key: "documentKey._id", Value: idValue(keyValue)}}
	} else {
		match = bson.D{{Key: "fullDocument." + keyField, Value: keyValue}}
	}
	return mongo.Pipeline{{{Key: "$match", Value: match}}}
}

// idValue interprets a <docid> as an ObjectID when it is a valid 24-char hex
// string, otherwise as the raw string, so both ObjectID and string _ids work.
func idValue(s string) any {
	if oid, err := primitive.ObjectIDFromHex(s); err == nil {
		return oid
	}
	return s
}
