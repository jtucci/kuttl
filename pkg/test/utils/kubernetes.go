package utils

// Contains methods helpful for interacting with and manipulating Kubernetes resources from YAML.

import (
	"bufio"
	"bytes"
	"context"
	ejson "encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/shlex"
	"github.com/pmezard/go-difflib/difflib"
	"github.com/spf13/pflag"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	batchv1beta1 "k8s.io/api/batch/v1beta1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer/json"
	"k8s.io/apimachinery/pkg/types"
	apijson "k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/discovery"
	fakediscovery "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth" // package needed for auth providers like GCP
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	coretesting "k8s.io/client-go/testing"
	api "k8s.io/client-go/tools/clientcmd/api/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/kudobuilder/kuttl/pkg/apis"
	harness "github.com/kudobuilder/kuttl/pkg/apis/testharness/v1beta1"
	"github.com/kudobuilder/kuttl/pkg/env"
)

// ensure that we only add to the scheme once.
var schemeLock sync.Once

// APIServerDefaultArgs are copied from the internal controller-runtime pkg/internal/testing/integration/internal/apiserver.go
// sadly, we can't import them anymore since it is an internal package
var APIServerDefaultArgs = []string{
	"--advertise-address=127.0.0.1",
	"--etcd-servers={{ if .EtcdURL }}{{ .EtcdURL.String }}{{ end }}",
	"--cert-dir={{ .CertDir }}",
	"--insecure-port={{ if .URL }}{{ .URL.Port }}{{ end }}",
	"--insecure-bind-address={{ if .URL }}{{ .URL.Hostname }}{{ end }}",
	"--secure-port={{ if .SecurePort }}{{ .SecurePort }}{{ end }}",
	"--disable-admission-plugins=ServiceAccount,NamespaceLifecycle",
	"--service-cluster-ip-range=10.0.0.0/24",
	"--advertise-address={{ if .URL }}{{ .URL.Hostname }}{{ end }}",
}

//TODO (kensipe): need to consider options around AlwaysAdmin https://github.com/kudobuilder/kudo/pull/1420/files#r391449597

// IsJSONSyntaxError returns true if the error is a JSON syntax error.
func IsJSONSyntaxError(err error) bool {
	_, ok := err.(*ejson.SyntaxError)
	return ok
}

// ValidateErrors accepts an error as its first argument and passes it to each function in the errValidationFuncs slice,
// if any of the methods returns true, the method returns nil, otherwise it returns the original error.
func ValidateErrors(err error, errValidationFuncs ...func(error) bool) error {
	for _, errFunc := range errValidationFuncs {
		if errFunc(err) {
			return nil
		}
	}

	return err
}

// Retry retries a method until the context expires or the method returns an unvalidated error.
func Retry(ctx context.Context, fn func(context.Context) error, errValidationFuncs ...func(error) bool) error {
	var lastErr error
	errCh := make(chan error)
	doneCh := make(chan struct{})

	// do { } while (err != nil): https://stackoverflow.com/a/32844744/10892393
	for ok := true; ok; ok = lastErr != nil {
		// run the function in a goroutine and close it once it is finished so that
		// we can use select to wait for both the function return and the context deadline.

		go func() {
			if err := fn(ctx); err != nil {
				errCh <- err
			} else {
				doneCh <- struct{}{}
			}
		}()

		select {
		// the callback finished
		case <-doneCh:
			lastErr = nil
		case err := <-errCh:
			// check if we tolerate the error, return it if not.
			if e := ValidateErrors(err, errValidationFuncs...); e != nil {
				return e
			}
			lastErr = err
		// timeout exceeded
		case <-ctx.Done():
			if lastErr == nil {
				// there's no previous error, so just return the timeout error
				return ctx.Err()
			}

			// return the most recent error
			return lastErr
		}
	}

	return lastErr
}

// RetryClient implements the Client interface, with retries built in.
type RetryClient struct {
	Client    client.Client
	dynamic   dynamic.Interface
	discovery discovery.DiscoveryInterface
}

// RetryStatusWriter implements the StatusWriter interface, with retries built in.
type RetryStatusWriter struct {
	StatusWriter client.StatusWriter
}

// NewRetryClient initializes a new Kubernetes client that automatically retries on network-related errors.
func NewRetryClient(cfg *rest.Config, opts client.Options) (*RetryClient, error) {
	dynamicClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}

	discovery, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, err
	}

	if opts.Mapper == nil {
		opts.Mapper, err = apiutil.NewDynamicRESTMapper(cfg)
		if err != nil {
			return nil, err
		}
	}

	client, err := client.New(cfg, opts)
	return &RetryClient{Client: client, dynamic: dynamicClient, discovery: discovery}, err
}

