package k8s_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/xavidop/mamori"
	k8sprov "github.com/xavidop/mamori/providers/k8s"
	"github.com/xavidop/mamori/providertest"
)

const testNamespace = "default"

// rvCounter hands out monotonically increasing ResourceVersions so that the
// fake clientset produces changing Value.Version across updates (the fake does
// not manage ResourceVersion on its own).
type rvCounter struct {
	mu sync.Mutex
	n  int
}

func (c *rvCounter) next() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	return strconv.Itoa(c.n)
}

// failInjector adds error injection to a fake clientset. A single reactor
// installed at construction consults a map, so failures can be both added and
// removed. Calling PrependReactor per injection would be simpler but reactors
// accumulate on the chain and cannot be removed individually, which would make
// Clear impossible and would leak injected failures into later conformance
// subtests sharing the same clientset.
//
// The reactor only sees a GetAction's object name (via GetName()), not the
// conformance kit's namespaced key, so fail/clear key on the object name: it
// is the caller's job (see sanitize below) to translate the kit's key into
// the same name Seed/Ref used to create the object.
type failInjector struct {
	mu    sync.Mutex
	fails map[string]error
}

func newFailInjector(cs *fake.Clientset) *failInjector {
	fi := &failInjector{fails: map[string]error{}}
	cs.PrependReactor("*", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		ga, ok := action.(k8stesting.GetAction)
		if !ok {
			return false, nil, nil
		}
		fi.mu.Lock()
		defer fi.mu.Unlock()
		if err, found := fi.fails[ga.GetName()]; found {
			return true, nil, err
		}
		return false, nil, nil
	})
	return fi
}

func (f *failInjector) fail(name string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fails[name] = err
}

func (f *failInjector) clear(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.fails, name)
}

func TestSecretResolve(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: testNamespace, ResourceVersion: "1"},
		Data:       map[string][]byte{"password": []byte("s3cr3t")},
	})
	p := k8sprov.New(k8sprov.WithClient(client))

	ref, _ := mamori.ParseRef("k8s-secret://default/db#password")
	v, err := p.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if string(v.Bytes) != "s3cr3t" {
		t.Errorf("value = %q, want s3cr3t", v.Bytes)
	}
	if !v.Sensitive {
		t.Error("secret value should be Sensitive")
	}
	if v.Version != "1" {
		t.Errorf("version = %q, want 1", v.Version)
	}

	// Missing key -> ErrNotFound.
	missing, _ := mamori.ParseRef("k8s-secret://default/db#nope")
	if _, err := p.Resolve(context.Background(), missing); err == nil {
		t.Fatal("missing key should error")
	}
}

func TestConfigMapResolve(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: testNamespace, ResourceVersion: "1"},
		Data:       map[string]string{"log_level": "debug"},
	})
	p := k8sprov.NewConfigMap(k8sprov.WithClient(client))

	ref, _ := mamori.ParseRef("k8s-cm://default/app#log_level")
	v, err := p.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if string(v.Bytes) != "debug" {
		t.Errorf("value = %q, want debug", v.Bytes)
	}
	if v.Sensitive {
		t.Error("configmap value must not be Sensitive")
	}
}

func TestSecretNotFound(t *testing.T) {
	p := k8sprov.New(k8sprov.WithClient(fake.NewSimpleClientset()))
	ref, _ := mamori.ParseRef("k8s-secret://default/absent#x")
	_, err := p.Resolve(context.Background(), ref)
	if err == nil {
		t.Fatal("expected error for absent secret")
	}
}

