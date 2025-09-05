package clients

import (
	context "context"

	"github.com/vitistack/kea-operator/pkg/clients/keaclient"
	"github.com/vitistack/kea-operator/pkg/interfaces/keainterface"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// BuildKeaClientFromSecret fetches a Kubernetes secret and returns a configured Kea client
// using in-memory TLS material. If the secret can't be retrieved or lacks material, it
// returns nil (caller may fallback to non-TLS client).
func BuildKeaClientFromSecret(ctx context.Context, kube kubernetes.Interface, namespace, name string, baseOpts ...keaclient.KeaOption) (keainterface.KeaClient, error) {
	if kube == nil || name == "" {
		return nil, nil
	}
	sec, err := kube.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	opts := append(baseOpts, keaclient.OptionTLSFromSecret(sec))
	return keaclient.NewKeaClientWithOptions(opts...), nil
}
