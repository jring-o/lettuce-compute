import "@testing-library/jest-dom";
import { TextEncoder, TextDecoder } from "util";

if (typeof globalThis.TextEncoder === "undefined") {
  Object.assign(globalThis, { TextEncoder, TextDecoder });
}

// jsdom does not implement PointerEvent, but @base-ui/react's interaction handlers
// reference it (e.g. the Switch's onClick), so clicking those components in tests
// throws ReferenceError without this minimal stand-in. The MouseEvent guard skips
// node-environment suites (e.g. middleware.test.ts), which have no DOM events at all.
if (
  typeof globalThis.PointerEvent === "undefined" &&
  typeof MouseEvent !== "undefined"
) {
  class PointerEventPolyfill extends MouseEvent {
    readonly pointerId: number;
    readonly pointerType: string;

    constructor(type: string, init: PointerEventInit = {}) {
      super(type, init);
      this.pointerId = init.pointerId ?? 0;
      this.pointerType = init.pointerType ?? "";
    }
  }
  Object.assign(globalThis, { PointerEvent: PointerEventPolyfill });
}
