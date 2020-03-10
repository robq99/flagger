package controller

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	flaggerv1 "github.com/weaveworks/flagger/pkg/apis/flagger/v1beta1"
	"github.com/weaveworks/flagger/pkg/notifier"
)

func TestScheduler_DaemonSetInit(t *testing.T) {
	mocks := newDaemonSetFixture(nil)
	mocks.ctrl.advanceCanary("podinfo", "default", true)

	_, err := mocks.kubeClient.AppsV1().DaemonSets("default").Get("podinfo-primary", metav1.GetOptions{})
	require.NoError(t, err)
}

func TestScheduler_DaemonSetNewRevision(t *testing.T) {
	mocks := newDaemonSetFixture(nil)
	mocks.ctrl.advanceCanary("podinfo", "default", true)

	// update
	dae2 := newDaemonSetTestDaemonSetV2()
	_, err := mocks.kubeClient.AppsV1().DaemonSets("default").Update(dae2)
	require.NoError(t, err)

	// detect changes
	mocks.ctrl.advanceCanary("podinfo", "default", true)

	_, err = mocks.kubeClient.AppsV1().DaemonSets("default").Get("podinfo", metav1.GetOptions{})
	require.NoError(t, err)
}

func TestScheduler_DaemonSetRollback(t *testing.T) {
	mocks := newDaemonSetFixture(nil)
	// init
	mocks.ctrl.advanceCanary("podinfo", "default", true)

	// update failed checks to max
	err := mocks.deployer.SyncStatus(mocks.canary, flaggerv1.CanaryStatus{Phase: flaggerv1.CanaryPhaseProgressing, FailedChecks: 10})
	require.NoError(t, err)

	// set a metric check to fail
	c, err := mocks.flaggerClient.FlaggerV1beta1().Canaries("default").Get("podinfo", metav1.GetOptions{})
	require.NoError(t, err)

	cd := c.DeepCopy()
	cd.Spec.Analysis.Metrics = append(c.Spec.Analysis.Metrics, flaggerv1.CanaryMetric{
		Name:     "fail",
		Interval: "1m",
		ThresholdRange: &flaggerv1.CanaryThresholdRange{
			Min: toFloatPtr(0),
			Max: toFloatPtr(50),
		},
		Query: "fail",
	})
	_, err = mocks.flaggerClient.FlaggerV1beta1().Canaries("default").Update(cd)
	require.NoError(t, err)

	// run metric checks
	mocks.ctrl.advanceCanary("podinfo", "default", true)

	// finalise analysis
	mocks.ctrl.advanceCanary("podinfo", "default", true)

	// check status
	c, err = mocks.flaggerClient.FlaggerV1beta1().Canaries("default").Get("podinfo", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, flaggerv1.CanaryPhaseFailed, c.Status.Phase)
}

func TestScheduler_DaemonSetSkipAnalysis(t *testing.T) {
	mocks := newDaemonSetFixture(nil)
	// init
	mocks.ctrl.advanceCanary("podinfo", "default", true)

	// enable skip
	cd, err := mocks.flaggerClient.FlaggerV1beta1().Canaries("default").Get("podinfo", metav1.GetOptions{})
	require.NoError(t, err)

	cd.Spec.SkipAnalysis = true
	_, err = mocks.flaggerClient.FlaggerV1beta1().Canaries("default").Update(cd)
	require.NoError(t, err)

	// update
	dae2 := newDaemonSetTestDaemonSetV2()
	_, err = mocks.kubeClient.AppsV1().DaemonSets("default").Update(dae2)
	require.NoError(t, err)

	// detect changes
	mocks.ctrl.advanceCanary("podinfo", "default", true)
	// advance
	mocks.ctrl.advanceCanary("podinfo", "default", true)

	c, err := mocks.flaggerClient.FlaggerV1beta1().Canaries("default").Get("podinfo", metav1.GetOptions{})
	require.NoError(t, err)
	assert.True(t, c.Spec.SkipAnalysis)
	assert.Equal(t, flaggerv1.CanaryPhaseSucceeded, c.Status.Phase)
}

