package consistent

import (
	"context"
	"fmt"
	"os"

	"github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
	"github.com/argoproj/argo-cd/v2/util/errors"
	"github.com/buraksezer/consistent"
	"github.com/cespare/xxhash"
	"github.com/sirupsen/logrus"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

type Member string

func (m Member) String() string {
	return string(m)
}

type hasher struct{}

func (h hasher) Sum64(data []byte) uint64 {
	return xxhash.Sum64(data)
}

func initHashRing(members []consistent.Member) *consistent.Consistent {
	cfg := consistent.Config{
		PartitionCount:    17,
		ReplicationFactor: 2,
		Load:              20.0,
		Hasher:            hasher{},
	}
	return consistent.New(members, cfg)
}

func GetClusterFilter(kubernetesClient *kubernetes.Clientset) func(c *v1alpha1.Cluster) bool {
	podIPAddress := getPodIP()
	members, err := getMembers(kubernetesClient)
	members = append(members, Member(podIPAddress))
	consistentHashRing := initHashRing(members)
	if err != nil {
		errors.CheckError(err)
	}
	return func(c *v1alpha1.Cluster) bool {
		members, err := getMembers(kubernetesClient)
		if err != nil {
			logrus.Info("Error while getting members of consistent hash ring:" + err.Error())
			return false
		}
		if c == nil {
			logrus.Info("Cluster is nil")
			return false
		}
		for _, member := range members {
			consistentHashRing.Add(member)
		}
		ownerIP := consistentHashRing.LocateKey([]byte(c.ID))
		logrus.Info(fmt.Sprintf("Cluster server URL: %s, owner IP: %s, pod IP: %s", c.Server, ownerIP.String(), podIPAddress))
		return ownerIP.String() == podIPAddress
	}
}

func getMembers(kubernetesClient *kubernetes.Clientset) ([]consistent.Member, error) {
	ipAddresses := []consistent.Member{}
	endpoints, err := kubernetesClient.CoreV1().Endpoints(getPodNamespace()).Get(context.Background(), "argocd-metrics", v1.GetOptions{})
	if err != nil {
		return ipAddresses, err
	}
	for _, subset := range endpoints.Subsets {
		for _, address := range subset.Addresses {
			logrus.Info(fmt.Sprintf("Adding member: %s", address.IP))
			ipAddresses = append(ipAddresses, Member(address.IP))
		}
	}
	return ipAddresses, nil
}

func getPodIP() string {
	podIP, found := os.LookupEnv("POD_IP")
	if !found {
		errors.CheckError(fmt.Errorf("environment variable POD_IP required for getting the shard value"))
	}
	return podIP
}

func getPodNamespace() string {
	podNamespace, found := os.LookupEnv("POD_NAMESPACE")
	if !found {
		errors.CheckError(fmt.Errorf("environment variable POD_NAMESPACE required for getting the shard value is not set"))
	}
	return podNamespace
}