// Create saves the object obj in the Kubernetes cluster.
func (r *RetryClient) Create(ctx context.Context, obj runtime.Object, opts ...client.CreateOption) error {
	return Retry(ctx, func(ctx context.Context) error {
		return r.Client.Create(ctx, obj, opts...)
	}, IsJSONSyntaxError)
}

// Delete deletes the given obj from Kubernetes cluster.
func (r *RetryClient) Delete(ctx context.Context, obj runtime.Object, opts ...client.DeleteOption) error {
	return Retry(ctx, func(ctx context.Context) error {
		return r.Client.Delete(ctx, obj, opts...)
	}, IsJSONSyntaxError)
}

// DeleteAllOf deletes the given obj from Kubernetes cluster.
func (r *RetryClient) DeleteAllOf(ctx context.Context, obj runtime.Object, opts ...client.DeleteAllOfOption) error {
	return Retry(ctx, func(ctx context.Context) error {
		return r.Client.DeleteAllOf(ctx, obj, opts...)
	}, IsJSONSyntaxError)
}

// Update updates the given obj in the Kubernetes cluster. obj must be a
// struct pointer so that obj can be updated with the content returned by the Server.
func (r *RetryClient) Update(ctx context.Context, obj runtime.Object, opts ...client.UpdateOption) error {
	return Retry(ctx, func(ctx context.Context) error {
		return r.Client.Update(ctx, obj, opts...)
	}, IsJSONSyntaxError)
}

// Patch patches the given obj in the Kubernetes cluster. obj must be a
// struct pointer so that obj can be updated with the content returned by the Server.
func (r *RetryClient) Patch(ctx context.Context, obj runtime.Object, patch client.Patch, opts ...client.PatchOption) error {
	return Retry(ctx, func(ctx context.Context) error {
		return r.Client.Patch(ctx, obj, patch, opts...)
	}, IsJSONSyntaxError)
}

// Get retrieves an obj for the given object key from the Kubernetes Cluster.
// obj must be a struct pointer so that obj can be updated with the response
// returned by the Server.
func (r *RetryClient) Get(ctx context.Context, key client.ObjectKey, obj runtime.Object) error {
	return Retry(ctx, func(ctx context.Context) error {
		return r.Client.Get(ctx, key, obj)
	}, IsJSONSyntaxError)
}

// List retrieves list of objects for a given namespace and list options. On a
// successful call, Items field in the list will be populated with the
// result returned from the server.
func (r *RetryClient) List(ctx context.Context, list runtime.Object, opts ...client.ListOption) error {
	return Retry(ctx, func(ctx context.Context) error {
		return r.Client.List(ctx, list, opts...)
	}, IsJSONSyntaxError)
}

// Watch watches a specific object and returns all events for it.
func (r *RetryClient) Watch(ctx context.Context, obj runtime.Object) (watch.Interface, error) {
	meta, err := meta.Accessor(obj)
	if err != nil {
		return nil, err
	}

	gvk := obj.GetObjectKind().GroupVersionKind()

	groupResources, err := restmapper.GetAPIGroupResources(r.discovery)
	if err != nil {
		return nil, err
	}

	mapping, err := restmapper.NewDiscoveryRESTMapper(groupResources).RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return nil, err
	}

	return r.dynamic.Resource(mapping.Resource).Watch(context.TODO(), metav1.SingleObject(metav1.ObjectMeta{
		Name:      meta.GetName(),
		Namespace: meta.GetNamespace(),
	}))
}

// Status returns a client which can update status subresource for kubernetes objects.
func (r *RetryClient) Status() client.StatusWriter {
	return &RetryStatusWriter{
		StatusWriter: r.Client.Status(),
	}
}

// Update updates the given obj in the Kubernetes cluster. obj must be a
// struct pointer so that obj can be updated with the content returned by the Server.
func (r *RetryStatusWriter) Update(ctx context.Context, obj runtime.Object, opts ...client.UpdateOption) error {
	return Retry(ctx, func(ctx context.Context) error {
		return r.StatusWriter.Update(ctx, obj, opts...)
	}, IsJSONSyntaxError)
}

// Patch patches the given obj in the Kubernetes cluster. obj must be a
// struct pointer so that obj can be updated with the content returned by the Server.
func (r *RetryStatusWriter) Patch(ctx context.Context, obj runtime.Object, patch client.Patch, opts ...client.PatchOption) error {
	return Retry(ctx, func(ctx context.Context) error {
		return r.StatusWriter.Patch(ctx, obj, patch, opts...)
	}, IsJSONSyntaxError)
}

