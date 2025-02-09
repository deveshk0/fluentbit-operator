/*
Copyright 2021.

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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/source"

	fluentdv1alpha1 "fluent.io/fluent-operator/apis/fluentd/v1alpha1"
	"fluent.io/fluent-operator/apis/fluentd/v1alpha1/plugins"
)

const (
	FluentdConfig        = "FluentdConfig"
	ClusterFluentdConfig = "ClusterFluentdConfig"

	FluentdSecretMainKey   = "fluent.conf"
	FluentdSecretSystemKey = "system.conf"
	FluentdSecretAppKey    = "app.conf"
	FluentdSecretLogKey    = "log.conf"

	FlUENT_INCLUDE = `# includes all files
@include /fluentd/etc/system.conf
@include /fluentd/etc/app.conf
@include /fluentd/etc/log.conf
`

	SYSTEM = `# Enable RPC endpoint
<system>
	rpc_endpoint 127.0.0.1:24444
	log_level info
	workers %d
</system>
`
	FLUENTD_LOG = `# Do not collect fluentd's own logs to avoid infinite loops.
<match **>
	@type null
	@id main-no-output
</match>
<label @FLUENT_LOG>
	<match fluent.*>
		@type null
		@id main-fluentd-log
	</match>
</label>
`
)

// FluentdConfigReconciler reconciles a FluentdConfig object
type FluentdConfigReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=fluentd.fluent.io,resources=fluentdconfigs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=fluentd.fluent.io,resources=clusterfluentdconfigs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=fluentd.fluent.io,resources=inputs;filters;outputs,verbs=list
//+kubebuilder:rbac:groups=fluentd.fluent.io,resources=clusterinputs;clusterfilters;clusteroutputs,verbs=list;
//+kubebuilder:rbac:groups=fluentd.fluent.io,resources=fluentds,verbs=list
//+kubebuilder:rbac:groups=fluentd.fluent.io,resources=fluentds/status,verbs=patch
//+kubebuilder:rbac:groups=fluentd.fluent.io,resources=fluentdconfigs/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=fluentd.fluent.io,resources=fluentdconfigs/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the FluentdConfig object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.

// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.8.3/pkg/reconcile
func (r *FluentdConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = r.Log.WithValues("fluendconfig", req.NamespacedName)

	// Get Fluentd instances located ns
	var fluentdList fluentdv1alpha1.FluentdList

	if err := r.List(ctx, &fluentdList); err != nil {
		if errors.IsNotFound(err) {
			r.Log.V(1).Info("can not find fluentd CR definition.")
			return ctrl.Result{Requeue: true, RequeueAfter: time.Duration(1)}, nil
		}
		return ctrl.Result{}, err
	}

	for _, fd := range fluentdList.Items {
		// Get the selector contained in this fluentd instance
		fdSelector, err := metav1.LabelSelectorAsSelector(&fd.Spec.FluentdCfgSelector)
		if err != nil {
			// Patch this fluentd instance if the selectors exsits errors
			if err := r.PatchObjectErrors(ctx, &fd, err.Error()); err != nil {
				return ctrl.Result{}, err
			}
		}

		// A secret loader supports method to store the targeted fluentd config to the fd namespace, the the fd instance can share it.
		sl := plugins.NewSecretLoader(r.Client, fd.Namespace, r.Log)

		// pgr acts as a global plugins to store the related plugin resources
		pgr := fluentdv1alpha1.NewGlobalPluginResources("main")

		// Firstly, we will combine the defined global inputs.
		// Each cluster/namespace fluentd config will generate its own filters/outputs plugins with its cfgId/cfgLabel,
		// and finally they would combine here together.
		pgr.CombineGlobalInputsPlugins(sl, fd.Spec.GlobalInputs)

		// globalCfgLabels stores cfgLabels, the same cfg label is not allowed.
		globalCfgLabels := make(map[string]bool)

		// combine cluster cfgs
		if err := r.ClusterCfgsForFluentd(ctx, fdSelector, sl, pgr, globalCfgLabels); err != nil {
			return ctrl.Result{}, err
		}

		// combine namespaced cfgs
		if err := r.CfgsForFluentd(ctx, fdSelector, sl, pgr, globalCfgLabels); err != nil {
			return ctrl.Result{}, err
		}

		// Get fluentd workers
		var workers int32 = 1
		var enableMultiWorkers bool
		if fd.Spec.Workers != nil {
			workers = *fd.Spec.Workers
		}

		if workers > 1 {
			enableMultiWorkers = true
		}

		// Create or update the global main app secret of the fluentd instance in its namespace.
		mainAppCfg, err := pgr.RenderMainConfig(enableMultiWorkers)
		if err != nil {
			return ctrl.Result{}, err
		}

		secName := fmt.Sprintf("%s-config", fd.Name)

		sec := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      secName,
				Namespace: fd.Namespace,
			},
		}

		if _, err := controllerutil.CreateOrPatch(ctx, r.Client, sec, func() error {
			sec.Data = map[string][]byte{
				FluentdSecretMainKey:   []byte(FlUENT_INCLUDE),
				FluentdSecretAppKey:    []byte(mainAppCfg),
				FluentdSecretSystemKey: []byte(fmt.Sprintf(SYSTEM, workers)),
				FluentdSecretLogKey:    []byte(FLUENTD_LOG),
			}
			// The current fd owns the namespaced secret.
			sec.SetOwnerReferences(nil)
			if err := ctrl.SetControllerReference(&fd, sec, r.Scheme); err != nil {
				return err
			}
			return nil
		}); err != nil {
			return ctrl.Result{}, err
		}

		r.Log.Info("Main configuration has updated", "logging-control-plane", fd.Namespace, "fd", fd.Name, "secret", secName)

	}

	return ctrl.Result{}, nil
}

// ClusterCfgsForFluentd combines all cluster cfgs selected by this fd
func (r *FluentdConfigReconciler) ClusterCfgsForFluentd(
	ctx context.Context, fdSelector labels.Selector, sl plugins.SecretLoader, pgr *fluentdv1alpha1.PluginResources,
	globalCfgLabels map[string]bool) error {

	var clustercfgs fluentdv1alpha1.ClusterFluentdConfigList
	// Use fluentd selector to match the cluster config.
	if err := r.List(ctx, &clustercfgs, client.MatchingLabelsSelector{Selector: fdSelector}); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}

	allNamespaces := make([]string, 0)

	for _, cfg := range clustercfgs.Items {
		// If the field watchedNamespaces is empty, all namesapces will be watched.
		watchedNamespaces := cfg.GetWatchedNamespaces()

		if len(watchedNamespaces) == 0 {
			if len(allNamespaces) == 0 {
				var namespaceList corev1.NamespaceList
				if err := r.List(ctx, &namespaceList); err != nil {
					return err
				}

				for _, item := range namespaceList.Items {
					allNamespaces = append(allNamespaces, item.Name)
				}
			}

			cfg.Spec.WatchedNamespaces = allNamespaces
		}

		// Build the inner router for this cfg.
		// Each cfg is a workflow.
		cfgRouter, err := pgr.BuildCfgRouter(&cfg)
		if err != nil {
			return err
		}

		cfgRouterLabel := fmt.Sprint(*cfgRouter.Label)
		if err := r.registerCfgLabel(cfgRouterLabel, globalCfgLabels); err != nil {
			r.Log.V(1).Info(err.Error())
			return err
		}

		// list all cluster CRs
		clusterfilters, clusteroutputs, err := r.ListClusterLevelResources(ctx, cfg.GetCfgId(), &cfg.Spec.FilterSelector, &cfg.Spec.OutputSelector)
		if err != nil {
			return err
		}

		// The errors array patched to this cfg if this array is not empty.
		errs := make([]string, 0)

		// Combine the filter/output pluginstores in this fluentd config.
		cfgResouces, combinedErrs := pgr.PatchAndFilterClusterLevelResources(sl, cfg.GetCfgId(), clusterfilters, clusteroutputs)
		pgr.WithCfgResources(cfgRouterLabel, cfgResouces)
		errs = append(errs, combinedErrs...)

		if len(errs) > 0 {
			err = r.PatchObjectErrors(ctx, &cfg, strings.Join(errs, ","))
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// CfgsForFluentd combines all namespaced cfgs selected by this fd
func (r *FluentdConfigReconciler) CfgsForFluentd(ctx context.Context, fdSelector labels.Selector, sl plugins.SecretLoader,
	pgr *fluentdv1alpha1.PluginResources, globalCfgLabels map[string]bool) error {

	var cfgs fluentdv1alpha1.FluentdConfigList
	// Use fluentd selector to match the namespaced configs.
	if err := r.List(ctx, &cfgs, client.MatchingLabelsSelector{Selector: fdSelector}); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}

	for _, cfg := range cfgs.Items {
		// build the inner router for this cfg.
		cfgRouter, err := pgr.BuildCfgRouter(&cfg)
		if err != nil {
			return err
		}

		// register routeLabel, the same routelabel is not allowed.
		cfgRouterLabel := fmt.Sprint(*cfgRouter.Label)
		if err := r.registerCfgLabel(cfgRouterLabel, globalCfgLabels); err != nil {
			r.Log.V(1).Info(err.Error())
			return err
		}

		// list all cluster CRs
		clusterfilters, clusteroutputs, err := r.ListClusterLevelResources(ctx, cfg.GetCfgId(), &cfg.Spec.FilterSelector, &cfg.Spec.OutputSelector)
		if err != nil {
			return err
		}

		// list all namespaced CRs
		filters, outputs, err := r.ListNamespacedLevelResources(ctx, cfg.Namespace, cfg.GetCfgId(), &cfg.Spec.FilterSelector, &cfg.Spec.OutputSelector)
		if err != nil {
			return err
		}

		// The errors array patched to this cfg if this array is not empty.
		errs := make([]string, 0)

		// Combine the cluster filter/output pluginstores in this fluentd config.
		clustercfgResouces, cerrs := pgr.PatchAndFilterClusterLevelResources(sl, cfg.GetCfgId(), clusterfilters, clusteroutputs)
		errs = append(errs, cerrs...)

		// Combine the namespaced filter/output pluginstores in this fluentd config.
		cfgResouces, nerrs := pgr.PatchAndFilterNamespacedLevelResources(sl, cfg.GetCfgId(), filters, outputs)
		cfgResouces.FilterPlugins = append(cfgResouces.FilterPlugins, clustercfgResouces.FilterPlugins...)
		cfgResouces.OutputPlugins = append(cfgResouces.OutputPlugins, clustercfgResouces.OutputPlugins...)
		pgr.WithCfgResources(cfgRouterLabel, cfgResouces)
		errs = append(errs, nerrs...)

		if len(errs) > 0 {
			err = r.PatchObjectErrors(ctx, &cfg, strings.Join(errs, ","))
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// registerCfgLabel registers a cfglabel for this clustercfg/cfg
func (r *FluentdConfigReconciler) registerCfgLabel(cfgLabel string, globalCfgLabels map[string]bool) error {
	// cfgRouterLabel contains the important information for this cfg.
	if ok := globalCfgLabels[cfgLabel]; ok {
		return fmt.Errorf("the current configuration already exists: %s", cfgLabel)
	}

	// register the cfg labels, the same cfg labels is not allowed.
	globalCfgLabels[cfgLabel] = true
	return nil
}

func (r *FluentdConfigReconciler) ListClusterLevelResources(ctx context.Context, cfgId string,
	filterSelector, outputSelector *metav1.LabelSelector) ([]fluentdv1alpha1.ClusterFilter, []fluentdv1alpha1.ClusterOutput, error) {
	// List all filters matching the label selector.
	var clusterfilters fluentdv1alpha1.ClusterFilterList
	selector, err := metav1.LabelSelectorAsSelector(filterSelector)
	if err != nil {
		return nil, nil, err
	}
	if err = r.List(ctx, &clusterfilters, client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return nil, nil, err
	}

	// List all outputs matching the label selector.
	var clusteroutputs fluentdv1alpha1.ClusterOutputList
	selector, err = metav1.LabelSelectorAsSelector(outputSelector)
	if err != nil {
		return nil, nil, err
	}
	if err = r.List(ctx, &clusteroutputs, client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return nil, nil, err
	}

	return clusterfilters.Items, clusteroutputs.Items, nil
}

func (r *FluentdConfigReconciler) ListNamespacedLevelResources(ctx context.Context, namespace, cfgId string,
	filterSelector, outputSelector *metav1.LabelSelector) ([]fluentdv1alpha1.Filter, []fluentdv1alpha1.Output, error) {
	// List and patch the related cluster CRs
	var filters fluentdv1alpha1.FilterList
	selector, err := metav1.LabelSelectorAsSelector(filterSelector)
	if err != nil {
		return nil, nil, err
	}
	if err = r.List(ctx, &filters, client.InNamespace(namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return nil, nil, err
	}

	// List all outputs matching the label selector.
	var outputs fluentdv1alpha1.OutputList
	selector, err = metav1.LabelSelectorAsSelector(outputSelector)
	if err != nil {
		return nil, nil, err
	}
	if err = r.List(ctx, &outputs, client.InNamespace(namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return nil, nil, err
	}

	return filters.Items, outputs.Items, nil
}

// PatchObjectErrors patches the errors to the obj
func (r *FluentdConfigReconciler) PatchObjectErrors(ctx context.Context, obj client.Object, errs string) error {
	switch o := obj.(type) {
	case *fluentdv1alpha1.ClusterFluentdConfig:
		o.Status.Errors = errs
		err := r.Status().Patch(ctx, o, client.MergeFromWithOptions(o))
		if err != nil {
			return err
		}
	case *fluentdv1alpha1.FluentdConfig:
		o.Status.Errors = errs
		err := r.Status().Patch(ctx, o, client.MergeFromWithOptions(o))
		if err != nil {
			return err
		}
	case *fluentdv1alpha1.Fluentd:
		o.Status.Errors = errs
		err := r.Status().Patch(ctx, o, client.MergeFromWithOptions(o))
		if err != nil {
			return err
		}
	default:
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *FluentdConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &corev1.ServiceAccount{}, fluentdOwnerKey, func(rawObj client.Object) []string {
		// grab the job object, extract the owner.
		sa := rawObj.(*corev1.ServiceAccount)
		owner := metav1.GetControllerOf(sa)
		if owner == nil {
			return nil
		}
		// Make sure it's a Fluentd. If so, return it.
		if owner.APIVersion != fluentdApiGVStr || owner.Kind != "Fluentd" {
			return nil
		}
		return []string{owner.Name}
	}); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&fluentdv1alpha1.Fluentd{}).
		Owns(&corev1.Secret{}).
		Watches(&source.Kind{Type: &fluentdv1alpha1.ClusterFluentdConfig{}}, &handler.EnqueueRequestForObject{}).
		Watches(&source.Kind{Type: &fluentdv1alpha1.FluentdConfig{}}, &handler.EnqueueRequestForObject{}).
		Watches(&source.Kind{Type: &fluentdv1alpha1.Filter{}}, &handler.EnqueueRequestForObject{}).
		Watches(&source.Kind{Type: &fluentdv1alpha1.ClusterFilter{}}, &handler.EnqueueRequestForObject{}).
		Watches(&source.Kind{Type: &fluentdv1alpha1.Output{}}, &handler.EnqueueRequestForObject{}).
		Watches(&source.Kind{Type: &fluentdv1alpha1.ClusterOutput{}}, &handler.EnqueueRequestForObject{}).
		Complete(r)
}
