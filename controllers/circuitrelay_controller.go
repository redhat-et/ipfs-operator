/*
Copyright 2022.

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
	"net"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/json"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/libp2p/go-libp2p-core/crypto"
	peer "github.com/libp2p/go-libp2p-core/peer"
	relaydaemon "github.com/libp2p/go-libp2p-relay-daemon"
	relayv2 "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
	clusterv1alpha1 "github.com/redhat-et/ipfs-operator/api/v1alpha1"
	"github.com/redhat-et/ipfs-operator/controllers/utils"

	ma "github.com/multiformats/go-multiaddr"
)

// CircuitRelayReconciler reconciles a CircuitRelay object.
type CircuitRelayReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=*,resources=*,verbs=get;list
//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=cluster.ipfs.io,resources=circuitrelays,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=cluster.ipfs.io,resources=circuitrelays/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=cluster.ipfs.io,resources=circuitrelays/finalizers,verbs=update

func (r *CircuitRelayReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	instance, err := r.ensureCircuitRelay(ctx, req)
	if err != nil {
		log.Error(err, "failed to get CircuitRelay")
		return ctrl.Result{}, err
	}

	svc := corev1.Service{}
	svcMut := r.serviceRelay(instance, &svc)
	_, err = controllerutil.CreateOrPatch(ctx, r.Client, &svc, svcMut)
	if err != nil {
		log.Error(err, "error during CreateOrPatch service", "name", instance.Name)
		return ctrl.Result{
			Requeue: true,
		}, err
	}

	// Check that we have a public address
	// if not, wait until it's available.
	rsvc := corev1.Service{}
	err = r.Get(ctx, client.ObjectKeyFromObject(&svc), &rsvc)
	if err != nil {
		log.Error(err, "error looking up service", "name", instance.Name)
		return ctrl.Result{
			Requeue: true,
		}, err
	}

	if len(rsvc.Status.LoadBalancer.Ingress) == 0 {
		log.Info("still waiting for service ingress addresses", "name", instance.Name)
		return ctrl.Result{
			RequeueAfter: time.Minute,
		}, err
	}

	maddrs, err := multiaddrsFromIngress(rsvc, svc)
	if err != nil {
		return ctrl.Result{
			Requeue: true,
		}, err
	}
	trackedObjects := make(map[client.Object]controllerutil.MutateFn)

	// Test if we have already updated the status.
	// And if not, then generate a new identity
	if instance.Status.AddrInfo.ID == "" {
		var identity []byte
		identity, err = r.generateNewIdentity(ctx, instance, maddrs)
		if err != nil {
			log.Error(err, "error generating new identity")
			return ctrl.Result{
				RequeueAfter: time.Minute,
			}, err
		}
		sec := corev1.Secret{}
		mutsec := r.secretIdentity(instance, &sec, identity)
		trackedObjects[&sec] = mutsec
	}

	if err = instance.Status.AddrInfo.Parse(); err != nil {
		log.Error(err, "cannot parse AddrInfo for relay.")
		return ctrl.Result{
			Requeue: true,
		}, err
	}

	log.Info("create or patch circuitrelay deployment with addrs", "addrs", maddrs)

	cm := corev1.ConfigMap{}
	mutcm := r.configRelay(instance, &cm)
	trackedObjects[&cm] = mutcm

	dep := appsv1.Deployment{}
	mutdep := r.deploymentRelay(instance, &dep)
	trackedObjects[&dep] = mutdep

	shouldRequeue := utils.CreateOrPatchTrackedObjects(ctx, trackedObjects, r.Client, log)
	return ctrl.Result{Requeue: shouldRequeue}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *CircuitRelayReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		// Uncomment the following line adding a pointer to an instance of the controlled resource as an argument
		For(&clusterv1alpha1.CircuitRelay{}).
		Owns(&corev1.Service{}, builder.OnlyMetadata).
		Owns(&appsv1.Deployment{}, builder.OnlyMetadata).
		Complete(r)
}

func (r *CircuitRelayReconciler) ensureCircuitRelay(
	ctx context.Context,
	req ctrl.Request,
) (*clusterv1alpha1.CircuitRelay, error) {
	instance := &clusterv1alpha1.CircuitRelay{}
	if err := r.Get(ctx, req.NamespacedName, instance); err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return nil, fmt.Errorf("%w: %s", err, "CircuitRelay resource not found")
		}
		// Error reading the object - requeue the request.
		return nil, fmt.Errorf("failed to get CircuitRelay: %w", err)
	}
	return instance, nil
}

func (r *CircuitRelayReconciler) generateNewIdentity(
	ctx context.Context,
	instance *clusterv1alpha1.CircuitRelay,
	maddrs []ma.Multiaddr,
) ([]byte, error) {
	var err error
	addrStrings := make([]string, len(maddrs))
	for i, addr := range maddrs {
		addrStrings[i] = addr.String()
	}
	instance.Status.AddrInfo.Addrs = addrStrings
	var privkey crypto.PrivKey
	var pubkey peer.ID
	privkey, pubkey, err = newKey()
	if err != nil {
		return nil, fmt.Errorf("error during key generation: %w", err)
	}
	instance.Status.AddrInfo.ID = pubkey.String()
	if err = r.Status().Update(ctx, instance); err != nil {
		return nil, fmt.Errorf("error updating status: %w", err)
	}

	var identity []byte
	identity, err = crypto.MarshalPrivateKey(privkey)
	if err != nil {
		return nil, fmt.Errorf("error marshaling private key: %w", err)
	}
	return identity, nil
}

func (r *CircuitRelayReconciler) serviceRelay(
	m *clusterv1alpha1.CircuitRelay,
	svc *corev1.Service,
) controllerutil.MutateFn {
	svcName := "libp2p-relay-daemon-" + m.Name
	expected := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      svcName,
			Namespace: m.Namespace,
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeLoadBalancer,
			Ports: []corev1.ServicePort{
				{
					Name:       "swarm",
					Protocol:   corev1.ProtocolTCP,
					Port:       portSwarm,
					TargetPort: intstr.FromString("swarm"),
				},
				// Commented out because some load balancers don't support TCP+UDP:(
				//
				// {
				// 	Name:       "swarm-udp",
				// 	Protocol:   corev1.ProtocolUDP,
				// 	Port:       4001,
				// 	TargetPort: intstr.FromString("swarm-udp"),
				// },
				//
				// Commented out.
				// TODO: support websockets and TLS.
				// {
				// 	Name:       "swarm-ws",
				// 	Protocol:   corev1.ProtocolTCP,
				// 	Port:       8081,
				// 	TargetPort: intstr.FromString("swarm-ws"),
				// },
			},
			Selector: map[string]string{
				"app.kubernetes.io/name": svcName,
			},
		},
	}
	expected.DeepCopyInto(svc)
	if err := ctrl.SetControllerReference(m, svc, r.Scheme); err != nil {
		return func() error {
			return err
		}
	}
	return func() error {
		svc.Spec = expected.Spec
		return nil
	}
}

func (r *CircuitRelayReconciler) secretIdentity(
	m *clusterv1alpha1.CircuitRelay,
	sec *corev1.Secret,
	identity []byte,
) controllerutil.MutateFn {
	secName := "libp2p-relay-daemon-identity-" + m.Name
	expected := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secName,
			Namespace: m.Namespace,
		},
		Data: map[string][]byte{
			"identity": identity,
		},
	}
	expected.DeepCopyInto(sec)
	if err := ctrl.SetControllerReference(m, sec, r.Scheme); err != nil {
		return func() error {
			return err
		}
	}
	return func() error {
		return nil
	}
}

func multiaddrsFromIngress(
	rsvc corev1.Service,
	svc corev1.Service,
) ([]ma.Multiaddr, error) {
	maddrs := make([]ma.Multiaddr, len(rsvc.Status.LoadBalancer.Ingress)*len(rsvc.Spec.Ports))
	for i, addr := range rsvc.Status.LoadBalancer.Ingress {
		ip := net.ParseIP(addr.IP)
		var iptype, address string
		switch {
		case ip.To4() != nil:
			iptype = "ip4"
			address = addr.IP
		case ip.To16() != nil:
			iptype = "ip6"
			address = addr.IP
		default:
			iptype = "dns4"
			address = addr.Hostname
		}
		for j, port := range svc.Spec.Ports {
			var maddr ma.Multiaddr
			p := strings.ToLower(string(port.Protocol))
			mastr := fmt.Sprintf("/%s/%s/%s/%d", iptype, address, p, port.Port)
			maddr, err := ma.NewMultiaddr(mastr)
			if err != nil {
				return nil, fmt.Errorf("cannot parse multiaddr (ip: %s): %w", addr.IP, err)
			}
			idx := (i * len(svc.Spec.Ports)) + j
			maddrs[idx] = maddr
		}
	}
	return maddrs, nil
}

func (r *CircuitRelayReconciler) configRelay(
	m *clusterv1alpha1.CircuitRelay,
	cm *corev1.ConfigMap,
) controllerutil.MutateFn {
	cmName := "libp2p-relay-daemon-config-" + m.Name
	announceAddrs := m.Status.AddrInfo.Addrs

	// Based on default configuration at
	// https://github.com/libp2p/go-libp2p-relay-daemon/blob/a32147234644cfef5b42a9f5ccaf99b6e6021fd4/cmd/libp2p-relay-daemon/config.go#L51-L78
	cfg := relaydaemon.Config{
		Network: relaydaemon.NetworkConfig{
			ListenAddrs: []string{
				"/ip4/0.0.0.0/tcp/4001",
				"/ip6/::/tcp/4001",
			},
			AnnounceAddrs: announceAddrs,
		},
		ConnMgr: relaydaemon.ConnMgrConfig{
			ConnMgrLo:    1024,
			ConnMgrHi:    4096,
			ConnMgrGrace: 2 * time.Minute,
		},
		RelayV1: relaydaemon.RelayV1Config{
			Enabled: false,
			// Resources: relayv1.DefaultResources(),
		},
		RelayV2: relaydaemon.RelayV2Config{
			Enabled:   true,
			Resources: relayv2.DefaultResources(),
		},
		Daemon: relaydaemon.DaemonConfig{
			PprofPort: 6060,
		},
	}

	// Explicit adjustments to relay resource limits.
	cfg.RelayV2.Resources.Limit = nil // Full relay, we need to remove this limit.
	cfg.RelayV2.Resources.ReservationTTL = time.Hour
	cfg.RelayV2.Resources.MaxReservations = 128
	cfg.RelayV2.Resources.MaxCircuits = 16
	cfg.RelayV2.Resources.BufferSize = 2048
	cfg.RelayV2.Resources.MaxReservationsPerPeer = 4
	cfg.RelayV2.Resources.MaxReservationsPerIP = 8
	cfg.RelayV2.Resources.MaxReservationsPerASN = 32

	cfgbytes, err := json.Marshal(cfg)
	if err != nil {
		return func() error {
			return err
		}
	}
	expected := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: m.Namespace,
		},
		BinaryData: map[string][]byte{
			"config.json": cfgbytes,
		},
	}
	expected.DeepCopyInto(cm)
	if err := ctrl.SetControllerReference(m, cm, r.Scheme); err != nil {
		return func() error {
			return err
		}
	}
	return func() error {
		return nil
	}
}

func (r *CircuitRelayReconciler) deploymentRelay(
	m *clusterv1alpha1.CircuitRelay,
	dep *appsv1.Deployment,
) controllerutil.MutateFn {
	depName := "libp2p-relay-daemon-" + m.Name
	expected := appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      depName,
			Namespace: m.Namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app.kubernetes.io/name": depName,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app.kubernetes.io/name": depName,
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "relay",
							Image: "coryschwartz/libp2p-relay-daemon:latest",
							Args: []string{
								"-config",
								"/config.json",
								"-id",
								"/identity",
							},
							Ports: []corev1.ContainerPort{
								{
									Name:          "swarm",
									ContainerPort: portSwarm,
									Protocol:      "TCP",
								},
								{
									// Should this port number be the same as portSwarm or should it be a different one?
									Name:          "swarm-udp",
									ContainerPort: portSwarmUDP,
									Protocol:      "UDP",
								},
								{
									Name:          "pprof",
									ContainerPort: portPprof,
									Protocol:      "UDP",
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "config",
									MountPath: "/config.json",
									SubPath:   "config.json",
								},
								{
									Name:      "identity",
									MountPath: "/identity",
									SubPath:   "identity",
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "config",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: "libp2p-relay-daemon-config-" + m.Name,
									},
								},
							},
						},
						{
							Name: "identity",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName: "libp2p-relay-daemon-identity-" + m.Name,
								},
							},
						},
					},
				},
			},
		},
	}
	expected.DeepCopyInto(dep)
	// FIXME: return an error before returning a function which errors
	if err := ctrl.SetControllerReference(m, dep, r.Scheme); err != nil {
		return func() error {
			return err
		}
	}
	return func() error {
		dep.Spec = expected.Spec
		return nil
	}
}