// Scheme returns an initialized Kubernetes Scheme.
func Scheme() *runtime.Scheme {
	schemeLock.Do(func() {
		if err := apis.AddToScheme(scheme.Scheme); err != nil {
			fmt.Printf("failed to add API resources to the scheme: %v", err)
			os.Exit(-1)
		}
		if err := apiextensions.AddToScheme(scheme.Scheme); err != nil {
			fmt.Printf("failed to add API extension resources to the scheme: %v", err)
			os.Exit(-1)
		}
	})

	return scheme.Scheme
}

// ResourceID returns a human readable identifier indicating the object kind, name, and namespace.
func ResourceID(obj runtime.Object) string {
	m, err := meta.Accessor(obj)
	if err != nil {
		return ""
	}

	gvk := obj.GetObjectKind().GroupVersionKind()

	return fmt.Sprintf("%s:%s/%s", gvk.Kind, m.GetNamespace(), m.GetName())
}

// Namespaced sets the namespace on an object to namespace, if it is a namespace scoped resource.
// If the resource is cluster scoped, then it is ignored and the namespace is not set.
// If it is a namespaced resource and a namespace is already set, then the namespace is unchanged.
func Namespaced(dClient discovery.DiscoveryInterface, obj runtime.Object, namespace string) (string, string, error) {
	m, err := meta.Accessor(obj)
	if err != nil {
		return "", "", err
	}

	if m.GetNamespace() != "" {
		return m.GetName(), m.GetNamespace(), nil
	}

	resource, err := GetAPIResource(dClient, obj.GetObjectKind().GroupVersionKind())
	if err != nil {

		return "", "", fmt.Errorf("retrieving API resource for %v failed: %v", obj.GetObjectKind().GroupVersionKind(), err)
	}

	if !resource.Namespaced {
		return m.GetName(), "", nil
	}

	m.SetNamespace(namespace)
	return m.GetName(), namespace, nil
}

// PrettyDiff creates a unified diff highlighting the differences between two Kubernetes resources
func PrettyDiff(expected runtime.Object, actual runtime.Object) (string, error) {
	expectedBuf := &bytes.Buffer{}
	actualBuf := &bytes.Buffer{}

	if err := MarshalObject(expected, expectedBuf); err != nil {
		return "", err
	}

	if err := MarshalObject(actual, actualBuf); err != nil {
		return "", err
	}

	diffed := difflib.UnifiedDiff{
		A:        difflib.SplitLines(expectedBuf.String()),
		B:        difflib.SplitLines(actualBuf.String()),
		FromFile: ResourceID(expected),
		ToFile:   ResourceID(actual),
		Context:  3,
	}

	return difflib.GetUnifiedDiffString(diffed)
}

// ConvertUnstructured converts an unstructured object to the known struct. If the type is not known, then
// the unstructured object is returned unmodified.
func ConvertUnstructured(in runtime.Object) (runtime.Object, error) {
	unstruct, err := runtime.DefaultUnstructuredConverter.ToUnstructured(in)
	if err != nil {
		return nil, fmt.Errorf("error converting %s to unstructured error: %w", ResourceID(in), err)
	}

	var converted runtime.Object

	kind := in.GetObjectKind().GroupVersionKind().Kind
	group := in.GetObjectKind().GroupVersionKind().Group

	// Deprecated: kudo.dev is deprecated.
	kudoGroup := "kudo.dev"
	kuttlGroup := "kuttl.dev"
	if (group == kudoGroup || group == kuttlGroup) && kind == "TestStep" {
		converted = &harness.TestStep{}
	} else if (group == kudoGroup || group == kuttlGroup) && kind == "TestAssert" {
		converted = &harness.TestAssert{}
	} else if (group == kudoGroup || group == kuttlGroup) && kind == "TestSuite" {
		converted = &harness.TestSuite{}
	} else {
		return in, nil
	}

	err = runtime.DefaultUnstructuredConverter.FromUnstructured(unstruct, converted)
	if err != nil {
		return nil, fmt.Errorf("error converting %s from unstructured error: %w", ResourceID(in), err)
	}

	return converted, nil
}

// PatchObject updates expected with the Resource Version from actual.
// In the future, PatchObject may perform a strategic merge of actual into expected.
func PatchObject(actual, expected runtime.Object) error {
	actualMeta, err := meta.Accessor(actual)
	if err != nil {
		return err
	}

	expectedMeta, err := meta.Accessor(expected)
	if err != nil {
		return err
	}

	expectedMeta.SetResourceVersion(actualMeta.GetResourceVersion())
	return nil
}