// conformanceSecret runs the shared kit for the k8s-secret scheme. The fake
// clientset supports Watch, so the watch conformance checks run for real.
func TestConformanceSecret(t *testing.T) {
	client := fake.NewSimpleClientset()
	fi := newFailInjector(client)
	var rv rvCounter

	upsert := func(name, val string) {
		obj := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, ResourceVersion: rv.next()},
			Data:       map[string][]byte{"value": []byte(val)},
		}
		secrets := client.CoreV1().Secrets(testNamespace)
		if _, err := secrets.Get(context.Background(), name, metav1.GetOptions{}); err == nil {
			_, _ = secrets.Update(context.Background(), obj, metav1.UpdateOptions{})
		} else {
			_, _ = secrets.Create(context.Background(), obj, metav1.CreateOptions{})
		}
	}

	providertest.Run(t, providertest.Config{
		New: func() mamori.Provider { return k8sprov.New(k8sprov.WithClient(client)) },
		Ref: func(key string) string {
			// key is a bare name; map it to default/<name>#value
			return "k8s-secret://" + testNamespace + "/" + sanitize(key) + "#value"
		},
		Seed:   func(_ context.Context, key, val string) error { upsert(sanitize(key), val); return nil },
		Mutate: func(_ context.Context, key, val string) error { upsert(sanitize(key), val); return nil },
		// The kit's key is a namespaced conformance key; the reactor only sees
		// the object name that Ref/Seed derived from it via sanitize, so Fail
		// and Clear must apply the same translation or the reactor never fires.
		Fail: func(_ context.Context, key string, err error) error {
			fi.fail(sanitize(key), err)
			return nil
		},
		Clear: func(_ context.Context, key string) error {
			fi.clear(sanitize(key))
			return nil
		},
	})
}

func TestConformanceConfigMap(t *testing.T) {
	client := fake.NewSimpleClientset()
	fi := newFailInjector(client)
	var rv rvCounter

	upsert := func(name, val string) {
		obj := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, ResourceVersion: rv.next()},
			Data:       map[string]string{"value": val},
		}
		cms := client.CoreV1().ConfigMaps(testNamespace)
		if _, err := cms.Get(context.Background(), name, metav1.GetOptions{}); err == nil {
			_, _ = cms.Update(context.Background(), obj, metav1.UpdateOptions{})
		} else {
			_, _ = cms.Create(context.Background(), obj, metav1.CreateOptions{})
		}
	}

	providertest.Run(t, providertest.Config{
		New:    func() mamori.Provider { return k8sprov.NewConfigMap(k8sprov.WithClient(client)) },
		Ref:    func(key string) string { return "k8s-cm://" + testNamespace + "/" + sanitize(key) + "#value" },
		Seed:   func(_ context.Context, key, val string) error { upsert(sanitize(key), val); return nil },
		Mutate: func(_ context.Context, key, val string) error { upsert(sanitize(key), val); return nil },
		Fail: func(_ context.Context, key string, err error) error {
			fi.fail(sanitize(key), err)
			return nil
		},
		Clear: func(_ context.Context, key string) error {
			fi.clear(sanitize(key))
			return nil
		},
	})
}

// TestResolveClassifiesForbidden exercises classifyK8s through Resolve
// itself, not just as a direct function call. The conformance
// ErrorClassification case (and the failInjector powering it) injects a
// mamori sentinel directly, so it would still pass even if the classifyK8s
// call were deleted from mapGetError's fallback branch. This test instead
// injects a real *apierrors.StatusError via PrependReactor, the same shape a
// live API server returns for an RBAC denial, so it fails if the classify
// wiring in mapGetError is removed.
func TestResolveClassifiesForbidden(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("get", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		gr := schema.GroupResource{Group: "", Resource: "secrets"}
		return true, nil, apierrors.NewForbidden(gr, "db", errors.New("rbac denied"))
	})
	p := k8sprov.New(k8sprov.WithClient(client))

	ref, err := mamori.ParseRef("k8s-secret://default/db")
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	_, resolveErr := p.Resolve(context.Background(), ref)
	if resolveErr == nil {
		t.Fatal("Resolve returned a nil error while the backend was failing")
	}
	if got := mamori.ErrorKind(resolveErr); got != mamori.KindPermissionDenied {
		t.Fatalf("ErrorKind(err) = %q, want %q; classifyK8s may not be wired into mapGetError: %v",
			got, mamori.KindPermissionDenied, resolveErr)
	}
}

// sanitize maps arbitrary conformance keys to RFC-1123 object names.
func sanitize(key string) string {
	key = strings.ToLower(key)
	key = strings.ReplaceAll(key, "_", "-")
	var b strings.Builder
	for _, r := range key {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "obj"
	}
	return out
}
