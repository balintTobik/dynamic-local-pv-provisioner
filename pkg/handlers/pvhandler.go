package handlers

import (
	"errors"
	"log"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/nokia/dynamic-local-pv-provisioner/pkg/k8sclient"
	syscall "golang.org/x/sys/unix"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

type PvHandler struct {
	nodeName    string
	storagePath string
	k8sClient   kubernetes.Interface
}

func NewPvHandler(storagePath string, cfg *rest.Config) (*PvHandler, error) {
	kubeClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	nodeName := os.Getenv("NODE_NAME")
	pvHandler := PvHandler{
		nodeName:    nodeName,
		storagePath: storagePath,
		k8sClient:   kubeClient,
	}
	lvCap, err := lvmAvailableCapacity(storagePath)
	if err != nil {
		return nil, err
	}
	err = createLVCapacityResource(nodeName, lvCap, kubeClient)
	return &pvHandler, err
}

func (pvHandler *PvHandler) CreateController() cache.Controller {
	kubeInformerFactory := informers.NewSharedInformerFactory(pvHandler.k8sClient, time.Second*30)
	controller := kubeInformerFactory.Core().V1().PersistentVolumes().Informer()
	controller.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { pvHandler.pvAdded(*(reflect.ValueOf(obj).Interface().(*v1.PersistentVolume))) },
		DeleteFunc: func(obj interface{}) { pvHandler.pvDeleted(*(reflect.ValueOf(obj).Interface().(*v1.PersistentVolume))) },
		UpdateFunc: func(oldObj, newObj interface{}) {},
	})
	return controller
}

func (pvHandler *PvHandler) pvAdded(pv v1.PersistentVolume) {
	if !pvHandler.handlePv(pv) {
		return
	}
	err := pvHandler.decreaseStorageCap(pv)
	if err != nil {
		log.Println("PvHandler ERROR: PV Added failed: " + err.Error())
		return
	}
}

func (pvHandler *PvHandler) pvDeleted(pv v1.PersistentVolume) {
	if !pvHandler.handlePv(pv) {
		return
	}
	if pv.Spec.PersistentVolumeReclaimPolicy != v1.PersistentVolumeReclaimDelete {
		return
	}
	localVolumePath := pv.Spec.Local.Path
	// delete directory
	err := os.RemoveAll(localVolumePath)
	if err != nil {
		log.Println("PvHandler ERROR: Cannot delete " + localVolumePath + " , because: " + err.Error())
	}

	err = pvHandler.increaseStorageCap(pv)
	if err != nil {
		log.Println("PvHandler ERROR: PV Delete failed: " + err.Error())
		return
	}
}

func (pvHandler *PvHandler) handlePv(pv v1.PersistentVolume) bool {
	pvIsLocal, err := k8sclient.StorageClassIsNokiaLocal(pv.Spec.StorageClassName)
	if err == nil && pvIsLocal {
		nodeSelector := pv.Spec.NodeAffinity.Required.String()
		if strings.Contains(nodeSelector, pvHandler.nodeName) {
			return true
		}
	}
	return false
}

func (pvHandler *PvHandler) increaseStorageCap(pv v1.PersistentVolume) error {
	pvCapacity := pv.Spec.Capacity["storage"]
	node, err := k8sclient.GetNode(pvHandler.nodeName)
	if err != nil {
		return errors.New("Cannot get node(" + pvHandler.nodeName + "), because: " + err.Error())
	}
	nodeCap := node.Status.Capacity[k8sclient.LvCapacity]
	(&nodeCap).Add(pvCapacity)
	node.Status.Capacity[k8sclient.LvCapacity] = nodeCap
	err = k8sclient.UpdateNodeStatus(pvHandler.nodeName, node)
	if err != nil {
		return errors.New("Cannot update node(" + pvHandler.nodeName + "), because: " + err.Error())
	}
	return nil
}

func (pvHandler *PvHandler) decreaseStorageCap(pv v1.PersistentVolume) error {
	pvCapacity := pv.Spec.Capacity["storage"]
	node, err := k8sclient.GetNode(pvHandler.nodeName)
	if err != nil {
		return errors.New("Cannot get node(" + pvHandler.nodeName + "), because: " + err.Error())
	}
	nodeCap := node.Status.Capacity[k8sclient.LvCapacity]
	(&nodeCap).Sub(pvCapacity)
	node.Status.Capacity[k8sclient.LvCapacity] = nodeCap
	err = k8sclient.UpdateNodeStatus(pvHandler.nodeName, node)
	if err != nil {
		return errors.New("Cannot update node(" + pvHandler.nodeName + "), because: " + err.Error())
	}
	return nil
}

func createLVCapacityResource(nodeName string, lvCapacity int64, kubeClient kubernetes.Interface) error {
	node, err := k8sclient.GetNode(nodeName)
	if err != nil {
		return errors.New("Cannot get node(" + nodeName + "), because: " + err.Error())
	}
	lvCapQuantity := resource.NewQuantity(lvCapacity, resource.BinarySI)
	node.Status.Capacity[k8sclient.LvCapacity] = *lvCapQuantity
	err = k8sclient.UpdateNodeStatus(nodeName, node)
	if err != nil {
		return errors.New("Cannot update node(" + nodeName + "), because: " + err.Error())
	}
	return nil
}

func lvmAvailableCapacity(lvPath string) (int64, error) {
	fs := syscall.Statfs_t{}
	err := syscall.Statfs(lvPath, &fs)
	if err != nil {
		return 0, errors.New("Cannot get FS info from: " + lvPath + " because: " + err.Error())
	}
	return int64(fs.Bavail) * fs.Bsize, nil
}
