// Package k8s provides mamori providers backed by Kubernetes Secrets and
// ConfigMaps. It resolves values through the client-go typed clientset and
// implements native change notification via the Kubernetes watch API (the same
// mechanism informers are built on), so secret/configmap-backed values are
// hot-reloaded without polling.
//
// Two schemes are registered:
//
//	k8s-secret://<namespace>/<name>[#<key>]   // reads a core/v1 Secret (Sensitive)
//	k8s-cm://<namespace>/<name>[#<key>]       // reads a core/v1 ConfigMap
//
// With a #key, the value is the corresponding entry of the object's data map
// (client-go already base64-decodes Secret data). Without a #key, the entire
// data map is JSON-encoded as an object of string values. Value.Version is the
// object's ResourceVersion, giving mamori native, monotonic change detection.
package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/xavidop/mamori"
)

// Scheme constants for the two registered providers.
const (
	SchemeSecret    = "k8s-secret"
	SchemeConfigMap = "k8s-cm"
)

// watchRetryDelay bounds how quickly a watch is re-established after the watch
// channel closes or an error is received while the context is still alive.
const watchRetryDelay = time.Second

// resourceKind selects which core/v1 object a Provider reads.
type resourceKind int

const (
	kindSecret resourceKind = iota
	kindConfigMap
)

// Provider resolves a single scheme (k8s-secret or k8s-cm) against a Kubernetes
// API server. It is safe for concurrent use. The clientset is created lazily on
// first use so that constructing/registering a Provider never requires cluster
// access at init time.
type Provider struct {
	kind   resourceKind
	scheme string

	mu        sync.Mutex
	client    kubernetes.Interface
	clientErr error
	newClient func() (kubernetes.Interface, error)
	ownClient bool // true only when this provider built client itself (default resolution or WithClientFactory)
	closed    bool // Close has run; every later resolve/watch reports unavailable
}

// options holds constructor configuration mutated by Option values.
type options struct {
	client    kubernetes.Interface
	newClient func() (kubernetes.Interface, error)
}

// Option configures a Provider constructed by New or NewConfigMap.
type Option func(*options)

// WithClient injects a kubernetes.Interface. The real *kubernetes.Clientset
// satisfies it, as does k8s.io/client-go/kubernetes/fake for tests.
func WithClient(c kubernetes.Interface) Option {
	return func(o *options) { o.client = c }
}

// WithClientFactory injects a factory invoked lazily on first use to build the
// clientset. Use it to defer credential/config loading.
func WithClientFactory(fn func() (kubernetes.Interface, error)) Option {
	return func(o *options) { o.newClient = fn }
}

// WithKubeconfig builds the clientset from an explicit kubeconfig file path,
// overriding the default in-cluster/KUBECONFIG resolution.
func WithKubeconfig(path string) Option {
	return func(o *options) {
		o.newClient = func() (kubernetes.Interface, error) {
			cfg, err := clientcmd.BuildConfigFromFlags("", path)
			if err != nil {
				return nil, fmt.Errorf("k8s: loading kubeconfig %q: %w", path, err)
			}
			return kubernetes.NewForConfig(cfg)
		}
	}
}

// New constructs the k8s-secret provider.
func New(opts ...Option) *Provider {
	return newProvider(kindSecret, SchemeSecret, opts...)
}

// NewConfigMap constructs the k8s-cm provider.
func NewConfigMap(opts ...Option) *Provider {
	return newProvider(kindConfigMap, SchemeConfigMap, opts...)
}

func newProvider(kind resourceKind, scheme string, opts ...Option) *Provider {
	var cfg options
	for _, o := range opts {
		o(&cfg)
	}
	return &Provider{
		kind:      kind,
		scheme:    scheme,
		client:    cfg.client,
		newClient: cfg.newClient,
	}
}

func init() {
	mamori.Register(New())
	mamori.Register(NewConfigMap())
}

// Scheme reports the URL scheme this provider handles.
func (p *Provider) Scheme() string { return p.scheme }