// CleanObjectForMarshalling removes unnecessary object metadata that should not be included in serialization and diffs.
func CleanObjectForMarshalling(o runtime.Object) (runtime.Object, error) {
	copied := o.DeepCopyObject()

	meta, err := meta.Accessor(copied)
	if err != nil {
		return nil, err
	}

	meta.SetResourceVersion("")
	meta.SetCreationTimestamp(metav1.Time{})
	meta.SetSelfLink("")
	meta.SetUID(types.UID(""))
	meta.SetGeneration(0)

	annotations := meta.GetAnnotations()
	delete(annotations, "deployment.kubernetes.io/revision")

	if len(annotations) > 0 {
		meta.SetAnnotations(annotations)
	} else {
		meta.SetAnnotations(nil)
	}

	return copied, nil
}

// MarshalObject marshals a Kubernetes object to a YAML string.
func MarshalObject(o runtime.Object, w io.Writer) error {
	copied, err := CleanObjectForMarshalling(o)
	if err != nil {
		return err
	}

	return json.NewYAMLSerializer(json.DefaultMetaFactory, nil, nil).Encode(copied, w)
}

// MarshalObjectJSON marshals a Kubernetes object to a JSON string.
func MarshalObjectJSON(o runtime.Object, w io.Writer) error {
	copied, err := CleanObjectForMarshalling(o)
	if err != nil {
		return err
	}

	return json.NewSerializer(json.DefaultMetaFactory, nil, nil, false).Encode(copied, w)
}

// LoadYAMLFromFile loads all objects from a YAML file.
func LoadYAMLFromFile(path string) ([]runtime.Object, error) {
	opened, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer opened.Close()

	return LoadYAML(path, opened)
}

func LoadYAML(path string, r io.Reader) ([]runtime.Object, error) {
	yamlReader := yaml.NewYAMLReader(bufio.NewReader(r))

	objects := []runtime.Object{}

	for {
		data, err := yamlReader.Read()
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("error reading yaml %s: %w", path, err)
		}

		unstructuredObj := &unstructured.Unstructured{}
		decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewBuffer(data), len(data))

		if err = decoder.Decode(unstructuredObj); err != nil {
			return nil, fmt.Errorf("error decoding yaml %s: %w", path, err)
		}

		obj, err := ConvertUnstructured(unstructuredObj)
		if err != nil {
			return nil, fmt.Errorf("error converting unstructured object %s (%s): %w", ResourceID(unstructuredObj), path, err)
		}
		// discovered reader will return empty objects if a number of lines are preceding a yaml separator (---)
		// this detects that, logs and continues
		if obj.GetObjectKind().GroupVersionKind().Kind == "" {
			log.Println("object detected with no GVK Kind for path", path)
		} else {
			objects = append(objects, obj)
		}

	}

	return objects, nil

}

// MatchesKind returns true if the Kubernetes kind of obj matches any of kinds.
func MatchesKind(obj runtime.Object, kinds ...runtime.Object) bool {
	gvk := obj.GetObjectKind().GroupVersionKind()

	for _, kind := range kinds {
		if kind.GetObjectKind().GroupVersionKind() == gvk {
			return true
		}
	}

	return false
}

// InstallManifests recurses over ManifestsDir to install all resources defined in YAML manifests.
func InstallManifests(ctx context.Context, client client.Client, dClient discovery.DiscoveryInterface, manifestsDir string, kinds ...runtime.Object) ([]runtime.Object, error) {
	objects := []runtime.Object{}

	if manifestsDir == "" {
		return objects, nil
	}

	return objects, filepath.Walk(manifestsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.IsDir() {
			return nil
		}

		extensions := map[string]bool{
			".yaml": true,
			".yml":  true,
			".json": true,
		}
		if !extensions[filepath.Ext(path)] {
			return nil
		}

		objs, err := LoadYAMLFromFile(path)
		if err != nil {
			return err
		}

		for _, obj := range objs {
			if len(kinds) > 0 && !MatchesKind(obj, kinds...) {
				continue
			}

			objectKey := ObjectKey(obj)
			if objectKey.Namespace == "" {
				if _, _, err := Namespaced(dClient, obj, "default"); err != nil {
					return err
				}
			}

			updated, err := CreateOrUpdate(ctx, client, obj, true)
			if err != nil {
				return fmt.Errorf("error creating resource %s: %w", ResourceID(obj), err)
			}

			action := "created"
			if updated {
				action = "updated"
			}
			// TODO: use test logger instead of Go logger
			log.Println(ResourceID(obj), action)

			objects = append(objects, obj)
		}

		return nil
	})
}

// ObjectKey returns an instantiated ObjectKey for the provided object.
func ObjectKey(obj runtime.Object) client.ObjectKey {
	m, _ := meta.Accessor(obj)
	return client.ObjectKey{
		Name:      m.GetName(),
		Namespace: m.GetNamespace(),
	}
}

