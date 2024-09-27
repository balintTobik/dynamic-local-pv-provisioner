package mutator

import (
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"github.com/go-yaml/yaml"
	"k8s.io/api/admission/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"

	"github.com/nokia/dynamic-local-pv-provisioner/pkg/k8sclient"
	"github.com/sbabiv/roundrobin"
)

const (
	defaultSelectorFilePath = "/etc/config/config.yml"
	nodeNameAnnotation      = "nokia.k8s.io/nodeName"
	patchPvDirName          = "nokia.k8s.io~1pvDirName"
	nodeSelector            = "nokia.k8s.io/nodeSelector"
)

var (
	scheme                = runtime.NewScheme()
	codecs                = serializer.NewCodecFactory(scheme)
	parsedDefaultSelector map[string]struct {
		DefaultNodeSelector string `yaml:"defaultNodeSelector"`
	}
	nodeSelectMethod string
)

type patch struct {
	Op    string          `json:"op"`
	Path  string          `json:"path"`
	Value json.RawMessage `json:"value"`
}

func toAdmissionResponse(err error) *v1beta1.AdmissionResponse {
	return &v1beta1.AdmissionResponse{
		Result: &metav1.Status{
			Message: err.Error(),
		},
		Allowed: false,
	}
}

type Mutator struct {
	rr        *roundrobin.Balancer
	nodeLabel string
}

func NewMutator(method string, nodeLabel string) (*Mutator, error) {
	var nodeList []string
	err := parseDefaultNodeSelector()
	if err != nil {
		log.Println("WARNING: Cannot parse default node selector, because: " + err.Error() + ". Continue without it...")
	}
	mutator := Mutator{rr: nil, nodeLabel: nodeLabel}
	nodeSelectMethod = method
	if nodeSelectMethod == k8sclient.RR {
		nodes, err := k8sclient.GetAllNodes()
		if err != nil {
			return nil, errors.New("Cannot get list of all nodes, because: " + err.Error())
		}
		for _, node := range nodes.Items {
			nodeList = append(nodeList, node.ObjectMeta.Name)
		}
		nodeIds := make([]interface{}, len(nodeList))
		for i := 0; i < len(nodeList); i++ {
			nodeIds[i] = i
		}
		mutator.rr = roundrobin.New(nodeIds)
	}
	return &mutator, nil
}

func parseDefaultNodeSelector() error {
	file, err := ioutil.ReadFile(defaultSelectorFilePath)
	if err != nil {
		return err
	}
	err = yaml.Unmarshal([]byte(file), &parsedDefaultSelector)
	if err != nil {
		return err
	}
	return nil
}

func (mutator *Mutator) ServeMutatePvc(w http.ResponseWriter, r *http.Request) {
	var body []byte
	if r.Body != nil {
		if data, err := ioutil.ReadAll(r.Body); err == nil {
			body = data
		}
	}
	// verify the content type is accurate
	contentType := r.Header.Get("Content-Type")
	if contentType != "application/json" {
		log.Printf("ERROR: contentType=%s, expect application/json\n", contentType)
		return
	}
	requestedAdmissionReview := v1beta1.AdmissionReview{}

	responseAdmissionReview := v1beta1.AdmissionReview{}

	deserializer := codecs.UniversalDeserializer()
	if _, _, err := deserializer.Decode(body, nil, &requestedAdmissionReview); err != nil {
		log.Println("ERROR: Decode Pvc body is failed, because " + err.Error())
		responseAdmissionReview.Response = toAdmissionResponse(err)
	} else {
		responseAdmissionReview.Response = mutatePvcs(requestedAdmissionReview, mutator.rr, mutator.nodeLabel)
	}
	responseAdmissionReview.Response.UID = requestedAdmissionReview.Request.UID

	respBytes, err := json.Marshal(responseAdmissionReview)
	if err != nil {
		log.Println("ERROR: Marshal responseAdmissionReview is failed, because " + err.Error())
	}
	w.Header().Set("Content-Type", "application/json")

	if _, err := w.Write(respBytes); err != nil {
		log.Println("ERROR: Write response is failed, because " + err.Error())
	}
}

