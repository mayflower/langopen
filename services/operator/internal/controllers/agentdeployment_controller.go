package controllers

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"langopen.dev/operator/internal/apis/v1alpha1"
)

type AgentDeploymentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

func (r *AgentDeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var dep v1alpha1.AgentDeployment
	if err := r.Get(ctx, req.NamespacedName, &dep); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	replicas := int32(1)
	if dep.Spec.Replicas != nil {
		replicas = *dep.Spec.Replicas
	}
	runtimeClass := dep.Spec.RuntimeClassName
	if runtimeClass == "" {
		runtimeClass = "gvisor"
	}
	mode := strings.TrimSpace(dep.Spec.Mode)
	if mode == "" {
		mode = "mode_a"
	}
	image := strings.TrimSpace(dep.Spec.Image)
	if rollback := strings.TrimSpace(dep.Spec.RollbackImage); rollback != "" {
		image = rollback
	}

	apiDep := desiredAPIDeployment(&dep, image, runtimeClass, replicas)
	if err := ctrl.SetControllerReference(&dep, apiDep, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.reconcileDeployment(ctx, apiDep); err != nil {
		return ctrl.Result{}, err
	}

	workerDep := desiredWorkerDeployment(&dep, image, runtimeClass, replicas, mode)
	if err := ctrl.SetControllerReference(&dep, workerDep, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.reconcileDeployment(ctx, workerDep); err != nil {
		return ctrl.Result{}, err
	}

	svc := desiredAPIService(&dep)
	if err := ctrl.SetControllerReference(&dep, svc, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.reconcileService(ctx, svc); err != nil {
		return ctrl.Result{}, err
	}

	np := desiredNetworkPolicy(&dep)
	if err := ctrl.SetControllerReference(&dep, np, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.reconcileNetworkPolicy(ctx, np); err != nil {
		return ctrl.Result{}, err
	}

	if mode == "mode_b" {
		if err := r.reconcileSandboxResources(ctx, &dep, runtimeClass); err != nil {
			if apimeta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
				dep.Status.Ready = false
				dep.Status.ObservedGeneration = dep.Generation
				dep.Status.Message = "sandbox CRDs missing; install Agent Sandbox CRDs for mode_b"
				_ = r.Status().Update(ctx, &dep)
				return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
			}
			return ctrl.Result{}, err
		}
	}

	dep.Status.Ready = true
	dep.Status.ObservedGeneration = dep.Generation
	dep.Status.Message = fmt.Sprintf("reconciled image=%s mode=%s runtimeClass=%s", image, mode, runtimeClass)
	if err := r.Status().Update(ctx, &dep); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *AgentDeploymentReconciler) reconcileDeployment(ctx context.Context, desired *appsv1.Deployment) error {
	current := &appsv1.Deployment{}
	err := r.Get(ctx, client.ObjectKey{Name: desired.Name, Namespace: desired.Namespace}, current)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	current.Spec = desired.Spec
	return r.Update(ctx, current)
}

func (r *AgentDeploymentReconciler) reconcileService(ctx context.Context, desired *corev1.Service) error {
	current := &corev1.Service{}
	err := r.Get(ctx, client.ObjectKey{Name: desired.Name, Namespace: desired.Namespace}, current)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	clusterIP := current.Spec.ClusterIP
	current.Spec = desired.Spec
	current.Spec.ClusterIP = clusterIP
	return r.Update(ctx, current)
}

func (r *AgentDeploymentReconciler) reconcileNetworkPolicy(ctx context.Context, desired *networkingv1.NetworkPolicy) error {
	current := &networkingv1.NetworkPolicy{}
	err := r.Get(ctx, client.ObjectKey{Name: desired.Name, Namespace: desired.Namespace}, current)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	current.Spec = desired.Spec
	return r.Update(ctx, current)
}

func desiredAPIDeployment(dep *v1alpha1.AgentDeployment, image, runtimeClass string, replicas int32) *appsv1.Deployment {
	labels := map[string]string{"app": dep.Name, "component": "api"}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: dep.Name + "-api", Namespace: dep.Namespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RuntimeClassName:             &runtimeClass,
					AutomountServiceAccountToken: boolPtr(false),
					SecurityContext:              podSecurityContext(),
					Containers: []corev1.Container{{
						Name:            "api-server",
						Image:           image,
						ImagePullPolicy: corev1.PullIfNotPresent,
						Ports:           []corev1.ContainerPort{{Name: "http", ContainerPort: 8080}},
						SecurityContext: containerSecurityContext(),
					}},
				},
			},
		},
	}
}

func desiredWorkerDeployment(dep *v1alpha1.AgentDeployment, image, runtimeClass string, replicas int32, mode string) *appsv1.Deployment {
	labels := map[string]string{"app": dep.Name, "component": "worker"}
	env := []corev1.EnvVar{{Name: "RUN_MODE", Value: mode}}
	if mode == "mode_b" {
		templateName := strings.TrimSpace(dep.Spec.SandboxTemplate)
		if templateName == "" {
			templateName = dep.Name + "-sandbox-template"
		}
		env = append(env,
			corev1.EnvVar{Name: "SANDBOX_ENABLED", Value: "true"},
			corev1.EnvVar{Name: "SANDBOX_TEMPLATE_NAME", Value: templateName},
			corev1.EnvVar{Name: "SANDBOX_NAMESPACE", Value: dep.Namespace},
		)
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: dep.Name + "-worker", Namespace: dep.Namespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RuntimeClassName:             &runtimeClass,
					AutomountServiceAccountToken: boolPtr(false),
					SecurityContext:              podSecurityContext(),
					Containers: []corev1.Container{{
						Name:            "worker",
						Image:           image,
						ImagePullPolicy: corev1.PullIfNotPresent,
						Env:             env,
						SecurityContext: containerSecurityContext(),
					}},
				},
			},
		},
	}
}

