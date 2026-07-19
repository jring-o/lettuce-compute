"""
Gravitational N-Body Simulation for Lettuce

Simulates gravitational dynamics of a star cluster using the leapfrog
integrator with softened Newtonian gravity. Each work unit runs one
parameter combination and reports statistics about the final state.

Deterministic: same parameters always produce the same output.
"""

import hashlib
import json
import os
import time

import numpy as np

# Gravitational constant (normalized units).
G = 1.0

# Softening length to avoid singularities in close encounters.
SOFTENING = 0.01


def deterministic_seed(params: dict) -> int:
    """Derive a reproducible RNG seed from the parameter dict."""
    canonical = json.dumps(params, sort_keys=True)
    h = hashlib.sha256(canonical.encode()).hexdigest()
    return int(h[:8], 16)


def generate_positions(rng: np.random.Generator, n: int, spread: float) -> np.ndarray:
    """Generate initial positions in a spherical distribution."""
    # Uniform in a sphere of radius `spread`.
    u = rng.random(n)
    r = spread * np.cbrt(u)
    theta = np.arccos(2.0 * rng.random(n) - 1.0)
    phi = 2.0 * np.pi * rng.random(n)

    pos = np.zeros((n, 3))
    pos[:, 0] = r * np.sin(theta) * np.cos(phi)
    pos[:, 1] = r * np.sin(theta) * np.sin(phi)
    pos[:, 2] = r * np.cos(theta)
    return pos


def generate_velocities(rng: np.random.Generator, n: int, velocity_scale: float) -> np.ndarray:
    """Generate initial velocities with isotropic Gaussian distribution."""
    return rng.normal(0.0, velocity_scale / np.sqrt(3.0), size=(n, 3))


def generate_masses(rng: np.random.Generator, n: int, distribution: str) -> np.ndarray:
    """Generate masses according to the specified distribution."""
    if distribution == "uniform":
        return np.ones(n)
    elif distribution == "power_law":
        # Salpeter-like: P(m) ~ m^-2.35, range [0.1, 10]
        u = rng.random(n)
        alpha = 2.35
        m_min, m_max = 0.1, 10.0
        masses = (m_min ** (1 - alpha) + u * (m_max ** (1 - alpha) - m_min ** (1 - alpha))) ** (1.0 / (1 - alpha))
        return masses
    elif distribution == "bimodal":
        # Two populations: low-mass (mean=1) and high-mass (mean=5).
        is_heavy = rng.random(n) < 0.2
        masses = np.where(is_heavy, 5.0 + rng.normal(0, 0.5, n), 1.0 + rng.normal(0, 0.1, n))
        return np.maximum(masses, 0.1)
    else:
        return np.ones(n)


def compute_accelerations(pos: np.ndarray, masses: np.ndarray) -> np.ndarray:
    """Compute gravitational accelerations via direct O(N^2) summation."""
    n = len(masses)
    acc = np.zeros_like(pos)
    eps2 = SOFTENING * SOFTENING

    for i in range(n):
        dx = pos - pos[i]  # (n, 3) displacement vectors from i to all others
        r2 = np.sum(dx * dx, axis=1) + eps2
        inv_r3 = r2 ** (-1.5)
        inv_r3[i] = 0.0  # no self-interaction
        acc[i] = G * np.sum((masses * inv_r3)[:, np.newaxis] * dx, axis=0)

    return acc


def kinetic_energy(vel: np.ndarray, masses: np.ndarray) -> float:
    """Total kinetic energy."""
    return 0.5 * np.sum(masses * np.sum(vel * vel, axis=1))


def potential_energy(pos: np.ndarray, masses: np.ndarray) -> float:
    """Total gravitational potential energy."""
    n = len(masses)
    pe = 0.0
    eps2 = SOFTENING * SOFTENING
    for i in range(n):
        for j in range(i + 1, n):
            dx = pos[j] - pos[i]
            r = np.sqrt(np.sum(dx * dx) + eps2)
            pe -= G * masses[i] * masses[j] / r
    return pe


def total_energy(pos: np.ndarray, vel: np.ndarray, masses: np.ndarray) -> float:
    """Total energy (kinetic + potential)."""
    return kinetic_energy(vel, masses) + potential_energy(pos, masses)


def center_of_mass(pos: np.ndarray, masses: np.ndarray) -> np.ndarray:
    """Mass-weighted center of mass."""
    total_mass = np.sum(masses)
    return np.sum(masses[:, np.newaxis] * pos, axis=0) / total_mass


