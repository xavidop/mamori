package mamori

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/fsnotify/fsnotify"
)

// fileProvider is the built-in file:// provider. It reads a file's contents and
// natively watches it for changes with fsnotify, so file-backed values (TLS
// certs, mounted Kubernetes secrets, config files) are hot-reloaded.
//
//	TLSCert []byte `source:"file:///etc/tls/tls.crt"`
//
// The Version is a hash of the file size and modification time, giving cheap
// change detection without reading unchanged files twice.
type fileProvider struct{}

func (fileProvider) Scheme() string { return "file" }

func (fileProvider) Resolve(_ context.Context, ref Ref) (Value, error) {
	path := ref.Path
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Value{}, ErrNotFound
		}
		return Value{}, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Value{}, ErrNotFound
		}
		return Value{}, err
	}
	ver := fmt.Sprintf("%d-%d", info.Size(), info.ModTime().UnixNano())
	return Value{Bytes: b, Version: ver}, nil
}

// Watch implements WatchableProvider using fsnotify. It watches the parent
// directory (so atomic replace via rename - the common Kubernetes/secret-mount
// pattern - is detected) and re-resolves the target on relevant events.
func (p fileProvider) Watch(ctx context.Context, ref Ref) (<-chan Update, error) {
	return watchFilePath(ctx, ref.Path, func(c context.Context) (Value, error) {
		return p.Resolve(c, ref)
	})
}

// watchFilePath is the shared fsnotify watch loop used by the built-in file:// and
// dotenv: providers. It watches the parent directory of path (so atomic replace
// via rename is detected), emits a baseline immediately, and re-runs resolve on
// every relevant event. The channel closes on ctx cancellation and the goroutine
// never leaks (the watcher is always closed on exit).
func watchFilePath(ctx context.Context, path string, resolve func(context.Context) (Value, error)) (<-chan Update, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	target := filepath.Clean(path)
	dir := filepath.Dir(target)
	if err := w.Add(dir); err != nil {
		_ = w.Close()
		return nil, err
	}

	ch := make(chan Update, 1)
	go func() {
		defer close(ch)
		defer func() { _ = w.Close() }()

		emit := func() {
			v, err := resolve(ctx)
			select {
			case ch <- Update{Value: v, Err: err}:
			case <-ctx.Done():
			}
		}
		emit() // baseline

		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-w.Events:
				if !ok {
					return
				}
				if filepath.Clean(ev.Name) == target {
					emit()
				}
			case werr, ok := <-w.Errors:
				if !ok {
					return
				}
				select {
				case ch <- Update{Err: werr}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return ch, nil
}