// NewResource generates a Kubernetes object using the provided apiVersion, kind, name, and namespace.
func NewResource(apiVersion, kind, name, namespace string) runtime.Object {
	meta := map[string]interface{}{
		"name": name,
	}

	if namespace != "" {
		meta["namespace"] = namespace
	}

	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": apiVersion,
			"kind":       kind,
			"metadata":   meta,
		},
	}
}

// NewClusterRoleBinding Create a clusterrolebinding for the serviceAccount passed
func NewClusterRoleBinding(apiVersion, kind, name, namespace string, serviceAccount string, roleName string) runtime.Object {

	sa := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     roleName,
		},
		Subjects: []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      serviceAccount,
			Namespace: namespace,
		}},
	}

	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": apiVersion,
			"kind":       kind,
			"metadata":   sa.ObjectMeta,
			"subjects":   sa.Subjects,
			"roleRef":    sa.RoleRef,
		},
	}
}

// NewPod creates a new pod object.
func NewPod(name, namespace string) runtime.Object {
	return NewResource("v1", "Pod", name, namespace)
}

// WithNamespace naively applies the namespace to the object. Used mainly in tests, otherwise
// use Namespaced.
func WithNamespace(obj runtime.Object, namespace string) runtime.Object {
	obj = obj.DeepCopyObject()

	m, _ := meta.Accessor(obj)
	m.SetNamespace(namespace)

	return obj
}

// WithSpec applies the provided spec to the Kubernetes object.
func WithSpec(t *testing.T, obj runtime.Object, spec map[string]interface{}) runtime.Object {
	res, err := WithKeyValue(obj, "spec", spec)
	if err != nil {
		t.Fatalf("failed to apply spec %v to object %v: %v", spec, obj, err)
	}
	return res
}

// WithStatus applies the provided status to the Kubernetes object.
func WithStatus(t *testing.T, obj runtime.Object, status map[string]interface{}) runtime.Object {
	res, err := WithKeyValue(obj, "status", status)
	if err != nil {
		t.Fatalf("failed to apply status %v to object %v: %v", status, obj, err)
	}
	return res
}

// WithKeyValue sets key in the provided object to value.
func WithKeyValue(obj runtime.Object, key string, value map[string]interface{}) (runtime.Object, error) {
	obj = obj.DeepCopyObject()

	content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return nil, err
	}

	content[key] = value

	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(content, obj); err != nil {
		return nil, err
	}
	return obj, nil
}

// WithLabels sets the labels on an object.
func WithLabels(t *testing.T, obj runtime.Object, labels map[string]string) runtime.Object {
	obj = obj.DeepCopyObject()

	m, err := meta.Accessor(obj)
	if err != nil {
		t.Fatalf("failed to apply labels %v to object %v: %v", labels, obj, err)
	}
	m.SetLabels(labels)

	return obj
}

// WithAnnotations sets the annotations on an object.
func WithAnnotations(obj runtime.Object, annotations map[string]string) runtime.Object {
	obj = obj.DeepCopyObject()

	m, _ := meta.Accessor(obj)
	m.SetAnnotations(annotations)

	return obj
}

// FakeDiscoveryClient returns a fake discovery client that is populated with some types for use in
// unit tests.
func FakeDiscoveryClient() discovery.DiscoveryInterface {
	return &fakediscovery.FakeDiscovery{
		Fake: &coretesting.Fake{
			Resources: []*metav1.APIResourceList{
				{
					GroupVersion: corev1.SchemeGroupVersion.String(),
					APIResources: []metav1.APIResource{
						{Name: "pod", Namespaced: true, Kind: "Pod"},
						{Name: "namespace", Namespaced: false, Kind: "Namespace"},
						{Name: "service", Namespaced: true, Kind: "Service"},
					},
				},
				{
					GroupVersion: appsv1.SchemeGroupVersion.String(),
					APIResources: []metav1.APIResource{
						{Name: "statefulset", Namespaced: true, Kind: "StatefulSet"},
						{Name: "deployment", Namespaced: true, Kind: "Deployment"},
					},
				},
				{
					GroupVersion: batchv1.SchemeGroupVersion.String(),
					APIResources: []metav1.APIResource{
						{Name: "job", Namespaced: true, Kind: "Job"},
					},
				},
				{
					GroupVersion: batchv1beta1.SchemeGroupVersion.String(),
					APIResources: []metav1.APIResource{
						{Name: "job", Namespaced: true, Kind: "CronJob"},
					},
				},
				{
					GroupVersion: apiextensions.SchemeGroupVersion.String(),
					APIResources: []metav1.APIResource{
						{Name: "customresourcedefinitions", Namespaced: false, Kind: "CustomResourceDefinition"},
					},
				},
			},
		},
	}
}