// getClient lazily resolves the clientset, memoizing the result (and any
// error).
//
// The closed check runs first, ahead of the p.client != nil cache check, so a
// closed provider refuses locally even when a clientset (injected or
// previously built) is still sitting in p.client: Close clears p.client on
// the owned path, but the ordering here is what makes the refusal
// unconditional rather than incidental.
func (p *Provider) getClient() (kubernetes.Interface, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, fmt.Errorf("%w: %s: provider is closed", mamori.ErrUnavailable, p.scheme)
	}
	if p.client != nil {
		return p.client, nil
	}
	if p.clientErr != nil {
		return nil, p.clientErr
	}
	if p.newClient != nil {
		client, err := p.newClient()
		if err != nil {
			p.clientErr = err
			return nil, err
		}
		p.client = client
		p.ownClient = true
		return p.client, nil
	}
	client, err := defaultClient()
	if err != nil {
		p.clientErr = err
		return nil, err
	}
	p.client = client
	p.ownClient = true
	return p.client, nil
}

// Close releases the idle HTTP connections held by the clientset this
// provider built itself, whether via WithClientFactory or the default
// in-cluster/kubeconfig resolution (WithClientFactory counts as owned, not
// injected: the factory returns a client that does not exist until New()'s
// options run, so the caller cannot already hold a reference to it). It is a
// no-op when the clientset was injected directly with WithClient (the caller
// owns it) and when nothing was ever built, so New followed by Close never
// contacts a cluster. closed is set unconditionally before either of those
// checks, so the not-owned and never-built paths still make Close terminal:
// afterwards Resolve and Watch report mamori.ErrUnavailable, refused locally
// by getClient, rather than reaching for a clientset this provider was just
// told to stop using. Close is idempotent and safe for concurrent use.
//
// client-go's generated Clientset has no Close method of its own - each typed
// REST client just wraps an *http.Client - so releasing means asking that
// *http.Client to drop its idle keep-alive connections via
// CloseIdleConnections, reached through the CoreV1 REST client's transport.
func (p *Provider) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	if !p.ownClient || p.client == nil {
		return nil
	}
	closeIdleConnections(p.client)
	p.client = nil
	return nil
}

// closeIdleConnections best-effort releases the idle HTTP connections behind
// client's CoreV1 REST client. A client whose RESTClient is not the concrete
// *rest.RESTClient type has nothing to release through this path, so that
// case (and the fake clientset used throughout this package's tests, whose
// FakeCoreV1.RESTClient() deliberately returns a typed-nil *rest.RESTClient)
// is a silent no-op rather than a panic or a type-assertion failure.
func closeIdleConnections(client kubernetes.Interface) {
	rc, ok := client.CoreV1().RESTClient().(*rest.RESTClient)
	if !ok || rc == nil || rc.Client == nil {
		return
	}
	rc.Client.CloseIdleConnections()
}

// defaultClient builds a clientset from in-cluster config, falling back to the
// default kubeconfig loading rules (KUBECONFIG env, then ~/.kube/config).
func defaultClient() (kubernetes.Interface, error) {
	if cfg, err := rest.InClusterConfig(); err == nil {
		return kubernetes.NewForConfig(cfg)
	}
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, &clientcmd.ConfigOverrides{})
	cfg, err := cc.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("k8s: no in-cluster config and no usable kubeconfig: %w", err)
	}
	return kubernetes.NewForConfig(cfg)
}

// Resolve fetches the current Value for ref. It returns an error satisfying
// errors.Is(err, mamori.ErrNotFound) when the object (or the requested #key)
// does not exist.
func (p *Provider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	if err := ctx.Err(); err != nil {
		return mamori.Value{}, err
	}
	ns, name, err := splitPath(ref.Path)
	if err != nil {
		return mamori.Value{}, err
	}
	client, err := p.getClient()
	if err != nil {
		return mamori.Value{}, err
	}

	switch p.kind {
	case kindSecret:
		s, err := client.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return mamori.Value{}, mapGetError(p.scheme, "secret", ns, name, err)
		}
		return secretValue(s, ref.Key)
	case kindConfigMap:
		cm, err := client.CoreV1().ConfigMaps(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return mamori.Value{}, mapGetError(p.scheme, "configmap", ns, name, err)
		}
		return configMapValue(cm, ref.Key)
	default:
		return mamori.Value{}, fmt.Errorf("k8s: unknown resource kind")
	}
}

