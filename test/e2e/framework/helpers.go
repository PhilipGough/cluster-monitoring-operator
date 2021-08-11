package framework

import (
	"context"
	"fmt"
	"testing"
	"time"

	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	ClusterMonitorConfigMapName      = "cluster-monitoring-config"
	UserWorkloadMonitorConfigMapName = "user-workload-monitoring-config"

	UserWorkloadTestNs = "user-workload-test"
)

// GetUserWorkloadEnabledConfigMap returns a config map with uwm enabled
func (f *Framework) GetUserWorkloadEnabledConfigMap(t *testing.T) *v1.ConfigMap {
	t.Helper()
	return &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ClusterMonitorConfigMapName,
			Namespace: f.Ns,
		},
		Data: map[string]string{
			"config.yaml": `enableUserWorkload: true
`,
		},
	}
}

// MustCreateOrUpdateConfigMap or fail the test
func (f *Framework) MustCreateOrUpdateConfigMap(t *testing.T, cm *v1.ConfigMap) {
	t.Helper()
	if err := f.OperatorClient.CreateOrUpdateConfigMap(context.Background(), cm); err != nil {
		t.Fatalf("failed to create/update configmap - %s", err.Error())
	}
}

// SetupUserWorkloadAssets enables UWM via the config map and asserts resources are up and running
func (f *Framework) SetupUserWorkloadAssets(t *testing.T) {
	t.Helper()

	f.MustCreateOrUpdateConfigMap(t, f.GetUserWorkloadEnabledConfigMap(t))
	f.AssertDeploymentExists("prometheus-operator", f.UserWorkloadMonitoringNs)(t)
	f.AssertStatefulSetExistsAndRollout("prometheus-user-workload", f.UserWorkloadMonitoringNs)(t)
	f.AssertPrometheusIsCreated("user-workload", f.UserWorkloadMonitoringNs)(t)
}

// TearDownUserWorkloadAssets deletes the uwm enabled config map
func (f *Framework) TearDownUserWorkloadAssets(t *testing.T) {
	t.Helper()
	f.OperatorClient.DeleteConfigMap(ctx, f.GetUserWorkloadEnabledConfigMap(t))
}

// SetupUserApplication is idempotent and deploys the sample app and resources in UserWorkloadTestNs
func (f *Framework) SetupUserApplication(t *testing.T) {
	t.Helper()
	f.deployUserApplication(t)
	f.createPrometheusAlertmanagerInUserNamespace(t)
}

// TearDownUserApplication deletes the UserWorkloadTestNs and waits for deletion
func (f *Framework) TearDownUserApplication(t *testing.T) {
	// check if its deleted and return if true
	_, err := f.KubeClient.CoreV1().Namespaces().Get(context.TODO(), UserWorkloadTestNs, metav1.GetOptions{})
	if err != nil && apierrors.IsNotFound(err) {
		return
	}

	err = Poll(time.Second, 5*time.Minute, func() error {
		err = f.KubeClient.CoreV1().Namespaces().Delete(context.TODO(), UserWorkloadTestNs, metav1.DeleteOptions{})
		if err != nil {
			return err
		}
		return nil
	})

	if err != nil {
		t.Fatal(err)
	}

	assertResourceDoesNotExists(t, func() (metav1.Object, error) {
		return f.KubeClient.CoreV1().Namespaces().Get(ctx, UserWorkloadTestNs, metav1.GetOptions{})
	})
}

