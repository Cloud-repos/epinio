package v1

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"

	"github.com/epinio/epinio/internal/application"
	"github.com/epinio/epinio/internal/domain"
	"github.com/julienschmidt/httprouter"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	k8s "k8s.io/client-go/kubernetes"

	"github.com/epinio/epinio/helpers/kubernetes"
	"github.com/epinio/epinio/helpers/tracelog"
	"github.com/epinio/epinio/internal/api/v1/models"
)

const (
	DefaultInstances = int32(1)
)

type deployParam struct {
	models.AppRef
	Git         *models.GitRef
	Route       string
	ImageURL    string
	Instances   int32
	Domain      string
	Stage       models.StageRef
	Owner       metav1.OwnerReference
	Environment models.EnvVariableList
}

// Deploy will create the deployment, service and ingress for the app
func (hc ApplicationsController) Deploy(w http.ResponseWriter, r *http.Request) APIErrors {
	ctx := r.Context()
	log := tracelog.Logger(ctx)

	p := httprouter.ParamsFromContext(ctx)
	org := p.ByName("org")
	name := p.ByName("app")

	defer r.Body.Close()
	bodyBytes, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return InternalError(err)
	}

	req := models.DeployRequest{}
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		return NewBadRequest("Failed to unmarshal deploy request ", err.Error())
	}

	if name != req.App.Name {
		return NewBadRequest("name parameter from URL does not match name param in body")
	}
	if org != req.App.Org {
		return NewBadRequest("org parameter from URL does not match org param in body")
	}

	if req.Instances != nil && *req.Instances < 0 {
		return NewBadRequest("instances param should be integer equal or greater than zero")
	}

	cluster, err := kubernetes.GetCluster(ctx)
	if err != nil {
		return InternalError(err, "failed to get access to a kube client")
	}

	// check application resource
	applicationCR, err := application.Get(ctx, cluster, req.App)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return AppIsNotKnown("cannot deploy app, application resource is missing")
		}
		return InternalError(err, "failed to get the application resource")
	}
	owner := metav1.OwnerReference{
		APIVersion: applicationCR.GetAPIVersion(),
		Kind:       applicationCR.GetKind(),
		Name:       applicationCR.GetName(),
		UID:        applicationCR.GetUID(),
	}

	// find out the number of instances
	var instances int32
	if req.Instances != nil {
		instances = int32(*req.Instances)
	} else {
		instances, err = existingReplica(ctx, cluster.Kubectl, req.App)
		if err != nil {
			return InternalError(err)
		}
	}

	mainDomain, err := domain.MainDomain(ctx)
	if err != nil {
		return InternalError(err)
	}

	// determine runtime environment, if any
	environment, err := application.Environment(ctx, cluster, req.App)
	if err != nil {
		return InternalError(err, "failed to access application runtime environment")
	}

	deployParams := deployParam{
		AppRef:      req.App,
		Git:         req.Git,
		Route:       req.Route,
		Owner:       owner,
		Environment: environment,
		Instances:   instances,
		Domain:      mainDomain,
		ImageURL:    req.ImageURL,
	}

	log.Info("deploying app", "org", org, "app", req.App)
	deployment, err := newAppDeployment(req.Stage.ID, deployParams)
	if err != nil {
		return InternalError(err)
	}
	deployment.SetOwnerReferences([]metav1.OwnerReference{owner})
	if _, err := cluster.Kubectl.AppsV1().Deployments(req.App.Org).Create(ctx, deployment, metav1.CreateOptions{}); err != nil {
		if apierrors.IsAlreadyExists(err) {
			if _, err := cluster.Kubectl.AppsV1().Deployments(req.App.Org).Update(ctx, deployment, metav1.UpdateOptions{}); err != nil {
				return InternalError(err)
			}
		} else {
			return InternalError(err)
		}
	}

	svc, err := newAppService(req.App)
	if err != nil {
		return InternalError(err)
	}
	svc.SetOwnerReferences([]metav1.OwnerReference{owner})
	if _, err := cluster.Kubectl.CoreV1().Services(req.App.Org).Create(ctx, svc, metav1.CreateOptions{}); err != nil {
		if apierrors.IsAlreadyExists(err) {
			service, err := cluster.Kubectl.CoreV1().Services(req.App.Org).Get(ctx, svc.Name, metav1.GetOptions{})
			if err != nil {
				return InternalError(err)
			}

			svc.ResourceVersion = service.ResourceVersion
			svc.Spec.ClusterIP = service.Spec.ClusterIP
			if _, err := cluster.Kubectl.CoreV1().Services(req.App.Org).Update(ctx, svc, metav1.UpdateOptions{}); err != nil {
				return InternalError(err)
			}
		} else {
			return InternalError(err)
		}
	}

	ing, err := newAppIngress(req.App, req.Route)
	if err != nil {
		return InternalError(err)
	}
	ing.SetOwnerReferences([]metav1.OwnerReference{owner})
	if _, err := cluster.Kubectl.NetworkingV1().Ingresses(req.App.Org).Create(ctx, ing, metav1.CreateOptions{}); err != nil {
		if apierrors.IsAlreadyExists(err) {
			if _, err := cluster.Kubectl.NetworkingV1().Ingresses(req.App.Org).Update(ctx, ing, metav1.UpdateOptions{}); err != nil {
				return InternalError(err)
			}
		} else {
			return InternalError(err)
		}
	}

	// Delete previous pipelineruns except for the current one
	if req.Stage.ID != "" {
		if err := application.Unstage(ctx, cluster, req.App, req.Stage.ID); err != nil {
			return InternalError(err)
		}
	}

	return nil
}