// Watch implements mamori.WatchableProvider using the Kubernetes watch API. It
// emits a baseline Update for the current value, then an Update on every
// Added/Modified event for the target object. The returned channel is closed
// when ctx is cancelled. If the server-side watch ends while ctx is still
// alive, the watch is re-established (re-list + re-watch).
func (p *Provider) Watch(ctx context.Context, ref mamori.Ref) (<-chan mamori.Update, error) {
	ns, name, err := splitPath(ref.Path)
	if err != nil {
		return nil, err
	}
	client, err := p.getClient()
	if err != nil {
		return nil, err
	}
	key := ref.Key

	ch := make(chan mamori.Update, 1)
	go func() {
		defer close(ch)
		for {
			if ctx.Err() != nil {
				return
			}
			w, werr := p.startWatch(ctx, client, ns, name)
			if werr != nil {
				werr = fmt.Errorf("%s: watch %s/%s: %w", p.scheme, ns, name, classifyK8s(werr))
				if !send(ctx, ch, mamori.Update{Err: werr}) {
					return
				}
				if !sleep(ctx, watchRetryDelay) {
					return
				}
				continue
			}
			// Emit the current snapshot after the watch is established so no
			// change occurring between snapshot and watch is lost.
			if v, rerr := p.Resolve(ctx, ref); rerr == nil {
				if !send(ctx, ch, mamori.Update{Value: v}) {
					w.Stop()
					return
				}
			} else if !errors.Is(rerr, mamori.ErrNotFound) {
				if !send(ctx, ch, mamori.Update{Err: rerr}) {
					w.Stop()
					return
				}
			}

			cont := p.consume(ctx, w.ResultChan(), ch, name, key)
			w.Stop()
			if !cont {
				return
			}
			if !sleep(ctx, watchRetryDelay) {
				return
			}
		}
	}()
	return ch, nil
}

// consume drains a single watch's result channel. It returns true when the
// watch ended (channel closed or a watch.Error) and the caller should
// re-establish it, and false when the loop should terminate (ctx cancelled or a
// send was abandoned).
func (p *Provider) consume(ctx context.Context, results <-chan watch.Event, ch chan<- mamori.Update, name, key string) bool {
	for {
		select {
		case <-ctx.Done():
			return false
		case ev, ok := <-results:
			if !ok {
				return true // watch ended; re-establish
			}
			switch ev.Type {
			case watch.Added, watch.Modified:
				v, matched, verr := p.eventValue(ev.Object, name, key)
				if !matched {
					continue
				}
				if !send(ctx, ch, mamori.Update{Value: v, Err: verr}) {
					return false
				}
			case watch.Deleted:
				err := fmt.Errorf("%s: %s deleted: %w", p.scheme, name, mamori.ErrNotFound)
				if !send(ctx, ch, mamori.Update{Err: err}) {
					return false
				}
			case watch.Error:
				return true // re-list + re-watch
			}
		}
	}
}

// startWatch opens a name-scoped watch for the provider's resource kind.
func (p *Provider) startWatch(ctx context.Context, client kubernetes.Interface, ns, name string) (watch.Interface, error) {
	opts := metav1.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("metadata.name", name).String(),
	}
	switch p.kind {
	case kindSecret:
		return client.CoreV1().Secrets(ns).Watch(ctx, opts)
	case kindConfigMap:
		return client.CoreV1().ConfigMaps(ns).Watch(ctx, opts)
	default:
		return nil, fmt.Errorf("k8s: unknown resource kind")
	}
}

// eventValue extracts a Value from a watch event object. matched is false when
// the event is for a different object or an unexpected type.
func (p *Provider) eventValue(obj runtime.Object, name, key string) (v mamori.Value, matched bool, err error) {
	switch p.kind {
	case kindSecret:
		s, ok := obj.(*corev1.Secret)
		if !ok || s.Name != name {
			return mamori.Value{}, false, nil
		}
		v, err = secretValue(s, key)
		return v, true, err
	case kindConfigMap:
		cm, ok := obj.(*corev1.ConfigMap)
		if !ok || cm.Name != name {
			return mamori.Value{}, false, nil
		}
		v, err = configMapValue(cm, key)
		return v, true, err
	default:
		return mamori.Value{}, false, nil
	}
}

// secretValue builds a Value from a Secret. Secret data is always Sensitive.
func secretValue(s *corev1.Secret, key string) (mamori.Value, error) {
	if key != "" {
		b, ok := s.Data[key]
		if !ok {
			return mamori.Value{}, fmt.Errorf("%s: key %q not present in secret %s/%s: %w",
				SchemeSecret, key, s.Namespace, s.Name, mamori.ErrNotFound)
		}
		return mamori.Value{Bytes: b, Version: s.ResourceVersion, Sensitive: true}, nil
	}
	// No key: JSON-encode the whole data map as string values (not base64).
	m := make(map[string]string, len(s.Data))
	for k, v := range s.Data {
		m[k] = string(v)
	}
	b, err := json.Marshal(m)
	if err != nil {
		return mamori.Value{}, fmt.Errorf("%s: encoding secret %s/%s data: %w", SchemeSecret, s.Namespace, s.Name, err)
	}
	return mamori.Value{Bytes: b, Version: s.ResourceVersion, Sensitive: true}, nil
}

