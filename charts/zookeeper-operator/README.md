# Zookeeper Operator Helm Chart

Installs [Zookeeper Operator](https://github.com/pravega/zookeeper-operator) to create/configure/manage Zookeeper clusters atop Kubernetes.

## Introduction

This chart bootstraps a [Zookeeper Operator](https://github.com/pravega/zookeeper-operator) deployment on a [Kubernetes](http://kubernetes.io) cluster using the [Helm](https://helm.sh) package manager.

## Prerequisites
  - Kubernetes **1.34 or newer**; the chart rejects older versions.
  - Helm **3.8 or newer** for stable OCI support. CI also checks Helm 4.

Live CI targets Kubernetes **1.34** only, the minimum supported minor, with the
kind tool and node image pinned in the Makefile and e2e workflow. Later minors
are not tested or guaranteed. Use current security patches in production.
For clusters older than 1.34, retain an earlier operator/chart release.

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

## CRD ownership and upgrades

The recommended mode is external CRD ownership: apply the CRD manifest matching
the operator release first, then add `--set crd.create=false` to each Helm install
and upgrade. Helm will not create, adopt, update, or delete that CRD.

For chart-managed ownership, leave the existing default `crd.create=true`.
The chart updates its templated CRD on upgrade and annotates it with
`helm.sh/resource-policy: keep`, so removing the chart does not delete the CRD
or its custom resources. Validate schema compatibility before upgrades; retaining
the CRD is not a guarantee about PVC retention.

For an existing chart-managed release, first upgrade to this chart with
`crd.create=true`, so `keep` is recorded in the release manifest. Only then
upgrade again with `crd.create=false` to transfer schema management outside
Helm. Do not skip the first upgrade, delete the CRD, or strip ownership metadata
while the release still manages it.

An externally created CRD is not automatically adopted by enabling
`crd.create=true`. Keep using external mode unless you deliberately migrate
ownership. Retained CRDs keep their Helm ownership metadata; a different release
cannot silently take them over.

## Uninstalling the Chart

To uninstall/delete the zookeeper-operator chart, use the following command:

```
$ helm uninstall [RELEASE_NAME]
```

The command removes the release's operator resources. Chart-managed CRDs are
retained, including their custom resources; externally managed CRDs are untouched.
Delete a CRD only as an explicit, separate data-removal operation.
Deleting retained custom resources can still require a running operator to
complete their finalizers.

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
| crd.create | bool | `true` | Manage the ZooKeeper CRD with this Helm release; use false for an externally managed CRD. |
| disableFinalizer | bool | `false` | Disable finalizer for zookeeper clusters, PVCs clean-up will be skipped |
| global.imagePullSecrets | list | `[]` | Lists the secrets you need to use to pull zookeeper-operator image from a private registry. |
| image.pullPolicy | string | `"IfNotPresent"` | Image pull policy |
| image.repository | string | `"ghcr.io/adobe/zookeeper-operator"` | Image repository |
| image.tag | string | `"0.2.15-adobe-20261101"` | Image tag |
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

Run the chart and deployment rendering regression tests with Helm, Go, and the
Makefile-pinned Kustomize installed:

```bash
make kustomize
go test -tags=manifests ./test/manifests -count=1
```
