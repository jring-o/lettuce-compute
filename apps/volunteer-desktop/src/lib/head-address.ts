/**
 * Reduce whatever was typed into a head-address field to the head's bare
 * address, which is what the CLI stores and what the volunteer should see
 * back after a successful Test Connection (TB-51):
 *
 *   "https://Compute.Example.org/"  → "compute.example.org"
 *   "compute.example.org:8443/x"    → "compute.example.org:8443"
 *   "http://localhost:9090/"        → "http://localhost:9090"
 *
 * An `https://` scheme is the default and is dropped; `http://` is kept
 * because it changes how the head is reached (plain HTTP, no TLS). Any other
 * scheme is left as typed so the connection test refuses it visibly instead of
 * this helper quietly guessing. The path, query and fragment are dropped: a
 * head lives at its host.
 */
export function normalizeHeadAddress(raw: string): string {
  let rest = raw.trim();
  const scheme = /^([a-z][a-z0-9+.-]*):\/\//i.exec(rest);
  if (scheme) {
    const name = scheme[1].toLowerCase();
    if (name !== "http" && name !== "https") return rest;
    rest = rest.slice(scheme[0].length);
    const authority = rest.split(/[/?#]/)[0].toLowerCase();
    return name === "http" ? `http://${authority}` : authority;
  }
  return rest.split(/[/?#]/)[0].toLowerCase();
}
