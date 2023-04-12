/*
 * © 2023 Snyk Limited
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */
package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	promclient "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/snyk/kubernetes-scanner/internal/config"
)

var timeNow time.Time

func init() {
	var err error
	timeNow, err = time.Parse(time.RFC3339, "2023-02-20T16:41:17Z")
	if err != nil {
		panic("could not parse time!")
	}

	now = func() time.Time {
		return timeNow
	}
}

const testToken = "my-super-secret-token"

func TestBackend(t *testing.T) {
	const orgID = "org-123"
	ctx := context.Background()
	tu := testUpstream{t: t, preferredVersion: "v1", orgID: orgID, auth: testToken}
	ts := httptest.NewServer(http.HandlerFunc(tu.Handle))
	defer ts.Close()

	b := New("my-pet-cluster", &config.Egress{
		HTTPClientTimeout:       metav1.Duration{Duration: 1 * time.Second},
		SnykAPIBaseURL:          ts.URL,
		SnykServiceAccountToken: testToken,
	}, prometheus.NewPedanticRegistry())
	err := b.Upsert(ctx, "req-id", orgID, []KubeObj{
		{
			Obj:              pod,
			PreferredVersion: "v1",
			DeletedAt:        nil,
		},
	})
	require.NoError(t, err)

	tu.expectDeletion = true
	err = b.Upsert(ctx, "req-id", orgID, []KubeObj{
		{
			Obj:              pod,
			PreferredVersion: "v1",
			DeletedAt:        &metav1.Time{Time: now().Local()},
		},
	})
	require.NoError(t, err)

}

func TestBackendErrorHandling(t *testing.T) {
	const orgID = "org-123"
	ctx := context.Background()
	tu := testUpstream{t: t, preferredVersion: "v1", orgID: orgID, auth: testToken, statusCodeToReturn: 400}
	ts := httptest.NewServer(http.HandlerFunc(tu.Handle))
	defer ts.Close()

	b := New("my-pet-cluster", &config.Egress{
		HTTPClientTimeout:       metav1.Duration{Duration: 1 * time.Second},
		SnykAPIBaseURL:          ts.URL,
		SnykServiceAccountToken: testToken,
	}, prometheus.NewPedanticRegistry())

	err := b.Upsert(ctx, "req-id", orgID, []KubeObj{
		{
			Obj:              pod,
			PreferredVersion: "v1",
			DeletedAt:        nil,
		},
	})
	require.Error(t, err)
}

func TestMetricsFromBackend(t *testing.T) {
	const orgID = "org-123"
	ctx := context.Background()
	tu := testUpstream{t: t, preferredVersion: "v1", orgID: orgID, auth: testToken, statusCodeToReturn: 400}
	ts := httptest.NewServer(http.HandlerFunc(tu.Handle))
	defer ts.Close()

	b := New("my-pet-cluster", &config.Egress{
		HTTPClientTimeout:       metav1.Duration{Duration: 1 * time.Second},
		SnykAPIBaseURL:          ts.URL,
		SnykServiceAccountToken: testToken,
	}, prometheus.NewPedanticRegistry())

	err := b.Upsert(ctx, "req-id", orgID, []KubeObj{
		{
			Obj:              pod,
			PreferredVersion: "v1",
			DeletedAt:        nil,
		},
	})
	require.Error(t, err)
	require.Equal(t, uint8(1), b.failures[pod.UID].retries)
	require.Equal(t, 400, b.failures[pod.UID].code)

	tu.statusCodeToReturn = 0
	err = b.Upsert(ctx, "req-id", orgID, []KubeObj{
		{
			Obj:              pod,
			PreferredVersion: "v1",
			DeletedAt:        nil,
		},
	})
	require.NoError(t, err)
	_, ok := b.failures[pod.UID]
	require.False(t, ok)
}

type testUpstream struct {
	t                  *testing.T
	preferredVersion   string
	orgID              string
	expectDeletion     bool
	auth               string
	statusCodeToReturn int
}

