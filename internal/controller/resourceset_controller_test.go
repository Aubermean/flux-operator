// Copyright 2024 Stefan Prodan.
// SPDX-License-Identifier: AGPL-3.0

package controller

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/fluxcd/pkg/apis/meta"
	"github.com/fluxcd/pkg/runtime/conditions"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/yaml"

	fluxcdv1 "github.com/controlplaneio-fluxcd/flux-operator/api/v1"
)

func TestResourceSetReconciler_LifeCycle(t *testing.T) {
	// Disable notifications for the tests as no pod is running.
	// This is required to avoid the 30s retry loop performed by the HTTP client.
	t.Setenv("NOTIFICATIONS_DISABLED", "yes")

	g := NewWithT(t)
	reconciler := getResourceSetReconciler(t)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ns, err := testEnv.CreateNamespace(ctx, "test")
	g.Expect(err).ToNot(HaveOccurred())

	objDef := fmt.Sprintf(`
apiVersion: fluxcd.controlplane.io/v1
kind: ResourceSet
metadata:
  name: tenants
  namespace: "%[1]s"
spec:
  commonMetadata:
    annotations:
      owner: "%[1]s"
  inputs:
    - tenant: team1
    - tenant: team2
  resources:
    - apiVersion: v1
      kind: ServiceAccount
      metadata:
        name: << inputs.tenant >>-readonly
        namespace: "%[1]s"
    - apiVersion: v1
      kind: ServiceAccount
      metadata:
        name: << inputs.tenant >>-readwrite
        namespace: "%[1]s"
`, ns.Name)

	obj := &fluxcdv1.ResourceSet{}
	err = yaml.Unmarshal([]byte(objDef), obj)
	g.Expect(err).ToNot(HaveOccurred())

	// Initialize the instance.
	err = testEnv.Create(ctx, obj)
	g.Expect(err).ToNot(HaveOccurred())

	r, err := reconciler.Reconcile(ctx, reconcile.Request{
		NamespacedName: client.ObjectKeyFromObject(obj),
	})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(r.Requeue).To(BeTrue())

	// Check if the finalizer was added.
	resultInit := &fluxcdv1.ResourceSet{}
	err = testClient.Get(ctx, client.ObjectKeyFromObject(obj), resultInit)
	g.Expect(err).ToNot(HaveOccurred())

	logObjectStatus(t, resultInit)
	g.Expect(resultInit.Finalizers).To(ContainElement(fluxcdv1.Finalizer))

	r, err = reconciler.Reconcile(ctx, reconcile.Request{
		NamespacedName: client.ObjectKeyFromObject(obj),
	})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(r.Requeue).To(BeFalse())

	// Check if the instance was installed.
	result := &fluxcdv1.ResourceSet{}
	err = testClient.Get(ctx, client.ObjectKeyFromObject(obj), result)
	g.Expect(err).ToNot(HaveOccurred())

	logObjectStatus(t, result)
	g.Expect(conditions.GetReason(result, meta.ReadyCondition)).To(BeIdenticalTo(meta.ReconciliationSucceededReason))

	// Check if the inventory was updated.
	g.Expect(result.Status.Inventory.Entries).To(HaveLen(4))
	g.Expect(result.Status.Inventory.Entries).To(ContainElements(
		fluxcdv1.ResourceRef{
			ID:      fmt.Sprintf("%s_team2-readonly__ServiceAccount", ns.Name),
			Version: "v1",
		},
		fluxcdv1.ResourceRef{
			ID:      fmt.Sprintf("%s_team2-readwrite__ServiceAccount", ns.Name),
			Version: "v1",
		},
	))

	// Check if the status last applied revision was set.
	g.Expect(result.Status.LastAppliedRevision).ToNot(BeEmpty())
	lastAppliedRevision := result.Status.LastAppliedRevision

	// Check if the resources were created and labeled.
	resultSA := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team2-readwrite",
			Namespace: ns.Name,
		},
	}
	err = testClient.Get(ctx, client.ObjectKeyFromObject(resultSA), resultSA)
	g.Expect(err).ToNot(HaveOccurred())

	expectedLabel := fmt.Sprintf("resourceset.%s", fluxcdv1.GroupVersion.Group)
	g.Expect(resultSA.Labels).To(HaveKeyWithValue(expectedLabel+"/name", "tenants"))
	g.Expect(resultSA.Labels).To(HaveKeyWithValue(expectedLabel+"/namespace", ns.Name))
	g.Expect(resultSA.Annotations).To(HaveKeyWithValue("owner", ns.Name))

	// Check if events were recorded for each step.
	events := getEvents(result.Name)
	g.Expect(events).To(HaveLen(2))
	g.Expect(events[0].Reason).To(Equal("ApplySucceeded"))
	g.Expect(events[0].Message).To(ContainSubstring("team1-readonly created"))
	g.Expect(events[1].Reason).To(Equal(meta.ReconciliationSucceededReason))
	g.Expect(events[1].Message).To(HavePrefix("Reconciliation finished"))

	// Update the resource group.
	resultP := result.DeepCopy()
	resultP.SetAnnotations(map[string]string{
		fluxcdv1.ReconcileAnnotation:      fluxcdv1.EnabledValue,
		fluxcdv1.ReconcileEveryAnnotation: "1m",
	})
	resultP.Spec.Resources = resultP.Spec.Resources[:len(resultP.Spec.Resources)-1]

	err = testClient.Patch(ctx, resultP, client.MergeFrom(result))
	g.Expect(err).ToNot(HaveOccurred())

	r, err = reconciler.Reconcile(ctx, reconcile.Request{
		NamespacedName: client.ObjectKeyFromObject(obj),
	})
	g.Expect(err).ToNot(HaveOccurred())

	// Check if the instance was scheduled for reconciliation.
	g.Expect(r.RequeueAfter).To(Equal(time.Minute))

	// Check the final status.
	resultFinal := &fluxcdv1.ResourceSet{}
	err = testClient.Get(ctx, client.ObjectKeyFromObject(obj), resultFinal)
	g.Expect(err).ToNot(HaveOccurred())

	// Check if the inventory was updated.
	logObject(t, resultFinal)
	g.Expect(resultFinal.Status.Inventory.Entries).To(HaveLen(2))
	g.Expect(resultFinal.Status.Inventory.Entries).ToNot(ContainElements(
		fluxcdv1.ResourceRef{
			ID:      fmt.Sprintf("%s_team2-readwrite__ServiceAccount", ns.Name),
			Version: "v1",
		},
	))
	g.Expect(resultFinal.Status.Inventory.Entries).To(ContainElements(
		fluxcdv1.ResourceRef{
			ID:      fmt.Sprintf("%s_team1-readonly__ServiceAccount", ns.Name),
			Version: "v1",
		},
		fluxcdv1.ResourceRef{
			ID:      fmt.Sprintf("%s_team2-readonly__ServiceAccount", ns.Name),
			Version: "v1",
		},
	))

	// Check if the status last applied revision was updated.
	g.Expect(resultFinal.Status.LastAppliedRevision).ToNot(BeEmpty())
	g.Expect(resultFinal.Status.LastAppliedRevision).ToNot(BeEquivalentTo(lastAppliedRevision))

	// Check if the resources were deleted.
	err = testClient.Get(ctx, client.ObjectKeyFromObject(resultSA), resultSA)
	g.Expect(err).To(HaveOccurred())
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue())

	// Delete the resource group.
	err = testClient.Delete(ctx, obj)
	g.Expect(err).ToNot(HaveOccurred())

	r, err = reconciler.Reconcile(ctx, reconcile.Request{
		NamespacedName: client.ObjectKeyFromObject(obj),
	})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(r.IsZero()).To(BeTrue())

	// Check if the resource group was finalized.
	result = &fluxcdv1.ResourceSet{}
	err = testClient.Get(ctx, client.ObjectKeyFromObject(obj), result)
	g.Expect(err).To(HaveOccurred())
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
}

