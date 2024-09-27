package main

import (
	"crypto/tls"
	"flag"
	"log"
	"net/http"
	"time"

	"github.com/nokia/dynamic-local-pv-provisioner/pkg/k8sclient"
	"github.com/nokia/dynamic-local-pv-provisioner/pkg/mutator"
)

var nodeSelectMethod string

func main() {
	cert := flag.String("tls-cert-bundle", "", "file containing the x509 Certificate for HTTPS. (CA cert, if any, concatenated after server cert).")
	key := flag.String("tls-private-key-file", "", "file containing the x509 private key matching --tls-cert-bundle.")
	nodeLabel := flag.String("node-label-for-dynamic", "", " node label for dynamic local pv provisoner. Optional parameter, only required when local-storage not configured on all nodes.")
	flag.StringVar(&nodeSelectMethod, "node-selector-method", "round robin", "node selector method. Acceptable values: \"round robin\" or \"capacity\", default is \"round robin\"")
	flag.Parse()
	if nodeSelectMethod != k8sclient.RR && nodeSelectMethod != k8sclient.Cap {
		log.Fatalln("ERROR: Unacceptable node-selector-method! Acceptable values: \"round robin\" or \"capacity\", default is \"round robin\"")
	}
	mutate, err := mutator.NewMutator(nodeSelectMethod, *nodeLabel)
	if cert == nil || key == nil {
		log.Fatalln("ERROR: Configuring TLS is mandatory, --tls-cert-bundle and --tls-private-key-file cannot be empty!")
		return
	}
	tlsConf, err := tls.LoadX509KeyPair(*cert, *key)
	if err != nil {
		log.Println("ERROR: TLS configuration could not be initialized, because:" + err.Error())
		return
	}

	http.HandleFunc("/mutating-pvc", mutate.ServeMutatePvc)
	server := &http.Server{
		Addr:         ":443",
		TLSConfig:    &tls.Config{Certificates: []tls.Certificate{tlsConf}},
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	}
	log.Println("INFO:DLPP webhook is about to start listening on :443")
	err = server.ListenAndServeTLS("", "")
	log.Fatal(err)
}
