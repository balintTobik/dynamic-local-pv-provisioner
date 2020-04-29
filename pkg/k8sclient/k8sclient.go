package k8sclient

import (
	"errors"
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/api/core/v1"
)

const (
	NodeSelector = "nokia.k8s.io/nodeSelector"
	NodeName = "nokia.k8s.io/nodeName"
	LvCapacity = "nokia.k8s.io/lv-capacity"
	LocalScProvisioner = "nokia.k8s.io/local"
	LocalAnnotation = "volume.beta.kubernetes.io/storage-provisioner"
)

func GetNodeByLabel(label string, kubeClient kubernetes.Interface) (v1.Node, error) {
	var returnNode v1.Node
	var maxCapacity int64 = 0
	var listOption metav1.ListOptions

	listOption = metav1.ListOptions{LabelSelector: label}
	nodeList, err := kubeClient.CoreV1().Nodes().List(context.TODO(), listOption)
	if err != nil {
		return v1.Node{}, err
	}
	switch nodesLen := len(nodeList.Items); nodesLen {
	case 0:
		return v1.Node{}, errors.New("No nodes found for label:" + label + "!")
	case 1:
		return nodeList.Items[0], nil
	default:
		for _, node := range nodeList.Items {
			nodeCapacity, ok := node.Status.Capacity[LvCapacity]
			if !ok {
				continue
			}
			if (&nodeCapacity).CmpInt64(maxCapacity) == 1 {
				maxCapacity = (&nodeCapacity).Value()
				returnNode = node
			}
		}
	}
	if returnNode.ObjectMeta.Name == "" {
		return v1.Node{}, errors.New("No lv-capacity set, yet!")
	}
	return returnNode, nil
}

func UpdatePvc(pvc v1.PersistentVolumeClaim, kubeClient kubernetes.Interface) error {
	_, err := kubeClient.CoreV1().PersistentVolumeClaims(pvc.ObjectMeta.Namespace).Update(context.TODO(), &pvc, metav1.UpdateOptions{})
	if err != nil {
		return err
	}
	return nil
}

func GetNode(nodeName string, kubeClient kubernetes.Interface) (*v1.Node, error) {
	node, err := kubeClient.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
 	if err != nil {
		return nil, err
	}
	return node, nil
}

func UpdateNodeStatus(nodeName string, kubeClient kubernetes.Interface, node *v1.Node) error {
	_, err := kubeClient.CoreV1().Nodes().UpdateStatus(context.TODO(), node, metav1.UpdateOptions{})
	if err != nil {
		return err
	}
	return nil
}

func StorageClassIsNokiaLocal(storageClassName string, kubeClient kubernetes.Interface) (bool, error){
	storageClass, err := kubeClient.StorageV1().StorageClasses().Get(context.TODO(), storageClassName, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	return storageClass.Provisioner == LocalScProvisioner, nil
}
