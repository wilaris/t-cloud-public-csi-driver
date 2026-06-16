# T Cloud Public CSI Driver

[![Go Version](https://img.shields.io/badge/Go-1.26+-00ADD8?style=flat&logo=go)](https://go.dev)
[![CSI Spec](https://img.shields.io/badge/CSI_Spec-v1.13.0-blue?style=flat)](https://github.com/container-storage-interface/spec)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)
[![Status](https://img.shields.io/badge/Status-Active_Development-orange?style=flat)](#project-status)

The **T Cloud Public CSI Driver** is an external, alternative Container Storage Interface (CSI) driver for T Cloud Public (formerly Open Telekom Cloud) Elastic Volume Service (EVS). It provides dynamic volume provisioning, topology-aware attachment and block storage management for Kubernetes workloads running on T Cloud Elastic Cloud Server (ECS) instances.

---

<a id="project-status"></a>

> [!NOTE]
> **Read-Only Mirror and Project Status**
>
> This repository is a read-only mirror of the internal Wilaris driver repository. We develop the driver internally for our managed Kubernetes platform and publish the source here for transparency with our customers and the open source community.
>
> The driver is under active development and tested daily against real T Cloud infrastructure.

---

## Highlights

- **Dynamic Volume Provisioning**: Creates and deletes EVS volumes on demand as Kubernetes PersistentVolumeClaims are provisioned and deleted.
- **Topology Awareness**: Aligns volume placement with worker node availability zones (`topology.kubernetes.io/zone`) using `WaitForFirstConsumer` binding.
- **Filesystem and Raw Block Modes**: Supports `ext4` and `xfs` filesystems as well as raw block device mapping (`volumeMode: Block`).
- **Immutable OS Support**: Packages formatting and mount utilities inside the container image for compatibility with immutable distributions like Talos Linux and managed platforms like Gardener.
- **Direct API Integration**: Interacts directly with native T Cloud Public EVS and ECS APIs without external daemon sidecars or state stores.

---

## Feature Matrix

| Capability | Status | Details |
|---|---|---|
| Dynamic Provisioning (`CreateVolume` / `DeleteVolume`) | Supported | Provisions and deletes EVS volumes (minimum 10 GiB). |
| Topology Awareness (`topology.kubernetes.io/zone`) | Supported | Matches volume placement to the target node availability zone. |
| Volume Attachment (`ControllerPublish` / `ControllerUnpublish`) | Supported | Attaches and detaches EVS volumes to and from ECS instances. |
| Node Staging and Mount (`NodeStageVolume` / `NodePublishVolume`) | Supported | Formats block devices and mounts them to pod target paths. |
| Filesystems | Supported | `ext4` and `xfs` utilities packaged in the container image. |
| Raw Block Volumes | Supported | Direct block device pass-through (`volumeMode: Block`). |
| Access Modes | Supported | `SINGLE_NODE_WRITER` (`ReadWriteOnce`) and `SINGLE_NODE_READER_ONLY`. |
| Volume Expansion (Online / Offline) | Planned | Out of scope for initial release. |
| Volume Snapshots and Restores | Planned | Out of scope for initial release. |
| Volume Cloning | Planned | Out of scope for initial release. |
| Multi-Attach (`ReadWriteMany` / Shared Disk) | Planned | Out of scope for initial release. |
| Bare Metal Service (BMS) | Not supported | ECS virtual machines only. |

---

## Kubernetes Usage

### 1. StorageClass

Use `volumeBindingMode: WaitForFirstConsumer` so the CSI provisioner defers volume creation until the pod is scheduled. This guarantees volume provisioning in the availability zone of the scheduled node.

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: t-cloud-evs-ssd
provisioner: evs.csi.t-cloud.wilaris.dev
volumeBindingMode: WaitForFirstConsumer
allowVolumeExpansion: false
parameters:
  # EVS volume type: SSD, SAS, GPSSD or ESSD depending on region availability
  type: SSD
```

### 2. PersistentVolumeClaim

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: example-evs-pvc
spec:
  accessModes:
    - ReadWriteOnce
  storageClassName: t-cloud-evs-ssd
  resources:
    requests:
      storage: 10Gi # Minimum billable EVS volume size is 10GiB
```

### 3. Pod

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: example-app
spec:
  containers:
    - name: web
      image: nginx:alpine
      volumeMounts:
        - name: data
          mountPath: /var/data
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: example-evs-pvc
```

---

## Architecture

The driver runs as a single binary (`t-cloud-csi-driver`) configured with a role flag:

```
                  ┌─────────────────────────────────────────────────────────┐
                  │                 Kubernetes Control Plane                │
                  │  (kube-controller-manager / external-provisioner / etc) │
                  └────────────────────────────┬────────────────────────────┘
                                               │ gRPC
                                               ▼
                                 ┌───────────────────────────┐
                                 │   CSI Controller Role     │
                                 │  (--role=controller)      │
                                 └─────────────┬─────────────┘
                                               │ HTTPS / EVS API
                                               ▼
                              ┌─────────────────────────────────┐
                              │      T Cloud Public EVS         │
                              │   (Volume Create / Delete /     │
                              │        Attach / Detach)         │
                              └────────────────┬────────────────┘
                                               │ Virtual Disk Attachment
                                               ▼
                                ┌─────────────────────────────┐
                                │      CSI Node Role          │
                                │   (--role=node DaemonSet)   │
                                └──────────────┬──────────────┘
                                               │ Format (ext4/xfs) & Mount
                                               ▼
                                ┌─────────────────────────────┐
                                │       Application Pod       │
                                │     (/var/data volume)      │
                                └─────────────────────────────┘
```

- **Controller (`--role=controller`)**: Runs in the cluster control plane alongside standard CSI sidecars (`csi-provisioner` and `csi-attacher`). Handles cloud API interactions to provision, attach, detach and delete EVS volumes.
- **Node (`--role=node`)**: Runs as a `DaemonSet` on each worker node. Handles local disk discovery, filesystem formatting (`mkfs.ext4` or `mkfs.xfs`), host filesystem staging (`NodeStageVolume`) and bind-mounting into pod target paths (`NodePublishVolume`).

---

## End-to-End Conformance Suite

The repository includes a standalone conformance test suite: `t-cloud-csi-conformance` under [`test/e2e`](test/e2e/).

Adopters can evaluate the driver against their own T Cloud Public tenant before deploying. The test suite exercises the binary directly against live infrastructure by creating, attaching, formatting, mounting and verifying real EVS block volumes.

```sh
# 1. Compile the standalone conformance runner and driver binary
make e2e-build

# 2. Inspect the test catalog offline without touching cloud resources
make e2e-list
```

> [!NOTE]
> **Resource Isolation and Cleanup**
>
> Test fixtures use unique `csi-e2e-<run-id>-` name prefixes and track all allocations in a local ledger. The suite reclaims created resources on completion or failure. See [`test/e2e/README.md`](test/e2e/README.md) for configuration and execution details.

---

## Building and Images

### Customer Images
Pre-built driver images are served from a private registry for our customers.

### Building from Source
Build the container image locally with Docker or Podman:

```sh
# Build local container image (tagged as t-cloud-csi-driver:<version>)
make image

# Build and push to a custom registry
IMAGE_REPO=registry.example.com/your-org/t-cloud-csi-driver make image-push
```

---

## Development and Verification

### Prerequisites
- Go 1.26 or newer
- Docker or Podman for container builds

### Offline Verification Gate
Run the full offline verification suite before submitting changes:

```sh
make verify
```

The verification gate runs formatting checks (`gofmt`, `goimports`, `golines`), static analysis (`go vet`), unit tests, binary compilation, version smoke testing and E2E compilation checks without reaching the cloud.

### Common Makefile Targets

| Target | Description |
|---|---|
| `make build` | Compiles the driver binary into `bin/t-cloud-csi-driver` |
| `make test` | Runs unit tests across all packages |
| `make vet` | Runs `go vet` static analysis |
| `make fmt` | Formats Go sources with `goimports` and `golines` |
| `make fmt-check` | Verifies source code formatting |
| `make lint` | Runs `golangci-lint` |
| `make smoke` | Builds binary and verifies version stamping output |
| `make clean` | Cleans `bin/` and `dist/` directories |

---

## License

Licensed under the Apache License 2.0. See [LICENSE](LICENSE).
