# Cache model weights on nodes

KServe LocalModel stores model weights on selected nodes. New model Pods mount the cached PVC instead of downloading the model again.

The feature is off by default. It installs cluster-wide RBAC, a controller, a node agent DaemonSet, download Jobs, PVs, and PVCs.

## Enable LocalModel

Enable the controller during chart installation or upgrade:

```bash
helm upgrade --install provider-kserve \
  oci://ghcr.io/openeverest/charts/provider-kserve \
  --namespace everest-system \
  --set kserveResources.kserve.localmodel.enabled=true
```

Create the namespace that runs model download Jobs:

```bash
kubectl create namespace kserve-localmodel-jobs
```

The chart installs these LocalModel CRDs when the feature is enabled:

- `LocalModelNodeGroup`
- `LocalModelCache`
- `LocalModelNamespaceCache`
- `LocalModelNode`

## Prepare nodes

Label each cache node:

```bash
kubectl label node <node-name> kserve/localmodel=worker
```

Read the host path from the installed node agent. Use this exact path in the node group:

```bash
kubectl -n everest-system get daemonset kserve-localmodelnode-agent \
  -o jsonpath='{.spec.template.spec.volumes[?(@.name=="models")].hostPath.path}{"\n"}'
```

KServe v0.21 currently renders `/models`. Do not use the container mount path `/mnt/models` in the node group.

Apply the complete public-model example:

```bash
kubectl apply -f examples/local-model-cache.yaml
```

## Configure download credentials

Download Jobs run in `kserve-localmodel-jobs`. Create credential Secrets and ServiceAccounts in that namespace.

For a gated Hugging Face model, create these resources:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: hf-secret
  namespace: kserve-localmodel-jobs
type: Opaque
stringData:
  HF_TOKEN: <token>
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: hf-downloader
  namespace: kserve-localmodel-jobs
secrets:
  - name: hf-secret
```

Set `spec.serviceAccountName: hf-downloader` on the cache. Do not copy Instance namespace Secrets into the job namespace automatically.

## Create caches

Use `LocalModelNamespaceCache` unless several namespaces must share one cache. It limits cache use to one namespace.

The cache URI must match the Instance model URI. KServe admission adds cache labels and rewrites the workload to the cached PVC.

Do not set `disableStorageInitializer: true` on the Instance. KServe performs the cache rewrite inside the storage initialization path.

The first cache population still downloads the model. Later Pods reuse the node-local copy.

## Check cache state

Inspect the cache:

```bash
kubectl -n models get localmodelnamespacecache smollm2-135m -o yaml
```

Check the Instance message:

```bash
kubectl -n models get instance smollm2-cached \
  -o jsonpath='{.status.message}{"\n"}'
```

A populated cache reports a message such as:

```text
Model cache "smollm2-135m": 1/1 nodes ready
```

Verify that the admitted service uses the cache:

```bash
kubectl -n models get llminferenceservice smollm2-cached \
  -o jsonpath='{.metadata.labels.internal\.serving\.kserve\.io/localmodel}{"\n"}'
```

Verify that the model Pod mounts a PVC and has no storage initializer:

```bash
kubectl -n models get pod -l app.kubernetes.io/name=smollm2-cached -o yaml
```

## Capacity and deletion

`spec.modelSize` reserves capacity. Each selected node stores one full model copy.

Do not delete a cache while an Instance uses it. KServe rejects deletion when it finds a matching serving resource.

Node replacement removes that node's local copy. KServe downloads the model again on the replacement node.
