# T Cloud Public CSI Driver

Container Storage Interface (CSI) driver for T Cloud Public Elastic Volume Service (EVS), enabling
dynamic block storage provisioning and management for Kubernetes.

> This project is under active development and is not ready for production use.

## Development

Go 1.26 or newer is required. Run the offline verification gate with:

```sh
make verify
```

The Makefile also exposes `fmt`, `fmt-check`, `vet`, `test`, and `build` targets.

## License

Licensed under the Apache License 2.0. See [LICENSE](LICENSE).
