package kubernetes

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/suite"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/flowcontrol"
)

type KubernetesTestSuite struct {
	suite.Suite
	engine *kubernetesEngine
}

type mockKubernetesBackend struct {
	mock.Mock
	kubernetesBackend
}

func (m *mockKubernetesBackend) Create(ctx context.Context, pod *v1.Pod, opts metav1.CreateOptions) (*v1.Pod, error) {
	args := m.Called()
	if strings.HasPrefix(pod.Name, "-") {
		return nil, args.Error(0)
	}
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: pod.Name,
		},
	}, nil
}

func (m *mockKubernetesBackend) Delete(ctx context.Context, name string, opts metav1.DeleteOptions) error {
	args := m.Called(name)
	if name == "" {
		return args.Error(0)
	}
	if _, ok := ctx.Deadline(); ok {
		return ctx.Err()
	}
	return nil
}

func (m *mockKubernetesBackend) Get(ctx context.Context, name string, opts metav1.GetOptions) (*v1.Pod, error) {
	args := m.Called(name)
	if name == "" {
		return nil, args.Error(0)
	}
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}, nil
}

func (m *mockKubernetesBackend) List(ctx context.Context, opts metav1.ListOptions) (*v1.PodList, error) {
	m.Called()
	return &v1.PodList{
		Items: []v1.Pod{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test_atom",
				},
			},
		},
	}, nil
}

func (m *mockKubernetesBackend) GetLogs(name string, opts *v1.PodLogOptions) *rest.Request {
	m.Called(name)
	if name == "" {
		return nil
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := fmt.Fprint(w, "logs"); err != nil {
			panic(err)
		}
	}))

	u, _ := url.Parse(ts.URL)

	cli, _ := rest.NewRESTClient(
		u, "", rest.ClientContentConfig{},
		flowcontrol.NewFakeAlwaysRateLimiter(),
		ts.Client(),
	)

	return rest.NewRequest(cli)
}

var (
	testAtomID = "test_id"
	testImage  = "caesiumcloud/caesium"
)

func newPod(name string, status v1.PodStatus, createdAt, deletedAt time.Time) *v1.Pod {
	return &v1.Pod{
		Status: status,
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			CreationTimestamp: metav1.Time{
				Time: createdAt,
			},
			DeletionTimestamp: &metav1.Time{
				Time: deletedAt,
			},
		},
	}
}

func (s *KubernetesTestSuite) SetupTest() {
	s.engine = &kubernetesEngine{
		backend: &mockKubernetesBackend{},
		ctx:     context.Background(),
	}
}

func TestKubernetesTestSuite(t *testing.T) {
	suite.Run(t, new(KubernetesTestSuite))
}
