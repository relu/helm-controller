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

	"github.com/fluxcd/pkg/apis/meta"
	"github.com/fluxcd/pkg/runtime/events"
	"k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	v2 "github.com/fluxcd/helm-controller/api/v2beta1"
	"github.com/fluxcd/helm-controller/internal/runner"
	"github.com/fluxcd/helm-controller/internal/util"
)

func (r *HelmReleaseReconciler) reconcileRelease(ctx context.Context, run *runner.Runner, hr *v2.HelmRelease) (ctrl.Result, error) {
	// Get the Helm chart
	hc, err := r.getHelmChart(ctx, hr)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Check chart readiness
	if hc.Generation != hc.Status.ObservedGeneration || !meta.HasReadyCondition(hc.Status.Conditions) {
		err := fmt.Errorf("HelmChart '%s/%s' is not ready", hc.GetNamespace(), hc.GetName())
		return ctrl.Result{}, err
	}

	// Compose the values for this release, and record the checksum
	values, err := r.composeValues(ctx, *hr)
	if err != nil {
		return ctrl.Result{}, err
	}
	valuesChecksum := util.ValuesChecksum(values)

	// Retrieve the last release for observation
	rel, err := run.ObserveLastRelease(*hr)
	if err != nil {
		return ctrl.Result{}, err
	}

	// If new state, mark release as progressing which resets the conditions
	if hr.Status.LastAttemptedRevision != hc.GetArtifact().Revision ||
		hr.Status.LastAttemptedValuesChecksum != valuesChecksum ||
		hr.Status.LastReleaseRevision != util.ReleaseRevision(rel) ||
		hr.Status.ObservedGeneration != hr.Generation {
		v2.HelmReleaseProgressing(hr)
		if err := r.Client.Status().Update(ctx, hr); err != nil {
			return ctrl.Result{}, err
		}
		r.recordReadiness(hr, false)
	}

	// If the previous release was successful, return
	if released := meta.GetCondition(hr.Status.Conditions, v2.ReleasedCondition); released != nil {
		if released.Status == v1.ConditionTrue {
			return ctrl.Result{}, nil
		}
	}

	// Check if the previous remediation attempt failed
	if remediated := meta.GetCondition(hr.Status.Conditions, v2.RemediatedCondition); remediated != nil {
		if remediated.Status == v1.ConditionFalse {
			err = fmt.Errorf("previous remediation attempt failed")
			return ctrl.Result{}, err
		}
		switch rel {
		case nil:
			if hr.Spec.GetInstall().GetRemediation().RetriesExhausted(hr) {
				err = fmt.Errorf("install retries exhausted")
				return ctrl.Result{}, err
			}
		default:
			if hr.Spec.GetUpgrade().GetRemediation().RetriesExhausted(hr) {
				err = fmt.Errorf("upgrade retries exhausted")
				return ctrl.Result{}, err
			}
		}
	}

	// Load the chart
	loadedChart, err := r.loadHelmChart(hc)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Record the release attempt
	v2.HelmReleaseAttempted(hr, hc.GetArtifact().Revision, valuesChecksum)

	// Perform the release
	doRelease := run.Install
	remediation := hr.Spec.GetInstall().GetRemediation()
	successReason, failureReason := v2.InstallSucceededReason, v2.InstallFailedReason
	if rel != nil {
		doRelease = run.Upgrade
		remediation = hr.Spec.GetUpgrade().GetRemediation()
		successReason, failureReason = v2.UpgradeSucceededReason, v2.UpgradeFailedReason
	}
	rel, err = doRelease(*hr, loadedChart, values)
	if err != nil {
		v2.SetHelmReleaseCondition(hr, v2.ReleasedCondition, v1.ConditionFalse, failureReason, "")
		remediation.IncrementFailureCount(hr)
		r.event(*hr, hr.Status.LastAttemptedRevision, events.EventSeverityError,
			fmt.Sprintf("Helm release failed: %s", err.Error()))
		return ctrl.Result{}, err
	}

	// Register the post release values
	v2.SetHelmReleaseCondition(hr, v2.ReleasedCondition, v1.ConditionTrue, successReason, "")
	hr.Status.LastAppliedRevision = hr.Status.LastAttemptedRevision
	hr.Status.LastReleaseRevision = util.ReleaseRevision(rel)
	r.event(*hr, hr.Status.LastAttemptedRevision, events.EventSeverityInfo, fmt.Sprintf("Helm release succeeded"))

	return ctrl.Result{RequeueAfter: hr.Spec.Interval.Duration}, nil
}