func TestScheduler_DaemonSetNewRevisionReset(t *testing.T) {
	mocks := newDaemonSetFixture(nil)
	// init
	mocks.ctrl.advanceCanary("podinfo", "default", true)

	// first update
	dae2 := newDaemonSetTestDaemonSetV2()
	_, err := mocks.kubeClient.AppsV1().DaemonSets("default").Update(dae2)
	require.NoError(t, err)

	// detect changes
	mocks.ctrl.advanceCanary("podinfo", "default", true)
	// advance
	mocks.ctrl.advanceCanary("podinfo", "default", true)

	primaryWeight, canaryWeight, mirrored, err := mocks.router.GetRoutes(mocks.canary)
	require.NoError(t, err)
	assert.Equal(t, 90, primaryWeight)
	assert.Equal(t, 10, canaryWeight)
	assert.False(t, mirrored)

	// second update
	dae2.Spec.Template.Spec.ServiceAccountName = "test"
	_, err = mocks.kubeClient.AppsV1().DaemonSets("default").Update(dae2)
	require.NoError(t, err)

	// detect changes
	mocks.ctrl.advanceCanary("podinfo", "default", true)

	primaryWeight, canaryWeight, mirrored, err = mocks.router.GetRoutes(mocks.canary)
	require.NoError(t, err)
	assert.Equal(t, 100, primaryWeight)
	assert.Equal(t, 0, canaryWeight)
	assert.False(t, mirrored)
}

func TestScheduler_DaemonSetPromotion(t *testing.T) {
	mocks := newDaemonSetFixture(nil)

	// init
	mocks.ctrl.advanceCanary("podinfo", "default", true)

	// check initialized status
	c, err := mocks.flaggerClient.FlaggerV1beta1().Canaries("default").Get("podinfo", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, flaggerv1.CanaryPhaseInitialized, c.Status.Phase)

	// update
	dae2 := newDaemonSetTestDaemonSetV2()
	_, err = mocks.kubeClient.AppsV1().DaemonSets("default").Update(dae2)
	require.NoError(t, err)

	// detect pod spec changes
	mocks.ctrl.advanceCanary("podinfo", "default", true)

	config2 := newDaemonSetTestConfigMapV2()
	_, err = mocks.kubeClient.CoreV1().ConfigMaps("default").Update(config2)
	require.NoError(t, err)

	secret2 := newDaemonSetTestSecretV2()
	_, err = mocks.kubeClient.CoreV1().Secrets("default").Update(secret2)
	require.NoError(t, err)

	// detect configs changes
	mocks.ctrl.advanceCanary("podinfo", "default", true)

	primaryWeight, canaryWeight, mirrored, err := mocks.router.GetRoutes(mocks.canary)
	require.NoError(t, err)

	primaryWeight = 60
	canaryWeight = 40
	err = mocks.router.SetRoutes(mocks.canary, primaryWeight, canaryWeight, mirrored)
	require.NoError(t, err)

	// advance
	mocks.ctrl.advanceCanary("podinfo", "default", true)

	// check progressing status
	c, err = mocks.flaggerClient.FlaggerV1beta1().Canaries("default").Get("podinfo", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, flaggerv1.CanaryPhaseProgressing, c.Status.Phase)

	// promote
	mocks.ctrl.advanceCanary("podinfo", "default", true)

	// check promoting status
	c, err = mocks.flaggerClient.FlaggerV1beta1().Canaries("default").Get("podinfo", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, flaggerv1.CanaryPhasePromoting, c.Status.Phase)

	// finalise
	mocks.ctrl.advanceCanary("podinfo", "default", true)

	primaryWeight, canaryWeight, mirrored, err = mocks.router.GetRoutes(mocks.canary)
	require.NoError(t, err)
	assert.Equal(t, 100, primaryWeight)
	assert.Equal(t, 0, canaryWeight)
	assert.False(t, mirrored)

	primaryDae, err := mocks.kubeClient.AppsV1().DaemonSets("default").Get("podinfo-primary", metav1.GetOptions{})
	require.NoError(t, err)

	primaryImage := primaryDae.Spec.Template.Spec.Containers[0].Image
	canaryImage := dae2.Spec.Template.Spec.Containers[0].Image
	assert.Equal(t, canaryImage, primaryImage)

	configPrimary, err := mocks.kubeClient.CoreV1().ConfigMaps("default").Get("podinfo-config-env-primary", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, config2.Data["color"], configPrimary.Data["color"])

	secretPrimary, err := mocks.kubeClient.CoreV1().Secrets("default").Get("podinfo-secret-env-primary", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, string(secret2.Data["apiKey"]), string(secretPrimary.Data["apiKey"]))

	// check finalising status
	c, err = mocks.flaggerClient.FlaggerV1beta1().Canaries("default").Get("podinfo", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, flaggerv1.CanaryPhaseFinalising, c.Status.Phase)

	// scale canary to zero
	mocks.ctrl.advanceCanary("podinfo", "default", true)

	c, err = mocks.flaggerClient.FlaggerV1beta1().Canaries("default").Get("podinfo", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, flaggerv1.CanaryPhaseSucceeded, c.Status.Phase)
}

