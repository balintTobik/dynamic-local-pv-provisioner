package handlers

import (
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/nokia/dynamic-local-pv-provisioner/pkg/k8sclient"

	syscall "golang.org/x/sys/unix"
	"k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

const (
	fstabPath = "/rootfs/fstab"
)

type PvcHandler struct {
	nodeName    string
	storagePath string
	k8sClient   kubernetes.Interface
}

func NewPvcHandler(storagePath string, cfg *rest.Config) (*PvcHandler, error) {
	kubeClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	pvcHandler := PvcHandler{
		nodeName:    os.Getenv("NODE_NAME"),
		storagePath: storagePath,
		k8sClient:   kubeClient,
	}
	return &pvcHandler, err
}

func (pvcHandler *PvcHandler) CreateController() cache.Controller {
	kubeInformerFactory := informers.NewSharedInformerFactory(pvcHandler.k8sClient, time.Second*30)
	controller := kubeInformerFactory.Core().V1().PersistentVolumeClaims().Informer()
	controller.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			pvcHandler.pvcAdded(*(reflect.ValueOf(obj).Interface().(*v1.PersistentVolumeClaim)))
		},
		DeleteFunc: func(obj interface{}) {},
		UpdateFunc: func(oldObj, newObj interface{}) {
			pvcHandler.pvcChanged(*(reflect.ValueOf(oldObj).Interface().(*v1.PersistentVolumeClaim)), *(reflect.ValueOf(newObj).Interface().(*v1.PersistentVolumeClaim)))
		},
	})
	return controller
}


func (pvcHandler *PvcHandler) pvcAdded(pvc v1.PersistentVolumeClaim) {
	handlePvc, pvDirPath := shouldPvcBeHandled(v1.PersistentVolumeClaim{}, pvc, pvcHandler.nodeName, pvcHandler.storagePath)
	if !handlePvc || !pvcHandler.enoughLvCapacity(pvc) {
		return
	}
	pvcHandler.createPVStorage(pvc, pvDirPath)
}

func (pvcHandler *PvcHandler) pvcChanged(oldPvc v1.PersistentVolumeClaim, newPvc v1.PersistentVolumeClaim) {
	handlePvc, pvDirPath := shouldPvcBeHandled(oldPvc, newPvc, pvcHandler.nodeName, pvcHandler.storagePath)
	if !handlePvc || !pvcHandler.enoughLvCapacity(newPvc) {
		return
	}
	pvcHandler.createPVStorage(newPvc, pvDirPath)
}

func (pvcHandler *PvcHandler) enoughLvCapacity(pvc v1.PersistentVolumeClaim) bool {
	node, err := k8sclient.GetNode(pvcHandler.nodeName, pvcHandler.k8sClient)
	if err != nil {
		log.Println("ERROR: Cannot get node: " + pvcHandler.nodeName + ", because: " + err.Error())
		return false
	}
	nodeCapacity := node.Status.Capacity[k8sclient.LvCapacity]
	if (&nodeCapacity).Cmp(pvc.Spec.Resources.Requests["storage"]) < 0 {
		log.Println("ERROR: Not enough free space in storage!")
		return false
	}
	return true
}

func shouldPvcBeHandled(oldPvc v1.PersistentVolumeClaim, newPvc v1.PersistentVolumeClaim, nodeName string, storagePath string) (bool, string) {
// pvcStorageClaim := newPvc.Spec.StorageClassName
	if newPvc.ObjectMeta.Annotations[k8sclient.LocalAnnotation] == k8sclient.LocalScProvisioner && (newPvc.Status.Phase == v1.ClaimPending) {
		if isChangeEnoughToProceed(oldPvc, newPvc) {
			if pvcNodeName, ok := newPvc.ObjectMeta.Annotations[k8sclient.NodeName]; ok && (pvcNodeName == nodeName) {
				pvDir := storagePath + newPvc.ObjectMeta.Namespace + "_" + newPvc.ObjectMeta.Name + "-" + generateRandomSuffix(8)
				if _, err := os.Stat(pvDir); os.IsNotExist(err) {
					return true, pvDir
				}
			}
		}
	}
	return false, ""
}

