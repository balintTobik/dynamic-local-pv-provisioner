package main

import (
	"flag"
	"log"
	"os"
	"os/signal"

	"github.com/nokia/dynamic-local-pv-provisioner/pkg/handlers"
	syscall "golang.org/x/sys/unix"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	PvcController = "pvcHandler"
	PvController  = "pvHandler"
)

var (
	kubeConfig  string
	storagePath string
)

type Executor struct {
	Controllers map[string]cache.Controller
}

func main() {
	flag.Parse()
	executor := Executor{
		Controllers: make(map[string]cache.Controller),
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeConfig)
	if err != nil {
		log.Fatal("ERROR: Parsing kubeconfig failed with error: " + err.Error() + ", exiting!")
	}
	pvcHandler, err := handlers.NewPvcHandler(storagePath, cfg)
	if err != nil {
		log.Fatal("ERROR: Could not initalize K8s client for PvcHandler because of error: " + err.Error() + ", exiting!")
	}
	pvcController := pvcHandler.CreateController()
	executor.Controllers[PvcController] = pvcController

	pvHandler, err := handlers.NewPvHandler(storagePath, cfg)
	if err != nil {
		log.Fatal("ERROR: Could not initalize K8s client for PvHandler because of error: " + err.Error() + ", exiting!")
	}
	pvController := pvHandler.CreateController()
	executor.Controllers[PvController] = pvController

	stopChannel := make(chan struct{})
	signalChannel := make(chan os.Signal, 1)
	signal.Notify(signalChannel, syscall.SIGINT, syscall.SIGTERM)
	log.Println("Storage controller initalized successfully! Warm-up starts now!")
	for _, controller := range executor.Controllers {
		go controller.Run(stopChannel)
	}
	// Wait until Controller pushes a signal on the stop channel
	select {
	case <-stopChannel:
		log.Fatal("Storage controller stopped abruptly, exiting!")
	case <-signalChannel:
		log.Println("Orchestrator initiated graceful shutdown. See you soon!")
	}
}

func init() {
	flag.StringVar(&storagePath, "storagepath", "", "The path where VG is mounted and where sig-storage-controller is watching. Mandatory parameter.")
	flag.StringVar(&kubeConfig, "kubeconfig", "", "Path to a kubeconfig. Optional parameter, only required if out-of-cluster.")
}
