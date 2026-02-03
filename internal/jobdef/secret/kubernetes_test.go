package secret

import (
	"context"
	"testing"

	"github.com/stretchr/testify/suite"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

type KubernetesResolverSuite struct {
	suite.Suite
}

func TestKubernetesResolverSuite(t *testing.T) {
	suite.Run(t, new(KubernetesResolverSuite))
}

func (s *KubernetesResolverSuite) TestResolveDefaultNamespace() {
	client := fake.NewClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "git-creds", Namespace: "jobs"},
		Data:       map[string][]byte{"password": []byte("hunter2")},
	})

	r := NewKubernetesResolverWithClient(client, "jobs")
	value, err := r.Resolve(context.Background(), "secret://k8s/git-creds/password")
	s.Require().NoError(err)
	s.Equal("hunter2", value)
}

func (s *KubernetesResolverSuite) TestResolveExplicitNamespace() {
	client := fake.NewClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "git-creds", Namespace: "infra"},
		Data:       map[string][]byte{"token": []byte("abc")},
	})

	r := NewKubernetesResolverWithClient(client, "default")
	value, err := r.Resolve(context.Background(), "secret://k8s/infra/git-creds/token")
	s.Require().NoError(err)
	s.Equal("abc", value)
}

func (s *KubernetesResolverSuite) TestMissingSecretFails() {
	client := fake.NewClientset()
	r := NewKubernetesResolverWithClient(client, "default")
	_, err := r.Resolve(context.Background(), "secret://k8s/missing/password")
	s.Require().Error(err)
}

func (s *KubernetesResolverSuite) TestMalformedReferenceFails() {
	r := NewKubernetesResolver(KubernetesConfig{})
	_, err := r.Resolve(context.Background(), "secret://k8s/onlyone")
	s.Require().Error(err)
}
