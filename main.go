//go:generate go run pkg/codegen/cleanup/main.go
//go:generate /bin/rm -rf pkg/generated
//go:generate go run pkg/codegen/main.go
//go:generate /bin/bash scripts/generate-manifest

package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"

	"github.com/ehazlett/simplelog"
	"github.com/rancher/wrangler/pkg/kubeconfig"
	"github.com/rancher/wrangler/pkg/leader"
	"github.com/rancher/wrangler/pkg/signals"
	"github.com/rancher/wrangler/pkg/start"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli/v2"
	"k8s.io/client-go/kubernetes"

	"github.com/longhorn/node-disk-manager/pkg/block"
	blockdevicev1 "github.com/longhorn/node-disk-manager/pkg/controller/blockdevice"
	nodev1 "github.com/longhorn/node-disk-manager/pkg/controller/node"
	longhornvctl1 "github.com/longhorn/node-disk-manager/pkg/generated/controllers/longhorn.io"
	"github.com/longhorn/node-disk-manager/pkg/option"
	"github.com/longhorn/node-disk-manager/pkg/udev"
	"github.com/longhorn/node-disk-manager/pkg/version"
)

func main() {
	var opt option.Option
	app := cli.NewApp()
	app.Name = "node-disk-manager"
	app.Version = version.FriendlyVersion()
	app.Usage = "node-disk-manager help to manage node disks, implementing block device partition and file system formatting."
	app.Flags = []cli.Flag{
		&cli.StringFlag{
			Name:        "kubeconfig",
			EnvVars:     []string{"KUBECONFIG"},
			Destination: &opt.KubeConfig,
			Usage:       "Kube config for accessing k8s cluster",
		},
		&cli.StringFlag{
			Name:        "namespace",
			DefaultText: "longhorn-system",
			EnvVars:     []string{"LONGHORN_NAMESPACE"},
			Destination: &opt.Namespace,
		},
		&cli.IntFlag{
			Name:        "threadiness",
			DefaultText: "2",
			Destination: &opt.Threadiness,
		},
		&cli.BoolFlag{
			Name:        "debug",
			EnvVars:     []string{"NDM_DEBUG"},
			Usage:       "enable debug logs",
			Destination: &opt.Debug,
		},
		&cli.StringFlag{
			Name:        "profile-listen-address",
			Value:       "0.0.0.0:6060",
			Usage:       "Address to listen on for profiling",
			Destination: &opt.ProfilerAddress,
		},
		&cli.BoolFlag{
			Name:        "trace",
			EnvVars:     []string{"NDM_TRACE"},
			Usage:       "Enable trace logs",
			Destination: &opt.Trace,
		},
		&cli.StringFlag{
			Name:        "log-format",
			EnvVars:     []string{"NDM_LOG_FORMAT"},
			Usage:       "Log format",
			Value:       "text",
			Destination: &opt.LogFormat,
		},
		&cli.StringFlag{
			Name:        "node-name",
			EnvVars:     []string{"NODE_NAME"},
			Usage:       "Specify the node name",
			Destination: &opt.NodeName,
		},
	}

	app.Action = func(c *cli.Context) error {
		initProfiling(&opt)
		initLogs(&opt)
		return run(&opt)
	}

	if err := app.Run(os.Args); err != nil {
		logrus.Fatal(err)
	}
}

func initProfiling(opt *option.Option) {
	// enable profiler
	if opt.ProfilerAddress != "" {
		go func() {
			log.Println(http.ListenAndServe(opt.ProfilerAddress, nil))
		}()
	}
}

func initLogs(opt *option.Option) {
	switch opt.LogFormat {
	case "simple":
		logrus.SetFormatter(&simplelog.StandardFormatter{})
	case "json":
		logrus.SetFormatter(&logrus.JSONFormatter{})
	default:
		logrus.SetFormatter(&logrus.TextFormatter{})
	}
	logrus.SetOutput(os.Stdout)
	if opt.Debug {
		logrus.SetLevel(logrus.DebugLevel)
		logrus.Debugf("Loglevel set to [%v]", logrus.DebugLevel)
	}
	if opt.Trace {
		logrus.SetLevel(logrus.TraceLevel)
		logrus.Tracef("Loglevel set to [%v]", logrus.TraceLevel)
	}
}

func run(opt *option.Option) error {
	logrus.Info("Starting node disk manager controller")
	if opt.NodeName == "" || opt.Namespace == "" {
		return errors.New("either node name or namespace is empty")
	}

	ctx := signals.SetupSignalHandler(context.Background())

	// register block device detector
	block, err := block.New()
	if err != nil {
		return err
	}

	kubeConfig, err := kubeconfig.GetNonInteractiveClientConfig(opt.KubeConfig).ClientConfig()
	if err != nil {
		return fmt.Errorf("failed to find kubeconfig: %v", err)
	}

	lhs, err := longhornvctl1.NewFactoryFromConfig(kubeConfig)
	if err != nil {
		return fmt.Errorf("error building node-disk-manager controllers: %s", err.Error())
	}

	client := kubernetes.NewForConfigOrDie(kubeConfig)

	leader.RunOrDie(ctx, "", "node-disk-manager", client, func(ctx context.Context) {
		err = blockdevicev1.Register(ctx, lhs.Longhorn().V1beta1().BlockDevice(), block, opt)
		if err != nil {
			logrus.Fatalf("failed to register block device controller, %s", err.Error())
		}

		err = nodev1.Register(ctx, lhs.Longhorn().V1beta1().Node(), lhs.Longhorn().V1beta1().BlockDevice(), block, opt)
		if err != nil {
			logrus.Fatalf("failed to register ndm node controller, %s", err.Error())
		}

		if err := start.All(ctx, opt.Threadiness, lhs); err != nil {
			logrus.Fatalf("error starting, %s", err.Error())
		}

		// register to monitor the UDEV events, similar to run `udevadm monitor -u`
		go udev.NewUdev(block, lhs.Longhorn().V1beta1().BlockDevice(), opt).Monitor(ctx)

		// TODO
		// 1. add node actions, i.e. block device rescan
	})

	<-ctx.Done()
	return nil
}