func TestResourceSetReconciler_CopyFrom(t *testing.T) {
	g := NewWithT(t)
	reconciler := getResourceSetReconciler(t)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ns, err := testEnv.CreateNamespace(ctx, "test")
	g.Expect(err).ToNot(HaveOccurred())

	objDef := fmt.Sprintf(`
apiVersion: fluxcd.controlplane.io/v1
kind: ResourceSet
metadata:
  name: tenants
  namespace: "%[1]s"
spec:
  commonMetadata:
    annotations:
      owner: "%[1]s"
  inputs:
    - tenant: team1
    - tenant: team2
  resources:
    - apiVersion: v1
      kind: ConfigMap
      metadata:
        name: << inputs.tenant >>
        namespace: "%[1]s"
        annotations:
          fluxcd.controlplane.io/copyFrom: "%[1]s/test-cm"
    - apiVersion: v1
      kind: Secret
      metadata:
        name: << inputs.tenant >>
        namespace: "%[1]s"
        annotations:
          fluxcd.controlplane.io/copyFrom: "%[1]s/test-secret"
    - apiVersion: v1
      kind: Secret
      metadata:
        name: << inputs.tenant >>-docker
        namespace: "%[1]s"
        annotations:
          fluxcd.controlplane.io/copyFrom: "%[1]s/test-secret-docker"
    - apiVersion: v1
      kind: Secret
      metadata:
        name: << inputs.tenant >>-keep-type
        namespace: "%[1]s"
        annotations:
          fluxcd.controlplane.io/copyFrom: "%[1]s/test-secret"
      type: CustomType
`, ns.Name)

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cm",
			Namespace: ns.Name,
		},
		Data: map[string]string{
			"key": "value",
		},
	}
	err = testEnv.Create(ctx, cm)
	g.Expect(err).ToNot(HaveOccurred())

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-secret",
			Namespace: ns.Name,
		},
		StringData: map[string]string{
			"key": "value",
		},
	}
	err = testEnv.Create(ctx, secret)
	g.Expect(err).ToNot(HaveOccurred())

	dockerData := `{
	"auths": {
		"ghcr.io": {
			"auth": "dXNlcjpwYXNz"
		}
	}
}`
	secretDocker := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-secret-docker",
			Namespace: ns.Name,
		},
		Type: corev1.SecretTypeDockerConfigJson,
		StringData: map[string]string{
			corev1.DockerConfigJsonKey: dockerData,
		},
	}
	err = testEnv.Create(ctx, secretDocker)
	g.Expect(err).ToNot(HaveOccurred())

	obj := &fluxcdv1.ResourceSet{}
	err = yaml.Unmarshal([]byte(objDef), obj)
	g.Expect(err).ToNot(HaveOccurred())

	err = testEnv.Create(ctx, obj)
	g.Expect(err).ToNot(HaveOccurred())

	// Initialize the ResourceSet.
	r, err := reconciler.Reconcile(ctx, reconcile.Request{
		NamespacedName: client.ObjectKeyFromObject(obj),
	})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(r.Requeue).To(BeTrue())

	// Reconcile the ResourceSet.
	r, err = reconciler.Reconcile(ctx, reconcile.Request{
		NamespacedName: client.ObjectKeyFromObject(obj),
	})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(r.Requeue).To(BeFalse())

	// Check if the ResourceSet was deployed.
	result := &fluxcdv1.ResourceSet{}
	err = testClient.Get(ctx, client.ObjectKeyFromObject(obj), result)
	g.Expect(err).ToNot(HaveOccurred())

	logObjectStatus(t, result)
	g.Expect(conditions.GetReason(result, meta.ReadyCondition)).To(BeIdenticalTo(meta.ReconciliationSucceededReason))

	// Check if the inventory was updated.
	g.Expect(result.Status.Inventory.Entries).To(HaveLen(8))
	g.Expect(result.Status.Inventory.Entries).To(ContainElements(
		fluxcdv1.ResourceRef{
			ID:      fmt.Sprintf("%s_team1__ConfigMap", ns.Name),
			Version: "v1",
		},
		fluxcdv1.ResourceRef{
			ID:      fmt.Sprintf("%s_team1__Secret", ns.Name),
			Version: "v1",
		},
		fluxcdv1.ResourceRef{
			ID:      fmt.Sprintf("%s_team1-docker__Secret", ns.Name),
			Version: "v1",
		},
		fluxcdv1.ResourceRef{
			ID:      fmt.Sprintf("%s_team1-keep-type__Secret", ns.Name),
			Version: "v1",
		},
		fluxcdv1.ResourceRef{
			ID:      fmt.Sprintf("%s_team2__ConfigMap", ns.Name),
			Version: "v1",
		},
		fluxcdv1.ResourceRef{
			ID:      fmt.Sprintf("%s_team2__Secret", ns.Name),
			Version: "v1",
		},
		fluxcdv1.ResourceRef{
			ID:      fmt.Sprintf("%s_team2-docker__Secret", ns.Name),
			Version: "v1",
		},
		fluxcdv1.ResourceRef{
			ID:      fmt.Sprintf("%s_team2-keep-type__Secret", ns.Name),
			Version: "v1",
		},
	))

	// Check if the resources were created with the copied data.
	resultCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team1",
			Namespace: ns.Name,
		},
	}
	err = testClient.Get(ctx, client.ObjectKeyFromObject(resultCM), resultCM)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(resultCM.Annotations).To(HaveKeyWithValue("owner", ns.Name))
	g.Expect(resultCM.Data).To(HaveKeyWithValue("key", "value"))

	resultSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team2",
			Namespace: ns.Name,
		},
	}
	err = testClient.Get(ctx, client.ObjectKeyFromObject(resultSecret), resultSecret)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(resultSecret.Annotations).To(HaveKeyWithValue("owner", ns.Name))
	g.Expect(resultSecret.Data).To(HaveKeyWithValue("key", []byte("value")))

	resultSecretDocker := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team2-docker",
			Namespace: ns.Name,
		},
	}
	err = testClient.Get(ctx, client.ObjectKeyFromObject(resultSecretDocker), resultSecretDocker)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(resultSecretDocker.Annotations).To(HaveKeyWithValue("owner", ns.Name))
	g.Expect(resultSecretDocker.Type).To(Equal(corev1.SecretTypeDockerConfigJson))
	g.Expect(resultSecretDocker.Data).To(HaveKeyWithValue(corev1.DockerConfigJsonKey, []byte(dockerData)))

	resultSecretCustomType := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team2-keep-type",
			Namespace: ns.Name,
		},
	}
	err = testClient.Get(ctx, client.ObjectKeyFromObject(resultSecretCustomType), resultSecretCustomType)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(resultSecretCustomType.Annotations).To(HaveKeyWithValue("owner", ns.Name))
	g.Expect(resultSecretCustomType.Type).To(Equal(corev1.SecretType("CustomType")))
	g.Expect(resultSecretCustomType.Data).To(HaveKeyWithValue("key", []byte("value")))

	// Update the source ConfigMap.
	cm.Data = map[string]string{"key1": "updated1"}
	err = testClient.Update(ctx, cm)
	g.Expect(err).ToNot(HaveOccurred())

	// Update the source Secret.
	secret.Data["key"] = []byte("updated")
	err = testClient.Update(ctx, secret)
	g.Expect(err).ToNot(HaveOccurred())

	// Reconcile the ResourceSet.
	r, err = reconciler.Reconcile(ctx, reconcile.Request{
		NamespacedName: client.ObjectKeyFromObject(obj),
	})
	g.Expect(err).ToNot(HaveOccurred())

	// Check if the ConfigMap was updated.
	finalCM := &corev1.ConfigMap{}
	err = testClient.Get(ctx, client.ObjectKeyFromObject(resultCM), finalCM)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(finalCM.Data).NotTo(HaveKeyWithValue("key", "value"))
	g.Expect(finalCM.Data).To(HaveKeyWithValue("key1", "updated1"))

	// Check if the Secret was updated.
	finalSecret := &corev1.Secret{}
	err = testClient.Get(ctx, client.ObjectKeyFromObject(resultSecret), finalSecret)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(finalSecret.Data).To(HaveKeyWithValue("key", []byte("updated")))

	// Delete the resource group.
	err = testClient.Delete(ctx, obj)
	g.Expect(err).ToNot(HaveOccurred())

	r, err = reconciler.Reconcile(ctx, reconcile.Request{
		NamespacedName: client.ObjectKeyFromObject(obj),
	})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(r.IsZero()).To(BeTrue())

	// Check if the resource group was finalized.
	result = &fluxcdv1.ResourceSet{}
	err = testClient.Get(ctx, client.ObjectKeyFromObject(obj), result)
	g.Expect(err).To(HaveOccurred())
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue())

	// Check if the resources were deleted.
	err = testClient.Get(ctx, client.ObjectKeyFromObject(resultCM), resultCM)
	g.Expect(err).To(HaveOccurred())
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
	err = testClient.Get(ctx, client.ObjectKeyFromObject(resultSecret), resultSecret)
	g.Expect(err).To(HaveOccurred())
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
}

