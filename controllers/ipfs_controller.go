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
	crand "crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	mrand "math/rand"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	clusterv1alpha1 "github.com/redhat-et/ipfs-operator/api/v1alpha1"

	ci "github.com/libp2p/go-libp2p-crypto"
	peer "github.com/libp2p/go-libp2p-peer"
)

func init() {
	seed := make([]byte, 8)
	_, err := crand.Read(seed)
	if err != nil {
		panic(err)
	}

	useed, _ := binary.Uvarint(seed)
	mrand.Seed(int64(useed))
}

const (
	finalizer = "openshift.ifps.cluster"
)

// IpfsReconciler reconciles a Ipfs object
type IpfsReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=*,resources=*,verbs=get;list
//+kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=cluster.ipfs.io,resources=ipfs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=cluster.ipfs.io,resources=ipfs/finalizers,verbs=update
//+kubebuilder:rbac:groups=cluster.ipfs.io,resources=ipfs/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch;create;update;patch;delete

func (r *IpfsReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)
	// Fetch the Ipfs instance
	instance := &clusterv1alpha1.Ipfs{}
	if err := r.Get(ctx, req.NamespacedName, instance); err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			log.Info("Ipfs resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request.
		log.Error(err, "Failed to get Ipfs")
		return ctrl.Result{}, err
	}

	// Add finalizer for this CR
	if !controllerutil.ContainsFinalizer(instance, finalizer) {
		controllerutil.AddFinalizer(instance, finalizer)
		err := r.Update(ctx, instance)
		if err != nil {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{Requeue: true}, nil
	}

	if instance.DeletionTimestamp != nil {
		controllerutil.RemoveFinalizer(instance, finalizer)
		return ctrl.Result{}, r.Update(ctx, instance)
	}

	priv, peerid, err := newKey()
	if err != nil {
		log.Error(err, "cannot generate new key")
		return ctrl.Result{}, nil
	}
	privBytes, err := priv.Bytes()
	if err != nil {
		log.Error(err, "cannot get bytes from private key")
		return ctrl.Result{}, nil
	}
	privStr := base64.StdEncoding.EncodeToString(privBytes)

	clusSec, err := newClusterSecret()
	if err != nil {
		log.Error(err, "cannot generate new cluster secret")
		return ctrl.Result{}, nil
	}

	sa := r.serviceAccount(instance)
	svc, svcName := r.serviceCluster(instance)
	cmScripts, cmScriptName := r.configMapScripts(instance)
	cmConfig, cmConfigName := r.configMapConfig(instance, peerid.String())
	secConfig, secConfigName := r.secretConfig(instance, []byte(clusSec), []byte(privStr))
	sset := r.statefulSet(instance, svcName, secConfigName, cmConfigName, cmScriptName)

	requeue := false
	requeue = requeue || r.createOrPatch(ctx, sa, sa.DeepCopy(), "service account") != nil
	requeue = requeue || r.createOrPatch(ctx, svc, svc.DeepCopy(), "service") != nil
	requeue = requeue || r.createOrPatch(ctx, cmScripts, cmScripts.DeepCopy(), "scripts configmap") != nil
	requeue = requeue || r.createOrPatch(ctx, cmConfig, cmConfig.DeepCopy(), "config configmap") != nil
	requeue = requeue || r.createOrPatch(ctx, secConfig, secConfig.DeepCopy(), "config secret") != nil
	requeue = requeue || r.createOrPatch(ctx, sset, sset.DeepCopy(), "statefulset") != nil

	// requeue = requeue || r.createOrUpdate(ctx, sa, "service account") != nil
	// requeue = requeue || r.createOrUpdate(ctx, svc, "service") != nil
	// requeue = requeue || r.createOrUpdate(ctx, cmScripts, "scripts configmap") != nil
	// requeue = requeue || r.createOrUpdate(ctx, cmConfig, "config configmap") != nil
	// requeue = requeue || r.createOrUpdate(ctx, secConfig, "config secret") != nil
	// requeue = requeue || r.createOrUpdate(ctx, sset, "statefulset") != nil
	return ctrl.Result{Requeue: requeue}, nil
}

func (r *IpfsReconciler) createOrPatch(ctx context.Context, obj, rcvr client.Object, name string) error {
	log := ctrllog.FromContext(ctx)
	if err := r.Create(ctx, obj); err != nil {
		if errors.IsAlreadyExists(err) {
			key := client.ObjectKeyFromObject(obj)
			if err := r.Get(ctx, key, rcvr); err != nil {
				log.Error(err, "error retreiving existing endpoing")
				return err
			}
			p := client.MergeFrom(obj)
			if err := r.Patch(ctx, rcvr, p); err != nil {
				log.Error(err, "error updating "+name, "err", err)
				return err
			}
		} else {
			log.Error(err, "error creating "+name, "err", nil)
			return err
		}
	}
	return nil
}

func (r *IpfsReconciler) createOrUpdate(ctx context.Context, obj client.Object, name string) error {
	log := ctrllog.FromContext(ctx)
	if err := r.Create(ctx, obj); err != nil {
		if errors.IsAlreadyExists(err) {
			if err := r.Update(ctx, obj); err != nil {
				log.Error(err, "error updating "+name, "err", err)
				return err
			}
		} else {
			log.Error(err, "error creating "+name, "err", nil)
			return err
		}
	}
	return nil
}

func (r *IpfsReconciler) serviceAccount(m *clusterv1alpha1.Ipfs) *corev1.ServiceAccount {
	// Define a new Service Account object
	serviceAcct := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ipfs-cluster-" + m.Name,
			Namespace: m.Namespace,
		},
	}
	// Service Account reconcile finished
	ctrl.SetControllerReference(m, serviceAcct, r.Scheme)
	return serviceAcct
}

// SetupWithManager sets up the controller with the Manager.
func (r *IpfsReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&clusterv1alpha1.Ipfs{}).
		Owns(&appsv1.StatefulSet{}, builder.OnlyMetadata).
		Owns(&corev1.Service{}, builder.OnlyMetadata).
		Owns(&corev1.ServiceAccount{}, builder.OnlyMetadata).
		Owns(&corev1.Secret{}, builder.OnlyMetadata).
		Owns(&corev1.ConfigMap{}, builder.OnlyMetadata).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: 5,
		}).Complete(r)
}

func newClusterSecret() (string, error) {
	buf := make([]byte, 32)
	_, err := mrand.Read(buf)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func newKey() (ci.PrivKey, peer.ID, error) {
	priv, pub, err := ci.GenerateKeyPair(ci.Ed25519, 4096)
	if err != nil {
		return nil, "", err
	}
	peerid, err := peer.IDFromPublicKey(pub)
	if err != nil {
		return nil, "", err
	}
	return priv, peerid, nil
}