func TestScheduler_DaemonSetMirroring(t *testing.T) {
	mocks := newDaemonSetFixture(newDaemonSetTestCanaryMirror())
	// init
	mocks.ctrl.advanceCanary("podinfo", "default", true)

	// update
	dae2 := newDaemonSetTestDaemonSetV2()
	_, err := mocks.kubeClient.AppsV1().DaemonSets("default").Update(dae2)
	require.NoError(t, err)

	// detect pod spec changes
	mocks.ctrl.advanceCanary("podinfo", "default", true)

	// advance
	mocks.ctrl.advanceCanary("podinfo", "default", true)

	// check if traffic is mirrored to canary
	primaryWeight, canaryWeight, mirrored, err := mocks.router.GetRoutes(mocks.canary)
	require.NoError(t, err)
	assert.Equal(t, 100, primaryWeight)
	assert.Equal(t, 0, canaryWeight)
	assert.True(t, mirrored)

	// advance
	mocks.ctrl.advanceCanary("podinfo", "default", true)

	// check if traffic is mirrored to canary
	primaryWeight, canaryWeight, mirrored, err = mocks.router.GetRoutes(mocks.canary)
	require.NoError(t, err)
	assert.Equal(t, 90, primaryWeight)
	assert.Equal(t, 10, canaryWeight)
	assert.False(t, mirrored)
}

func TestScheduler_DaemonSetABTesting(t *testing.T) {
	mocks := newDaemonSetFixture(newDaemonSetTestCanaryAB())
	// init
	mocks.ctrl.advanceCanary("podinfo", "default", true)

	// update
	dae2 := newDaemonSetTestDaemonSetV2()
	_, err := mocks.kubeClient.AppsV1().DaemonSets("default").Update(dae2)
	require.NoError(t, err)

	// detect pod spec changes
	mocks.ctrl.advanceCanary("podinfo", "default", true)

	// advance
	mocks.ctrl.advanceCanary("podinfo", "default", true)

	// check if traffic is routed to canary
	primaryWeight, canaryWeight, mirrored, err := mocks.router.GetRoutes(mocks.canary)
	require.NoError(t, err)
	assert.Equal(t, 0, primaryWeight)
	assert.Equal(t, 100, canaryWeight)
	assert.False(t, mirrored)

	cd, err := mocks.flaggerClient.FlaggerV1beta1().Canaries("default").Get("podinfo", metav1.GetOptions{})
	require.NoError(t, err)

	// set max iterations
	err = mocks.deployer.SetStatusIterations(cd, 10)
	require.NoError(t, err)

	// advance
	mocks.ctrl.advanceCanary("podinfo", "default", true)

	// finalising
	mocks.ctrl.advanceCanary("podinfo", "default", true)

	// check finalising status
	c, err := mocks.flaggerClient.FlaggerV1beta1().Canaries("default").Get("podinfo", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, flaggerv1.CanaryPhaseFinalising, c.Status.Phase)

	// check if the container image tag was updated
	primaryDae, err := mocks.kubeClient.AppsV1().DaemonSets("default").Get("podinfo-primary", metav1.GetOptions{})
	require.NoError(t, err)

	primaryImage := primaryDae.Spec.Template.Spec.Containers[0].Image
	canaryImage := dae2.Spec.Template.Spec.Containers[0].Image
	assert.Equal(t, canaryImage, primaryImage)

	// shutdown canary
	mocks.ctrl.advanceCanary("podinfo", "default", true)

	// check rollout status
	c, err = mocks.flaggerClient.FlaggerV1beta1().Canaries("default").Get("podinfo", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, flaggerv1.CanaryPhaseSucceeded, c.Status.Phase)
}

