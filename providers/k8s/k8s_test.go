package k8s_test

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

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
	})
}

func TestConformanceConfigMap(t *testing.T) {
	client := fake.NewSimpleClientset()
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
	})
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
