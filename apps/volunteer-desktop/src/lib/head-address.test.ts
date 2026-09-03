import { describe, it, expect } from "vitest";
import { normalizeHeadAddress } from "./head-address";

// TB-51: the wizard's placeholder invited "https://…", the connection test
// accepted it, and the CLI then stored a URL as the gRPC target. The field
// must reduce to the bare head address the CLI actually wants.
describe("normalizeHeadAddress", () => {
  it("drops an https:// scheme, path and trailing slash", () => {
    expect(normalizeHeadAddress("https://compute.example.org/")).toBe("compute.example.org");
    expect(normalizeHeadAddress("https://compute.example.org/some/path?x=1#f")).toBe("compute.example.org");
    expect(normalizeHeadAddress("HTTPS://Compute.Example.ORG")).toBe("compute.example.org");
  });

  it("keeps a port", () => {
    expect(normalizeHeadAddress("https://compute.example.org:8443/")).toBe("compute.example.org:8443");
    expect(normalizeHeadAddress("compute.example.org:8443/x")).toBe("compute.example.org:8443");
  });

  it("keeps http:// because it changes how the head is reached", () => {
    expect(normalizeHeadAddress("http://localhost:9090/")).toBe("http://localhost:9090");
  });

  it("leaves a bare host alone apart from whitespace and case", () => {
    expect(normalizeHeadAddress("  Compute.Example.org ")).toBe("compute.example.org");
    expect(normalizeHeadAddress("")).toBe("");
  });

  it("leaves an unknown scheme for the connection test to refuse", () => {
    expect(normalizeHeadAddress("ftp://compute.example.org")).toBe("ftp://compute.example.org");
  });
});