func TestScheduler_DaemonSetPortDiscovery(t *testing.T) {
	mocks := newDaemonSetFixture(nil)

	// enable port discovery
	cd, err := mocks.flaggerClient.FlaggerV1beta1().Canaries("default").Get("podinfo", metav1.GetOptions{})
	require.NoError(t, err)
	cd.Spec.Service.PortDiscovery = true
	_, err = mocks.flaggerClient.FlaggerV1beta1().Canaries("default").Update(cd)
	require.NoError(t, err)

	mocks.ctrl.advanceCanary("podinfo", "default", true)

	canarySvc, err := mocks.kubeClient.CoreV1().Services("default").Get("podinfo-canary", metav1.GetOptions{})
	require.NoError(t, err)
	require.Len(t, canarySvc.Spec.Ports, 3)

	matchPorts := func(lookup string) bool {
		switch lookup {
		case
			"http 9898",
			"http-metrics 8080",
			"tcp-podinfo-2 8888":
			return true
		}
		return false
	}

	for _, port := range canarySvc.Spec.Ports {
		require.True(t, matchPorts(fmt.Sprintf("%s %v", port.Name, port.Port)))
	}
}

func TestScheduler_DaemonSetTargetPortNumber(t *testing.T) {
	mocks := newDaemonSetFixture(nil)

	cd, err := mocks.flaggerClient.FlaggerV1beta1().Canaries("default").Get("podinfo", metav1.GetOptions{})
	require.NoError(t, err)
	cd.Spec.Service.Port = 80
	cd.Spec.Service.TargetPort = intstr.FromInt(9898)
	cd.Spec.Service.PortDiscovery = true
	_, err = mocks.flaggerClient.FlaggerV1beta1().Canaries("default").Update(cd)
	require.NoError(t, err)

	mocks.ctrl.advanceCanary("podinfo", "default", true)

	canarySvc, err := mocks.kubeClient.CoreV1().Services("default").Get("podinfo-canary", metav1.GetOptions{})
	require.NoError(t, err)
	require.Len(t, canarySvc.Spec.Ports, 3)

	matchPorts := func(lookup string) bool {
		switch lookup {
		case
			"http 80",
			"http-metrics 8080",
			"tcp-podinfo-2 8888":
			return true
		}
		return false
	}

	for _, port := range canarySvc.Spec.Ports {
		require.True(t, matchPorts(fmt.Sprintf("%s %v", port.Name, port.Port)))
	}
}

func TestScheduler_DaemonSetTargetPortName(t *testing.T) {
	mocks := newDaemonSetFixture(nil)

	cd, err := mocks.flaggerClient.FlaggerV1beta1().Canaries("default").Get("podinfo", metav1.GetOptions{})
	require.NoError(t, err)
	cd.Spec.Service.Port = 8080
	cd.Spec.Service.TargetPort = intstr.FromString("http")
	cd.Spec.Service.PortDiscovery = true
	_, err = mocks.flaggerClient.FlaggerV1beta1().Canaries("default").Update(cd)
	require.NoError(t, err)

	mocks.ctrl.advanceCanary("podinfo", "default", true)

	canarySvc, err := mocks.kubeClient.CoreV1().Services("default").Get("podinfo-canary", metav1.GetOptions{})
	require.NoError(t, err)
	require.Len(t, canarySvc.Spec.Ports, 3)

	matchPorts := func(lookup string) bool {
		switch lookup {
		case
			"http 8080",
			"http-metrics 8080",
			"tcp-podinfo-2 8888":
			return true
		}
		return false
	}

	for _, port := range canarySvc.Spec.Ports {
		require.True(t, matchPorts(fmt.Sprintf("%s %v", port.Name, port.Port)))
	}
}

func TestScheduler_DaemonSetAlerts(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := ioutil.ReadAll(r.Body)
		require.NoError(t, err)
		var payload = notifier.SlackPayload{}
		err = json.Unmarshal(b, &payload)
		require.NoError(t, err)
		require.Equal(t, "podinfo.default", payload.Attachments[0].AuthorName)
	}))
	defer ts.Close()

	canary := newDaemonSetTestCanary()
	canary.Spec.Analysis.Alerts = []flaggerv1.CanaryAlert{
		{
			Name:     "slack-dev",
			Severity: "info",
			ProviderRef: flaggerv1.CrossNamespaceObjectReference{
				Name:      "slack",
				Namespace: "default",
			},
		},
		{
			Name:     "slack-prod",
			Severity: "info",
			ProviderRef: flaggerv1.CrossNamespaceObjectReference{
				Name: "slack",
			},
		},
	}
	mocks := newDaemonSetFixture(canary)

	secret := newDaemonSetTestAlertProviderSecret()
	secret.Data = map[string][]byte{
		"address": []byte(ts.URL),
	}
	_, err := mocks.kubeClient.CoreV1().Secrets("default").Update(secret)
	require.NoError(t, err)

	// init canary and send alerts
	mocks.ctrl.advanceCanary("podinfo", "default", true)
}
