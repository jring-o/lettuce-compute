import { vi } from "vitest";

export const listen = vi.fn(() => Promise.resolve(() => {}));
export const emit = vi.fn();