func (tu *testUpstream) Handle(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "token "+tu.auth {
		http.Error(w, fmt.Sprintf("invalid authorization header provided: %v", r.Header.Get("Authorization")), 403)
		return
	}
	matches, err := path.Match(fmt.Sprintf("*/hidden/orgs/%s/kubernetes_resources", tu.orgID), r.URL.Path)
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid path, could not match: %v", err), 400)
		return
	}
	if !matches {
		http.Error(w, fmt.Sprintf("path does not match expectations: %v", r.URL.Path), 400)
		return
	}

	body, err := io.ReadAll(r.Body)
	defer r.Body.Close()
	if err != nil {
		http.Error(w, fmt.Sprintf("could not read body: %v", err), 400)
		return
	}

	req := request{}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, fmt.Sprintf("could not unmarshal body to JSON: %v", err), 400)
		return
	}

	if tu.statusCodeToReturn != 0 {
		http.Error(w, "an error occurred", tu.statusCodeToReturn)
		return
	}

	expected := []resource{{
		ManifestBlob:     pod,
		PreferredVersion: tu.preferredVersion,
		// when unmarshaling from JSON, metav1.Time also calls Local(), so we need to do too.
		ScannedAt: metav1.Time{Time: now().Local()},
	}}

	if tu.expectDeletion {
		expected[0].DeletedAt = &metav1.Time{Time: now().Local()}
	}

	require.Equal(tu.t, expected, req.Data.Attributes.Resources)
}

var pod = &corev1.Pod{
	ObjectMeta: metav1.ObjectMeta{
		Name:      "normal-pod",
		Namespace: "default",
		UID:       types.UID("21E430E3-FA43-45B9-B5EC-AE27FFA16D82"),
	},
	TypeMeta: metav1.TypeMeta{
		Kind:       "Pod",
		APIVersion: "v1",
	},
	Spec: corev1.PodSpec{
		Containers: []corev1.Container{{
			Name:  "bla",
			Image: "bla:latest",
		}},
	},
}

// UnmarshalJSON unmarshals the resource type. This needs a custom unmarshal function because the
// resource type contains a `client.Object` interface, which cannot be unmarshalled automatically.
func (r *resource) UnmarshalJSON(data []byte) error {
	// create a temporary type to avoid recursive calls to this method.
	type s resource
	tmp := s{
		// we simply set the manifestBlob to an unstructured object, which satisfies the
		// `client.Object` interface and allows us to extract the apiVersion & kind from it.
		ManifestBlob: &unstructured.Unstructured{},
	}
	if err := json.Unmarshal(data, &tmp); err != nil {
		return err
	}

	// figure out the actual type of the manifestBlob and add it to tmp so that we can do another
	// round of json unmarshaling.
	apiVersion, kind := tmp.ManifestBlob.GetObjectKind().GroupVersionKind().ToAPIVersionAndKind()
	switch {
	case kind == "Pod" && apiVersion == "v1":
		tmp.ManifestBlob = &corev1.Pod{}
	default:
		return fmt.Errorf("unknown APIVersion / Kind %v in %s", tmp, data)
	}

	if err := json.Unmarshal(data, &tmp); err != nil {
		return err
	}

	// the GVK is lost when unmarshalling / decoding objects, so we need to add it back.
	// https://github.com/kubernetes/client-go/issues/541#issuecomment-452312901
	tmp.ManifestBlob.GetObjectKind().SetGroupVersionKind(tmp.ManifestBlob.GetObjectKind().GroupVersionKind())
	*r = resource(tmp)
	return nil
}

func TestJSONMatches(t *testing.T) {
	b := New("my pet cluster", &config.Egress{}, prometheus.NewPedanticRegistry())
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "a-pod",
			Namespace: "default",
			UID:       types.UID("ADD9E4F4-5154-4261-81FD-F14A4358F772"),
		},
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Pod",
		},
	}
	r, err := b.newPostBody([]KubeObj{
		{
			Obj:              pod,
			PreferredVersion: "v1",
			DeletedAt:        nil,
		},
	})
	require.NoError(t, err)

	body, err := io.ReadAll(r)
	require.NoError(t, err)

	var prettyJSON bytes.Buffer
	err = json.Indent(&prettyJSON, body, "", "\t")
	require.NoError(t, err)

	const expectedJSON = `{
	"data": {
		"type": "kubernetes_resource",
		"attributes": {
			"cluster_name": "my pet cluster",
			"resources": [
				{
					"manifest_blob": {
						"kind": "Pod",
						"apiVersion": "v1",
						"metadata": {
							"name": "a-pod",
							"namespace": "default",
							"uid": "ADD9E4F4-5154-4261-81FD-F14A4358F772",
							"creationTimestamp": null
						},
						"spec": {
							"containers": null
						},
						"status": {}
					},
					"preferred_version": "v1",
					"scanned_at": "2023-02-20T16:41:17Z"
				}
			]
		}
	}
}`
	require.Equal(t, expectedJSON, prettyJSON.String())
}