// CreateOrUpdate will create obj if it does not exist and update if it it does.
// retryonerror indicates whether we retry in case of conflict
// Returns true if the object was updated and false if it was created.
func CreateOrUpdate(ctx context.Context, cl client.Client, obj runtime.Object, retryOnError bool) (updated bool, err error) {
	orig := obj.DeepCopyObject()

	validators := []func(err error) bool{k8serrors.IsAlreadyExists}

	if retryOnError {
		validators = append(validators, k8serrors.IsConflict)
	}

	return updated, Retry(ctx, func(ctx context.Context) error {
		expected := orig.DeepCopyObject()
		actual := orig.DeepCopyObject()

		err := cl.Get(ctx, ObjectKey(actual), actual)
		if err == nil {
			if err = PatchObject(actual, expected); err != nil {
				return err
			}

			var expectedBytes []byte
			expectedBytes, err = apijson.Marshal(expected)
			if err != nil {
				return err
			}

			err = cl.Patch(ctx, actual, client.RawPatch(types.MergePatchType, expectedBytes))
			updated = true
		} else if k8serrors.IsNotFound(err) {
			err = cl.Create(ctx, obj)
			updated = false
		}
		return err
	}, validators...)
}

// SetAnnotation sets the given key and value in the object's annotations, returning a copy.
func SetAnnotation(obj runtime.Object, key, value string) runtime.Object {
	obj = obj.DeepCopyObject()

	meta, _ := meta.Accessor(obj)

	annotations := meta.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[key] = value
	meta.SetAnnotations(annotations)

	return obj
}

// GetAPIResource returns the APIResource object for a specific GroupVersionKind.
func GetAPIResource(dClient discovery.DiscoveryInterface, gvk schema.GroupVersionKind) (metav1.APIResource, error) {
	resourceTypes, err := dClient.ServerResourcesForGroupVersion(gvk.GroupVersion().String())
	if err != nil {
		return metav1.APIResource{}, err
	}

	for _, resource := range resourceTypes.APIResources {
		if !strings.EqualFold(resource.Kind, gvk.Kind) {
			continue
		}

		return resource, nil
	}

	return metav1.APIResource{}, errors.New("resource type not found")
}

// WaitForDelete waits for the provide runtime objects to be deleted from cluster
func WaitForDelete(c *RetryClient, objs []runtime.Object) error {
	// Wait for resources to be deleted.
	return wait.PollImmediate(100*time.Millisecond, 10*time.Second, func() (done bool, err error) {
		for _, obj := range objs {
			err = c.Get(context.TODO(), ObjectKey(obj), obj.DeepCopyObject())
			if err == nil || !k8serrors.IsNotFound(err) {
				return false, err
			}
		}

		return true, nil
	})
}

// WaitForCRDs waits for the provided CRD types to be available in the Kubernetes API.
func WaitForCRDs(dClient discovery.DiscoveryInterface, crds []runtime.Object) error {
	waitingFor := []schema.GroupVersionKind{}
	crdKind := NewResource("apiextensions.k8s.io/v1beta1", "CustomResourceDefinition", "", "")

	for _, crdObj := range crds {
		if !MatchesKind(crdObj, crdKind) {
			return fmt.Errorf("the following passed object does not match %v: %v", crdKind, crdObj)
		}

		switch crd := crdObj.(type) {
		case *apiextensions.CustomResourceDefinition:
			waitingFor = append(waitingFor, schema.GroupVersionKind{
				Group:   crd.Spec.Group,
				Version: crd.Spec.Version,
				Kind:    crd.Spec.Names.Kind,
			})
		case *unstructured.Unstructured:
			waitingFor = append(waitingFor, schema.GroupVersionKind{
				Group:   crd.Object["spec"].(map[string]interface{})["group"].(string),
				Version: crd.Object["spec"].(map[string]interface{})["version"].(string),
				Kind:    crd.Object["spec"].(map[string]interface{})["names"].(map[string]interface{})["kind"].(string),
			})
		default:
			return fmt.Errorf("the folllowing passed object is not a CRD: %v", crdObj)
		}
	}

	return wait.PollImmediate(100*time.Millisecond, 10*time.Second, func() (done bool, err error) {
		for _, resource := range waitingFor {
			_, err := GetAPIResource(dClient, resource)
			if err != nil {
				fmt.Printf("Waiting for resource %s... \n", resource)
				return false, nil
			}
		}

		return true, nil
	})
}

