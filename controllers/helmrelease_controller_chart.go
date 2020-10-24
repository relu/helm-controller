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
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"strings"

	sourcev1 "github.com/fluxcd/source-controller/api/v1beta1"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	v2 "github.com/fluxcd/helm-controller/api/v2beta1"
)

func (r *HelmReleaseReconciler) reconcileChart(ctx context.Context, hr *v2.HelmRelease) (ctrl.Result, error) {
	chartName := types.NamespacedName{
		Namespace: hr.Spec.Chart.GetNamespace(hr.Namespace),
		Name:      hr.GetHelmChartName(),
	}

	// Garbage collect the previous HelmChart if the namespace named changed.
	if hr.Status.HelmChart != "" && hr.Status.HelmChart != chartName.String() {
		if err := r.deleteHelmChart(ctx, *hr); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Continue with the reconciliation of the current template.
	var helmChart sourcev1.HelmChart
	err := r.Client.Get(ctx, chartName, &helmChart)
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	hc := helmChartFromTemplate(*hr)
	switch {
	case apierrors.IsNotFound(err):
		if err = r.Client.Create(ctx, hc); err != nil {
			return ctrl.Result{}, err
		}
	case helmChartRequiresUpdate(*hr, helmChart):
		r.Log.Info("chart diverged from template", strings.ToLower(sourcev1.HelmChartKind), chartName.String())
		helmChart.Spec = hc.Spec
		if err = r.Client.Update(ctx, &helmChart); err != nil {
			return ctrl.Result{}, err
		}
	}

	hr.Status.HelmChart = chartName.String()
	return ctrl.Result{}, nil
}

func (r *HelmReleaseReconciler) getHelmChart(ctx context.Context, hr *v2.HelmRelease) (*sourcev1.HelmChart, error) {
	namespace, name := hr.Status.GetHelmChart()
	hc := &sourcev1.HelmChart{}
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, hc); err != nil {
		return nil, err
	}
	return hc, nil
}

// loadHelmChart attempts to download the artifact from the provided source,
// loads it into a chart.Chart, and removes the downloaded artifact.
// It returns the loaded chart.Chart on success, or an error.
func (r *HelmReleaseReconciler) loadHelmChart(source *sourcev1.HelmChart) (*chart.Chart, error) {
	f, err := ioutil.TempFile("", fmt.Sprintf("%s-%s-*.tgz", source.GetNamespace(), source.GetName()))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	defer os.Remove(f.Name())

	res, err := http.Get(source.GetArtifact().URL)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("artifact '%s' download failed (status code: %s)", source.GetArtifact().URL, res.Status)
	}

	if _, err = io.Copy(f, res.Body); err != nil {
		return nil, err
	}

	return loader.Load(f.Name())
}

// deleteHelmChart deletes the v1beta1.HelmChart of the v2beta1.HelmRelease.
func (r *HelmReleaseReconciler) deleteHelmChart(ctx context.Context, hr v2.HelmRelease) error {
	if hr.Status.HelmChart == "" {
		return nil
	}
	var hc sourcev1.HelmChart
	chartNS, chartName := hr.Status.GetHelmChart()
	err := r.Client.Get(ctx, types.NamespacedName{Namespace: chartNS, Name: chartName}, &hc)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		err = fmt.Errorf("failed to delete HelmChart '%s': %w", hr.Status.HelmChart, err)
		return err
	}
	err = r.Client.Delete(ctx, &hc)
	if err != nil {
		err = fmt.Errorf("failed to delete HelmChart '%s': %w", hr.Status.HelmChart, err)
	}
	return err
}
