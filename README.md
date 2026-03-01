> **⚠️ Do not clone this repository directly!**
>
> Use `formae plugin init` to create your plugin. This command scaffolds a new
> plugin from this template with proper naming and configuration.
>
> ```bash
> formae plugin init my-plugin
> ```

---

## Setup Checklist

*Remove this section and the warning above after completing setup.*

After creating your plugin with `formae plugin init`, complete these steps:

- [ ] Update `formae-plugin.pkl` with your plugin metadata (name, namespace, description)
- [ ] Define your resource types in `schema/pkl/*.pkl`
- [ ] Implement CRUD operations in `plugin.go`
- [ ] Update test fixtures in `testdata/*.pkl` to use your resources
- [ ] Update this README (replace title, description, resources table, etc.)
- [ ] Set up local credentials for testing
- [ ] Run conformance tests locally: `make conformance-test`
- [ ] Configure CI credentials in `.github/workflows/ci.yml` (optional)
- [ ] Remove this checklist section and the warning box above

For detailed guidance, see the [Plugin SDK Documentation](https://docs.formae.io/plugin-sdk).

---

# Example Plugin for formae

*TODO: Update title and description for your plugin*

Example Formae plugin template - replace this with a description of what your plugin manages.

## Installation

```bash
# Install the plugin
make install
```

## Supported Resources

*TODO: Document your supported resource types*

| Resource Type | Description |
|---------------|-------------|
| `PROXMOX::Service::Resource` | Example resource (replace with your actual resources) |

## Configuration

Configure a target in your Forma file:

```pkl
new formae.Target {
    label = "my-target"
    namespace = "PROXMOX"  // TODO: Update with your namespace
    config = new Mapping {
        ["region"] = "us-east-1"
        // TODO: Add your provider-specific configuration
    }
}
```

## Examples

See the [examples/](examples/) directory for usage examples.

```bash
# Evaluate an example
formae eval examples/basic/main.pkl

# Apply resources
formae apply --mode reconcile --watch examples/basic/main.pkl
```

## Development

### Prerequisites

- Go 1.25+
- [Pkl CLI](https://pkl-lang.org/main/current/pkl-cli/index.html)
- Cloud provider credentials (for conformance testing)

### Building

```bash
make build      # Build plugin binary
make test       # Run unit tests
make lint       # Run linter
make install    # Build + install locally
```

### Local Testing

```bash
# Install plugin locally
make install

# Start formae agent
formae agent start

# Apply example resources
formae apply --mode reconcile --watch examples/basic/main.pkl
```

### Conformance Testing

Conformance tests validate your plugin's CRUD lifecycle using the test fixtures in `testdata/`:

| File | Purpose |
|------|---------|
| `resource.pkl` | Initial resource creation |
| `resource-update.pkl` | In-place update (mutable fields) |
| `resource-replace.pkl` | Replacement (createOnly fields) |

The test harness sets `FORMAE_TEST_RUN_ID` for unique resource naming between runs.

```bash
make conformance-test                  # Latest formae version
make conformance-test VERSION=0.80.0   # Specific version
```

The `scripts/ci/clean-environment.sh` script cleans up test resources. It runs before and after conformance tests and should be idempotent.

## Licensing

Plugins are independent works and may be licensed under any license of the author’s choosing.

See the formae plugin policy:
<https://docs.formae.io/plugin-sdk/
