package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/labooner/docker-registry-client/registry"
	"github.com/opencontainers/go-digest"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

type PatchSpec struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value string `json:"value"`
}

type PatchMapSpec struct {
	Op    string            `json:"op"`
	Path  string            `json:"path"`
	Value map[string]string `json:"value"`
}

func splitImageDescriptor(descriptor string) (string, string, string) {
	descriptor_parts := strings.Split(descriptor, ":")
	url := descriptor_parts[0]
	tag := descriptor_parts[1]
	url_parts := strings.Split(url, "/")
	server := url_parts[0]
	repo := strings.Join(url_parts[1:], "/")

	return server, repo, tag
}

func makeInsecureRegistry(registryURL, username, password string) (*registry.Registry, error) {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
		},
	}

	url := strings.TrimSuffix(registryURL, "/")
	wrappedTransport := registry.WrapTransport(transport, url, username, password)
	registry := &registry.Registry{
		URL: url,
		Client: &http.Client{
			Transport: wrappedTransport,
		},
		Logf: log.Printf,
	}

	if err := registry.Ping(); err != nil {
		return nil, err
	}

	return registry, nil
}

func getLatestDigest(server string, repository string, tag string, user string, password string) (*digest.Digest, error) {
	registry, err := makeInsecureRegistry(server, user, password)
	if err != nil {
		return nil, err
	}

	manifest, err := registry.ManifestV2(repository, tag)
	if err != nil {
		return nil, err
	}
	return &manifest.Config.Digest, nil
}

type DockerRepoAuth struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type DockerConfig struct {
	Auths map[string]DockerRepoAuth `json:"auths"`
}

func parseDockerConfig(config string) (*DockerConfig, error) {
	var dockerConfig DockerConfig
	err := json.Unmarshal([]byte(config), &dockerConfig)

	return &dockerConfig, err
}

func encodeJSONPointer(input string) string {
	return strings.ReplaceAll(strings.ReplaceAll(input, "~", "~0"), "/", "~1")
}

func main() {
	var checks []string
	var namespace string = "default"
	if env, found := os.LookupEnv("CHECKS"); found {
		checks = strings.Split(env, ",")
	} else {
		fmt.Println("CHECKS is not defined")
		os.Exit(1)
	}

	if env, found := os.LookupEnv("NAMESPACE"); found {
		namespace = env
	}
	log.Println("Using namespace:", namespace)

	var dockerConfig DockerConfig
	if env, found := os.LookupEnv("DOCKER_CONFIG"); found {
		pDockerConfig, err := parseDockerConfig(env)
		if err != nil {
			fmt.Println("Error occurred when parsing docker config")
			panic(err)
		}

		dockerConfig = *pDockerConfig
	}

	var config *rest.Config = nil
	if os.Getenv("ENV") == "PROD" {
		log.Println("use in-cluster config")
		pconfig, err := rest.InClusterConfig()
		if err != nil {
			panic(err.Error())
		}
		config = pconfig
	} else {
		log.Println("use kubeconfig")
		pconfig, err := clientcmd.BuildConfigFromFlags("", filepath.Join(homedir.HomeDir(), ".kube", "config"))
		if err != nil {
			panic(err.Error())
		}
		config = pconfig
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	for _, deploymentName := range checks {
		log.Printf("processing deployment %s\n", deploymentName)
		deployment, err := clientset.AppsV1().Deployments(namespace).Get(context.TODO(), deploymentName, metav1.GetOptions{})
		if err != nil {
			panic(err.Error())
		}

		var hasChanged = false
		for _, container := range deployment.Spec.Template.Spec.Containers {
			server, repo, tag := splitImageDescriptor(container.Image)
			var username = ""
			var password = ""
			if auth, found := dockerConfig.Auths[server]; found {
				username = auth.Username
				password = auth.Password
			}
			var serverUrlBuilder strings.Builder
			serverUrlBuilder.WriteString("https://")
			serverUrlBuilder.WriteString(server)
			digest, err := getLatestDigest(serverUrlBuilder.String(), repo, tag, username, password)
			if err != nil {
				fmt.Println("error encountered when fetching latest image digest", err)
				continue
			}
			log.Printf("image %s/%s:%s, digest: %s\n", server, repo, tag, digest.String())

			var annotationNameBuilder strings.Builder
			annotationNameBuilder.WriteString("imagePoller.zero.tw/last-known-digest-")
			annotationNameBuilder.WriteString(container.Name)
			annotationName := annotationNameBuilder.String()

			lastKnownDigest, _ := deployment.Annotations[annotationName]

			// update annotation
			patches := make([]PatchSpec, 1)
			patches[0].Op = "add"
			var pathBuilder strings.Builder
			pathBuilder.WriteString("/metadata/annotations/")
			pathBuilder.WriteString(encodeJSONPointer(annotationName))
			patches[0].Path = pathBuilder.String()
			patches[0].Value = digest.String()
			patchBytes, err := json.Marshal(patches)
			if err != nil {
				fmt.Println("error encountered when marshalling patches: ", err)
				continue
			}
			_, err = clientset.AppsV1().Deployments(namespace).Patch(context.TODO(), deploymentName, types.JSONPatchType, patchBytes, metav1.PatchOptions{})
			if err != nil {
				fmt.Println("error encountered when updating last known digest: ", err)
				continue
			}

			if lastKnownDigest != digest.String() {
				log.Printf("last known digest: %s - differs, will restart deployment\n", lastKnownDigest)
				hasChanged = true
			} else {
				log.Println("same digest - no change")
			}
		}

		// restart deployment if digest changed
		if hasChanged {
			log.Println("restarting deployment")

			patchBytes := make([]byte, 0)
			if len(deployment.Spec.Template.Annotations) == 0 {
				patches := make([]PatchMapSpec, 1)
				patches[0].Op = "add"
				var pathBuilder strings.Builder
				pathBuilder.WriteString("/spec/template/metadata/annotations")
				patches[0].Path = pathBuilder.String()
				annotations := make(map[string]string)
				annotations["kubectl.kubernetes.io/restartedAt"] = time.Now().Format(time.RFC3339)
				patches[0].Value = annotations
				patchBytes, err = json.Marshal(patches)
			} else {
				patches := make([]PatchSpec, 1)
				patches[0].Op = "add"
				var pathBuilder strings.Builder
				pathBuilder.WriteString("/spec/template/metadata/annotations/")
				pathBuilder.WriteString(encodeJSONPointer("kubectl.kubernetes.io/restartedAt"))
				patches[0].Path = pathBuilder.String()
				patches[0].Value = time.Now().Format(time.RFC3339)
				patchBytes, err = json.Marshal(patches)
			}

			if err != nil {
				fmt.Println("error encountered when marshalling patches: ", err)
				continue
			}
			_, err = clientset.AppsV1().Deployments(namespace).Patch(context.TODO(), deploymentName, types.JSONPatchType, patchBytes, metav1.PatchOptions{})
			if err != nil {
				fmt.Println("error encountered when restarting deployment: ", err)
				continue
			}
			log.Println("deployment restarted")
		}
	}
}