func TestMetricsRetries(t *testing.T) {
	var (
		ctx          = context.Background()
		testFailures = map[types.UID]int{
			"a": 1, "b": 6, "c": 5,
			"d": 2, "e": 1, "f": 4,
			"g": 3, "h": 3, "i": 1,
			"j": 40, "k": 0, "l": 8,
		}
		// see the retriesBuckets var for the bucket values.
		// the actual values have been "calculated" manually.
		expectedBucketSizes = []uint64{3, 4, 6, 8, 10, 11}
		registry            = prometheus.NewPedanticRegistry()
		m                   = newMetrics(registry)
		failures            = make(chan types.UID)
		wg                  sync.WaitGroup
	)
	const metricName = "kubernetes_scanner_backend_retries"

	// we spawn some goroutines to recordFailures so that we can
	// make sure they're thread-safe.
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				uid, ok := <-failures
				if !ok {
					return
				}
				m.recordFailure(ctx, 404, uid)
			}
		}()
	}

	// send all the required failures into the channels
	var allFailuresDone bool
	for !allFailuresDone {
		allFailuresDone = true
		for uid, numFailures := range testFailures {
			if numFailures == 0 {
				continue
			}
			failures <- uid
			testFailures[uid]--
			allFailuresDone = false

		}
	}

	close(failures)
	wg.Wait()

	// now mark all requests as successful. We can't do that above
	// because we need to make sure that these calls come strictly
	// after m.recordFailure calls to get the right count. However
	// because we still want to test that calls to m.recordSuccess
	// are also thread-safe, we spawn a goroutine for each call as
	// well.
	wg.Add(len(testFailures))
	for uid := range testFailures {
		go func(uid types.UID) {
			defer wg.Done()
			m.recordSuccess(ctx, uid)
		}(uid)
	}
	wg.Wait()

	requireMetric(t, registry, metricName, func(t *testing.T, metric *promclient.Metric) {
		for i, bucket := range metric.Histogram.Bucket {
			require.Equal(t, expectedBucketSizes[i], *bucket.CumulativeCount)
		}
	})
}

func TestMetricsOldest(t *testing.T) {
	const metricName = "kubernetes_scanner_backend_oldest_failure"

	setup := func() (*metrics, *prometheus.Registry) {
		registry := prometheus.NewPedanticRegistry()
		m := newMetrics(registry)
		return m, registry
	}

	ctx := context.Background()
	t.Run("cleanup with no other failures", func(t *testing.T) {
		m, registry := setup()

		m.recordFailure(ctx, 403, "x")
		requireGauge(t, registry, metricName, float64(*m.failures["x"].added))

		m.recordSuccess(ctx, "x")
		requireGauge(t, registry, metricName, math.Inf(0))
	})

	t.Run("cleanup with replacement", func(t *testing.T) {
		m, registry := setup()

		m.recordFailure(ctx, 403, "a")
		requireGauge(t, registry, metricName, float64(*m.failures["a"].added))

		m.recordFailure(ctx, 403, "b")
		requireGauge(t, registry, metricName, float64(*m.failures["a"].added))

		m.recordSuccess(ctx, "a")
		requireGauge(t, registry, metricName, float64(*m.failures["b"].added))
		m.recordSuccess(ctx, "b")
	})

	const ageMetricName = "kubernetes_scanner_backend_oldest_failure_age_seconds"
	t.Run("test age", func(t *testing.T) {
		m, registry := setup()

		m.recordFailure(ctx, 403, "c")
		requireGauge(t, registry, ageMetricName, 0)
		// fast-forward time.
		now = func() time.Time {
			return timeNow.Add(50 * time.Second)
		}
		requireGauge(t, registry, ageMetricName, 50)

		m.recordSuccess(ctx, "c")
		requireGauge(t, registry, ageMetricName, math.Inf(0))
	})
}

func TestMetricsErrors(t *testing.T) {
	registry := prometheus.NewPedanticRegistry()
	m := newMetrics(registry)
	m.recordFailure(context.Background(), 500, "a-uid")
	requireMetric(t, registry, "kubernetes_scanner_backend_errors_total", func(t *testing.T, metric *promclient.Metric) {
		require.Equal(t, 1.0, metric.Counter.GetValue())
	})
}

// requireMetrics requires the given metric to be present in the registry and pass the requireFn.
func requireMetric(t *testing.T, registry prometheus.Gatherer, metricName string,
	requireFn func(*testing.T, *promclient.Metric),
) {
	t.Helper()
	metrics, err := registry.Gather()
	require.NoError(t, err)

	for _, m := range metrics {
		if *m.Name == metricName {
			requireFn(t, m.GetMetric()[0])
			return
		}
	}
	t.Fatalf("metric %v is not present in registry", metricName)
}

// requireGauge requires the given metric to be present in the registry with the expected value.
func requireGauge(t *testing.T, registry prometheus.Gatherer, metricName string, expected float64) {
	t.Helper()
	requireMetric(t, registry, metricName, func(t *testing.T, metric *promclient.Metric) {
		require.Equal(t, expected, metric.Gauge.GetValue())
	})
}