func (r *HelmReleaseReconciler) reconcileTest(ctx context.Context, run *runner.Runner, hr *v2.HelmRelease) (ctrl.Result, error) {
	// Tests are not enabled for this release, return.
	if !hr.Spec.GetTest().Enable {
		return ctrl.Result{}, nil
	}

	// If we already run the test, return
	if tested := meta.GetCondition(hr.Status.Conditions, v2.TestSuccessCondition); tested != nil {
		return ctrl.Result{}, nil
	}

	// If the release did not succeed, return
	released := meta.GetCondition(hr.Status.Conditions, v2.ReleasedCondition)
	if released == nil || released.Status == v1.ConditionFalse {
		return ctrl.Result{}, nil
	}

	if _, err := run.Test(*hr); err != nil {
		v2.SetHelmReleaseCondition(hr, v2.TestSuccessCondition, v1.ConditionFalse, v2.TestFailedReason, "")
		r.event(*hr, hr.Status.LastAttemptedRevision, events.EventSeverityError,
			fmt.Sprintf("Helm test failed: %s", err.Error()))

		remediation := hr.Spec.GetInstall().GetRemediation()
		if released.Reason == v2.UpgradeSucceededReason {
			remediation = hr.Spec.GetUpgrade().GetRemediation()
		}

		if remediation.MustIgnoreTestFailures(hr.Spec.GetTest().IgnoreFailures) {
			return ctrl.Result{}, nil
		}

		remediation.IncrementFailureCount(hr)
		v2.SetHelmReleaseCondition(hr, v2.ReleasedCondition, v1.ConditionFalse, v2.TestFailedReason, "")
		return ctrl.Result{}, err
	}

	v2.SetHelmReleaseCondition(hr, v2.TestSuccessCondition, v1.ConditionTrue, v2.TestSucceededReason, "")
	r.event(*hr, hr.Status.LastAttemptedRevision, events.EventSeverityInfo, "Helm test passed")
	return ctrl.Result{}, nil
}

func (r *HelmReleaseReconciler) reconcileRemediation(ctx context.Context, run *runner.Runner, hr *v2.HelmRelease) (ctrl.Result, error) {
	// If the release did not explicitly fail, we should not remediate
	released := meta.GetCondition(hr.Status.Conditions, v2.ReleasedCondition)
	if released == nil || released.Status != v1.ConditionFalse {
		return ctrl.Result{}, nil
	}

	// Get the right remediation strategy based on the failure count,
	// looking back, this would have been easier if we did not decide
	// to flatten our conditions (https://github.com/fluxcd/helm-controller/pull/75)
	remediation := hr.Spec.GetInstall().GetRemediation()
	if remediation.GetFailureCount(hr) == 0 {
		remediation = hr.Spec.GetUpgrade().GetRemediation()
	}
	if remediation.RetriesExhausted(hr) || !remediation.MustRemediateLastFailure() {
		return ctrl.Result{}, nil
	}

	// Observe the last release, if this still equals to the last successful
	// release we have recorded in our status, we should not remediate.
	rel, err := run.ObserveLastRelease(*hr)
	if err != nil {
		v2.SetHelmReleaseCondition(hr, v2.RemediatedCondition, v1.ConditionFalse, v2.GetLastReleaseFailedReason, "")
		return ctrl.Result{}, err
	}
	if util.ReleaseRevision(rel) <= hr.Status.LastReleaseRevision {
		return ctrl.Result{}, nil
	}

	// Remediate based on the configured strategy
	var successReason, failureReason string
	switch remediation.GetStrategy() {
	case v2.RollbackRemediationStrategy:
		successReason, failureReason = v2.RollbackSucceededReason, v2.RollbackFailedReason
		err = run.Rollback(*hr)
	case v2.UninstallRemediationStrategy:
		successReason, failureReason = v2.UninstallSucceededReason, v2.UninstallFailedReason
		err = run.Uninstall(*hr)
	}
	if err != nil {
		v2.SetHelmReleaseCondition(hr, v2.RemediatedCondition, v1.ConditionFalse, failureReason, "")
		r.event(*hr, hr.Status.LastAttemptedRevision, events.EventSeverityError,
			fmt.Sprintf("Helm %s failed: %s", remediation.GetStrategy(), err.Error()))
		return ctrl.Result{}, err
	}
	v2.SetHelmReleaseCondition(hr, v2.RemediatedCondition, v1.ConditionTrue, successReason, "")
	r.event(*hr, hr.Status.LastAttemptedRevision, events.EventSeverityInfo,
		fmt.Sprintf("Helm %s succeeded", remediation.GetStrategy()))

	// Observe release after remediation to determine the new revision
	rel, err = run.ObserveLastRelease(*hr)
	if err != nil {
		v2.SetHelmReleaseCondition(hr, v2.RemediatedCondition, v1.ConditionFalse, v2.GetLastReleaseFailedReason, "")
		return ctrl.Result{}, err
	}
	hr.Status.LastReleaseRevision = util.ReleaseRevision(rel)
	return ctrl.Result{}, nil
}