// Client is the controller-runtime Client interface with an added Watch method.
type Client interface {
	client.Client
	// Watch watches a specific object and returns all events for it.
	Watch(ctx context.Context, obj runtime.Object) (watch.Interface, error)
}

// TestEnvironment is a struct containing the envtest environment, Kubernetes config and clients.
type TestEnvironment struct {
	Environment     *envtest.Environment
	Config          *rest.Config
	Client          Client
	DiscoveryClient discovery.DiscoveryInterface
}

// StartTestEnvironment is a wrapper for controller-runtime's envtest that creates a Kubernetes API server and etcd
// suitable for use in tests.
func StartTestEnvironment(KubeAPIServerFlags []string) (env TestEnvironment, err error) {
	env.Environment = &envtest.Environment{
		KubeAPIServerFlags: KubeAPIServerFlags,
	}

	env.Config, err = env.Environment.Start()

	if err != nil {
		return
	}

	env.Client, err = NewRetryClient(env.Config, client.Options{})
	if err != nil {
		return
	}

	env.DiscoveryClient, err = discovery.NewDiscoveryClientForConfig(env.Config)
	return
}

// GetArgs parses a command line string into its arguments and appends a namespace if it is not already set.
func GetArgs(ctx context.Context, cmd harness.Command, namespace string, envMap map[string]string) (*exec.Cmd, error) {
	argSlice := []string{}

	if cmd.Command != "" && cmd.Script != "" {
		return nil, errors.New("command and script can not be set in the same configuration")
	}
	if cmd.Command == "" && cmd.Script == "" {
		return nil, errors.New("command or script must be set")
	}
	if cmd.Script != "" && cmd.Namespaced {
		return nil, errors.New("script can not used 'namespaced', use the $NAMESPACE environment variable instead")
	}

	if cmd.Script != "" {
		builtCmd := exec.CommandContext(ctx, "sh", "-c", cmd.Script)
		return builtCmd, nil
	}
	c := env.ExpandWithMap(cmd.Command, envMap)

	argSplit, err := shlex.Split(c)
	if err != nil {
		return nil, err
	}

	argSlice = append(argSlice, argSplit...)

	if cmd.Namespaced {
		fs := pflag.NewFlagSet("", pflag.ContinueOnError)
		fs.ParseErrorsWhitelist.UnknownFlags = true

		namespaceParsed := fs.StringP("namespace", "n", "", "")
		if err := fs.Parse(argSplit); err != nil {
			return nil, err
		}

		if *namespaceParsed == "" {
			argSlice = append(argSlice, "--namespace", namespace)
		}
	}

	//nolint:gosec // We're running a user provided command. This is insecure by definition
	builtCmd := exec.CommandContext(ctx, argSlice[0])
	builtCmd.Args = argSlice
	return builtCmd, nil
}

// RunCommand runs a command with args.
// args gets split on spaces (respecting quoted strings).
// if the command is run in the background a reference to the process is returned for later cleanup
func RunCommand(ctx context.Context, namespace string, cmd harness.Command, cwd string, stdout io.Writer, stderr io.Writer, logger Logger, timeout int) (*exec.Cmd, error) {
	actualDir, err := os.Getwd()
	if err != nil {
		return nil, err
	}

	kudoENV := make(map[string]string)
	kudoENV["NAMESPACE"] = namespace
	kudoENV["KUBECONFIG"] = fmt.Sprintf("%s/kubeconfig", actualDir)
	kudoENV["PATH"] = fmt.Sprintf("%s/bin/:%s", actualDir, os.Getenv("PATH"))

	// by default testsuite timeout is the command timeout
	// 0 is allowed for testsuite which means forever (or no timeout)
	// cmd.timeout defaults to 0 and is NOT and explicit override to mean forever.
	// using a negative value for cmd.timeout is an override of testsuite.timeout to mean forever
	// if testsuite.timeout is set,  set cmd.timeout = -1 (means no timeout), 0 (default) means using testsuite.timeout, and anything else is an override of time.
	// if testsuite.timeout = 0, cmd.timeout -1 and 0 means forever
	if cmd.Timeout < 0 {
		// negative is always forever
		timeout = 0
	}
	if cmd.Timeout > 0 {
		timeout = cmd.Timeout
	}

	// command context is provided context or a cancel context but only from cmds that are not background
	cmdCtx := ctx
	if timeout > 0 && !cmd.Background {
		var cancel context.CancelFunc
		cmdCtx, cancel = context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
		defer cancel()
	}

	builtCmd, err := GetArgs(cmdCtx, cmd, namespace, kudoENV)
	if err != nil {
		return nil, err
	}

	logger.Logf("running command: %v", builtCmd.Args)

	builtCmd.Dir = cwd
	if !cmd.SkipLogOutput {
		builtCmd.Stdout = stdout
		builtCmd.Stderr = stderr
	}
	builtCmd.Env = os.Environ()
	for key, value := range kudoENV {
		builtCmd.Env = append(builtCmd.Env, fmt.Sprintf("%s=%s", key, value))
	}

	// process started and exited with error
	var exerr *exec.ExitError
	err = builtCmd.Start()
	if err != nil {
		if errors.As(err, &exerr) && cmd.IgnoreFailure {
			return nil, nil
		}
		return nil, err
	}

	if cmd.Background {
		return builtCmd, nil
	}

	err = builtCmd.Wait()
	if errors.As(err, &exerr) && cmd.IgnoreFailure {
		return nil, nil
	}
	if errors.Is(cmdCtx.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("command %q exceeded %v sec timeout", cmd.Command, timeout)
	}
	return nil, err
}

