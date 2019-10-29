## Usage

To install the Helm Broker in your cluster, use this command:

```
helm install https://github.com/kyma-project/helm-broker/releases/download/v1.1.0/helm-broker-chart.tar.gz --wait
```

Provide the latest Add Ons for the Helm Broker, by creating the `ClusterAddonsConfiguration`:

```yaml
apiVersion: addons.kyma-project.io/v1alpha1
kind: ClusterAddonsConfiguration
metadata:
  labels:
    addons.kyma-project.io/managed: "true"
  name: my-addons
spec:
  repositories:
  - url: "https://github.com/kyma-project/addons/releases/download/latest/index.yaml"
```

Learn more how to use the Helm Broker, by reading the [documentation](https://github.com/kyma-project/helm-broker/blob/v1.1.0/README.md#table-of-contents).
