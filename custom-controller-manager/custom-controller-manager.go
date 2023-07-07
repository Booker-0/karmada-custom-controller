package main

import (
	"custom-controller/custom-controller-manager/app"
	apiserver "k8s.io/apiserver/pkg/server"
	"k8s.io/component-base/logs"
	"os"
)

func main() {
	logs.InitLogs()
	defer logs.FlushLogs()

	ctx := apiserver.SetupSignalContext()
	if err := app.NewCustomControllerManagerCommand(ctx).Execute(); err != nil {
		os.Exit(1)
	}
}
