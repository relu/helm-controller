/*
Copyright 2020 The Flux CD contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/strvals"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/rest"
	kuberecorder "k8s.io/client-go/tools/record"
	"k8s.io/client-go/tools/reference"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/fluxcd/pkg/apis/meta"
	"github.com/fluxcd/pkg/runtime/events"
	"github.com/fluxcd/pkg/runtime/metrics"
	"github.com/fluxcd/pkg/runtime/predicates"
	sourcev1 "github.com/fluxcd/source-controller/api/v1beta1"

	v2 "github.com/fluxcd/helm-controller/api/v2beta1"
	"github.com/fluxcd/helm-controller/internal/kube"
	"github.com/fluxcd/helm-controller/internal/runner"
	"github.com/fluxcd/helm-controller/internal/util"
)

// +kubebuilder:rbac:groups=helm.toolkit.fluxcd.io,resources=helmreleases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=helm.toolkit.fluxcd.io,resources=helmreleases/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// HelmReleaseReconciler reconciles a v2beta1.HelmRelease object.
type HelmReleaseReconciler struct {
	client.Client
	Config                *rest.Config
	Log                   logr.Logger
	Scheme                *runtime.Scheme
	requeueDependency     time.Duration
	EventRecorder         kuberecorder.EventRecorder
	ExternalEventRecorder *events.Recorder
	MetricsRecorder       *metrics.Recorder
}

type HelmReleaseReconcilerOptions struct {
	MaxConcurrentReconciles   int
	DependencyRequeueInterval time.Duration
}

func (r *HelmReleaseReconciler) SetupWithManager(mgr ctrl.Manager, opts HelmReleaseReconcilerOptions) error {
	r.requeueDependency = opts.DependencyRequeueInterval
	return ctrl.NewControllerManagedBy(mgr).
		For(&v2.HelmRelease{}).
		WithEventFilter(predicates.ChangePredicate{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: opts.MaxConcurrentReconciles}).
		Complete(r)
}

func (r *HelmReleaseReconciler) Reconcile(req ctrl.Request) (_ ctrl.Result, retErr error) {
	ctx := context.Background()
	reconcileStart := time.Now()

	hr := &v2.HelmRelease{}
	if err := r.Get(ctx, req.NamespacedName, hr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	log := r.Log.WithValues("controller", strings.ToLower(v2.HelmReleaseKind), "request", req.NamespacedName)

	// Return early if the HelmRelease is suspended
	if hr.Spec.Suspend {
		log.Info("Reconciliation is suspended for this object")
		return ctrl.Result{}, nil
	}

	// Add our finalizer if it does not exist
	if !controllerutil.ContainsFinalizer(hr, v2.HelmReleaseFinalizer) {
		controllerutil.AddFinalizer(hr, v2.HelmReleaseFinalizer)
		if err := r.Client.Update(ctx, hr); err != nil {
			log.Error(err, "unable to register finalizer")
			return ctrl.Result{}, err
		}
	}

	// Initialize Helm action runner
	getter, err := r.getRESTClientGetter(ctx, hr)
	if err != nil {
		v2.SetHelmReleaseCondition(hr, meta.ReadyCondition, corev1.ConditionFalse, v2.InitFailedReason, err.Error())
		if clientErr := r.Client.Status().Update(ctx, hr); clientErr != nil {
			err = utilerrors.NewAggregate([]error{err, clientErr})
		}
		r.recordReadiness(hr, false)
		return ctrl.Result{}, err
	}
	run, err := runner.NewRunner(getter, hr.GetNamespace(), r.Log)
	if err != nil {
		v2.SetHelmReleaseCondition(hr, meta.ReadyCondition, corev1.ConditionFalse, v2.InitFailedReason, err.Error())
		if clientErr := r.Client.Status().Update(ctx, hr); clientErr != nil {
			err = utilerrors.NewAggregate([]error{err, clientErr})
		}
		r.recordReadiness(hr, false)
		return ctrl.Result{}, err
	}

	// Examine if our object is under deletion
	if !hr.ObjectMeta.DeletionTimestamp.IsZero() {
		result, err := r.reconcileDelete(ctx, run, hr)
		if err != nil {
			return result, err
		}
		r.recordReadiness(hr, true)
		if err := r.Client.Update(ctx, hr); err != nil {
			return result, err
		}
		return result, retErr
	}

	// Record reconciliation duration
	if r.MetricsRecorder != nil {
		objRef, err := reference.GetReference(r.Scheme, hr)
		if err != nil {
			return ctrl.Result{}, err
		}
		defer r.MetricsRecorder.RecordDuration(*objRef, reconcileStart)
	}

	// Always attempt to patch the status after each reconciliation
	defer func() {
		// Record the value of the reconciliation request
		if v, ok := meta.ReconcileAnnotationValue(hr.GetAnnotations()); ok {
			hr.Status.LastHandledReconcileAt = v
		}
		// Record observed generation
		hr.Status.ObservedGeneration = hr.Generation
		if retErr != nil {
			v2.HelmReleaseNotReady(hr, meta.ReconciliationFailedReason, err.Error())
			r.event(*hr, hr.Status.LastAttemptedRevision, events.EventSeverityError, retErr.Error())
		} else {
			v2.HelmReleaseReady(hr)
		}
		if err := r.Client.Status().Update(ctx, hr); err != nil {
			retErr = utilerrors.NewAggregate([]error{retErr, err})
		}
		r.recordReadiness(hr, false)
	}()

	// Reconcile the chart
	if result, err := r.reconcileChart(ctx, hr); err != nil {
		r.event(*hr, hr.Status.LastAttemptedRevision, events.EventSeverityError, err.Error())
		return result, err
	}
	// Reconcile our dependencies
	if result, err := r.reconcileDependencies(hr); err != nil {
		r.event(*hr, hr.Status.LastAttemptedRevision, events.EventSeverityInfo,
			fmt.Sprintf("%s (retrying in %s)", err.Error(), result.RequeueAfter.String()))
		// We do not return the error here on purpose, as this would
		// result in a back-off strategy.
		return result, nil
	}

	return r.reconcile(ctx, run, hr)
}

func (r *HelmReleaseReconciler) reconcile(ctx context.Context, run *runner.Runner, hr *v2.HelmRelease) (ctrl.Result, error) {
	steps := []func(context.Context, *runner.Runner, *v2.HelmRelease) (ctrl.Result, error){
		r.reconcileRelease,
		r.reconcileTest,
		r.reconcileRemediation,
	}
	result := ctrl.Result{}
	var errs []error
	for _, step := range steps {
		stepResult, err := step(ctx, run, hr)
		if err != nil {
			errs = append(errs, err)
		}
		// Update the status after every step
		_ = r.Client.Status().Update(ctx, hr)
		if len(errs) > 0 {
			continue
		}
		result = util.LowestNonZeroResult(result, stepResult)
	}
	return result, utilerrors.NewAggregate(errs)
}

func (r *HelmReleaseReconciler) reconcileDependencies(hr *v2.HelmRelease) (ctrl.Result, error) {
	for _, d := range hr.Spec.DependsOn {
		if d.Namespace == "" {
			d.Namespace = hr.GetNamespace()
		}
		dName := types.NamespacedName(d)
		var dHr v2.HelmRelease
		err := r.Get(context.Background(), dName, &dHr)
		if err != nil {
			return ctrl.Result{RequeueAfter: r.requeueDependency}, fmt.Errorf("unable to get '%s' dependency: %w", dName, err)
		}

		if !meta.HasReadyCondition(dHr.Status.Conditions) {
			return ctrl.Result{RequeueAfter: r.requeueDependency}, fmt.Errorf("dependency '%s' is not ready", dName)
		}

		if dHr.Generation != dHr.Status.ObservedGeneration {
			return ctrl.Result{RequeueAfter: r.requeueDependency}, fmt.Errorf("dependency '%s' is not ready", dName)
		}
	}
	return ctrl.Result{}, nil
}

func (r *HelmReleaseReconciler) reconcileDelete(ctx context.Context, run *runner.Runner, hr *v2.HelmRelease) (ctrl.Result, error) {
	if err := run.Uninstall(*hr); err != nil {
		return ctrl.Result{}, err
	}
	controllerutil.RemoveFinalizer(hr, v2.HelmReleaseFinalizer)
	return ctrl.Result{}, nil
}

func (r *HelmReleaseReconciler) getRESTClientGetter(ctx context.Context, hr *v2.HelmRelease) (genericclioptions.RESTClientGetter, error) {
	if hr.Spec.KubeConfig == nil {
		return kube.NewInClusterRESTClientGetter(r.Config, hr.GetReleaseNamespace()), nil
	}
	secretName := types.NamespacedName{
		Namespace: hr.GetNamespace(),
		Name:      hr.Spec.KubeConfig.SecretRef.Name,
	}
	var secret corev1.Secret
	if err := r.Get(ctx, secretName, &secret); err != nil {
		return nil, fmt.Errorf("could not find KubeConfig secret '%s': %w", secretName, err)
	}
	kubeConfig, ok := secret.Data["value"]
	if !ok {
		return nil, fmt.Errorf("KubeConfig secret '%s' does not contain a 'value' key", secretName)
	}
	return kube.NewMemoryRESTClientGetter(kubeConfig, hr.GetReleaseNamespace()), nil
}

// composeValues attempts to resolve all v2beta1.ValuesReference resources
// and merges them as defined. Referenced resources are only retrieved once
// to ensure a single version is taken into account during the merge.
func (r *HelmReleaseReconciler) composeValues(ctx context.Context, hr v2.HelmRelease) (chartutil.Values, error) {
	result := chartutil.Values{}

	configMaps := make(map[string]*corev1.ConfigMap)
	secrets := make(map[string]*corev1.Secret)

	for _, v := range hr.Spec.ValuesFrom {
		namespacedName := types.NamespacedName{Namespace: hr.Namespace, Name: v.Name}
		var valuesData []byte

		switch v.Kind {
		case "ConfigMap":
			resource, ok := configMaps[namespacedName.String()]
			if !ok {
				// The resource may not exist, but we want to act on a single version
				// of the resource in case the values reference is marked as optional.
				configMaps[namespacedName.String()] = nil

				resource = &corev1.ConfigMap{}
				if err := r.Get(ctx, namespacedName, resource); err != nil {
					if apierrors.IsNotFound(err) {
						if v.Optional {
							r.Log.Info("could not find optional %s '%s'", v.Kind, namespacedName)
							continue
						}
						return nil, fmt.Errorf("could not find %s '%s'", v.Kind, namespacedName)
					}
					return nil, err
				}
				configMaps[namespacedName.String()] = resource
			}
			if resource == nil {
				if v.Optional {
					r.Log.Info("could not find optional %s '%s'", v.Kind, namespacedName)
					continue
				}
				return nil, fmt.Errorf("could not find %s '%s'", v.Kind, namespacedName)
			}
			if data, ok := resource.Data[v.GetValuesKey()]; !ok {
				return nil, fmt.Errorf("missing key '%s' in %s '%s'", v.GetValuesKey(), v.Kind, namespacedName)
			} else {
				valuesData = []byte(data)
			}
		case "Secret":
			resource, ok := secrets[namespacedName.String()]
			if !ok {
				// The resource may not exist, but we want to act on a single version
				// of the resource in case the values reference is marked as optional.
				secrets[namespacedName.String()] = nil

				resource = &corev1.Secret{}
				if err := r.Get(ctx, namespacedName, resource); err != nil {
					if apierrors.IsNotFound(err) {
						if v.Optional {
							r.Log.Info("could not find optional %s '%s'", v.Kind, namespacedName)
							continue
						}
						return nil, fmt.Errorf("could not find %s '%s'", v.Kind, namespacedName)
					}
					return nil, err
				}
				secrets[namespacedName.String()] = resource
			}
			if resource == nil {
				if v.Optional {
					r.Log.Info("could not find optional %s '%s'", v.Kind, namespacedName)
					continue
				}
				return nil, fmt.Errorf("could not find %s '%s'", v.Kind, namespacedName)
			}
			if data, ok := resource.Data[v.GetValuesKey()]; !ok {
				return nil, fmt.Errorf("missing key '%s' in %s '%s'", v.GetValuesKey(), v.Kind, namespacedName)
			} else {
				valuesData = data
			}
		default:
			return nil, fmt.Errorf("unsupported ValuesReference kind '%s'", v.Kind)
		}
		switch v.TargetPath {
		case "":
			values, err := chartutil.ReadValues(valuesData)
			if err != nil {
				return nil, fmt.Errorf("unable to read values from key '%s' in %s '%s': %w", v.GetValuesKey(), v.Kind, namespacedName, err)
			}
			result = util.MergeMaps(result, values)
		default:
			// TODO(hidde): this is a bit of hack, as it mimics the way the option string is passed
			// 	to Helm from a CLI perspective. Given the parser is however not publicly accessible
			// 	while it contains all logic around parsing the target path, it is a fair trade-off.
			singleValue := v.TargetPath + "=" + string(valuesData)
			if err := strvals.ParseInto(singleValue, result); err != nil {
				return nil, fmt.Errorf("unable to merge value from key '%s' in %s '%s' into target path '%s': %w", v.GetValuesKey(), v.Kind, namespacedName, v.TargetPath, err)
			}
		}
	}
	return util.MergeMaps(result, hr.GetValues()), nil
}

// event emits a Kubernetes event and forwards the event to notification controller if configured.
func (r *HelmReleaseReconciler) event(hr v2.HelmRelease, revision, severity, msg string) {
	r.EventRecorder.Event(&hr, "Normal", severity, msg)
	objRef, err := reference.GetReference(r.Scheme, &hr)
	if err != nil {
		r.Log.WithValues(
			"request",
			fmt.Sprintf("%s/%s", hr.GetNamespace(), hr.GetName()),
		).Error(err, "unable to send event")
		return
	}

	if r.ExternalEventRecorder != nil {
		var meta map[string]string
		if revision != "" {
			meta = map[string]string{"revision": revision}
		}
		if err := r.ExternalEventRecorder.Eventf(*objRef, meta, severity, severity, msg); err != nil {
			r.Log.WithValues(
				"request",
				fmt.Sprintf("%s/%s", hr.GetNamespace(), hr.GetName()),
			).Error(err, "unable to send event")
			return
		}
	}
}

func (r *HelmReleaseReconciler) recordReadiness(hr *v2.HelmRelease, deleted bool) {
	if r.MetricsRecorder == nil {
		return
	}

	objRef, err := reference.GetReference(r.Scheme, hr)
	if err != nil {
		r.Log.WithValues(
			strings.ToLower(hr.Kind),
			fmt.Sprintf("%s/%s", hr.GetNamespace(), hr.GetName()),
		).Error(err, "unable to record readiness metric")
		return
	}
	if rc := meta.GetCondition(hr.Status.Conditions, meta.ReadyCondition); rc != nil {
		r.MetricsRecorder.RecordCondition(*objRef, *rc, deleted)
	} else {
		r.MetricsRecorder.RecordCondition(*objRef, meta.Condition{
			Type:   meta.ReadyCondition,
			Status: corev1.ConditionUnknown,
		}, deleted)
	}
}

func helmChartFromTemplate(hr v2.HelmRelease) *sourcev1.HelmChart {
	template := hr.Spec.Chart
	return &sourcev1.HelmChart{
		ObjectMeta: v1.ObjectMeta{
			Name:      hr.GetHelmChartName(),
			Namespace: hr.Spec.Chart.GetNamespace(hr.Namespace),
		},
		Spec: sourcev1.HelmChartSpec{
			Chart:   template.Spec.Chart,
			Version: template.Spec.Version,
			SourceRef: sourcev1.LocalHelmChartSourceReference{
				Name: template.Spec.SourceRef.Name,
				Kind: template.Spec.SourceRef.Kind,
			},
			Interval:   template.GetInterval(hr.Spec.Interval),
			ValuesFile: template.Spec.ValuesFile,
		},
	}
}

func helmChartRequiresUpdate(hr v2.HelmRelease, chart sourcev1.HelmChart) bool {
	template := hr.Spec.Chart
	switch {
	case template.Spec.Chart != chart.Spec.Chart:
		return true
	case template.Spec.Version != chart.Spec.Version:
		return true
	case template.Spec.SourceRef.Name != chart.Spec.SourceRef.Name:
		return true
	case template.Spec.SourceRef.Kind != chart.Spec.SourceRef.Kind:
		return true
	case template.GetInterval(hr.Spec.Interval) != chart.Spec.Interval:
		return true
	default:
		return false
	}
}
