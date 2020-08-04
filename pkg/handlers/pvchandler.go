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
	"errors"
	"time"
	"hash/fnv"
	"k8s.io/klog"
	"encoding/json"

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

type patch struct {
	Op    string          `json:"op"`
	Path  string          `json:"path"`
	Value json.RawMessage `json:"value"`
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
		DeleteFunc: func(obj interface{}) {pvcHandler.pvcDeleted(*(reflect.ValueOf(obj).Interface().(*v1.PersistentVolumeClaim)))},
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

func (pvcHandler *PvcHandler) pvcDeleted(pvc v1.PersistentVolumeClaim) {
	if handlePvc, _ := shouldPvcBeHandled(v1.PersistentVolumeClaim{}, pvc, pvcHandler.nodeName, pvcHandler.storagePath); handlePvc {
		pv, err := k8sclient.GetVolume(pvc.Spec.VolumeName, pvcHandler.k8sClient)
		if err != nil {
			log.Println("PvcHandler ERROR: Cannot get pv "+ pvc.Spec.VolumeName +", because: " + err.Error())
		}
		deletePVStorage(*pv, pvcHandler.storagePath)
	}
}

func (pvcHandler *PvcHandler) enoughLvCapacity(pvc v1.PersistentVolumeClaim) bool {
	node, err := k8sclient.GetNode(pvcHandler.nodeName, pvcHandler.k8sClient)
	if err != nil {
		log.Println("PvcHandler ERROR: Cannot get node: " + pvcHandler.nodeName + ", because: " + err.Error())
		return false
	}
	nodeCapacity := node.Status.Capacity[k8sclient.LvCapacity]
	if (&nodeCapacity).Cmp(pvc.Spec.Resources.Requests["storage"]) < 0 {
		log.Println("PvcHandler ERROR: Not enough free space in storage!")
		return false
	}
	return true
}

func shouldPvcBeHandled(oldPvc v1.PersistentVolumeClaim, newPvc v1.PersistentVolumeClaim, nodeName string, storagePath string) (bool, string) {
	if newPvc.Annotations[k8sclient.LocalAnnotation] == k8sclient.LocalScProvisioner && isChangeEnoughToProceed(oldPvc, newPvc) {
		klog.Infof("DEBUG: pvc-name: %s nodeName: %s , pvcNodeName: %s", newPvc.ObjectMeta.Name, nodeName, newPvc.ObjectMeta.Annotations[k8sclient.NodeName])
		if pvcNodeName, ok := newPvc.ObjectMeta.Annotations[k8sclient.NodeName]; ok && pvcNodeName == nodeName {
			if newPvc.Status.Phase == v1.ClaimPending {
				pvDir := storagePath + newPvc.ObjectMeta.Namespace + "_" + newPvc.ObjectMeta.Name + "-" + generateRandomSuffix(8)
				if _, err := os.Stat(pvDir); os.IsNotExist(err) {
					return true, pvDir
				}
			} else if newPvc.Status.Phase == v1.ClaimBound && newPvc.Spec.VolumeName != "" {
				return true, ""
			}
		}
	}
	return false, ""
}

func (pvcHandler *PvcHandler) createPVStorage(pvc v1.PersistentVolumeClaim, pvDirPath string) {
	var projID int = 1

	pvcStorageReq, ok := pvc.Spec.Resources.Requests["storage"]
	if !ok {
		log.Println("PvcHandler ERROR: Storage request is empty!")
		return
	}
	storageRequest := strconv.FormatInt((&pvcStorageReq).Value(), 10)
	projectsContent, err := ioutil.ReadFile("/etc/projects")
	if err != nil {
		log.Println("PvcHandler ERROR: Cannot read /etc/projects file: " + err.Error())
		return
	}
	if string(projectsContent) != "" {
		lines := strings.Split(strings.TrimRight(string(projectsContent),"\n"), "\n")
		projid, err := strconv.Atoi(strings.Split(lines[len(lines)-1], ":")[0])
		if err != nil {
			log.Println("PvcHandler ERROR: Cannot convert project id from " + lines[len(lines)-2] + " because: " + err.Error())
			return
		}
		projID = projid + 1
	}
	// create directory with new projID
	err = os.Mkdir(pvDirPath, os.ModePerm)
	if err != nil {
		log.Println("PvcHandler ERROR: Cannot create directory on host, because: " + err.Error())
		return
	}

	projFile, err := os.OpenFile("/etc/projects", os.O_APPEND|os.O_WRONLY|os.O_SYNC, 0755)
	if err != nil {
		log.Println("PvcHandler ERROR: Cannot open /etc/projects file, because: " + err.Error())
		return
	}
	defer projFile.Close()
	project := fmt.Sprintf("%d:%s\n", projID, pvDirPath)
	_, err = projFile.WriteString(project)
	if err != nil {
		log.Println("PvcHandler ERROR: Cannot modify /etc/projects file, because: " + err.Error())
		return
	}
	projIdFile, err := os.OpenFile("/etc/projid", os.O_APPEND|os.O_WRONLY|os.O_SYNC, 0755)
	if err != nil {
		log.Println("PvcHandler ERROR: Cannot open /etc/projid file, because: " + err.Error())
		return
	}
	defer projIdFile.Close()
	projName := filepath.Base(pvDirPath)
	projid := fmt.Sprintf("%s:%d\n", projName, projID)
	_, err = projIdFile.WriteString(projid)
	if err != nil {
		log.Println("PvcHandler ERROR: Cannot modify /etc/projid file, because: " + err.Error())
		return
	}
	// set xfs_quota limit
	subcommand := fmt.Sprintf("project -s %s", projName)
	command := exec.Command("xfs_quota", "-x", "-c", subcommand, pvcHandler.storagePath)
	_, err = command.CombinedOutput()
	if err != nil {
		log.Println("PvcHandler ERROR: Cannot set xfs_quota project, because: " + err.Error())
		return
	}
	subcommand = fmt.Sprintf("limit -p bhard=%s %s", storageRequest, projName)
	command = exec.Command("xfs_quota", "-x", "-c", subcommand, pvcHandler.storagePath)
	_, err = command.CombinedOutput()
	if err != nil {
		log.Println("PvcHandler ERROR: Cannot set xfs_quota limit, because: " + err.Error())
		return
	}
	// Bind mounting
	err = syscall.Mount(pvDirPath, pvDirPath, "none", syscall.MS_BIND, "")
	if err != nil {
		log.Println("PvcHandler ERROR: bind mount directories, because: " + err.Error())
		return
	}
	// Set fstab file
	file, err := os.OpenFile(fstabPath, os.O_APPEND|os.O_WRONLY|os.O_SYNC, 0755)
	if err != nil {
		log.Println("PvcHandler ERROR: Cannot open fstab file: " + fstabPath + " because: " + err.Error() + "\nCannot save mountpoint!")
		return
	}
	defer file.Close()
	bindMountCommand := fmt.Sprintf("%[1]s %[1]s none bind 0 0\n", pvDirPath)
	_, err = file.WriteString(bindMountCommand)
	if err != nil {
		log.Println("PvcHandler ERROR: Cannot modify fstab file: " + fstabPath + " because: " + err.Error() + "\nCannot save mountpoint!")
		return
	}
	pvName := generatePVName(filepath.Base(pvDirPath), pvcHandler.nodeName, *(pvc.Spec.StorageClassName))

	pvc.Spec.VolumeName = pvName
	err = k8sclient.UpdatePvc(pvc, pvcHandler.k8sClient)
	if err != nil {
		log.Println("PvcHandler ERROR: Cannot update volumename in " + pvc.ObjectMeta.Name + " pvc, because: " + err.Error())
	}
}

func removePvDataFromFile(filePath string, searchData string) error {
	var removedList []string
	fileContent, err := ioutil.ReadFile(filePath)
	if err != nil {
		return errors.New("Cannot read "+ filePath +" file: " + err.Error())
	}
	fileContentList := strings.Split(string(fileContent), "\n")
	removeIdx := 0
	for idx, data := range fileContentList {
			if strings.Contains(data, searchData){
				removeIdx = idx
			}
	}
	removedList = append(removedList, fileContentList[:removeIdx]...)
	removedList = append(removedList, fileContentList[removeIdx+1:]...)
	file, err := os.OpenFile(filePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0755)
	if err != nil {
		return errors.New("Cannot open" + filePath + " file, because: " + err.Error())
	}
	defer file.Close()
	_, err = file.WriteString(strings.Join(removedList, "\n"))
	if err != nil {
		return errors.New("Cannot modify" + filePath + " file, because: " + err.Error())
	}
	return nil
}

func deletePVStorage(pv v1.PersistentVolume, storagePath string){
	if pv.Spec.PersistentVolumeReclaimPolicy != v1.PersistentVolumeReclaimDelete{
		return
	}
	klog.Infof("DEBUG: Delete pvc: %s",pv.ObjectMeta.Name)
	localVolumePath := pv.Spec.Local.Path
	// unmount pv directory
	err := syscall.Unmount(localVolumePath, 0)
	if err != nil {
		log.Println("PvcHandler ERROR: Cannot UNMOUNT directory (" + localVolumePath + "), because: " + err.Error())
		return
	}
	klog.Infof("DEBUG: unmount successfull, pv:%s",pv.ObjectMeta.Name)
	// delete xfs_quota data
	subcommand := fmt.Sprintf("limit -p bsoft=0 bhard=0 %s", filepath.Base(localVolumePath))
	command := exec.Command("xfs_quota", "-x", "-c", subcommand, storagePath)
	_, err = command.CombinedOutput()
	if err != nil {
		log.Println("PvcHandler ERROR: Cannot set xfs_quota project, because: " + err.Error())
		return
	}
	klog.Infof("DEBUG: set limit 0, pv:%s",pv.ObjectMeta.Name)
	subcommand = fmt.Sprintf("project -C %s", filepath.Base(localVolumePath))
	command = exec.Command("xfs_quota", "-x", "-c", subcommand, storagePath)
	_, err = command.CombinedOutput()
	if err != nil {
		log.Println("PvcHandler ERROR: Cannot set xfs_quota project, because: " + err.Error())
		return
	}
	klog.Infof("DEBUG: clear xfs_quota project, pv:%s",pv.ObjectMeta.Name)
	//remove data from projects file
	err = removePvDataFromFile("/etc/projects", filepath.Base(localVolumePath))
	if err != nil {
		log.Println("PvcHandler ERROR: " + err.Error())
		return
	}
	klog.Infof("DEBUG: remove project from projects file, pv:%s",pv.ObjectMeta.Name)
	//remove data from projid file
	err = removePvDataFromFile("/etc/projid", filepath.Base(localVolumePath))
	if err != nil {
		log.Println("PvcHandler ERROR: " + err.Error())
		return
	}
	klog.Infof("DEBUG: remove project from projid file, pv:%s",pv.ObjectMeta.Name)
	//remove data from fstab file
	err = removePvDataFromFile(fstabPath, localVolumePath)
	if err != nil {
		log.Println("PvcHandler ERROR: " + err.Error())
		return
	}
	klog.Infof("DEBUG: remove mount from fstab, pv:%s",pv.ObjectMeta.Name)
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

func generatePVName(file, node, class string) string {
	h := fnv.New32a()
	h.Write([]byte(file))
	h.Write([]byte(node))
	h.Write([]byte(class))
	// This is the FNV-1a 32-bit hash
	return fmt.Sprintf("local-pv-%x", h.Sum32())
}