func desiredAPIService(dep *v1alpha1.AgentDeployment) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: dep.Name, Namespace: dep.Namespace},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": dep.Name, "component": "api"},
			Ports: []corev1.ServicePort{{
				Name:       "http",
				Port:       80,
				TargetPort: intstr.FromInt(8080),
			}},
		},
	}
}

func desiredNetworkPolicy(dep *v1alpha1.AgentDeployment) *networkingv1.NetworkPolicy {
	egressRules := []networkingv1.NetworkPolicyEgressRule{{
		Ports: []networkingv1.NetworkPolicyPort{{
			Protocol: protocolPtr(corev1.ProtocolUDP),
			Port:     intstrPtr(53),
		}, {
			Protocol: protocolPtr(corev1.ProtocolTCP),
			Port:     intstrPtr(53),
		}},
	}}

	for _, cidr := range dep.Spec.EgressAllowlist {
		cidr = strings.TrimSpace(cidr)
		if cidr == "" {
			continue
		}
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			continue
		}
		egressRules = append(egressRules, networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{{
			IPBlock: &networkingv1.IPBlock{CIDR: cidr},
		}}})
	}

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: dep.Name + "-egress", Namespace: dep.Namespace},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": dep.Name}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress:      egressRules,
		},
	}
}

func (r *AgentDeploymentReconciler) reconcileSandboxResources(ctx context.Context, dep *v1alpha1.AgentDeployment, runtimeClass string) error {
	groupVersion := schema.GroupVersion{Group: "extensions.agents.x-k8s.io", Version: "v1alpha1"}
	templateName := strings.TrimSpace(dep.Spec.SandboxTemplate)
	if templateName == "" {
		templateName = dep.Name + "-sandbox-template"
	}

	template := &unstructured.Unstructured{}
	template.SetGroupVersionKind(groupVersion.WithKind("SandboxTemplate"))
	template.SetNamespace(dep.Namespace)
	template.SetName(templateName)
	template.Object["spec"] = map[string]any{
		"podTemplate": map[string]any{
			"spec": map[string]any{
				"runtimeClassName":             runtimeClass,
				"automountServiceAccountToken": false,
				"containers": []map[string]any{{
					"name":    "sandbox",
					"image":   dep.Spec.Image,
					"command": []string{"/bin/sh", "-c", "sleep infinity"},
					"securityContext": map[string]any{
						"runAsNonRoot":             true,
						"allowPrivilegeEscalation": false,
						"readOnlyRootFilesystem":   true,
					},
				}},
			},
		},
	}
	if err := r.reconcileUnstructured(ctx, template); err != nil {
		return err
	}

	warmPoolReplicas := int32(1)
	if dep.Spec.WarmPoolReplicas != nil {
		warmPoolReplicas = *dep.Spec.WarmPoolReplicas
	}
	warmPool := &unstructured.Unstructured{}
	warmPool.SetGroupVersionKind(groupVersion.WithKind("SandboxWarmPool"))
	warmPool.SetNamespace(dep.Namespace)
	warmPool.SetName(dep.Name + "-warm-pool")
	warmPool.Object["spec"] = map[string]any{
		"sandboxTemplateRef": map[string]any{"name": templateName},
		"replicas":           warmPoolReplicas,
	}
	return r.reconcileUnstructured(ctx, warmPool)
}

func (r *AgentDeploymentReconciler) reconcileUnstructured(ctx context.Context, desired *unstructured.Unstructured) error {
	current := &unstructured.Unstructured{}
	current.SetGroupVersionKind(desired.GroupVersionKind())
	err := r.Get(ctx, client.ObjectKey{Name: desired.GetName(), Namespace: desired.GetNamespace()}, current)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	current.Object["spec"] = desired.Object["spec"]
	return r.Update(ctx, current)
}

func podSecurityContext() *corev1.PodSecurityContext {
	uid := int64(65532)
	gid := int64(65532)
	return &corev1.PodSecurityContext{
		RunAsNonRoot: boolPtr(true),
		RunAsUser:    &uid,
		RunAsGroup:   &gid,
	}
}

func containerSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		RunAsNonRoot:             boolPtr(true),
		AllowPrivilegeEscalation: boolPtr(false),
		ReadOnlyRootFilesystem:   boolPtr(true),
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
		},
	}
}

func protocolPtr(p corev1.Protocol) *corev1.Protocol {
	return &p
}

func intstrPtr(v int32) *intstr.IntOrString {
	x := intstr.FromInt32(v)
	return &x
}

func (r *AgentDeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.AgentDeployment{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Complete(r)
}

func boolPtr(v bool) *bool { return &v }
