#!/bin/bash
# hack/generate-scale-test-apps.sh — Generate test applications for performance benchmarking
# Usage: ./hack/generate-scale-test-apps.sh [count] [namespace]
#   count:     Number of applications to create (default: 1000)
#   namespace: Argo CD namespace (default: argocd)
#
# Each application gets:
#   - A unique destination namespace (scale-ns-NNNN)
#   - Auto-sync enabled (so you can watch syncs in the UI)
#   - The guestbook example app from argocd-example-apps
set -e

COUNT=${1:-1000}
NAMESPACE=${2:-argocd}

echo "Generating ApplicationSet with $COUNT applications in namespace $NAMESPACE..."
echo "  - Unique destination namespace per app (scale-ns-NNNN)"
echo "  - Auto-sync enabled"
echo ""

cat <<EOF | kubectl apply --server-side -f -
apiVersion: argoproj.io/v1alpha1
kind: ApplicationSet
metadata:
  name: scale-test
  namespace: $NAMESPACE
spec:
  goTemplate: true
  goTemplateOptions: ["missingkey=error"]
  generators:
  - list:
      elements:
$(for i in $(seq -w 0 $((COUNT - 1))); do
  echo "      - name: scale-app-${i}"
  echo "        destNamespace: scale-ns-${i}"
done)
  template:
    metadata:
      name: '{{ .name }}'
    spec:
      project: default
      source:
        repoURL: https://github.com/argoproj/argocd-example-apps.git
        targetRevision: HEAD
        path: guestbook
      destination:
        server: https://kubernetes.default.svc
        namespace: '{{ .destNamespace }}'
      syncPolicy:
        automated:
          prune: true
          selfHeal: true
        syncOptions:
          - CreateNamespace=true
EOF

echo ""
echo "Waiting for applications to be created..."
sleep 5

ACTUAL=$(kubectl get applications -n "$NAMESPACE" --no-headers 2>/dev/null | wc -l | tr -d ' ')
echo "Created $ACTUAL applications (requested $COUNT)"

echo ""
echo "To verify via API:"
echo "  curl -sk https://localhost:8080/api/v1/applications | jq '.items | length'"
echo ""
echo "To clean up:"
echo "  kubectl delete applicationset scale-test -n $NAMESPACE"
echo "  # Namespaces are NOT auto-deleted. To clean them up:"
echo "  kubectl get ns -o name | grep scale-ns- | xargs kubectl delete"
