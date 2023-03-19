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
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

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
		for _, address := range subset.NotReadyAddresses {
			ipAddresses = append(ipAddresses, address.IP)
		}
	}
	sort.Strings(ipAddresses)
	for index, ipAddress := range ipAddresses {
		if ipAddress == podIP {
			return index, nil
		}
	}

	return -1, fmt.Errorf("could not find pod ip %s in the endpoints object", podIP)
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

func GetClusterFilter(replicas int, shard int, shardFn getClusterShardFn) func(c *v1alpha1.Cluster) bool {
	return func(c *v1alpha1.Cluster) bool {
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

type getClusterShardFn func(c *v1alpha1.Cluster) int

func GetShardFnById(replicas int) getClusterShardFn {
	replicasInternal := replicas
	return func(c *v1alpha1.Cluster) int {
		if c.ID == "" {
			return 0
		} else {
			h := fnv.New32a()
			_, _ = h.Write([]byte(c.ID))
			return int(h.Sum32() % uint32(replicasInternal))
		}
	}
}

func GetShardFnByIndexPos(replicas int, client *redis.Client) getClusterShardFn {
	replicasInternal := replicas
	redisClient := client
	return func(c *v1alpha1.Cluster) int {
		if c == nil {
			return 0
		}
		_, err := redisClient.ZAdd(context.Background(), "clusterset", redis.Z{Member: c.Server, Score: float64(1)}).Result()
		if err != nil {
			return 0
		}
		index, err := redisClient.ZRank(context.Background(), "clusterset", c.Server).Result()
		if err != nil {
			return 0
		}
		return int(index) % replicasInternal
	}
}
