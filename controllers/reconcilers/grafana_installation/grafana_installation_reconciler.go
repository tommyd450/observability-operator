package grafana_installation

import (
	"context"
	"strings"

	"github.com/go-logr/logr"
	coreosv1 "github.com/operator-framework/api/pkg/operators/v1"
	"github.com/operator-framework/api/pkg/operators/v1alpha1"
	v1 "github.com/redhat-developer/observability-operator/v3/api/v1"
	"github.com/redhat-developer/observability-operator/v3/controllers/model"
	"github.com/redhat-developer/observability-operator/v3/controllers/reconcilers"
	"github.com/redhat-developer/observability-operator/v3/controllers/utils"
	v12 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

type Reconciler struct {
	client client.Client
	logger logr.Logger
}

func NewReconciler(client client.Client, logger logr.Logger) reconcilers.ObservabilityReconciler {
	return &Reconciler{
		client: client,
		logger: logger,
	}
}

func (r *Reconciler) Cleanup(ctx context.Context, cr *v1.Observability) (v1.ObservabilityStageStatus, error) {
	source := model.GetGrafanaCatalogSource(cr)
	err := r.client.Delete(ctx, source)
	if err != nil && !errors.IsNotFound(err) {
		return v1.ResultFailed, err
	}

	subscription := model.GetGrafanaSubscription(cr)
	err = r.client.Delete(ctx, subscription)
	if err != nil && !errors.IsNotFound(err) {
		return v1.ResultFailed, err
	}

	operatorgroup := model.GetGrafanaOperatorGroup(cr)
	err = r.client.Delete(ctx, operatorgroup)
	if err != nil && !errors.IsNotFound(err) {
		return v1.ResultFailed, err
	}

	// We have to remove the grafana operator deployment manually
	deployments := &v12.DeploymentList{}
	opts := &client.ListOptions{
		Namespace: cr.Namespace,
	}
	err = r.client.List(ctx, deployments, opts)
	if err != nil {
		return v1.ResultFailed, err
	}

	for _, deployment := range deployments.Items {
		if deployment.Name == "grafana-operator" {
			err = r.client.Delete(ctx, &deployment)
			if err != nil && !errors.IsNotFound(err) {
				return v1.ResultFailed, err
			}
		}
	}

	return v1.ResultSuccess, nil
}

func (r *Reconciler) Reconcile(ctx context.Context, cr *v1.Observability, s *v1.ObservabilityStatus) (v1.ObservabilityStageStatus, error) {
	// Remove old subscriptions
	status, err := r.deleteUnrequestedSubscriptions(ctx, cr)
	if status != v1.ResultSuccess {
		return status, err
	}

	// Grafana catalog source
	status, err = r.reconcileCatalogSource(ctx, cr)
	if status != v1.ResultSuccess {
		return status, err
	}

	// Grafana subscription
	status, err = r.reconcileSubscription(ctx, cr)
	if status != v1.ResultSuccess {
		return status, err
	}

	// Observability operator group
	status, err = r.reconcileOperatorgroup(ctx, cr)
	if status != v1.ResultSuccess {
		return status, err
	}

	status, err = r.waitForGrafanaOperator(ctx, cr)
	if status != v1.ResultSuccess {
		return status, err
	}

	return v1.ResultSuccess, nil
}

func (r *Reconciler) deleteUnrequestedSubscriptions(ctx context.Context, cr *v1.Observability) (v1.ObservabilityStageStatus, error) {
	grafanaSubscription := model.GetGrafanaSubscription(cr)
	list := &v1alpha1.SubscriptionList{}
	opts := &client.ListOptions{
		Namespace: cr.Namespace,
	}

	err := r.client.List(ctx, list, opts)
	if err != nil {
		return v1.ResultFailed, err
	}

	var foundOldSubscription = false
	for _, subscription := range list.Items {
		if subscription.Name == grafanaSubscription.Name &&
			subscription.Spec.CatalogSourceNamespace == "openshift-marketplace" &&
			subscription.Spec.CatalogSource == "community-operators" {
			err = r.client.Delete(ctx, &subscription)
			foundOldSubscription = true
			if err != nil {
				return v1.ResultFailed, err
			}
		}
	}

	// If no old (pre product) subscription is found, we can just exit here
	if !foundOldSubscription {
		return v1.ResultSuccess, nil
	}

	// Remove the old CSV
	csvList := &v1alpha1.ClusterServiceVersionList{}
	err = r.client.List(ctx, csvList, opts)
	if err != nil {
		return v1.ResultFailed, err
	}

	for _, csv := range csvList.Items {
		if csv.Namespace == cr.Namespace && strings.HasPrefix(csv.Name, "grafana-operator.") {
			err := r.client.Delete(ctx, &csv)
			if err != nil && !errors.IsNotFound(err) {
				return v1.ResultFailed, err
			}
			return v1.ResultInProgress, nil
		}
	}

	return v1.ResultSuccess, nil
}

func (r *Reconciler) reconcileCatalogSource(ctx context.Context, cr *v1.Observability) (v1.ObservabilityStageStatus, error) {
	source := model.GetGrafanaCatalogSource(cr)
	version := model.GetGrafanaOperatorVersion(cr)

	_, err := controllerutil.CreateOrUpdate(ctx, r.client, source, func() error {
		source.Spec = v1alpha1.CatalogSourceSpec{
			SourceType: v1alpha1.SourceTypeGrpc,
			Image:      "quay.io/rhoas/grafana-operator-index:" + version,
		}
		return nil
	})

	if err != nil {
		return v1.ResultFailed, err
	}

	return v1.ResultSuccess, nil
}

func (r *Reconciler) reconcileSubscription(ctx context.Context, cr *v1.Observability) (v1.ObservabilityStageStatus, error) {
	subscription := model.GetGrafanaSubscription(cr)
	source := model.GetGrafanaCatalogSource(cr)
	version := model.GetGrafanaOperatorVersion(cr)

	_, err := controllerutil.CreateOrUpdate(ctx, r.client, subscription, func() error {
		subscription.Spec = &v1alpha1.SubscriptionSpec{
			CatalogSource:          source.Name,
			CatalogSourceNamespace: source.Namespace,
			Package:                "grafana-operator",
			Channel:                "alpha",
			StartingCSV:            "grafana-operator." + version,
			Config:                 v1alpha1.SubscriptionConfig{Resources: model.GetGrafanaOperatorResourceRequirement(cr)},
		}
		return nil
	})

	if err != nil {
		return v1.ResultFailed, err
	}

	return v1.ResultSuccess, nil
}

func (r *Reconciler) reconcileOperatorgroup(ctx context.Context, cr *v1.Observability) (v1.ObservabilityStageStatus, error) {
	exists, err := utils.HasOperatorGroupForNamespace(ctx, r.client, cr.Namespace)
	if err != nil {
		return v1.ResultFailed, err
	}

	if exists {
		return v1.ResultSuccess, nil
	}

	operatorgroup := model.GetGrafanaOperatorGroup(cr)

	_, err = controllerutil.CreateOrUpdate(ctx, r.client, operatorgroup, func() error {
		operatorgroup.Spec = coreosv1.OperatorGroupSpec{
			TargetNamespaces: []string{cr.Namespace},
		}
		return nil
	})

	if err != nil {
		return v1.ResultFailed, err
	}

	return v1.ResultSuccess, nil
}

func (r *Reconciler) waitForGrafanaOperator(ctx context.Context, cr *v1.Observability) (v1.ObservabilityStageStatus, error) {
	// We have to remove the prometheus operator deployment manually
	deployments := &v12.DeploymentList{}
	opts := &client.ListOptions{
		Namespace: cr.Namespace,
	}
	err := r.client.List(ctx, deployments, opts)
	if err != nil {
		return v1.ResultFailed, err
	}

	for _, deployment := range deployments.Items {
		if strings.HasPrefix(deployment.Name, "grafana-operator") {
			if deployment.Status.ReadyReplicas > 0 {
				return v1.ResultSuccess, nil
			}
		}
	}
	return v1.ResultInProgress, nil
}