func mutatePvcs(ar v1beta1.AdmissionReview, rr *roundrobin.Balancer, nodeLabel string) *v1beta1.AdmissionResponse {
	var (
		patchList []patch
		err       error
	)
	raw := ar.Request.Object.Raw
	pvc := corev1.PersistentVolumeClaim{}
	deserializer := codecs.UniversalDeserializer()
	if _, _, err = deserializer.Decode(raw, nil, &pvc); err != nil {
		log.Println("ERROR: Decode Pvc body is failed, because " + err.Error())
		return toAdmissionResponse(err)
	}
	reviewResponse := v1beta1.AdmissionResponse{}
	reviewResponse.Allowed = true

	mutatePvc, err := k8sclient.StorageClassIsNokiaLocal(*(pvc.Spec.StorageClassName))
	if !mutatePvc {
		if err != nil {
			log.Println("ERROR: Cannot check storageclass " + pvc.ObjectMeta.Name + " pvc, ID: " + string(pvc.ObjectMeta.UID) + ", because " + err.Error())
		}
		return &reviewResponse
	}
	nodeAnnotation, nodeAnnotationExists := pvc.ObjectMeta.Annotations[k8sclient.NodeName]
	if !nodeAnnotationExists {
		patchList, nodeAnnotation, err = setNodeSelector(pvc, patchList, rr, nodeLabel)
		if err != nil {
			return toAdmissionResponse(err)
		}
	}
	patchList = patchVolumeNameAndPvDir(pvc, nodeAnnotation, patchList)

	if len(patchList) > 0 {
		patch, err := json.Marshal(patchList)
		if err != nil {
			log.Printf("ERROR: Patch marshall error %v:%v\n", patchList, err)
			reviewResponse.Allowed = false
			return toAdmissionResponse(err)
		}
		reviewResponse.Patch = []byte(patch)
		pt := v1beta1.PatchTypeJSONPatch
		reviewResponse.PatchType = &pt
	}

	return &reviewResponse
}

func setNodeSelector(pvc corev1.PersistentVolumeClaim, patchList []patch, rr *roundrobin.Balancer, nodeLabel string) ([]patch, string, error) {
	var patchItem patch
	nodeSelectorMap := make(map[string]string)
	if nodeSel, ok := pvc.ObjectMeta.Annotations[nodeSelector]; ok {
		if nodeSel != "" {
			err := json.Unmarshal([]byte(nodeSel), &nodeSelectorMap)
			if err != nil {
				return patchList, "", errors.New("ERROR: Cannot parse nodeselector " + nodeSel + " because: " + err.Error())
			}
		}
	}
	s := []string{}
	if nodeLabel != "" {
		s = append(s, nodeLabel)
	}
	if len(nodeSelectorMap) > 0 {
		for key, value := range nodeSelectorMap {
			s = append(s, key+"="+value)
		}
	} else {
		if scDefaultSelector, ok := parsedDefaultSelector[*pvc.Spec.StorageClassName]; ok {
			for _, selector := range strings.Split(scDefaultSelector.DefaultNodeSelector, ",") {
				key := strings.Trim(strings.Split(selector, ":")[0], "\"{}")
				value := strings.Trim(strings.Split(selector, ":")[1], "\"{}")
				s = append(s, key+"="+value)
			}
		}
	}
	selector := strings.Join(s, ",")
	node, err := k8sclient.GetNodeByLabel(selector, nodeSelectMethod, rr)
	if err != nil {
		return patchList, "", errors.New("ERROR: Cannot query node by label, because: " + err.Error())
	}
	patchItem.Op = "add"
	patchItem.Path = "/metadata/annotations"
	patchItem.Value = json.RawMessage(`{"` + nodeNameAnnotation + `":"` + node.ObjectMeta.Name + `"}`)

	patchList = append(patchList, patchItem)
	return patchList, node.ObjectMeta.Name, nil
}

func patchVolumeNameAndPvDir(pvc corev1.PersistentVolumeClaim, nodeName string, patchList []patch) []patch {
	var patchItem patch
	pvDirName := pvc.ObjectMeta.Namespace + "_" + pvc.ObjectMeta.Name + "-" + generateRandomSuffix(8)
	volumeName := generatePVName(pvDirName, nodeName, *(pvc.Spec.StorageClassName))

	patchItem.Op = "add"
	patchItem.Path = "/metadata/annotations/" + patchPvDirName
	patchItem.Value = json.RawMessage(`"` + pvDirName + `"`)
	patchList = append(patchList, patchItem)

	patchItem.Path = "/spec/volumeName"
	patchItem.Value = json.RawMessage(`"` + volumeName + `"`)
	patchList = append(patchList, patchItem)

	return patchList
}

func generatePVName(file, node, class string) string {
	h := fnv.New32a()
	h.Write([]byte(file))
	h.Write([]byte(node))
	h.Write([]byte(class))
	// This is the FNV-1a 32-bit hash
	return fmt.Sprintf("local-pv-%x", h.Sum32())
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
