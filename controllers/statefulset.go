package controllers

import (
	"regexp"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	clusterv1alpha1 "github.com/redhat-et/ipfs-operator/api/v1alpha1"
)

var (
	// objects need to be RFC-1123 compliant, and k8s uses this regex to test.
	// https://github.com/kubernetes/apimachinery/blob/v0.24.2/pkg/util/validation/validation.go
	// dns1123LabelFmt "[a-z0-9]([-a-z0-9]*[a-z0-9])?"
	// We want to match the opposite.
	notdns *regexp.Regexp = regexp.MustCompile("[[:^alnum:]]")
)

func (r *IpfsReconciler) statefulSet(m *clusterv1alpha1.Ipfs,
	sts *appsv1.StatefulSet,
	serviceName string,
	secretName string,
	configMapName string,
	configMapBootstrapScriptName string) controllerutil.MutateFn {
	ssName := "ipfs-cluster-" + m.Name

	var ipfsResources corev1.ResourceRequirements
	ipfsStorageQuantity, err := resource.ParseQuantity(m.Spec.IpfsStorage)

	// Determine resource constraints from how much we are storing.
	// for every TB of storage, Request 1GB of memory and limit if we exceed 2x this amount.
	// memory floor is 2G.
	// The CPU requirement starts at 4 cores and increases by 500m for every TB of storage
	// many block storage providers have a maximum block storage of 16TB, so in this case, the
	// biggest node we would allocate would request a minimum allocation of 16G of RAM and 12 cores
	// and would permit usage up to twice this size

	if err != nil {
		ipfsResources = corev1.ResourceRequirements{}
	} else {
		ipfsStoragei64, _ := ipfsStorageQuantity.AsInt64()
		ipfsStorageTB := ipfsStoragei64 / 1024 / 1024 / 1024 / 1024
		ipfsMilliCoresMin := 4000 + (500 * ipfsStorageTB)
		ipfsRamGBMin := ipfsStorageTB
		if ipfsRamGBMin < 2 {
			ipfsRamGBMin = 2
		}

		ipfsRamMinQuantity := resource.NewScaledQuantity(ipfsRamGBMin, resource.Giga)
		ipfsRamMaxQuantity := resource.NewScaledQuantity(2*ipfsRamGBMin, resource.Giga)
		ipfsCoresMinQuantity := resource.NewScaledQuantity(ipfsMilliCoresMin, resource.Milli)
		ipfsCoresMaxQuantity := resource.NewScaledQuantity(2*ipfsMilliCoresMin, resource.Milli)

		ipfsResources = corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceMemory: *ipfsRamMinQuantity,
				corev1.ResourceCPU:    *ipfsCoresMinQuantity,
			},
			Limits: corev1.ResourceList{
				corev1.ResourceMemory: *ipfsRamMaxQuantity,
				corev1.ResourceCPU:    *ipfsCoresMaxQuantity,
			},
		}
	}

	expected := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ssName,
			Namespace: m.Namespace,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &m.Spec.Replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app.kubernetes.io/name": ssName,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app.kubernetes.io/name": ssName,
					},
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: ssName,
					InitContainers: []corev1.Container{
						{
							Name:  "configure-ipfs",
							Image: "ipfs/go-ipfs:v0.12.2",
							Command: []string{
								"sh",
								"/custom/configure-ipfs.sh",
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "ipfs-storage",
									MountPath: "/data/ipfs",
								},
								{
									Name:      "configure-script",
									MountPath: "custom",
								},
							},
						},
					},
					Containers: []corev1.Container{
						{
							Name:            "ipfs",
							Image:           "ipfs/go-ipfs:v0.12.2",
							ImagePullPolicy: corev1.PullIfNotPresent,
							Env: []corev1.EnvVar{
								{
									Name:  "IPFS_FD_MAX",
									Value: "4096",
								},
							},
							Ports: []corev1.ContainerPort{
								{
									Name:          "swarm",
									ContainerPort: 4001,
									Protocol:      corev1.ProtocolTCP,
								},
								{
									Name:          "swarm-udp",
									ContainerPort: 4002,
									Protocol:      corev1.ProtocolUDP,
								},
								{
									Name:          "api",
									ContainerPort: 5001,
									Protocol:      corev1.ProtocolTCP,
								},
								{
									Name:          "ws",
									ContainerPort: 8081,
									Protocol:      corev1.ProtocolTCP,
								},
								{
									Name:          "http",
									ContainerPort: 8080,
									Protocol:      corev1.ProtocolTCP,
								},
							},
							LivenessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									TCPSocket: &corev1.TCPSocketAction{
										Port: intstr.FromString("swarm"),
									},
								},
								InitialDelaySeconds: 30,
								TimeoutSeconds:      10,
								PeriodSeconds:       60,
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "ipfs-storage",
									MountPath: "/data/ipfs",
								},
							},
							Resources: ipfsResources,
						},
						{
							Name:            "ipfs-cluster",
							Image:           "ipfs/ipfs-cluster:v1.0.1",
							ImagePullPolicy: corev1.PullIfNotPresent,
							Command: []string{
								"sh",
								"/custom/entrypoint.sh",
							},
							Env: []corev1.EnvVar{
								{
									Name: "BOOTSTRAP_PEER_ID",
									ValueFrom: &corev1.EnvVarSource{
										ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
											LocalObjectReference: corev1.LocalObjectReference{
												Name: configMapName,
											},
											Key: "BOOTSTRAP_PEER_ID",
										},
									},
								},
								{
									Name: "BOOTSTRAP_PEER_PRIV_KEY",
									ValueFrom: &corev1.EnvVarSource{
										SecretKeyRef: &corev1.SecretKeySelector{
											LocalObjectReference: corev1.LocalObjectReference{
												Name: secretName,
											},
											Key: "BOOTSTRAP_PEER_PRIV_KEY",
										},
									},
								},
								{
									Name: "CLUSTER_SECRET",
									ValueFrom: &corev1.EnvVarSource{
										SecretKeyRef: &corev1.SecretKeySelector{
											LocalObjectReference: corev1.LocalObjectReference{
												Name: secretName,
											},
											Key: "CLUSTER_SECRET",
										},
									},
								},
								{
									Name:  "CLUSTER_MONITOR_PING_INTERVAL",
									Value: "3m",
								},
								{
									Name:  "SVC_NAME",
									Value: serviceName,
								},
							},
							Ports: []corev1.ContainerPort{
								{
									Name:          "api-http",
									ContainerPort: 9094,
									Protocol:      corev1.ProtocolTCP,
								},
								{
									Name:          "proxy-http",
									ContainerPort: 9095,
									Protocol:      corev1.ProtocolUDP,
								},
								{
									Name:          "cluster-swarm",
									ContainerPort: 9096,
									Protocol:      corev1.ProtocolTCP,
								},
							},
							LivenessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									TCPSocket: &corev1.TCPSocketAction{
										Port: intstr.FromString("cluster-swarm"),
									},
								},
								InitialDelaySeconds: 30,
								TimeoutSeconds:      10,
								PeriodSeconds:       60,
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "cluster-storage",
									MountPath: "/data/ipfs-cluster",
								},
								{
									Name:      "configure-script",
									MountPath: "custom",
								},
							},
							Resources: corev1.ResourceRequirements{},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "configure-script",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: configMapBootstrapScriptName,
									},
								},
							},
						},
					},
				},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "cluster-storage",
					},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{
							corev1.ReadWriteOnce,
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: resource.MustParse(m.Spec.ClusterStorage),
							},
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "ipfs-storage",
					},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{
							corev1.ReadWriteOnce,
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: resource.MustParse(m.Spec.IpfsStorage),
							},
						},
					},
				},
			},
			ServiceName: serviceName,
		},
	}

	// Add a follower container for each follow.
	for _, follow := range m.Spec.Follows {
		container := corev1.Container{
			Name:            "ipfs-cluster-follow-" + notdns.ReplaceAllString(strings.ToLower(follow.Name), "-"),
			Image:           "ipfs/ipfs-cluster:v1.0.1",
			ImagePullPolicy: corev1.PullIfNotPresent,
			Command: []string{
				"ipfs-cluster-follow",
				follow.Name,
				"run",
				"--init",
				follow.Template,
			},
			VolumeMounts: []corev1.VolumeMount{
				{
					Name:      "cluster-storage",
					MountPath: "/data/ipfs-cluster",
				},
			},
		}
		expected.Spec.Template.Spec.Containers = append(expected.Spec.Template.Spec.Containers, container)
	}

	expected.DeepCopyInto(sts)
	ctrl.SetControllerReference(m, sts, r.Scheme)
	return func() error {
		sts.Spec = expected.Spec
		return nil
	}
}