// RunCommands runs a set of commands, returning any errors.
// If `command` is set, then `command` will be the command that is invoked (if a command specifies it already, it will not be prepended again).
// commands running in the background are returned
func RunCommands(logger Logger, namespace string, commands []harness.Command, workdir string, timeout int) ([]*exec.Cmd, []error) {
	errs := []error{}
	bgs := []*exec.Cmd{}

	if commands == nil {
		return nil, nil
	}

	for _, cmd := range commands {
		logger.Logf("running command: %q", cmd.Command)

		bg, err := RunCommand(context.Background(), namespace, cmd, workdir, logger, logger, logger, timeout)
		if err != nil {
			errs = append(errs, err)
		}
		if bg != nil {
			bgs = append(bgs, bg)
		} else {
			// We only need to flush if this is not a background command
			logger.Flush()
		}
	}

	if len(bgs) > 0 {
		logger.Log("background processes", bgs)
	}
	// handling of errs and bg processes external to this function
	return bgs, errs
}

// Kubeconfig converts a rest.Config into a YAML kubeconfig and writes it to w
func Kubeconfig(cfg *rest.Config, w io.Writer) error {
	var authProvider *api.AuthProviderConfig
	var execConfig *api.ExecConfig
	if cfg.AuthProvider != nil {
		authProvider = &api.AuthProviderConfig{
			Name:   cfg.AuthProvider.Name,
			Config: cfg.AuthProvider.Config,
		}
	}

	if cfg.ExecProvider != nil {
		execConfig = &api.ExecConfig{
			Command:    cfg.ExecProvider.Command,
			Args:       cfg.ExecProvider.Args,
			APIVersion: cfg.ExecProvider.APIVersion,
			Env:        []api.ExecEnvVar{},
		}

		for _, envVar := range cfg.ExecProvider.Env {
			execConfig.Env = append(execConfig.Env, api.ExecEnvVar{
				Name:  envVar.Name,
				Value: envVar.Value,
			})
		}
	}
	err := rest.LoadTLSFiles(cfg)
	if err != nil {
		return err
	}
	return json.NewYAMLSerializer(json.DefaultMetaFactory, nil, nil).Encode(&api.Config{
		CurrentContext: "cluster",
		Clusters: []api.NamedCluster{
			{
				Name: "cluster",
				Cluster: api.Cluster{
					Server:                   cfg.Host,
					CertificateAuthorityData: cfg.TLSClientConfig.CAData,
					InsecureSkipTLSVerify:    cfg.TLSClientConfig.Insecure,
				},
			},
		},
		Contexts: []api.NamedContext{
			{
				Name: "cluster",
				Context: api.Context{
					Cluster:  "cluster",
					AuthInfo: "user",
				},
			},
		},
		AuthInfos: []api.NamedAuthInfo{
			{
				Name: "user",
				AuthInfo: api.AuthInfo{
					ClientCertificateData: cfg.TLSClientConfig.CertData,
					ClientKeyData:         cfg.TLSClientConfig.KeyData,
					Token:                 cfg.BearerToken,
					Username:              cfg.Username,
					Password:              cfg.Password,
					Impersonate:           cfg.Impersonate.UserName,
					ImpersonateGroups:     cfg.Impersonate.Groups,
					ImpersonateUserExtra:  cfg.Impersonate.Extra,
					AuthProvider:          authProvider,
					Exec:                  execConfig,
				},
			},
		},
	}, w)
}

func GetDiscoveryClient(mgr manager.Manager) (*discovery.DiscoveryClient, error) {
	// use manager rest config to create a discovery client
	dc, err := discovery.NewDiscoveryClientForConfig(mgr.GetConfig())
	if err != nil || dc == nil {
		return nil, fmt.Errorf("failed to create a discovery client: %v", err)
	}

	return dc, nil
}
