# Zookeeper Operator Helm Chart

Installs [Zookeeper Operator](https://github.com/pravega/zookeeper-operator) to create/configure/manage Zookeeper clusters atop Kubernetes.

## Introduction

This chart bootstraps a [Zookeeper Operator](https://github.com/pravega/zookeeper-operator) deployment on a [Kubernetes](http://kubernetes.io) cluster using the [Helm](https://helm.sh) package manager.

## Prerequisites
  - Kubernetes 1.15+ with Beta APIs
  - Helm 3.2.1+

## Installing the Chart

To install the zookeeper-operator chart, use the following command:

```bash
# Install latest version
$ helm install [RELEASE_NAME] oci://ghcr.io/adobe/helm-charts/zookeeper-operator

# Or install a specific version
$ helm install [RELEASE_NAME] oci://ghcr.io/adobe/helm-charts/zookeeper-operator --version=[VERSION]
```

> **Note:** You can view all available versions at [adobe/helm-charts/zookeeper-operator](https://github.com/adobe/zookeeper-operator/pkgs/container/helm-charts%2Fzookeeper-operator/versions?filters%5Bversion_type%5D=tagged)
- **[RELEASE_NAME]** is the release name for the zookeeper-operator chart.
- **[DEPLOYMENT_NAME]** is the name of the zookeeper-operator deployment so created. (If [RELEASE_NAME] contains the string `zookeeper-operator`, `[DEPLOYMENT_NAME] = [RELEASE_NAME]`, else `[DEPLOYMENT_NAME] = [RELEASE_NAME]-zookeeper-operator`. The [DEPLOYMENT_NAME] can however be overridden by providing `--set fullnameOverride=[DEPLOYMENT_NAME]` along with the helm install command)
- **[VERSION]** can be any stable release version for zookeeper-operator from 0.2.8 onwards.

This command deploys a zookeeper-operator on the Kubernetes cluster in its default configuration. The [configuration](#configuration) section lists the parameters that can be configured during installation.

## Uninstalling the Chart

To uninstall/delete the zookeeper-operator chart, use the following command:

```
$ helm uninstall [RELEASE_NAME]
```

The command removes all the Kubernetes components associated with the chart and deletes the release.

## Configuration

The following table lists the configurable parameters of the zookeeper-operator chart and their default values.

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| additionalEnv | list | `[]` | Additional Environment Variables |
| additionalSidecars | list | `[]` | Additional Sidecars Configuration |
| additionalVolumes | list | `[]` | Additional volumes required for sidecars |
| affinity | object | `{}` | Operator pod affinity can be set |
| annotations | object | `{}` | Pod annotations |
| crd.create | bool | `true` | Create zookeeper CRD |
| disableFinalizer | bool | `false` | Disable finalizer for zookeeper clusters, PVCs clean-up will be skipped |
| global.imagePullSecrets | list | `[]` | Lists the secrets you need to use to pull zookeeper-operator image from a private registry. |
| image.pullPolicy | string | `"IfNotPresent"` | Image pull policy |
| image.repository | string | `"ghcr.io/adobe/zookeeper-operator"` | Image repository |
| image.tag | string | `"0.2.15"` | Image tag |
| labels | object | `{}` | Additional labels to be added to resources |
| metricsBindAddress | string | `"127.0.0.1"` |  |
| metricsPort | string | `"6000"` |  |
| nodeSelector | object | `{}` | Operator pod node selector can be set |
| rbac.create | bool | `true` | Install RBAC roles and bindings. |
| resources | object | `{}` | Specifies resource requirements for the container |
| securityContext | object | `{}` | Security context for the operator container (not the Pod or additional sidecars). |
| serviceAccount.create | bool | `true` | Create service account |
| serviceAccount.name | string | `"zookeeper-operator"` | Name for the service account |
| tolerations | list | `[]` | Operator pod tolerations can be set |
| watchNamespace | string | `""` | List of namespaces where Operator watches for custom resources. |

`securityContext: {}` keeps the image's existing user and security settings.
Nonempty settings now apply to the operator container. Choose a user and
filesystem policy compatible with your operator image; an arbitrary `runAsUser`
is not guaranteed to work. Additional sidecars keep their own settings.

Run the chart rendering regression tests with Helm and Go installed:

```bash
go test -tags=manifests ./test/manifests -count=1
```
