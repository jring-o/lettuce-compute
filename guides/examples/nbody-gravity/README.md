# N-Body Gravity — container example

A gravitational N-body simulation packaged as a container. Each work unit simulates a
star cluster with different initial conditions (body count, spread, velocity, mass
distribution) using a leapfrog integrator, and reports statistics about the final state
(energy conservation, bound fraction, ejected bodies, virial ratio). Deterministic: the
same parameters always produce the same output.

- **Language:** Python + NumPy (runs in a container — no cross-compilation)
- **Task pattern:** `PARAMETER_SWEEP`
- **Compute contract:** reads `LETTUCE_PARAMETERS_FILE`, writes `LETTUCE_OUTPUT_DIR/output.json`

> A container leaf can be **any** language — Python is just convenient here because
> shipping NumPy in an image beats cross-compiling. Volunteers run it with Podman
> (bundled with the CLI) or Docker.

## Build

```bash
podman build -t your-domain.com/nbody:latest .   # or: docker build ...
```

## Test locally

```bash
mkdir -p /tmp/nbody/input /tmp/nbody/output
echo '{"num_bodies":100,"spread":5.0,"velocity_scale":0.5,"mass_distribution":"uniform","timestep":0.01,"num_steps":1000}' \
  > /tmp/nbody/input/parameters.json

podman run --rm \
  -v /tmp/nbody/input:/work/input:ro \
  -v /tmp/nbody/output:/work/output \
  -e LETTUCE_PARAMETERS_FILE=/work/input/parameters.json \
  -e LETTUCE_OUTPUT_DIR=/work/output \
  your-domain.com/nbody:latest

cat /tmp/nbody/output/output.json
```

## Deploy it to your head

This is the container walkthrough in **[../../first-leaf.md](../../first-leaf.md)** —
build → push to your head's registry → create, configure, activate the leaf →
generate work units.
