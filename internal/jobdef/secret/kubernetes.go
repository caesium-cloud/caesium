package secret

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	providerKubernetes = "k8s"
	defaultNamespace   = "default"
)

// KubernetesConfig describes how to connect to the Kubernetes API.
type KubernetesConfig struct {
	KubeConfigPath string
	Namespace      string
}

// KubernetesResolver fetches secrets from Kubernetes Secret resources.
type KubernetesResolver struct {
	client     kubernetes.Interface
	config     KubernetesConfig
	clientOnce sync.Once
	clientErr  error
}

// NewKubernetesResolver builds a resolver using the provided config.
func NewKubernetesResolver(cfg KubernetesConfig) *KubernetesResolver {
	if strings.TrimSpace(cfg.Namespace) == "" {
		cfg.Namespace = defaultNamespace
	}
	return &KubernetesResolver{config: cfg}
}

// NewKubernetesResolverWithClient constructs a resolver with a pre-built client (primarily for tests).
func NewKubernetesResolverWithClient(client kubernetes.Interface, namespace string) *KubernetesResolver {
	cfg := KubernetesConfig{Namespace: namespace}
	if strings.TrimSpace(cfg.Namespace) == "" {
		cfg.Namespace = defaultNamespace
	}
	return &KubernetesResolver{client: client, config: cfg}
}

// Resolve implements the Resolver interface.
func (r *KubernetesResolver) Resolve(ctx context.Context, ref string) (string, error) {
	reference, err := Parse(ref)
	if err != nil {
		return "", err
	}
	if reference.Provider != providerKubernetes && reference.Provider != "kubernetes" {
		return "", fmt.Errorf("kubernetes resolver cannot handle provider %q", reference.Provider)
	}

	namespace, name, key, err := r.parseReference(reference)
	if err != nil {
		return "", err
	}

	client, err := r.clientset()
	if err != nil {
		return "", err
	}

	secret, err := client.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("load kubernetes secret %s/%s: %w", namespace, name, err)
	}

	if secret.Data == nil {
		return "", fmt.Errorf("kubernetes secret %s/%s has no data", namespace, name)
	}

	value, ok := secret.Data[key]
	if !ok {
		return "", fmt.Errorf("kubernetes secret %s/%s missing key %s", namespace, name, key)
	}

	return string(value), nil
}

func (r *KubernetesResolver) parseReference(ref *Reference) (namespace, name, key string, err error) {
	namespace = r.config.Namespace
	segments := ref.Segments

	switch len(segments) {
	case 2:
		name = strings.TrimSpace(segments[0])
		key = strings.TrimSpace(segments[1])
	case 3:
		namespace = strings.TrimSpace(segments[0])
		name = strings.TrimSpace(segments[1])
		key = strings.TrimSpace(segments[2])
	default:
		return "", "", "", fmt.Errorf("kubernetes secret references must be secret://k8s/<secret>/<key> or secret://k8s/<namespace>/<secret>/<key>")
	}

	if qns := strings.TrimSpace(ref.Query.Get("namespace")); qns != "" {
		namespace = qns
	}
	if qname := strings.TrimSpace(ref.Query.Get("name")); qname != "" {
		name = qname
	}
	if qkey := strings.TrimSpace(ref.Query.Get("key")); qkey != "" {
		key = qkey
	}

	if namespace == "" {
		namespace = defaultNamespace
	}
	if name == "" || key == "" {
		return "", "", "", fmt.Errorf("kubernetes secret reference %q missing name or key", ref.Raw)
	}

	return namespace, name, key, nil
}

func (r *KubernetesResolver) clientset() (kubernetes.Interface, error) {
	if r.client != nil {
		return r.client, nil
	}

	r.clientOnce.Do(func() {
		cfg, err := buildKubeConfig(r.config.KubeConfigPath)
		if err != nil {
			r.clientErr = err
			return
		}
		client, err := kubernetes.NewForConfig(cfg)
		if err != nil {
			r.clientErr = fmt.Errorf("create kubernetes client: %w", err)
			return
		}
		r.client = client
	})

	return r.client, r.clientErr
}

func buildKubeConfig(kubeconfig string) (*rest.Config, error) {
	if strings.TrimSpace(kubeconfig) != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}

	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}

	home, err := os.UserHomeDir()
	if err == nil {
		path := filepath.Join(home, ".kube", "config")
		if _, statErr := os.Stat(path); statErr == nil {
			return clientcmd.BuildConfigFromFlags("", path)
		}
	}

	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, &clientcmd.ConfigOverrides{}).ClientConfig()
}