func (f *Framework) deployUserApplication(t *testing.T) error {
	t.Helper()
	_, err := f.KubeClient.CoreV1().Namespaces().Create(ctx, &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: UserWorkloadTestNs,
		},
	}, metav1.CreateOptions{})
	if err != nil && !errors.IsAlreadyExists(err) {
		return err
	}

	assertResourceExists(t, func() (metav1.Object, error) {
		return f.KubeClient.CoreV1().Namespaces().Get(ctx, UserWorkloadTestNs, metav1.GetOptions{})
	})

	app, err := f.KubeClient.AppsV1().Deployments(UserWorkloadTestNs).Create(ctx, &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: "prometheus-example-app",
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: toInt32(1),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": "prometheus-example-app",
				},
			},
			Template: v1.PodTemplateSpec{
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{
							Name:  "prometheus-example-app",
							Image: "ghcr.io/rhobs/prometheus-example-app:0.3.0",
						},
					},
				},
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": "prometheus-example-app",
					},
				},
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return err
	}

	_, err = f.KubeClient.CoreV1().Services(UserWorkloadTestNs).Create(ctx, &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "prometheus-example-app",
			Labels: map[string]string{
				"app": "prometheus-example-app",
			},
		},
		Spec: v1.ServiceSpec{
			Ports: []v1.ServicePort{
				{
					Name:       "web",
					Protocol:   "TCP",
					Port:       8080,
					TargetPort: intstr.FromInt(8080),
				},
			},
			Selector: map[string]string{
				"app": "prometheus-example-app",
			},
			Type: v1.ServiceTypeClusterIP,
		},
	}, metav1.CreateOptions{})

	_, err = f.MonitoringClient.ServiceMonitors(UserWorkloadTestNs).Create(ctx, &monitoringv1.ServiceMonitor{
		ObjectMeta: metav1.ObjectMeta{
			Name: "prometheus-example-monitor",
			Labels: map[string]string{
				"k8s-app": "prometheus-example-monitor",
			},
		},
		Spec: monitoringv1.ServiceMonitorSpec{
			Endpoints: []monitoringv1.Endpoint{
				{
					Port:     "web",
					Scheme:   "http",
					Interval: "30s",
				},
			},
			Selector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": "prometheus-example-app",
				},
			},
		},
	}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}

	_, err = f.MonitoringClient.PrometheusRules(UserWorkloadTestNs).Create(ctx, &monitoringv1.PrometheusRule{
		ObjectMeta: metav1.ObjectMeta{
			Name: "prometheus-example-rule",
			Labels: map[string]string{
				"k8s-app": "prometheus-example-rule",
			},
		},
		Spec: monitoringv1.PrometheusRuleSpec{
			Groups: []monitoringv1.RuleGroup{
				{
					Name: "example",
					Rules: []monitoringv1.Rule{
						{
							Record: "version:blah:count",
							Expr:   intstr.FromString(`count(version)`),
						},
						{
							Alert: "VersionAlert",
							Expr:  intstr.FromString(fmt.Sprintf(`version{namespace="%s",job="prometheus-example-app"} == 1`, UserWorkloadTestNs)),
							For:   "1s",
						},
					},
				},
			},
		},
	}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}

	_, err = f.MonitoringClient.PrometheusRules(UserWorkloadTestNs).Create(ctx, &monitoringv1.PrometheusRule{
		ObjectMeta: metav1.ObjectMeta{
			Name: "prometheus-example-rule-leaf",
			Labels: map[string]string{
				"k8s-app": "prometheus-example-rule-leaf",
				"openshift.io/prometheus-rule-evaluation-scope": "leaf-prometheus",
			},
		},
		Spec: monitoringv1.PrometheusRuleSpec{
			Groups: []monitoringv1.RuleGroup{
				{
					Name: "example",
					Rules: []monitoringv1.Rule{
						{
							Record: "version:blah:leaf:count",
							Expr:   intstr.FromString(`count(version)`),
						},
					},
				},
			},
		},
	}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}

	err = f.OperatorClient.WaitForDeploymentRollout(ctx, app)
	if err != nil {
		return err
	}
	return nil
}

func (f *Framework) createPrometheusAlertmanagerInUserNamespace(t *testing.T) error {
	t.Helper()
	_, err := f.MonitoringClient.Alertmanagers(UserWorkloadTestNs).Create(ctx, &monitoringv1.Alertmanager{
		ObjectMeta: metav1.ObjectMeta{
			Name: "not-to-be-reconciled",
		},
		Spec: monitoringv1.AlertmanagerSpec{
			Replicas: toInt32(1),
		},
	}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}

	_, err = f.MonitoringClient.Prometheuses(UserWorkloadTestNs).Create(ctx, &monitoringv1.Prometheus{
		ObjectMeta: metav1.ObjectMeta{
			Name: "not-to-be-reconciled",
		},
		Spec: monitoringv1.PrometheusSpec{
			Replicas: toInt32(1),
		},
	}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

func toInt32(v int32) *int32 { return &v }