func TestResourceSetReconciler_DependsOn(t *testing.T) {
	g := NewWithT(t)
	reconciler := getResourceSetReconciler(t)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ns, err := testEnv.CreateNamespace(ctx, "test")
	g.Expect(err).ToNot(HaveOccurred())

	objDef := fmt.Sprintf(`
apiVersion: fluxcd.controlplane.io/v1
kind: ResourceSet
metadata:
  name: tenants
  namespace: "%[1]s"
spec:
  dependsOn:
    - apiVersion: apiextensions.k8s.io/v1
      kind: CustomResourceDefinition
      name: fluxinstances.fluxcd.controlplane.io
      ready: true
      readyExpr: |
        status.conditions.filter(e, e.type == 'Established').all(e, e.status == 'True') &&
        status.storedVersions.exists(e, e =='v1')
    - apiVersion: v1
      kind: ServiceAccount
      name: test
      namespace: "%[1]s"
  resources:
    - apiVersion: v1
      kind: ServiceAccount
      metadata:
        name: readonly
        namespace: "%[1]s"
`, ns.Name)

	obj := &fluxcdv1.ResourceSet{}
	err = yaml.Unmarshal([]byte(objDef), obj)
	g.Expect(err).ToNot(HaveOccurred())

	// Initialize the instance.
	err = testEnv.Create(ctx, obj)
	g.Expect(err).ToNot(HaveOccurred())

	r, err := reconciler.Reconcile(ctx, reconcile.Request{
		NamespacedName: client.ObjectKeyFromObject(obj),
	})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(r.Requeue).To(BeTrue())

	// Reconcile with not found dependency.
	r, err = reconciler.Reconcile(ctx, reconcile.Request{
		NamespacedName: client.ObjectKeyFromObject(obj),
	})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(r.RequeueAfter).To(Equal(5 * time.Second))

	// Check if the instance was installed.
	result := &fluxcdv1.ResourceSet{}
	err = testClient.Get(ctx, client.ObjectKeyFromObject(obj), result)
	g.Expect(err).ToNot(HaveOccurred())

	logObjectStatus(t, result)
	g.Expect(conditions.GetReason(result, meta.ReadyCondition)).To(BeIdenticalTo(meta.DependencyNotReadyReason))
	g.Expect(conditions.GetMessage(result, meta.ReadyCondition)).To(ContainSubstring("\"test\" not found"))

	// Create the dependency.
	dep := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test",
			Namespace: ns.Name,
		},
	}

	err = testClient.Create(ctx, dep)
	g.Expect(err).ToNot(HaveOccurred())

	// Reconcile with ready dependencies.
	r, err = reconciler.Reconcile(ctx, reconcile.Request{
		NamespacedName: client.ObjectKeyFromObject(obj),
	})
	g.Expect(err).ToNot(HaveOccurred())

	// Check if the instance was installed.
	resultFinal := &fluxcdv1.ResourceSet{}
	err = testClient.Get(ctx, client.ObjectKeyFromObject(obj), resultFinal)
	g.Expect(err).ToNot(HaveOccurred())

	logObjectStatus(t, resultFinal)
	g.Expect(conditions.GetReason(resultFinal, meta.ReadyCondition)).To(BeIdenticalTo(meta.ReconciliationSucceededReason))

	// Delete the resource group.
	err = testClient.Delete(ctx, obj)
	g.Expect(err).ToNot(HaveOccurred())

	r, err = reconciler.Reconcile(ctx, reconcile.Request{
		NamespacedName: client.ObjectKeyFromObject(obj),
	})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(r.IsZero()).To(BeTrue())
}

