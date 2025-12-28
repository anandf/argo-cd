package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"

	graphcache "github.com/argoproj/argo-cd/v3/controller/graphcache"
)

func main() {
	// Command line flags
	kubeconfig := flag.String("kubeconfig", "", "Path to kubeconfig file")
	namespace := flag.String("namespace", "", "Namespace to watch (empty for all namespaces)")
	trackingMethod := flag.String("tracking-method", "annotation+label", "Tracking method: label, annotation, or annotation+label")
	logLevel := flag.String("log-level", "info", "Log level: debug, info, warn, error")
	duration := flag.Int("duration", 60, "How long to run (seconds)")
	flag.Parse()

	// Configure logging
	level, err := log.ParseLevel(*logLevel)
	if err != nil {
		log.Fatalf("Invalid log level: %s", *logLevel)
	}
	log.SetLevel(level)
	log.SetFormatter(&log.TextFormatter{
		FullTimestamp: true,
	})

	log.Info("Graph Cache POC Demo")
	log.Info("===================")

	// Build Kubernetes client config
	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		log.Fatalf("Failed to build config: %v", err)
	}

	// Create dynamic client
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		log.Fatalf("Failed to create dynamic client: %v", err)
	}

	// Create discovery client
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		log.Fatalf("Failed to create discovery client: %v", err)
	}

	// Parse tracking method
	method, err := graphcache.ParseTrackingMethod(*trackingMethod)
	if err != nil {
		log.Fatalf("Invalid tracking method: %v", err)
	}

	// Determine namespaces to watch
	var namespaces []string
	if *namespace != "" {
		namespaces = []string{*namespace}
	}

	// Create graph cache
	log.WithFields(log.Fields{
		"tracking_method": method,
		"namespaces":      namespaces,
	}).Info("Creating graph cache")

	cache, err := graphcache.NewGraphCache(graphcache.Config{
		DynamicClient:   dynamicClient,
		DiscoveryClient: discoveryClient,
		TrackingMethod:  method,
		Namespaces:      namespaces,
	})
	if err != nil {
		log.Fatalf("Failed to create graph cache: %v", err)
	}

	// Start the cache
	if err := cache.Start(); err != nil {
		log.Fatalf("Failed to start graph cache: %v", err)
	}

	log.Info("Graph cache started successfully")
	log.Info("")

	// Print initial metrics
	printMetrics(cache)

	// Set up signal handling
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Create ticker for periodic metrics
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	// Create timeout
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*duration)*time.Second)
	defer cancel()

	log.Infof("Running for %d seconds (Ctrl+C to stop early)...", *duration)
	log.Info("")

	// Main loop
	running := true
	for running {
		select {
		case <-ctx.Done():
			log.Info("Timeout reached")
			running = false

		case <-sigChan:
			log.Info("Received interrupt signal")
			running = false

		case <-ticker.C:
			printMetrics(cache)
		}
	}

	log.Info("")
	log.Info("Final Metrics:")
	log.Info("==============")
	printMetrics(cache)
	printResourceSummary(cache)

	// Shutdown
	log.Info("Shutting down...")
	cache.Shutdown()
	log.Info("Shutdown complete")
}

func printMetrics(cache *graphcache.GraphCache) {
	metrics := cache.GetMetrics()

	log.Info("Cache Metrics:")
	log.Infof("  Total Managed Resources: %d", metrics.TotalManagedResources)
	log.Infof("  Active Watches: %d", metrics.ActiveWatches)
	log.Infof("  Unique Resource Types: %d", len(metrics.ResourcesByType))
	log.Infof("  Unique Applications: %d", len(metrics.ResourcesByApplication))
	log.Infof("  Total Events: %d (Add: %d, Update: %d, Delete: %d)",
		metrics.TotalEvents, metrics.AddEvents, metrics.UpdateEvents, metrics.DeleteEvents)

	if metrics.DiscoveryRuns > 0 {
		log.Infof("  Last Discovery: %s (took %s)",
			metrics.LastDiscoveryTime.Format("15:04:05"),
			metrics.LastDiscoveryDuration)
		log.Infof("  Resources Discovered: %d", metrics.ResourcesDiscovered)
		log.Infof("  Descendant Types Added: %d", metrics.DescendantTypesAdded)
	}

	if metrics.AverageEventProcessTime > 0 {
		log.Infof("  Avg Event Process Time: %s", metrics.AverageEventProcessTime)
	}

	log.Info("")
}

func printResourceSummary(cache *graphcache.GraphCache) {
	metrics := cache.GetMetrics()

	log.Info("Resources by Type:")
	for gk, count := range metrics.ResourcesByType {
		group := gk.Group
		if group == "" {
			group = "core"
		}
		log.Infof("  %s/%s: %d", group, gk.Kind, count)
	}
	log.Info("")

	log.Info("Resources by Application:")
	for app, count := range metrics.ResourcesByApplication {
		log.Infof("  %s: %d resources", app, count)
	}
	log.Info("")

	log.Info("Active Watches:")
	for gk := range metrics.WatchesByType {
		group := gk.Group
		if group == "" {
			group = "core"
		}
		log.Infof("  %s/%s", group, gk.Kind)
	}
	log.Info("")

	// Print comparison estimate
	estimateTraditionalWatches := estimateTraditionalCacheWatches()
	reduction := 0.0
	if estimateTraditionalWatches > 0 {
		reduction = float64(estimateTraditionalWatches-metrics.ActiveWatches) / float64(estimateTraditionalWatches) * 100
	}

	log.Info("Comparison:")
	log.Infof("  Estimated Traditional Cache Watches: %d", estimateTraditionalWatches)
	log.Infof("  Graph Cache Watches: %d", metrics.ActiveWatches)
	log.Infof("  Reduction: %.1f%%", reduction)
	log.Info("")
}

// estimateTraditionalCacheWatches provides a rough estimate of how many watches
// the traditional cache would create. This is a simplified calculation.
func estimateTraditionalCacheWatches() int {
	// Common resource types watched by traditional cache
	// This is a conservative estimate
	commonTypes := []string{
		"Deployment", "StatefulSet", "DaemonSet", "ReplicaSet",
		"Pod", "Service", "Endpoints", "EndpointSlice",
		"ConfigMap", "Secret", "ServiceAccount",
		"Ingress", "NetworkPolicy",
		"Job", "CronJob",
		"PersistentVolume", "PersistentVolumeClaim",
		"Role", "RoleBinding", "ClusterRole", "ClusterRoleBinding",
		"HorizontalPodAutoscaler",
		// Plus many CRDs...
	}
	return len(commonTypes)
}

func printHelp() {
	fmt.Println("Graph Cache POC Demo")
	fmt.Println("")
	fmt.Println("This demo shows the graph-based cache discovering and watching")
	fmt.Println("only resources managed by Argo CD applications.")
	fmt.Println("")
	fmt.Println("Usage:")
	flag.PrintDefaults()
	fmt.Println("")
	fmt.Println("Examples:")
	fmt.Println("  # Watch all namespaces with annotation+label tracking")
	fmt.Println("  go run main.go")
	fmt.Println("")
	fmt.Println("  # Watch specific namespace with label-only tracking")
	fmt.Println("  go run main.go -namespace=argocd -tracking-method=label")
	fmt.Println("")
	fmt.Println("  # Debug mode with custom kubeconfig")
	fmt.Println("  go run main.go -kubeconfig=~/.kube/config -log-level=debug")
}
