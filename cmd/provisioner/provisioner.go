package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"time"
	"reflect"
	"strings"
	"encoding/json"
	syscall "golang.org/x/sys/unix"
	"github.com/nokia/dynamic-local-pv-provisioner/pkg/k8sclient"

	"k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
)

var (
	kubeConfig 	string
)

type Provisoner struct {
	k8sClient kubernetes.Interface
}

func main() {
	flag.Parse()
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeConfig)
	if err != nil {
		log.Fatal("ERROR: Parsing kubeconfig failed with error: " + err.Error() + ", exiting!")
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Fatal("ERROR: Get k8s client: " + err.Error())
	}
	provisioner := Provisoner{
		k8sClient: client,
	}

	kubeInformerFactory := informers.NewSharedInformerFactory(provisioner.k8sClient, time.Second*10)
	provisonerController := kubeInformerFactory.Core().V1().PersistentVolumeClaims().Informer()
	provisonerController.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { provisioner.pvcAdded(*(reflect.ValueOf(obj).Interface().(*v1.PersistentVolumeClaim))) },
		DeleteFunc: func(obj interface{}) {},
		UpdateFunc: func(oldObj, newObj interface{}) {
			provisioner.pvcChanged(*(reflect.ValueOf(oldObj).Interface().(*v1.PersistentVolumeClaim)), *(reflect.ValueOf(newObj).Interface().(*v1.PersistentVolumeClaim)))
		},
	})
	stopChannel := make(chan struct{})
	signalChannel := make(chan os.Signal, 1)
	signal.Notify(signalChannel, syscall.SIGINT, syscall.SIGTERM)
	log.Println("Storage Provisoner controller initalized successfully! Warm-up starts now!")

	go provisonerController.Run(stopChannel)
	// Wait until Controller pushes a signal on the stop channel
	select {
	case <-stopChannel:
		log.Fatal("Storage Provisoner controller stopped abruptly, exiting!")
	case <-signalChannel:
		log.Println("Orchestrator initiated graceful shutdown. See you soon!")
	}
}

func (provisioner *Provisoner) pvcAdded(pvc v1.PersistentVolumeClaim) {
	_, nodeNameOk := pvc.ObjectMeta.Annotations[k8sclient.NodeName]
	if pvc.ObjectMeta.Annotations[k8sclient.LocalAnnotation] == k8sclient.LocalScProvisioner && !nodeNameOk && (pvc.Status.Phase == v1.ClaimPending) {
		provisioner.handlePvc(pvc)
	}
}

func (provisioner *Provisoner) pvcChanged(oldPvc v1.PersistentVolumeClaim, newPvc v1.PersistentVolumeClaim) {
	_, nodeNameOk := newPvc.ObjectMeta.Annotations[k8sclient.NodeName]
	if newPvc.ObjectMeta.Annotations[k8sclient.LocalAnnotation] == k8sclient.LocalScProvisioner && !nodeNameOk && (newPvc.Status.Phase == v1.ClaimPending) {
		provisioner.handlePvc(newPvc)
	}
}

func (provisioner *Provisoner) handlePvc (pvc v1.PersistentVolumeClaim) {
	nodeSelectorMap := make(map[string]string)
	if nodeSel, ok := pvc.ObjectMeta.Annotations[k8sclient.NodeSelector]; ok {
		if nodeSel != "" {
			err := json.Unmarshal([]byte(nodeSel),&nodeSelectorMap)
			if err != nil {
				log.Println("ERROR: Cannot parse nodeselector "+ nodeSel +" because: " + err.Error())
				return
			}
		}
	}
	s := []string{}
	for key, value := range nodeSelectorMap {
		s = append(s, key + "=" + value)
	}
	selector := strings.Join(s,",")
	node, err := k8sclient.GetNodeByLabel(selector, provisioner.k8sClient)
	if err != nil {
		log.Println("ERROR: Cannot query node by label, because: " + err.Error())
		return
	}
	if pvc.ObjectMeta.Annotations == nil {
		pvc.ObjectMeta.Annotations = make(map[string]string)
	}
	pvc.ObjectMeta.Annotations[k8sclient.NodeName] = node.ObjectMeta.Name
	// test if could be updated
	pvc.ObjectMeta.ResourceVersion = ""
	err = k8sclient.UpdatePvc(pvc, provisioner.k8sClient)
	if err != nil {
		log.Println("ERROR: Cannot update PVC, because: " + err.Error())
	}
}

func init() {
	// flag.StringVar(&storagePath, "storagepath", "", "The path where VG is mounted and where sig-storage-controller is watching. Mandatory parameter.")
	flag.StringVar(&kubeConfig, "kubeconfig", "", "Path to a kubeconfig. Optional parameter, only required if out-of-cluster.")
}
