package sharding

import (
	"context"
	"fmt"
	"hash/fnv"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
	"github.com/redis/go-redis/v9"
	log "github.com/sirupsen/logrus"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

type getClusterShardFn func(c *v1alpha1.Cluster) int

func InferShard() (int, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return 0, err
	}
	parts := strings.Split(hostname, "-")
	if len(parts) == 0 {
		return 0, fmt.Errorf("hostname should ends with shard number separated by '-' but got: %s", hostname)
	}
	shard, err := strconv.Atoi(parts[len(parts)-1])
	if err != nil {
		return 0, fmt.Errorf("hostname should ends with shard number separated by '-' but got: %s", hostname)
	}
	return shard, nil
}

func InferShardFromPodIP(kubernetesClient *kubernetes.Clientset) (int, error) {
	podIP, found := os.LookupEnv("POD_IP")
	if !found {
		return 0, fmt.Errorf("environment variable POD_IP required for getting the shard value is not set")

	}
	podNamespace, found := os.LookupEnv("POD_NAMESPACE")
	if !found {
		return 0, fmt.Errorf("environment variable POD_NAMESPACE required for getting the shard value is not set")
	}
	endpoints, err := kubernetesClient.CoreV1().Endpoints(podNamespace).Get(context.Background(), "argocd-metrics", v1.GetOptions{})
	if err != nil {
		return 0, err
	}
	ipAddresses := []string{}
	for _, subset := range endpoints.Subsets {
		for _, address := range subset.Addresses {
			ipAddresses = append(ipAddresses, address.IP)
		}
	}
	sort.Strings(ipAddresses)
	for index, ipAddress := range ipAddresses {
		if ipAddress == podIP {
			log.Info(fmt.Sprintf("Returning shard  %d for Pod IP %s", index, podIP))
			return index, nil
		}
	}
	return 0, fmt.Errorf("could not find pod ip %s in the endpoints object", podIP)
}

// GetReplicaCount returns the replica count configured for the application controller deployment
func GetReplicaCount(kubernetesClient *kubernetes.Clientset) (int, error) {
	podNamespace, found := os.LookupEnv("POD_NAMESPACE")
	if !found {
		return 0, fmt.Errorf("environment variable POD_NAMESPACE required for getting the shard value is not set")
	}
	deployment, err := kubernetesClient.AppsV1().Deployments(podNamespace).Get(context.Background(), "argocd-application-controller", v1.GetOptions{})
	if err != nil {
		return 0, err
	}
	return int(*deployment.Spec.Replicas), nil
}

// GetShardByID calculates cluster shard as `clusterSecret.UID % replicas count`
func GetShardByID(id string, replicas int) int {
	if id == "" {
		return 0
	} else {
		h := fnv.New32a()
		_, _ = h.Write([]byte(id))
		return int(h.Sum32() % uint32(replicas))
	}
}

func GetClusterFilter(kubernetesClient *kubernetes.Clientset, shardFn getClusterShardFn) func(c *v1alpha1.Cluster) bool {
	return func(c *v1alpha1.Cluster) bool {
		shard, err := InferShardFromPodIP(kubernetesClient)
		if err != nil {
			log.Error(fmt.Sprintf("error while getting the shard of the current POD %s", err.Error()))
			return false
		}
		clusterShard := 0
		//  cluster might be nil if app is using invalid cluster URL, assume shard 0 in this case.
		if c != nil {
			if c.Shard != nil {
				clusterShard = int(*c.Shard)
			} else {
				clusterShard = shardFn(c)
			}
		}
		return clusterShard == shard
	}
}

func GetShardFnById(kubernetesClient *kubernetes.Clientset) getClusterShardFn {
	return func(c *v1alpha1.Cluster) int {
		replicas := getReplicaCount(kubernetesClient)
		if replicas <= 1 {
			return 0
		}
		if c.ID == "" {
			log.Info("cluster ID is empty, returning 0")
			return 0
		} else {
			h := fnv.New32a()
			_, _ = h.Write([]byte(c.ID))
			shardValue := int(h.Sum32() % uint32(replicas))
			log.Info(fmt.Sprintf("Calculated shard value from cluster secret UUID: %d", shardValue))
			return shardValue
		}
	}
}

func GetShardFnByIndexPos(kubernetesClient *kubernetes.Clientset, redisClient *redis.Client) getClusterShardFn {
	return func(c *v1alpha1.Cluster) int {
		replicas := getReplicaCount(kubernetesClient)
		if replicas <= 1 {
			return 0
		}
		if c == nil {
			log.Error("cluster is nil, returning default shard value 0")
			return 0
		}
		_, err := redisClient.ZAdd(context.Background(), "clusterset", redis.Z{Member: c.Server, Score: float64(1)}).Result()
		if err != nil {
			log.Error(fmt.Sprintf("could not add cluster server url %s to the redis cache: %s", c.Server, err.Error()))
			return 0
		}
		index, err := redisClient.ZRank(context.Background(), "clusterset", c.Server).Result()
		if err != nil {
			log.Error(fmt.Sprintf("could not get the index for cluster server url %s from the redis cache: %s", c.Server, err.Error()))
			return 0
		}
		return int(index) % replicas
	}
}

func getReplicaCount(kubernetesClient *kubernetes.Clientset) int {
	replicas, err := GetReplicaCount(kubernetesClient)
	if err != nil {
		log.Error(fmt.Sprintf("error while getting the replica count %s", err.Error()))
		return 0
	}
	log.Info(fmt.Sprintf("Replica count: %d", replicas))
	return replicas
}