// configMapValue builds a Value from a ConfigMap. ConfigMap data is not
// Sensitive. A #key is looked up in Data, then BinaryData.
func configMapValue(cm *corev1.ConfigMap, key string) (mamori.Value, error) {
	if key != "" {
		if b, ok := cm.Data[key]; ok {
			return mamori.Value{Bytes: []byte(b), Version: cm.ResourceVersion}, nil
		}
		if b, ok := cm.BinaryData[key]; ok {
			return mamori.Value{Bytes: b, Version: cm.ResourceVersion}, nil
		}
		return mamori.Value{}, fmt.Errorf("%s: key %q not present in configmap %s/%s: %w",
			SchemeConfigMap, key, cm.Namespace, cm.Name, mamori.ErrNotFound)
	}
	b, err := json.Marshal(cm.Data)
	if err != nil {
		return mamori.Value{}, fmt.Errorf("%s: encoding configmap %s/%s data: %w", SchemeConfigMap, cm.Namespace, cm.Name, err)
	}
	return mamori.Value{Bytes: b, Version: cm.ResourceVersion}, nil
}

// classifyK8s maps a Kubernetes API error onto a mamori classification
// sentinel, using the apierrors predicates rather than raw status codes so it
// stays correct across API versions.
//
// The %w: %w wrapping keeps the underlying *StatusError reachable, so callers
// can still use apierrors.IsForbidden and friends on an error that has passed
// through mamori.
func classifyK8s(err error) error {
	if err == nil {
		return nil
	}
	var sentinel error
	switch {
	case apierrors.IsNotFound(err):
		sentinel = mamori.ErrNotFound
	case apierrors.IsForbidden(err):
		sentinel = mamori.ErrPermissionDenied
	case apierrors.IsUnauthorized(err):
		sentinel = mamori.ErrUnauthenticated
	case apierrors.IsTooManyRequests(err):
		sentinel = mamori.ErrRateLimited
	case apierrors.IsServiceUnavailable(err), apierrors.IsTimeout(err), apierrors.IsServerTimeout(err):
		sentinel = mamori.ErrUnavailable
	case apierrors.IsBadRequest(err), apierrors.IsInvalid(err):
		sentinel = mamori.ErrInvalid
	default:
		return err
	}
	return fmt.Errorf("%w: %w", sentinel, err)
}

// mapGetError converts a client-go Get error into a mamori error, mapping
// NotFound to mamori.ErrNotFound. The IsNotFound branch is checked first and
// verbatim so its message stays stable; every other condition falls through
// to classifyK8s so RBAC denials, throttling, etc. are typed rather than
// opaque.
func mapGetError(scheme, kind, ns, name string, err error) error {
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("%s: %s %s/%s not found: %w: %w", scheme, kind, ns, name, mamori.ErrNotFound, err)
	}
	return fmt.Errorf("%s: getting %s %s/%s: %w", scheme, kind, ns, name, classifyK8s(err))
}

// splitPath parses "<namespace>/<name>" from a ref path.
func splitPath(path string) (ns, name string, err error) {
	parts := strings.SplitN(path, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("k8s: ref path %q must be of the form <namespace>/<name>: %w", path, mamori.ErrInvalid)
	}
	if strings.Contains(parts[1], "/") {
		return "", "", fmt.Errorf("k8s: ref path %q must be of the form <namespace>/<name>: %w", path, mamori.ErrInvalid)
	}
	return parts[0], parts[1], nil
}

// send delivers u on ch, or returns false if ctx is cancelled first.
func send(ctx context.Context, ch chan<- mamori.Update, u mamori.Update) bool {
	select {
	case ch <- u:
		return true
	case <-ctx.Done():
		return false
	}
}

// sleep waits for d, returning false if ctx is cancelled first.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// Compile-time interface assertions.
var (
	_ mamori.Provider          = (*Provider)(nil)
	_ mamori.WatchableProvider = (*Provider)(nil)
)