// newAppDeployment will create a deployment for the app
func newAppDeployment(stageID string, deployParams deployParam) (*appsv1.Deployment, error) {
	automountServiceAccountToken := false
	labels := map[string]string{
		"app.kubernetes.io/name":       deployParams.Name,
		"app.kubernetes.io/part-of":    deployParams.Org,
		"app.kubernetes.io/component":  "application",
		"app.kubernetes.io/managed-by": "epinio",
	}
	if stageID != "" {
		labels["epinio.suse.org/stage-id"] = stageID
	}

	deploymentData := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: deployParams.AppRef.Name,
			Labels: map[string]string{
				"app.kubernetes.io/name":       deployParams.Name,
				"app.kubernetes.io/part-of":    deployParams.Org,
				"app.kubernetes.io/component":  "application",
				"app.kubernetes.io/managed-by": "epinio",
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &deployParams.Instances,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app.kubernetes.io/name": deployParams.Name,
				},
			},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
					Annotations: map[string]string{
						"app.kubernetes.io/name": deployParams.Name,
					},
				},
				Spec: v1.PodSpec{
					ServiceAccountName:           deployParams.Org,
					AutomountServiceAccountToken: &automountServiceAccountToken,
					Containers: []v1.Container{
						{
							Name:  deployParams.Name,
							Image: deployParams.ImageURL,
							Ports: []v1.ContainerPort{
								{
									ContainerPort: 8080,
								},
							},
							Env: deployParams.Environment.ToEnvVarArray(deployParams.AppRef),
						},
					},
				},
			},
		},
	}

	return deploymentData, nil
}

// newAppService will create a service for the app
func newAppService(app models.AppRef) (*v1.Service, error) {
	serviceData := v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      app.Name,
			Namespace: app.Org,
			Annotations: map[string]string{
				"kubernetes.io/ingress.class":                      "traefik",
				"traefik.ingress.kubernetes.io/router.entrypoints": "websecure",
				"traefik.ingress.kubernetes.io/router.tls":         "true",
			},
			Labels: map[string]string{
				"app.kubernetes.io/component":  "application",
				"app.kubernetes.io/managed-by": "epinio",
				"app.kubernetes.io/name":       app.Name,
				"app.kubernetes.io/part-of":    app.Org,
			},
		},
		Spec: v1.ServiceSpec{
			Ports: []v1.ServicePort{
				{
					Port:       8080,
					Protocol:   v1.ProtocolTCP,
					TargetPort: intstr.IntOrString{IntVal: 8080},
				},
			},
			Selector: map[string]string{
				"app.kubernetes.io/component": "application",
				"app.kubernetes.io/name":      app.Name,
			},
			Type: v1.ServiceTypeClusterIP,
		},
	}

	return &serviceData, nil
}

func newAppIngress(appRef models.AppRef, route string) (*networkingv1.Ingress, error) {
	pathTypeImplementationSpecific := networkingv1.PathTypeImplementationSpecific

	ingressData := networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name: appRef.Name,
			Annotations: map[string]string{
				"traefik.ingress.kubernetes.io/router.entrypoints": "websecure",
				"traefik.ingress.kubernetes.io/router.tls":         "true",
				"kubernetes.io/ingress.class":                      "traefik",
			},
			Labels: map[string]string{
				"app.kubernetes.io/component":  "application",
				"app.kubernetes.io/managed-by": "epinio",
				"app.kubernetes.io/name":       appRef.Name,
				"app.kubernetes.io/part-of":    appRef.Org,
			},
		},
		Spec: networkingv1.IngressSpec{
			Rules: []networkingv1.IngressRule{
				{
					Host: route,
					IngressRuleValue: networkingv1.IngressRuleValue{
						HTTP: &networkingv1.HTTPIngressRuleValue{
							Paths: []networkingv1.HTTPIngressPath{
								{
									Backend: networkingv1.IngressBackend{
										Service: &networkingv1.IngressServiceBackend{
											Name: appRef.Name,
											Port: networkingv1.ServiceBackendPort{
												Number: 8080,
											},
										},
									},
									Path:     "/",
									PathType: &pathTypeImplementationSpecific,
								},
							},
						},
					},
				},
			},
			TLS: []networkingv1.IngressTLS{
				{
					Hosts: []string{
						route,
					},
					SecretName: fmt.Sprintf("%s-tls", route),
				},
			},
		},
	}

	return &ingressData, nil
}

func existingReplica(ctx context.Context, client *k8s.Clientset, app models.AppRef) (int32, error) {
	// if a deployment exists, use that deployment's replica count
	result, err := client.AppsV1().Deployments(app.Org).Get(ctx, app.Name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return DefaultInstances, nil
		}
		return 0, err
	}
	return *result.Spec.Replicas, nil
}
