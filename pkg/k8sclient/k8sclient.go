package k8sclient

import (
	"context"
	"errors"

	"github.com/sbabiv/roundrobin"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	LvCapacity         = "nokia.k8s.io/lv-capacity"
	LocalScProvisioner = "nokia.k8s.io/local"
	NodeName           = "nokia.k8s.io/nodeName"
	RR                 = "round robin"
	Cap                = "capacity"
)

func getClientSet() (kubernetes.Interface, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, errors.New("Error creating InCluster config: " + err.Error())
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, errors.New("Error creating clientset: " + err.Error())
	}
	return clientset, nil
}

func GetAllNodes() (v1.NodeList, error) {
	clientSet, err := getClientSet()
	if err != nil {
		return v1.NodeList{}, err
	}
	nodes, err := clientSet.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
	return *nodes, err
}

func GetNodeByLabel(label string, selectorMethod string, rr *roundrobin.Balancer) (v1.Node, error) {
	var (
		returnNode  v1.Node
		maxCapacity int64 = 0
		listOption  metav1.ListOptions
	)
	clientSet, err := getClientSet()
	if err != nil {
		return v1.Node{}, err
	}
	listOption = metav1.ListOptions{LabelSelector: label}
	nodeList, err := clientSet.CoreV1().Nodes().List(context.TODO(), listOption)
	if err != nil {
		return v1.Node{}, err
	}
	switch nodesLen := len(nodeList.Items); nodesLen {
	case 0:
		return v1.Node{}, errors.New("No nodes found for label:" + label + "!")
	case 1:
		return nodeList.Items[0], nil
	default:
		if selectorMethod == RR {
			nodeId, _ := rr.Pick()
			returnNode = nodeList.Items[nodeId.(int)%len(nodeList.Items)]
		} else if selectorMethod == Cap {
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
	}
	if returnNode.ObjectMeta.Name == "" {
		return v1.Node{}, errors.New("No lv-capacity set, yet!")
	}
	return returnNode, nil
}

func UpdateNodeStatus(nodeName string, node *v1.Node) error {
	clientSet, err := getClientSet()
	if err != nil {
		return err
	}
	_, err = clientSet.CoreV1().Nodes().UpdateStatus(context.TODO(), node, metav1.UpdateOptions{})
	if err != nil {
		return err
	}
	return nil
}

func StorageClassIsNokiaLocal(storageClassName string) (bool, error) {
	clientSet, err := getClientSet()
	if err != nil {
		return false, err
	}

	storageClass, err := clientSet.StorageV1().StorageClasses().Get(context.TODO(), storageClassName, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	return storageClass.Provisioner == LocalScProvisioner, nil
}

func GetNode(nodeName string) (*v1.Node, error) {
	clientSet, err := getClientSet()
	if err != nil {
		return nil, err
	}
	node, err := clientSet.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return node, nil
}

func GetVolume(pvName string) (*v1.PersistentVolume, error) {
	clientSet, err := getClientSet()
	if err != nil {
		return nil, err
	}
	return clientSet.CoreV1().PersistentVolumes().Get(context.TODO(), pvName, metav1.GetOptions{})
}
