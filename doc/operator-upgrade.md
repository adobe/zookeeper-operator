# Upgrading Operator

## Before upgrading to a release that applies StatefulSets and Services

- Check the controller owner reference on each existing ZooKeeper StatefulSet and
  client, headless, and admin Service. Its UID must match the current
  `ZookeeperCluster` UID. The operator does not automatically adopt restored,
  manually created, or orphaned resources. If references do not match, pause the
  upgrade. Identify the resources and their storage before attempting recovery;
  do not delete the StatefulSet or PVCs to clear the error. Explicit orphan
  recovery is tracked in [#122](https://github.com/adobe/zookeeper-operator/issues/122).
- Check any custom selectors on the existing Services. The operator restores
  their selector to `app: <cluster-name>`, even when the owner UID matches.
  If clients need custom routing, create a separately managed Service with a
  different name and switch the clients to it before the upgrade.
- If you manage your own operator RBAC (for example `rbac.create: false`),
  compare it with the release's bundled role. In addition to existing permissions,
  grant `patch` on `statefulsets` (`apps`), `services`, `configmaps`, and
  `serviceaccounts` (core API group), `poddisruptionbudgets` (`policy`), and
  `zookeeperclusters` and `zookeeperclusters/status` (`zookeeper.pravega.io`).
  The bundled roles already include these permissions. A Ready operator Pod
  does not prove that the operator can write these resources.

## Upgrade the operator

zookeeperoperator can be upgraded to a version **[VERSION]** via helm using the following command

```
$ helm upgrade [ZOOKEEPER_OPERATOR_RELEASE_NAME] pravega/zookeeper-operator --version=[VERSION]
```
The zookeeper operator with deployment name **[DEPLOYMENT_NAME]** can also be upgraded manually by modifying the image tag using kubectl edit, patch or apply
```
$ kubectl edit deploy [DEPLOYMENT_NAME]
```
> Note: If you are upgrading zookeeper operator version to 0.2.9 or above manually, clusterrole has to be updated to include serviceaccounts. After updating clusterroles, zookeeper operator pod has to be restarted for the changes to take effect.

```
- apiGroups:
  - ""
  resources:
  - pods
  - services
  - endpoints
  - persistentvolumeclaims
  - events
  - configmaps
  - secrets
  - serviceaccounts
  verbs:
  - "*"
```
