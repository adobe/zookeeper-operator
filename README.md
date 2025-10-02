
# Zookeeper Operator

### Project status: beta

The project is currently beta. While no breaking API changes are currently planned, we reserve the right to address bugs and change the API before the project is declared stable.

## Table of Contents

- [Zookeeper Operator](#zookeeper-operator)
    - [Project status: beta](#project-status-beta)
  - [Table of Contents](#table-of-contents)
    - [Overview](#overview)
  - [Requirements](#requirements)
  - [Usage](#usage)
    - [Install the operator](#install-the-operator)
      - [Install via helm](#install-via-helm)
      - [Manual deployment](#manual-deployment)
    - [Deploy a sample Zookeeper cluster](#deploy-a-sample-zookeeper-cluster)
      - [Manual deployment](#manual-deployment-1)
    - [Deploy a sample Zookeeper cluster with Ephemeral storage](#deploy-a-sample-zookeeper-cluster-with-ephemeral-storage)
    - [Deploy a sample Zookeeper cluster with Istio](#deploy-a-sample-zookeeper-cluster-with-istio)
    - [Upgrade a Zookeeper cluster](#upgrade-a-zookeeper-cluster)
      - [Trigger the upgrade manually](#trigger-the-upgrade-manually)
    - [Upgrade the Operator](#upgrade-the-operator)
    - [Uninstall the Zookeeper cluster](#uninstall-the-zookeeper-cluster)
      - [Manual uninstall](#manual-uninstall)
    - [Uninstall the operator](#uninstall-the-operator)
      - [Uninstall via helm](#uninstall-via-helm-1)
      - [Manual uninstall](#manual-uninstall-1)
    - [The AdminServer](#the-adminserver)
  - [Development](#development)
    - [Build the operator image](#build-the-operator-image)
    - [Published Docker Images](#published-docker-images)
    - [Direct access to the cluster](#direct-access-to-the-cluster)
    - [Run the operator locally](#run-the-operator-locally)
    - [Installation on Google Kubernetes Engine](#installation-on-google-kubernetes-engine)
    - [How to build Zookeeper Operator](#how-to-build-zookeeper-operator)


### Overview

This operator runs a Zookeeper 3.9.3 cluster, and uses Zookeeper dynamic reconfiguration to handle node membership.

The operator itself is built with the [Operator framework](https://github.com/operator-framework/operator-sdk).

## Requirements

- Access to a Kubernetes v1.15.0+ cluster

## Usage

We recommend using our [helm charts](charts) for all installation and upgrades. The helm chart for zookeeper operator is published to GitHub Container Registry (GHCR).

### Install the operator

> Note: if you are running on Google Kubernetes Engine (GKE), please [check this first](#installation-on-google-kubernetes-engine).

#### Install via helm

To install the zookeeper operator using helm:

> **Note:** You can view all available versions at [https://github.com/adobe/zookeeper-operator/pkgs/container/zookeeper-operator%2Fzookeeper-operator](https://github.com/adobe/zookeeper-operator/pkgs/container/zookeeper-operator%2Fzookeeper-operator)


```bash
# Install the CRDs
kubectl create -f https://raw.githubusercontent.com/adobe/zookeeper-operator/master/config/crd/bases/zookeeper.pravega.io_zookeeperclusters.yaml


# Install latest version
helm install zookeeper-operator oci://ghcr.io/adobe/helm-charts/zookeeper-operator

# Or install a specific version
helm install zookeeper-operator oci://ghcr.io/adobe/helm-charts/zookeeper-operator --version [VERSION]
```

For more detailed configuration options, refer to [this](charts/zookeeper-operator#installing-the-chart).

#### Manual deployment

Register the `ZookeeperCluster` custom resource definition (CRD).

```
$ kubectl create -f config/crd/bases
```

You can choose to enable Zookeeper operator for all namespaces or just for a specific namespace. The example is using the `default` namespace, but feel free to edit the Yaml files and use a different namespace.

Create the operator role and role binding.

```
// default namespace
$ kubectl create -f config/rbac/default_ns_rbac.yaml

// all namespaces
$ kubectl create -f config/rbac/all_ns_rbac.yaml
```

Deploy the Zookeeper operator.

```
$ kubectl create -f config/manager/manager.yaml
```

Verify that the Zookeeper operator is running.

```
$ kubectl get deploy
NAME                 DESIRED   CURRENT   UP-TO-DATE   AVAILABLE   AGE
zookeeper-operator   1         1         1            1           12m
```

### Deploy a sample Zookeeper cluster

#### Manual deployment

Create a Yaml file called `zk.yaml` with the following content to install a 3-node Zookeeper cluster.

```yaml
apiVersion: "zookeeper.pravega.io/v1beta1"
kind: "ZookeeperCluster"
metadata:
  name: "zookeeper"
spec:
  replicas: 3
```

```
$ kubectl create -f zk.yaml
```

After a couple of minutes, all cluster members should become ready.

```
$ kubectl get zk

NAME        REPLICAS   READY REPLICAS    VERSION   DESIRED VERSION   INTERNAL ENDPOINT    EXTERNAL ENDPOINT   AGE
zookeeper   3          3                 0.2.8     0.2.8             10.100.200.18:2181   N/A                 94s
```
>Note: when the Version field is set as well as Ready Replicas are equal to Replicas that signifies our cluster is in Ready state

Additionally, check the output of describe command which should show the following cluster condition

```
$ kubectl describe zk

Conditions:
  Last Transition Time:    2020-05-18T10:17:03Z
  Last Update Time:        2020-05-18T10:17:03Z
  Status:                  True
  Type:                    PodsReady

```
>Note: User should wait for the Pods Ready condition to be True

```
$ kubectl get all -l app=zookeeper
NAME                     DESIRED   CURRENT   AGE
statefulsets/zookeeper   3         3         2m

NAME             READY     STATUS    RESTARTS   AGE
po/zookeeper-0   1/1       Running   0          2m
po/zookeeper-1   1/1       Running   0          1m
po/zookeeper-2   1/1       Running   0          1m

NAME                     TYPE        CLUSTER-IP      EXTERNAL-IP   PORT(S)             AGE
svc/zookeeper-client     ClusterIP   10.31.243.173   <none>        2181/TCP            2m
svc/zookeeper-headless   ClusterIP   None            <none>        2888/TCP,3888/TCP   2m
```

```
apiVersion: "zookeeper.pravega.io/v1beta1"
kind: "ZookeeperCluster"
metadata:
  name: "example"
spec:
  pod:
    serviceAccountName: "zookeeper"
    resources:
        requests:
          cpu: 200m
          memory: 256Mi
        limits:
          cpu: 200m
          memory: 256Mi
```

### Deploy a sample Zookeeper cluster with Ephemeral storage

Create a Yaml file called `zk.yaml` with the following content to install a 3-node Zookeeper cluster.

```yaml
apiVersion: "zookeeper.pravega.io/v1beta1"
kind: "ZookeeperCluster"
metadata:
  name: "example"
spec:
  replicas: 3        
  storageType: ephemeral
```

```
$ kubectl create -f zk.yaml
```

After a couple of minutes, all cluster members should become ready.

```
$ kubectl get zk

NAME      REPLICAS   READY REPLICAS   VERSION   DESIRED VERSION   INTERNAL ENDPOINT    EXTERNAL ENDPOINT   AGE
example   3          3                 0.2.7     0.2.7             10.100.200.18:2181   N/A                 94s
```
>Note: User should only provide value for either the field persistence or ephemeral in the spec if none of the values is specified default is persistence

>Note: In case of ephemeral storage, the cluster may not be able to come back up if more than quorum number of nodes are restarted simultaneously.

>Note: In case of ephemeral storage, there will be loss of data when the node gets restarted.

### Deploy a sample Zookeeper cluster with Istio
Create a Yaml file called `zk-with-istio.yaml` with the following content to install a 3-node Zookeeper cluster.

```yaml
apiVersion: zookeeper.pravega.io/v1beta1
kind: ZookeeperCluster
metadata:
  name: zk-with-istio
spec:
  replicas: 3
  config:
    initLimit: 10
    tickTime: 2000
    syncLimit: 5
    quorumListenOnAllIPs: true
```

```
$ kubectl create -f zk-with-istio.yaml
```

### Upgrade a Zookeeper cluster

#### Trigger the upgrade manually

To initiate an upgrade process manually, a user has to update the `spec.image.tag` field of the `ZookeeperCluster` custom resource. This can be done in three different ways using the `kubectl` command.
1. `kubectl edit zk <name>`, modify the `tag` value in the YAML resource, save, and exit.
2. If you have the custom resource defined in a local YAML file, e.g. `zk.yaml`, you can modify the `tag` value, and reapply the resource with `kubectl apply -f zk.yaml`.
3. `kubectl patch zk <name> --type='json' -p='[{"op": "replace", "path": "/spec/image/tag", "value": "X.Y.Z"}]'`.

After the `tag` field is updated, the StatefulSet will detect the version change and it will trigger the upgrade process.

To detect whether a `ZookeeperCluster` upgrade is in progress or not, check the output of the command `kubectl describe zk`. Output of this command should contain the following entries

```
$ kubectl describe zk

status:
Last Transition Time:    2020-05-18T10:25:12Z
Last Update Time:        2020-05-18T10:25:12Z
Message:                 0
Reason:                  Updating Zookeeper
Status:                  True
Type:                    Upgrading
```
Additionally, the Desired Version will be set to the version that we are upgrading our cluster to.

```
$ kubectl get zk

NAME            REPLICAS   READY REPLICAS   VERSION   DESIRED VERSION   INTERNAL ENDPOINT     EXTERNAL ENDPOINT   AGE
zookeeper       3          3                0.2.6     0.2.7             10.100.200.126:2181   N/A                 11m

```
Once the upgrade completes, the Version field is set to the Desired Version, as shown below

```
$ kubectl get zk

NAME            REPLICAS   READY REPLICAS   VERSION   DESIRED VERSION   INTERNAL ENDPOINT     EXTERNAL ENDPOINT   AGE
zookeeper       3          3                0.2.7     0.2.7             10.100.200.126:2181   N/A                 11m


```
Additionally, the Upgrading status is set to False and PodsReady status is set to True, which signifies that the upgrade has completed, as shown below

```
$ kubectl describe zk

Status:
  Conditions:
    Last Transition Time:    2020-05-18T10:28:22Z
    Last Update Time:        2020-05-18T10:28:22Z
    Status:                  True
    Type:                    PodsReady
    Last Transition Time:    2020-05-18T10:28:22Z
    Last Update Time:        2020-05-18T10:28:22Z
    Status:                  False
    Type:                    Upgrading
```
>Note: The value of the tag field should not be modified while an upgrade is already in progress.

### Upgrade the Operator

For upgrading the zookeeper operator check the document [operator-upgrade](doc/operator-upgrade.md)

### Uninstall the Zookeeper cluster


#### Manual uninstall

```
$ kubectl delete -f zk.yaml
```

### Uninstall the operator

> Note that the Zookeeper clusters managed by the Zookeeper operator will NOT be deleted even if the operator is uninstalled.

#### Uninstall via helm

Refer to [this](charts/zookeeper-operator#uninstalling-the-chart).

#### Manual uninstall

To delete all clusters, delete all cluster CR objects before uninstalling the operator.

```
$ kubectl delete -f config/manager/manager.yaml
$ kubectl delete -f config/rbac/default_ns_rbac.yaml
// or, depending on how you deployed it
$ kubectl delete -f config/rbac/all_ns_rbac.yaml
```

### The AdminServer
The AdminServer is an embedded Jetty server that provides an HTTP interface to the four letter word commands. This port is made accessible to the outside world via the AdminServer service.
By default, the server is started on port 8080, but this configuration can be modified by providing the desired port number within the values.yaml file of the zookeeper cluster charts
```
ports:
   - containerPort: 8118
     name: admin-server
```
This would bring up the AdminServer service on port 8118 as shown below
```
$ kubectl get svc
NAME                                TYPE           CLUSTER-IP       EXTERNAL-IP    PORT(S)
zookeeper-admin-server              LoadBalancer   10.100.200.104   10.243.39.62   8118:30477/TCP
```
The commands are issued by going to the URL `/commands/<command name>`, e.g. `http://10.243.39.62:8118/commands/stat`
The list of available commands are
```
/commands/configuration
/commands/connection_stat_reset
/commands/connections
/commands/dirs
/commands/dump
/commands/environment
/commands/get_trace_mask
/commands/hash
/commands/initial_configuration
/commands/is_read_only
/commands/last_snapshot
/commands/leader
/commands/monitor
/commands/observer_connection_stat_reset
/commands/observers
/commands/ruok
/commands/server_stats
/commands/set_trace_mask
/commands/stat_reset
/commands/stats
/commands/system_properties
/commands/voting_view
/commands/watch_summary
/commands/watches
/commands/watches_by_path
/commands/zabstate
```

## Development

### Build the operator image

Requirements:
  - Go 1.25+

Use the `make` command to build the Zookeeper operator image.

```
$ make build
```
That will generate a Docker image with the format
`<latest_release_tag>-<number_of_commits_after_the_release>` (it will append-dirty if there are uncommitted changes). The image will also be tagged as `latest`.

Example image after running `make build`.

The Zookeeper operator image will be available in your Docker environment.

```
$ docker images adobe/zookeeper-operator

REPOSITORY                    TAG              IMAGE ID        CREATED         SIZE   

adobe/zookeeper-operator      0.2.15-3-dirty   2b2d5bcbedf5    10 minutes ago  41.7MB

adobe/zookeeper-operator      latest           2b2d5bcbedf5    10 minutes ago  41.7MB

```

## Published Docker Images

The official Docker images are published to both:

- **GitHub Container Registry (GHCR)**: `ghcr.io/adobe/zookeeper-operator`
- **Docker Hub**: `adobe/zookeeper-operator`
- **ZooKeeper images**: 
  - GHCR: `ghcr.io/adobe/zookeeper-operator/zookeeper`
  - Docker Hub: `adobe/zookeeper`

To pull the latest operator image:
```bash
# From GHCR (recommended)
docker pull ghcr.io/adobe/zookeeper-operator:latest

# From Docker Hub  
docker pull adobe/zookeeper-operator:latest
```

Optionally push your local build to a Docker registry:

```
docker tag adobe/zookeeper-operator [REGISTRY_HOST]:[REGISTRY_PORT]/adobe/zookeeper-operator
docker push [REGISTRY_HOST]:[REGISTRY_PORT]/adobe/zookeeper-operator
```

where:

- `[REGISTRY_HOST]` is your registry host or IP (e.g. `registry.example.com`)
- `[REGISTRY_PORT]` is your registry port (e.g. `5000`)

### Direct access to the cluster

For debugging and development you might want to access the Zookeeper cluster directly. For example, if you created the cluster with name `zookeeper` in the `default` namespace you can forward the Zookeeper port from any of the pods (e.g. `zookeeper-0`) as follows:

```
$ kubectl port-forward -n default zookeeper-0 2181:2181
```

### Run the operator locally

You can run the operator locally to help with development, testing, and debugging tasks.

The following command will run the operator locally with the default Kubernetes config file present at `$HOME/.kube/config`. Use the `--kubeconfig` flag to provide a different path.

```
$ make run-local
```

### Installation on Google Kubernetes Engine

The Operator requires elevated privileges in order to watch for the custom resources.

According to Google Container Engine docs:

> Ensure the creation of RoleBinding as it grants all the permissions included in the role that we want to create. Because of the way Container Engine checks permissions when we create a Role or ClusterRole.
>
> An example workaround is to create a RoleBinding that gives your Google identity a cluster-admin role before attempting to create additional Role or ClusterRole permissions.
>
> This is a known issue in the Beta release of Role-Based Access Control in Kubernetes and Container Engine version 1.6.

On GKE, the following command must be run before installing the operator, replacing the user with your own details.

```
$ kubectl create clusterrolebinding your-user-cluster-admin-binding --clusterrole=cluster-admin --user=your.google.cloud.email@example.org
```

##### How to build Zookeeper Operator

When you build Operator, the Exporter is built along with it.
`make build-go` - will build both Operator as well as Exporter.

##### How to use exporter

Just run zookeeper-exporter binary with -help option. It will guide you to input ZookeeperCluster YAML file. There are couple of more options to specify.
Example: `./zookeeper-exporter -i ./ZookeeperCluster.yaml -o .`