func TestResourceSetReconciler_DependsOnInvalidExpression(t *testing.T) {
	g := NewWithT(t)
	reconciler := getResourceSetReconciler(t)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ns, err := testEnv.CreateNamespace(ctx, "test")
	g.Expect(err).ToNot(HaveOccurred())

	objDef := fmt.Sprintf(`
apiVersion: fluxcd.controlplane.io/v1
kind: ResourceSet
metadata:
  name: tenants
  namespace: "%[1]s"
spec:
  dependsOn:
    - apiVersion: apiextensions.k8s.io/v1
      kind: CustomResourceDefinition
      name: fluxinstances.fluxcd.controlplane.io
      ready: true
      readyExpr: status.
    - apiVersion: v1
      kind: ServiceAccount
      name: test
      namespace: "%[1]s"
  resources:
    - apiVersion: v1
      kind: ServiceAccount
      metadata:
        name: readonly
        namespace: "%[1]s"
`, ns.Name)

	obj := &fluxcdv1.ResourceSet{}
	err = yaml.Unmarshal([]byte(objDef), obj)
	g.Expect(err).ToNot(HaveOccurred())

	// Initialize the instance.
	err = testEnv.Create(ctx, obj)
	g.Expect(err).ToNot(HaveOccurred())

	r, err := reconciler.Reconcile(ctx, reconcile.Request{
		NamespacedName: client.ObjectKeyFromObject(obj),
	})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(r.Requeue).To(BeTrue())

	// Reconcile with invalid expression.
	r, err = reconciler.Reconcile(ctx, reconcile.Request{
		NamespacedName: client.ObjectKeyFromObject(obj),
	})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(r.RequeueAfter).To(Equal(time.Duration(0)))

	// Check if the instance was installed.
	result := &fluxcdv1.ResourceSet{}
	err = testClient.Get(ctx, client.ObjectKeyFromObject(obj), result)
	g.Expect(err).ToNot(HaveOccurred())

	logObjectStatus(t, result)
	g.Expect(conditions.GetReason(result, meta.ReadyCondition)).To(BeIdenticalTo(meta.InvalidCELExpressionReason))
	g.Expect(conditions.GetMessage(result, meta.ReadyCondition)).To(ContainSubstring("failed to parse expression"))
}

