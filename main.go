/*
 * © 2023 Snyk Limited
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/snyk/kubernetes-scanner/build"
	"github.com/snyk/kubernetes-scanner/internal/backend"
	"github.com/snyk/kubernetes-scanner/internal/config"
	"github.com/snyk/kubernetes-scanner/licenses"

	"github.com/google/uuid"
	"golang.org/x/exp/slices"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

var setupLog = ctrl.Log.WithName("setup")

func main() {
	var (
		printVersion = flag.Bool("version", false, "print the version of the kubernetes-scanner and exit")
		configFile   = flag.String("config", "/etc/kubernetes-scanner/config.yaml", "defines the location of the config file")
		showLicenses = flag.Bool("licenses", false, "show license information")
		logOpts      = zap.Options{
			// The various `zap-` flags in this struct definition can be passed to
			// this program due to the call to BindFlags() below. None of this is
			// exposed through helm, yet - a decision we might revisit.
		}
	)

	logOpts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&logOpts)))

	if *printVersion {
		fmt.Println(build.Version())
		os.Exit(0)
	}
	if *showLicenses {
		os.Exit(licenses.Print())
	}

	cfg, err := config.Read(*configFile)
	if err != nil {
		setupLog.Error(err, "unable to read config file")
		os.Exit(1)
	}

	batchingBackend := backend.NewBatchingBackend(
		backend.New(cfg.ClusterName, cfg.Egress, ctrlmetrics.Registry),
		100, time.Second*10).Start()

	mgr, err := setupController(cfg, batchingBackend)
	if err != nil {
		setupLog.Error(err, "unable to setup controller")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

func setupController(cfg *config.Config, s store) (manager.Manager, error) {
	mgr, err := ctrl.NewManager(cfg.RestConfig, ctrl.Options{
		Scheme:                 cfg.Scheme,
		MetricsBindAddress:     cfg.MetricsAddress,
		HealthProbeBindAddress: cfg.ProbeAddress,
	})
	if err != nil {
		return nil, fmt.Errorf("unable to start manager: %w", err)
	}

	discovery, err := cfg.Discovery()
	if err != nil {
		return nil, fmt.Errorf("unable to create discovery client: %w", err)
	}

	for _, scanType := range cfg.Scanning.Types {
		gvks, err := scanType.GetGVKs(discovery, setupLog)
		if err != nil {
			return nil, fmt.Errorf("could not get GVK: %w", err)
		}

		for _, gvk := range gvks {
			if err := (&reconciler{
				Reader:       mgr.GetClient(),
				requeueAfter: cfg.Scanning.RequeueAfter.Duration,
				store:        s,
				gvk:          gvk,
				orgID:        cfg.OrganizationID,
				namespaces:   scanType.Namespaces,
			}).SetupWithManager(mgr); err != nil {
				return nil, fmt.Errorf("unable to create controller for GVK %v: %w", gvk, err)
			}
		}
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return nil, fmt.Errorf("unable to setup health check: %w", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return nil, fmt.Errorf("unable to setup readiness check: %w", err)
	}

	return mgr, nil
}

type reconciler struct {
	client.Reader
	requeueAfter time.Duration
	gvk          config.GroupVersionKind
	store
	namespaces []string
	orgID      string
}

type store interface {
	// Upsert an object into the store. If the deletedAt time is non-zero, a deletion-event should
	// be recorded. Otherwise, the store should simply ensure that the object saved in the store
	// matches the one we're providing.
	Upsert(ctx context.Context, requestID string, orgID string, kubeObjects []backend.KubeObj) error
}

func (r *reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	requestID := uuid.New().String()
	logger := log.FromContext(ctx).WithValues(
		"group", r.gvk.Group,
		"version", r.gvk.Version,
		"kind", r.gvk.Kind,
		"name", req.Name,
		"namespace", req.Namespace,
		"request_id", requestID,
	)
	logger.Info("reconciling resource")

	// as long as r.namespaces is set, we want to check it. It might be 0-length, which will skip
	// all namespaced resources. This is expected behavior.
	if req.Namespace != "" && r.namespaces != nil && !slices.Contains(r.namespaces, req.Namespace) {
		// don't set the requeueafter, we don't need it.
		logger.V(1).Info("skipping resources as namespace is ignored")
		return ctrl.Result{}, nil
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(r.gvk.GroupVersionKind)
	if err := r.Get(ctx, req.NamespacedName, obj); err != nil {
		if !kerrors.IsNotFound(err) {
			logger.Error(err, "could not get object from api server")
			return ctrl.Result{}, fmt.Errorf("could not get referenced object %v: %w", req.NamespacedName, err)
		}

		obj.SetName(req.Name)
		obj.SetNamespace(req.Namespace)
		// don't requeue after deletion.
		now := metav1.Now()
		err := r.store.Upsert(ctx, requestID, r.orgID, []backend.KubeObj{
			{
				Obj:              obj,
				PreferredVersion: r.gvk.PreferredVersion,
				DeletedAt:        &now,
			},
		})
		if err != nil {
			logger.Error(err, "could not publish deletion to store")
		}
		return ctrl.Result{}, err
	}

	logger = logger.WithValues("uid", obj.GetUID())
	ctx = log.IntoContext(ctx, logger)

	err := r.store.Upsert(ctx, requestID, r.orgID, []backend.KubeObj{
		{
			Obj:              obj,
			PreferredVersion: r.gvk.PreferredVersion,
			DeletedAt:        nil,
		},
	})
	if err != nil {
		logger.Error(err, "could not publish upsert to store")
		return ctrl.Result{}, err
	}

	logger.Info("successful reconciliation")
	return ctrl.Result{RequeueAfter: r.requeueAfter}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *reconciler) SetupWithManager(mgr ctrl.Manager) error {
	o := &unstructured.Unstructured{}
	o.SetGroupVersionKind(r.gvk.GroupVersionKind)

	return ctrl.NewControllerManagedBy(mgr).
		For(o).
		Complete(r)
}
