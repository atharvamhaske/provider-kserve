#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
CHART=${CHART_DIR:-"$ROOT/charts/provider-kserve"}
YQ_BIN=${YQ:-yq}
TMP_DIR=$(mktemp -d)
trap 'rm -rf "$TMP_DIR"' EXIT

DEFAULT_MANIFEST="$TMP_DIR/default.yaml"
ENABLED_MANIFEST="$TMP_DIR/enabled.yaml"

helm lint "$CHART" >/dev/null
helm template provider-kserve "$CHART" >"$DEFAULT_MANIFEST"
helm template provider-kserve "$CHART" \
  --set kserveResources.kserve.localmodel.enabled=true >"$ENABLED_MANIFEST"

resource_name() {
  kind=$1
  name=$2
  manifest=$3
  "$YQ_BIN" ea -r \
    "select(.kind == \"$kind\" and .metadata.name == \"$name\") | .metadata.name" \
    "$manifest"
}

crd_hook_command() {
  "$YQ_BIN" ea -r \
    'select(.kind == "Job") | .spec.template.spec.containers[] | select(.name == "kserve-crds") | .command[2]' \
    "$1"
}

if [ -n "$(resource_name Deployment kserve-localmodel-controller-manager "$DEFAULT_MANIFEST")" ]; then
  echo "LocalModel controller rendered while disabled" >&2
  exit 1
fi
if [ -n "$(resource_name DaemonSet kserve-localmodelnode-agent "$DEFAULT_MANIFEST")" ]; then
  echo "LocalModel node agent rendered while disabled" >&2
  exit 1
fi
case "$(crd_hook_command "$DEFAULT_MANIFEST")" in
  *kserve-localmodel-crd*)
    echo "LocalModel CRD rendered while disabled" >&2
    exit 1
    ;;
esac

if [ "$(resource_name Deployment kserve-localmodel-controller-manager "$ENABLED_MANIFEST")" != "kserve-localmodel-controller-manager" ]; then
  echo "LocalModel controller missing while enabled" >&2
  exit 1
fi
if [ "$(resource_name DaemonSet kserve-localmodelnode-agent "$ENABLED_MANIFEST")" != "kserve-localmodelnode-agent" ]; then
  echo "LocalModel node agent missing while enabled" >&2
  exit 1
fi
if [ "$("$YQ_BIN" ea -r 'select(.kind == "ConfigMap" and .metadata.name == "inferenceservice-config") | .data.localModel | from_json | .enabled' "$ENABLED_MANIFEST")" != "true" ]; then
  echo "KServe LocalModel admission config is not enabled" >&2
  exit 1
fi
case "$(crd_hook_command "$ENABLED_MANIFEST")" in
  *kserve-localmodel-crd*) ;;
  *)
    echo "LocalModel CRD is missing while enabled" >&2
    exit 1
    ;;
esac
