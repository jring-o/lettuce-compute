import { describe, it, expect } from "vitest";
import conf from "../../src-tauri/tauri.conf.json";

/**
 * The bundle targets decide which platforms can update themselves. The
 * Windows and Linux installers are their own signed update payloads (MSI,
 * NSIS setup, AppImage, deb); on macOS the only payload is the `.app.tar.gz`
 * archive, which the bundler builds and signs only when the `app` target is
 * requested. A DMG alone builds the `.app`, deletes it, and leaves the update
 * manifest with no macOS entry, so every Mac install would stay on the version
 * it was installed with.
 */
describe("tauri.conf.json bundle targets", () => {
  it("build a signed update payload for every platform the app ships for", () => {
    expect(conf.bundle.createUpdaterArtifacts).toBe(true);
    const targets: string[] = conf.bundle.targets;
    expect(targets, "macOS updates from the .app archive").toContain("app");
    expect(targets, "Windows updates from the MSI or NSIS installer").toEqual(
      expect.arrayContaining(["msi", "nsis"])
    );
    expect(targets, "Linux updates from the AppImage").toContain("appimage");
  });
});