func (pvcHandler *PvcHandler) createPVStorage(pvc v1.PersistentVolumeClaim, pvDirPath string) {
	var projID int = 1

	pvcStorageReq, ok := pvc.Spec.Resources.Requests["storage"]
	if !ok {
		log.Println("ERROR: Storage request is empty!")
		return
	}
	storageRequest := strconv.FormatInt((&pvcStorageReq).Value(), 10)
	projectsContent, err := ioutil.ReadFile("/etc/projects")
	if err != nil {
		log.Println("ERROR: Cannot read /etc/projects file: " + err.Error())
		return
	}
	if string(projectsContent) != "" {
		lines := strings.Split(string(projectsContent), "\n")
		projid, err := strconv.Atoi(strings.Split(lines[len(lines)-2], ":")[0])
		if err != nil {
			log.Println("ERROR: Cannot convert project id from " + lines[len(lines)-2] + " because: " + err.Error())
			return
		}
		projID = projid + 1
	}
	// create directory with new projID
	err = os.Mkdir(pvDirPath, os.ModePerm)
	if err != nil {
		log.Println("ERROR: Cannot create directory on host, because: " + err.Error())
		return
	}

	projFile, err := os.OpenFile("/etc/projects", os.O_APPEND|os.O_WRONLY|os.O_SYNC, 0755)
	if err != nil {
		log.Println("ERROR: Cannot open /etc/projects file, because: " + err.Error())
		return
	}
	defer projFile.Close()
	project := fmt.Sprintf("%d:%s\n", projID, pvDirPath)
	_, err = projFile.WriteString(project)
	if err != nil {
		log.Println("ERROR: Cannot modify /etc/projects file, because: " + err.Error())
		return
	}
	projIdFile, err := os.OpenFile("/etc/projid", os.O_APPEND|os.O_WRONLY|os.O_SYNC, 0755)
	if err != nil {
		log.Println("ERROR: Cannot open /etc/projid file, because: " + err.Error())
		return
	}
	defer projIdFile.Close()
	projName := filepath.Base(pvDirPath)
	projid := fmt.Sprintf("%s:%d\n", projName, projID)
	_, err = projIdFile.WriteString(projid)
	if err != nil {
		log.Println("ERROR: Cannot modify /etc/projid file, because: " + err.Error())
		return
	}
	// set xfs_quota limit
	subcommand := fmt.Sprintf("project -s %s", projName)
	command := exec.Command("xfs_quota", "-x", "-c", subcommand, pvcHandler.storagePath)
	_, err = command.CombinedOutput()
	if err != nil {
		log.Println("ERROR: Cannot set xfs_quota project, because: " + err.Error())
		return
	}
	subcommand = fmt.Sprintf("limit -p bhard=%s %s", storageRequest, projName)
	command = exec.Command("xfs_quota", "-x", "-c", subcommand, pvcHandler.storagePath)
	_, err = command.CombinedOutput()
	if err != nil {
		log.Println("ERROR: Cannot set xfs_quota limit, because: " + err.Error())
		return
	}
	// Bind mounting
	err = syscall.Mount(pvDirPath, pvDirPath, "none", syscall.MS_BIND, "")
	if err != nil {
		log.Println("ERROR: bind mount directories, because: " + err.Error())
		return
	}
	// Set fstab file
	file, err := os.OpenFile(fstabPath, os.O_APPEND|os.O_WRONLY|os.O_SYNC, 0755)
	if err != nil {
		log.Println("ERROR: Cannot open fstab file: " + fstabPath + " because: " + err.Error() + "\nCannot save mountpoint!")
		return
	}
	defer file.Close()
	bindMountCommand := fmt.Sprintf("%[1]s %[1]s none bind 0 0\n", pvDirPath)
	_, err = file.WriteString(bindMountCommand)
	if err != nil {
		log.Println("ERROR: Cannot modify fstab file: " + fstabPath + " because: " + err.Error() + "\nCannot save mountpoint!")
		return
	}
}

func generateRandomSuffix(suffixlength int) string {
	charPool := []byte("abcdefghijklmnopqrstuvwxyz1234567890")
	rand.Seed(time.Now().Unix())
	bytes := make([]byte, suffixlength)
	for i := range bytes {
		bytes[i] = charPool[rand.Intn(len(charPool))]
	}
	return string(bytes)
}

func isChangeEnoughToProceed(oldPvc v1.PersistentVolumeClaim, newPvc v1.PersistentVolumeClaim) bool {
	old_nodename := oldPvc.ObjectMeta.Annotations[k8sclient.NodeName]
	new_nodename := newPvc.ObjectMeta.Annotations[k8sclient.NodeName]
	if !reflect.DeepEqual(oldPvc, v1.PersistentVolumeClaim{}) {
		if old_nodename != new_nodename && old_nodename == "" {
			return true
		}
	} else { // in case the created PVC already has the "nokia.k8s.io/nodeName" annotation otherwise set by provisioner
		if new_nodename != "" {
			return true
		}
	}
	return false
}
