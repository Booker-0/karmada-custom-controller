package app

import (
	"context"
	"custom-controller/custom-controller-manager/app/options"
	"custom-controller/custom-controller-manager/pkg/controllers/deployment"
	"custom-controller/custom-controller-manager/pkg/util"
	"flag"
	"fmt"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	"net"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"strconv"
)

//为了方便管理自定义的控制器，我们先实现一个控制器管理器(Controller Manager) 对自定义的控制器进行管理。

const (
	CheckEndpointHealthz = "healthz"
	CheckEndpointReadyz  = "readyz"
)

//使用 cobra 库构建控制器管理器入口
func NewCustomControllerManagerCommand(ctx context.Context) *cobra.Command {
	opts := options.NewOptions()

	cmd := &cobra.Command{
		Use:  "karmada-custom-controller-manager",
		Long: `karmada custom controller manager`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := opts.Validate(); err != nil {
				return err
			}
			return Run(ctx, opts)
		},
	}
	klog.InitFlags(flag.CommandLine)
	cmd.Flags().AddGoFlagSet(flag.CommandLine)
	opts.AddFlags(cmd.Flags())
	return cmd
}

//controller-runtime 库可以帮助我们快速实现一个控制器管理器
func Run(ctx context.Context, opts *options.Options) error {
	config, err := ctrl.GetConfig()
	if err != nil {
		return err
	}
	util.SetupKubeConfig(config)


	mgr, err := ctrl.NewManager(config, ctrl.Options{
		Scheme:                     runtime.NewScheme(),
		SyncPeriod:                 &opts.ResyncPeriod.Duration,
		LeaderElection:             opts.LeaderElection.LeaderElect,
		LeaderElectionID:           opts.LeaderElection.ResourceName,
		LeaderElectionNamespace:    opts.LeaderElection.ResourceNamespace,
		LeaderElectionResourceLock: opts.LeaderElection.ResourceLock,
		HealthProbeBindAddress:     net.JoinHostPort(opts.BindAddress, strconv.Itoa(opts.SecurePort)),
		MetricsBindAddress:         opts.MetricsBindAddress,
	})
	if err != nil {
		return fmt.Errorf("new controller manager failed: %v", err)
	}

	if err := mgr.AddHealthzCheck(CheckEndpointHealthz, healthz.Ping); err != nil {
		return fmt.Errorf("failed to add %q health check endpoint: %v", CheckEndpointHealthz, err)
	}
	if err := mgr.AddReadyzCheck(CheckEndpointReadyz, healthz.Ping); err != nil {
		return fmt.Errorf("failed to add %q health check endpoint: %v", CheckEndpointReadyz, err)
	}

	if err := deployment.AddToManager(mgr); err != nil {
		return err
	}
	if err := mgr.Start(ctx); err != nil {
		return fmt.Errorf("controller manager exit: %v", err)
	}
	return nil
}
