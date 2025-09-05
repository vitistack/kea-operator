package unstructuredconv

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	vitistackcrdsv1alpha1 "github.com/vitistack/crds/pkg/v1alpha1"
)

// NetworkConfiguration
func ToNetworkConfiguration(u *unstructured.Unstructured) (*vitistackcrdsv1alpha1.NetworkConfiguration, error) {
	out := &vitistackcrdsv1alpha1.NetworkConfiguration{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, out); err != nil {
		return nil, err
	}
	return out, nil
}

func FromNetworkConfiguration(nc *vitistackcrdsv1alpha1.NetworkConfiguration) (*unstructured.Unstructured, error) {
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(nc)
	if err != nil {
		return nil, err
	}
	u := &unstructured.Unstructured{Object: m}
	u.SetAPIVersion("vitistack.io/v1alpha1")
	u.SetKind("NetworkConfiguration")
	return u, nil
}

// NetworkNamespace
func ToNetworkNamespace(u *unstructured.Unstructured) (*vitistackcrdsv1alpha1.NetworkNamespace, error) {
	out := &vitistackcrdsv1alpha1.NetworkNamespace{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, out); err != nil {
		return nil, err
	}
	return out, nil
}

func FromNetworkNamespace(nn *vitistackcrdsv1alpha1.NetworkNamespace) (*unstructured.Unstructured, error) {
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(nn)
	if err != nil {
		return nil, err
	}
	u := &unstructured.Unstructured{Object: m}
	u.SetAPIVersion("vitistack.io/v1alpha1")
	u.SetKind("NetworkNamespace")
	return u, nil
}