func TestResourceSetReconciler_Impersonation(t *testing.T) {
	g := NewWithT(t)
	reconciler := getResourceSetReconciler(t)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ns, err := testEnv.CreateNamespace(ctx, "test")
	g.Expect(err).ToNot(HaveOccurred())

	objDef := fmt.Sprintf(`
apiVersion: fluxcd.controlplane.io/v1
kind: ResourceSet
metadata:
  name: test
  namespace: "%[1]s"
spec:
  serviceAccountName: flux-operator
  resources:
    - apiVersion: v1
      kind: ConfigMap
      metadata:
        name: test
        namespace: "%[1]s"
`, ns.Name)

	obj := &fluxcdv1.ResourceSet{}
	err = yaml.Unmarshal([]byte(objDef), obj)
	g.Expect(err).ToNot(HaveOccurred())

	// Initialize the instance.
	err = testEnv.Create(ctx, obj)
	g.Expect(err).ToNot(HaveOccurred())

	r, err := reconciler.Reconcile(ctx, reconcile.Request{
		NamespacedName: client.ObjectKeyFromObject(obj),
	})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(r.Requeue).To(BeTrue())

	// Reconcile with missing service account.
	r, err = reconciler.Reconcile(ctx, reconcile.Request{
		NamespacedName: client.ObjectKeyFromObject(obj),
	})
	g.Expect(err).To(HaveOccurred())

	// Check if the instance was installed.
	result := &fluxcdv1.ResourceSet{}
	err = testClient.Get(ctx, client.ObjectKeyFromObject(obj), result)
	g.Expect(err).ToNot(HaveOccurred())

	logObjectStatus(t, result)
	g.Expect(conditions.GetReason(result, meta.ReadyCondition)).To(BeIdenticalTo(meta.ReconciliationFailedReason))

	// Create the service account and role binding.
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "flux-operator",
			Namespace: ns.Name,
		},
	}

	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "flux-operator",
			Namespace: ns.Name,
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      "flux-operator",
				Namespace: ns.Name,
			},
		},
		RoleRef: rbacv1.RoleRef{
			Kind: "ClusterRole",
			Name: "cluster-admin",
		},
	}

	err = testClient.Create(ctx, sa)
	g.Expect(err).ToNot(HaveOccurred())
	err = testClient.Create(ctx, rb)
	g.Expect(err).ToNot(HaveOccurred())

	// Reconcile with existing service account.
	r, err = reconciler.Reconcile(ctx, reconcile.Request{
		NamespacedName: client.ObjectKeyFromObject(obj),
	})
	g.Expect(err).ToNot(HaveOccurred())

	// Check if the instance was installed.
	resultFinal := &fluxcdv1.ResourceSet{}
	err = testClient.Get(ctx, client.ObjectKeyFromObject(obj), resultFinal)
	g.Expect(err).ToNot(HaveOccurred())

	logObjectStatus(t, resultFinal)
	g.Expect(conditions.GetReason(resultFinal, meta.ReadyCondition)).To(BeIdenticalTo(meta.ReconciliationSucceededReason))

	// Delete the resource group.
	err = testClient.Delete(ctx, obj)
	g.Expect(err).ToNot(HaveOccurred())

	r, err = reconciler.Reconcile(ctx, reconcile.Request{
		NamespacedName: client.ObjectKeyFromObject(obj),
	})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(r.IsZero()).To(BeTrue())
}

func getResourceSetReconciler(t *testing.T) *ResourceSetReconciler {
	tmpDir := t.TempDir()
	err := os.WriteFile(fmt.Sprintf("%s/kubeconfig", tmpDir), testKubeConfig, 0644)
	if err != nil {
		panic(fmt.Sprintf("failed to create the testenv-admin user kubeconfig: %v", err))
	}

	// Set the kubeconfig environment variable for the impersonator.
	t.Setenv("KUBECONFIG", fmt.Sprintf("%s/kubeconfig", tmpDir))

	return &ResourceSetReconciler{
		Client:        testClient,
		APIReader:     testClient,
		Scheme:        NewTestScheme(),
		StatusManager: controllerName,
		EventRecorder: testEnv.GetEventRecorderFor(controllerName),
	}
}