def leapfrog_step(pos: np.ndarray, vel: np.ndarray, acc: np.ndarray,
                  masses: np.ndarray, dt: float) -> tuple:
    """One leapfrog integration step (kick-drift-kick)."""
    vel_half = vel + 0.5 * dt * acc
    pos_new = pos + dt * vel_half
    acc_new = compute_accelerations(pos_new, masses)
    vel_new = vel_half + 0.5 * dt * acc_new
    return pos_new, vel_new, acc_new


def run_simulation(params: dict) -> dict:
    """Run the N-body simulation and return statistics."""
    n = int(params["num_bodies"])
    spread = float(params["spread"])
    velocity_scale = float(params["velocity_scale"])
    mass_distribution = str(params["mass_distribution"])
    dt = float(params["timestep"])
    num_steps = int(params["num_steps"])

    progress_file = os.environ.get("LETTUCE_PROGRESS_FILE")

    seed = deterministic_seed(params)
    rng = np.random.default_rng(seed)

    # Initialize.
    pos = generate_positions(rng, n, spread)
    vel = generate_velocities(rng, n, velocity_scale)
    masses = generate_masses(rng, n, mass_distribution)

    # Center the system (zero total momentum).
    total_mass = np.sum(masses)
    total_momentum = np.sum(masses[:, np.newaxis] * vel, axis=0)
    vel -= total_momentum / total_mass

    # Initial measurements.
    com_initial = center_of_mass(pos, masses)
    e_initial = total_energy(pos, vel, masses)

    # Integrate.
    last_progress = time.time()
    acc = compute_accelerations(pos, masses)
    for step in range(num_steps):
        pos, vel, acc = leapfrog_step(pos, vel, acc, masses, dt)
        if progress_file:
            now = time.time()
            if now - last_progress >= 5.0:
                pct = (step + 1) / num_steps * 100
                with open(progress_file, "w") as f:
                    f.write(f"{pct:.1f}")
                last_progress = now

    # Final progress write, unthrottled: a short simulation can finish inside the
    # 5s throttle window above, and the volunteer's status display expects 100 at
    # completion. Best-effort like every other progress write.
    if progress_file:
        try:
            with open(progress_file, "w") as f:
                f.write("100")
        except OSError:
            pass

    # Final measurements.
    e_final = total_energy(pos, vel, masses)
    com_final = center_of_mass(pos, masses)
    com_drift = float(np.linalg.norm(com_final - com_initial))

    # Pairwise distances for max separation and closest encounter.
    max_sep = 0.0
    min_sep = float("inf")
    for i in range(n):
        dx = pos[i + 1:] - pos[i]
        dists = np.sqrt(np.sum(dx * dx, axis=1))
        if len(dists) > 0:
            max_sep = max(max_sep, float(np.max(dists)))
            min_sep = min(min_sep, float(np.min(dists)))

    # Bound fraction: bodies with negative specific energy relative to center.
    ke_per_body = 0.5 * np.sum(vel * vel, axis=1)
    r_from_com = pos - com_final
    r_mag = np.sqrt(np.sum(r_from_com * r_from_com, axis=1))
    # Approximate potential per body as -G*M_enclosed/r.
    pe_per_body = -G * total_mass / np.maximum(r_mag, SOFTENING)
    specific_energy = ke_per_body + pe_per_body
    bound = int(np.sum(specific_energy < 0))
    ejected = n - bound

    # Virial ratio: 2K / |W|.
    ke_total = kinetic_energy(vel, masses)
    pe_total = potential_energy(pos, masses)
    virial_ratio = 2.0 * ke_total / abs(pe_total) if pe_total != 0 else float("inf")

    # Energy drift.
    if e_initial != 0:
        energy_drift_pct = abs((e_final - e_initial) / e_initial) * 100.0
    else:
        energy_drift_pct = 0.0

    return {
        "params": params,
        "total_energy_initial": round(float(e_initial), 6),
        "total_energy_final": round(float(e_final), 6),
        "energy_drift_pct": round(energy_drift_pct, 4),
        "center_of_mass_drift": round(com_drift, 6),
        "max_separation": round(max_sep, 4),
        "bound_fraction": round(bound / n, 4),
        "ejected_bodies": ejected,
        "closest_encounter": round(min_sep, 6),
        "virial_ratio": round(float(virial_ratio), 4),
    }


def main():
    # Read parameters from Lettuce container environment.
    params_path = os.environ.get("LETTUCE_PARAMETERS_FILE", "/work/input/parameters.json")
    output_dir = os.environ.get("LETTUCE_OUTPUT_DIR", "/work/output")

    with open(params_path) as f:
        params = json.load(f)

    result = run_simulation(params)

    output_path = os.path.join(output_dir, "output.json")
    with open(output_path, "w") as f:
        json.dump(result, f, indent=2)


if __name__ == "__main__":
    main()
