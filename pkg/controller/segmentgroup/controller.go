/*
Copyright 2021 The Crossplane Authors.
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

package segment

import (
	"context"

	"github.com/go-openapi/runtime"
	"github.com/go-openapi/strfmt"
	"github.com/google/go-cmp/cmp"

	"github.com/pkg/errors"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"

	v1 "github.com/crossplane/crossplane-runtime/apis/common/v1"
	"github.com/crossplane/crossplane-runtime/pkg/event"
	"github.com/crossplane/crossplane-runtime/pkg/logging"
	"github.com/crossplane/crossplane-runtime/pkg/meta"
	"github.com/crossplane/crossplane-runtime/pkg/ratelimiter"
	"github.com/crossplane/crossplane-runtime/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/pkg/resource"

	zpa "github.com/haarchri/zpa-go-client/pkg/client"
	"github.com/haarchri/zpa-go-client/pkg/client/segment_group_controller"
	"github.com/haarchri/zpa-go-client/pkg/models"

	v1alpha1 "github.com/crossplane-contrib/provider-zpa/apis/segmentgroup/v1alpha1"
	zpaclient "github.com/crossplane-contrib/provider-zpa/pkg/client"
)

const (
	errNotSegmentGroup = "managed resource is not an SegmentGroup custom resource"
	errCreateFailed    = "cannot create SegmentGroup"
	errUpdateFailed    = "cannot update SegmentGroup"
	errDescribeFailed  = "cannot describe SegmentGroup"
	errDeleteFailed    = "cannot delete SegmentGroup"
)

// SetupSegmentGroup adds a controller that reconciles SegmentGroups.
func SetupSegmentGroup(mgr ctrl.Manager, l logging.Logger, rl workqueue.RateLimiter) error {
	name := managed.ControllerName(v1alpha1.SegmentGroupGroupKind)

	return ctrl.NewControllerManagedBy(mgr).
		Named(name).
		WithOptions(controller.Options{
			RateLimiter: ratelimiter.NewController(rl),
		}).
		For(&v1alpha1.SegmentGroup{}).
		Complete(managed.NewReconciler(mgr,
			resource.ManagedKind(v1alpha1.SegmentGroupGroupVersionKind),
			managed.WithExternalConnecter(&connector{kube: mgr.GetClient(), newClientFn: zpa.New}),
			managed.WithInitializers(managed.NewDefaultProviderConfig(mgr.GetClient())),
			managed.WithReferenceResolver(managed.NewAPISimpleReferenceResolver(mgr.GetClient())),
			managed.WithLogger(l.WithValues("controller", name)),
			managed.WithRecorder(event.NewAPIRecorder(mgr.GetEventRecorderFor(name)))))
}

type connector struct {
	kube        client.Client
	newClientFn func(transport runtime.ClientTransport, formats strfmt.Registry) *zpa.ZscalerPrivateAccessAPIPortal
}

type external struct {
	client *zpa.ZscalerPrivateAccessAPIPortal
	kube   client.Client
}

func (c *connector) Connect(ctx context.Context, mg resource.Managed) (managed.ExternalClient, error) {
	_, ok := mg.(*v1alpha1.SegmentGroup)
	if !ok {
		return nil, errors.New(errNotSegmentGroup)
	}

	cfg, err := zpaclient.GetConfig(ctx, c.kube, mg)
	if err != nil {
		return nil, err
	}

	client := c.newClientFn(cfg, strfmt.Default)
	return &external{client, c.kube}, nil
}

func (e *external) Observe(ctx context.Context, mg resource.Managed) (managed.ExternalObservation, error) {
	cr, ok := mg.(*v1alpha1.SegmentGroup)
	if !ok {
		return managed.ExternalObservation{}, errors.New(errNotSegmentGroup)
	}

	id := meta.GetExternalName(cr)
	if id == "" {
		return managed.ExternalObservation{
			ResourceExists:   false,
			ResourceUpToDate: false,
		}, nil
	}

	req := &segment_group_controller.GetSegmentGroupUsingGET1Params{
		Context:        ctx,
		SegmentGroupID: id,
		CustomerID:     cr.Spec.ForProvider.CustomerID,
	}
	resp, reqErr := e.client.SegmentGroupController.GetSegmentGroupUsingGET1(req)
	if reqErr != nil {
		return managed.ExternalObservation{ResourceExists: false}, errors.Wrap(resource.Ignore(IsNotFound, reqErr), errDescribeFailed)
	}

	cr.Status.AtProvider = generateObservation(resp)

	currentSpec := cr.Spec.ForProvider.DeepCopy()
	e.LateInitialize(cr, resp)

	cr.Status.SetConditions(v1.Available())

	return managed.ExternalObservation{
		ResourceExists:          true,
		ResourceUpToDate:        isUpToDate(&cr.Spec.ForProvider, resp),
		ResourceLateInitialized: !cmp.Equal(&cr.Spec.ForProvider, currentSpec),
	}, nil
}

func (e *external) Create(ctx context.Context, mg resource.Managed) (managed.ExternalCreation, error) {
	cr, ok := mg.(*v1alpha1.SegmentGroup)
	if !ok {
		return managed.ExternalCreation{}, errors.New(errNotSegmentGroup)
	}

	req := &segment_group_controller.AddSegmentGroupUsingPOST1Params{
		Context:    ctx,
		CustomerID: cr.Spec.ForProvider.CustomerID,
		SegmentGroup: &models.SegmentGroup{
			Name:                zpaclient.String(cr.Name),
			ConfigSpace:         cr.Spec.ForProvider.ConfigSpace,
			Description:         cr.Spec.ForProvider.Description,
			Enabled:             zpaclient.BoolValue(cr.Spec.ForProvider.Enabled),
			TCPKeepAliveEnabled: cr.Spec.ForProvider.TCPKeepAliveEnabled,
		},
	}

	resp, err := e.client.SegmentGroupController.AddSegmentGroupUsingPOST1(req)
	if err != nil {
		return managed.ExternalCreation{}, errors.Wrap(err, errCreateFailed)
	}

	meta.SetExternalName(cr, *zpaclient.String(resp.Payload.ID))
	return managed.ExternalCreation{
		ExternalNameAssigned: true,
	}, nil

}

func (e *external) Update(ctx context.Context, mg resource.Managed) (managed.ExternalUpdate, error) {
	cr, ok := mg.(*v1alpha1.SegmentGroup)
	if !ok {
		return managed.ExternalUpdate{}, errors.New(errNotSegmentGroup)
	}

	req := &segment_group_controller.UpdateSegmentGroupUsingPUT1Params{
		Context:        ctx,
		CustomerID:     cr.Spec.ForProvider.CustomerID,
		SegmentGroupID: meta.GetExternalName(cr),
		SegmentGroup: &models.SegmentGroup{
			Name:        zpaclient.String(cr.Name),
			ID:          meta.GetExternalName(cr),
			ConfigSpace: cr.Spec.ForProvider.ConfigSpace,
			Description: cr.Spec.ForProvider.Description,
			// update enable to false is not possible via update in the api
			Enabled:             zpaclient.BoolValue(cr.Spec.ForProvider.Enabled),
			TCPKeepAliveEnabled: cr.Spec.ForProvider.TCPKeepAliveEnabled,
		},
	}

	if _, _, err := e.client.SegmentGroupController.UpdateSegmentGroupUsingPUT1(req); err != nil {
		return managed.ExternalUpdate{}, errors.Wrap(err, errUpdateFailed)
	}

	return managed.ExternalUpdate{}, nil
}

func (e *external) Delete(ctx context.Context, mg resource.Managed) error {
	cr, ok := mg.(*v1alpha1.SegmentGroup)
	if !ok {
		return errors.New(errNotSegmentGroup)
	}

	id := meta.GetExternalName(cr)
	if id == "" {
		return errors.New(errNotSegmentGroup)
	}

	req := &segment_group_controller.DeleteSegmentGroupUsingDELETE1Params{
		Context:        ctx,
		SegmentGroupID: id,
		CustomerID:     cr.Spec.ForProvider.CustomerID,
	}

	_, err := e.client.SegmentGroupController.DeleteSegmentGroupUsingDELETE1(req)
	if err != nil {
		return errors.Wrap(err, errDeleteFailed)
	}

	return nil
}

func (e *external) LateInitialize(cr *v1alpha1.SegmentGroup, obj *segment_group_controller.GetSegmentGroupUsingGET1OK) { // nolint:gocyclo

	if cr.Spec.ForProvider.ConfigSpace == "" && obj.Payload.ConfigSpace != "" {
		cr.Spec.ForProvider.ConfigSpace = obj.Payload.ConfigSpace
	}

	if cr.Spec.ForProvider.TCPKeepAliveEnabled == "" && obj.Payload.TCPKeepAliveEnabled != "" {
		cr.Spec.ForProvider.TCPKeepAliveEnabled = obj.Payload.TCPKeepAliveEnabled
	}

}

// generateObservation generates observation for the input object segment_group_controller.GetSegmentGroupUsingGET1OK
func generateObservation(in *segment_group_controller.GetSegmentGroupUsingGET1OK) v1alpha1.Observation {
	cr := v1alpha1.Observation{}

	obj := in.Payload

	cr.CreationTime = obj.CreationTime
	cr.ID = obj.ID
	cr.ModifiedBy = obj.ModifiedBy
	cr.ModifiedTime = obj.ModifiedTime
	cr.PolicyMigrated = obj.PolicyMigrated

	return cr
}

// isUpToDate checks whether there is a change in any of the modifiable fields.
func isUpToDate(cr *v1alpha1.SegmentGroupParameters, gobj *segment_group_controller.GetSegmentGroupUsingGET1OK) bool { // nolint:gocyclo
	obj := gobj.Payload

	if !zpaclient.IsEqualString(zpaclient.StringToPtr(cr.Description), zpaclient.StringToPtr(obj.Description)) {
		return false
	}

	if !zpaclient.IsEqualString(zpaclient.StringToPtr(cr.ConfigSpace), zpaclient.StringToPtr(obj.ConfigSpace)) {
		return false
	}

	if !zpaclient.IsEqualString(zpaclient.StringToPtr(cr.TCPKeepAliveEnabled), zpaclient.StringToPtr(obj.TCPKeepAliveEnabled)) {
		return false
	}

	return true
}
