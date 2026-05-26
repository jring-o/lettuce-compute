// --- Mocks ---

const mockMkdir = jest.fn().mockResolvedValue(undefined);
const mockWriteFile = jest.fn().mockResolvedValue(undefined);
const mockStat = jest.fn().mockResolvedValue({ size: 100 });
const mockRm = jest.fn().mockResolvedValue(undefined);
const mockReadFile = jest.fn().mockResolvedValue(Buffer.from("file-content"));

jest.mock("fs/promises", () => ({
  mkdir: (...args: unknown[]) => mockMkdir(...args),
  writeFile: (...args: unknown[]) => mockWriteFile(...args),
  stat: (...args: unknown[]) => mockStat(...args),
  rm: (...args: unknown[]) => mockRm(...args),
  readFile: (...args: unknown[]) => mockReadFile(...args),
}));

const mockInsert = jest.fn().mockReturnValue({
  values: jest.fn().mockResolvedValue(undefined),
});
const mockSelectResult = [
  {
    id: "file-1",
    leafId: "leaf-1",
    fileType: "CODE_ARTIFACT",
    filename: "app.wasm",
    storageKey: "leafs/proj-1/CODE_ARTIFACT/app.wasm",
    sizeBytes: 100,
    contentType: "application/wasm",
    checksumSha256: "abc123",
    uploadedBy: "user-1",
    createdAt: new Date(),
  },
];
const mockSelectWhere = jest.fn().mockReturnValue({
  limit: jest.fn().mockResolvedValue(mockSelectResult),
});
const mockSelectFrom = jest.fn().mockReturnValue({
  where: mockSelectWhere,
});
const mockSelect = jest.fn().mockReturnValue({
  from: mockSelectFrom,
});
const mockDeleteWhere = jest.fn().mockResolvedValue(undefined);
const mockDeleteFrom = jest.fn().mockReturnValue({
  where: mockDeleteWhere,
});

jest.mock("@/lib/db", () => ({
  db: {
    insert: (...args: unknown[]) => mockInsert(...args),
    select: (...args: unknown[]) => mockSelect(...args),
    delete: (...args: unknown[]) => mockDeleteFrom(...args),
  },
}));

jest.mock("@/lib/db/schema", () => ({
  fileUploads: { id: "id_column" },
}));

// Mock crypto.randomUUID to return a predictable value
const mockUUID = "test-uuid-1234";
jest.spyOn(
  // eslint-disable-next-line @typescript-eslint/no-require-imports
  require("crypto") as { randomUUID: () => string },
  "randomUUID",
).mockReturnValue(mockUUID);

import { saveFile, getFile, deleteFile, getFileContent } from "@/lib/file-storage";

function createMockFile(
  name: string,
  content: string,
  type: string = "application/octet-stream",
): File {
  const buffer = Buffer.from(content);
  const file = new File([buffer], name, { type });
  // jsdom File doesn't implement arrayBuffer(), so we polyfill it
  file.arrayBuffer = () => Promise.resolve(buffer.buffer.slice(buffer.byteOffset, buffer.byteOffset + buffer.byteLength));
  return file;
}

beforeEach(() => {
  jest.clearAllMocks();
  // Re-mock insert chain after clearing
  mockInsert.mockReturnValue({
    values: jest.fn().mockResolvedValue(undefined),
  });
});

describe("File Storage Service", () => {
  describe("saveFile", () => {
    it("creates directory and writes file", async () => {
      const file = createMockFile("app.wasm", "binary-content", "application/wasm");

      const result = await saveFile(file, "leaf-1", "CODE_ARTIFACT", "user-1");

      expect(mockMkdir).toHaveBeenCalledWith(
        expect.stringContaining(mockUUID),
        { recursive: true },
      );
      expect(mockWriteFile).toHaveBeenCalledWith(
        expect.stringContaining("app.wasm"),
        expect.any(Buffer),
      );
      expect(result.fileId).toBe(mockUUID);
      expect(result.storageKey).toBe("leafs/leaf-1/CODE_ARTIFACT/app.wasm");
      expect(result.sizeBytes).toBe(file.size);
    });

    it("computes SHA-256 checksum", async () => {
      const file = createMockFile("data.csv", "test-data");

      const result = await saveFile(file, "leaf-1", "INPUT_DATA");

      // SHA-256 of "test-data" is deterministic
      expect(result.checksum).toMatch(/^[a-f0-9]{64}$/);
    });

    it("inserts file_uploads row via Drizzle", async () => {
      const file = createMockFile("script.py", "print('hello')");

      await saveFile(file, "leaf-1", "CODE_ARTIFACT", "user-1");

      expect(mockInsert).toHaveBeenCalled();
      const valuesCall = mockInsert.mock.results[0].value.values;
      expect(valuesCall).toHaveBeenCalledWith(
        expect.objectContaining({
          id: mockUUID,
          leafId: "leaf-1",
          fileType: "CODE_ARTIFACT",
          filename: "script.py",
          uploadedBy: "user-1",
        }),
      );
    });

    it("rejects files exceeding CODE_ARTIFACT size limit", async () => {
      // Create a mock file that reports size > 500 MB
      const file = createMockFile("huge.bin", "x");
      Object.defineProperty(file, "size", { value: 600 * 1024 * 1024 });

      await expect(
        saveFile(file, "leaf-1", "CODE_ARTIFACT"),
      ).rejects.toThrow("File exceeds size limit of 500 MB");
    });

    it("rejects empty files", async () => {
      const file = createMockFile("empty.txt", "");
      Object.defineProperty(file, "size", { value: 0 });

      await expect(
        saveFile(file, "leaf-1", "INPUT_DATA"),
      ).rejects.toThrow("File must not be empty");
    });
  });

  describe("getFile", () => {
    it("returns file info when file exists", async () => {
      const result = await getFile("file-1");

      expect(result).not.toBeNull();
      expect(result!.filename).toBe("app.wasm");
      expect(result!.contentType).toBe("application/wasm");
      expect(result!.sizeBytes).toBe(100);
      expect(result!.checksum).toBe("abc123");
      expect(result!.path).toContain("app.wasm");
    });

    it("returns null when file not in database", async () => {
      mockSelectWhere.mockReturnValueOnce({
        limit: jest.fn().mockResolvedValue([]),
      });

      const result = await getFile("missing-id");
      expect(result).toBeNull();
    });

    it("returns null when file not on disk", async () => {
      mockStat.mockRejectedValueOnce(new Error("ENOENT"));

      const result = await getFile("file-1");
      expect(result).toBeNull();
    });
  });

  describe("deleteFile", () => {
    it("removes from database and disk", async () => {
      await deleteFile("file-1");

      expect(mockDeleteFrom).toHaveBeenCalled();
      expect(mockRm).toHaveBeenCalledWith(
        expect.stringContaining("file-1"),
        { recursive: true, force: true },
      );
    });

    it("does not throw when directory does not exist on disk", async () => {
      mockRm.mockRejectedValueOnce(new Error("ENOENT"));

      // Should not throw
      await expect(deleteFile("file-1")).resolves.toBeUndefined();
    });
  });

  describe("getFileContent", () => {
    it("returns buffer with metadata", async () => {
      const result = await getFileContent("file-1");

      expect(result).not.toBeNull();
      expect(result!.buffer).toBeInstanceOf(Buffer);
      expect(result!.filename).toBe("app.wasm");
      expect(result!.contentType).toBe("application/wasm");
    });

    it("returns null when file not found", async () => {
      mockSelectWhere.mockReturnValueOnce({
        limit: jest.fn().mockResolvedValue([]),
      });

      const result = await getFileContent("missing");
      expect(result).toBeNull();
    });
  });

  // --- Additional Coverage: Edge Cases ---

  describe("saveFile edge cases", () => {
    it("accepts file with unknown fileType (no size limit enforced)", async () => {
      const file = createMockFile("data.bin", "some-content");

      // An unknown file type should not be rejected by size check
      // because SIZE_LIMITS[unknownType] is undefined
      const result = await saveFile(file, "leaf-1", "UNKNOWN_TYPE");

      expect(result.fileId).toBe(mockUUID);
      expect(result.storageKey).toBe("leafs/leaf-1/UNKNOWN_TYPE/data.bin");
      expect(mockWriteFile).toHaveBeenCalled();
    });

    it("saves file with uploadedBy as null when not provided", async () => {
      const file = createMockFile("data.csv", "col1,col2");

      await saveFile(file, "leaf-1", "INPUT_DATA");

      const valuesCall = mockInsert.mock.results[0].value.values;
      expect(valuesCall).toHaveBeenCalledWith(
        expect.objectContaining({
          uploadedBy: null,
        }),
      );
    });

    it("maps empty file.type to null contentType", async () => {
      const file = createMockFile("data.bin", "binary-data", "");

      await saveFile(file, "leaf-1", "INPUT_DATA", "user-1");

      const valuesCall = mockInsert.mock.results[0].value.values;
      expect(valuesCall).toHaveBeenCalledWith(
        expect.objectContaining({
          contentType: null,
        }),
      );
    });

    it("rejects files exceeding INPUT_DATA size limit (1 GB)", async () => {
      const file = createMockFile("huge.bin", "x");
      Object.defineProperty(file, "size", { value: 1.5 * 1024 * 1024 * 1024 });

      await expect(
        saveFile(file, "leaf-1", "INPUT_DATA"),
      ).rejects.toThrow("File exceeds size limit of 1024 MB");
    });

    it("accepts file exactly at size limit boundary", async () => {
      const file = createMockFile("exact.bin", "x");
      // Exactly 500 MB for CODE_ARTIFACT
      Object.defineProperty(file, "size", { value: 500 * 1024 * 1024 });

      const result = await saveFile(file, "leaf-1", "CODE_ARTIFACT");
      expect(result.fileId).toBe(mockUUID);
    });

    it("rejects files with negative size", async () => {
      const file = createMockFile("bad.bin", "x");
      Object.defineProperty(file, "size", { value: -1 });

      await expect(
        saveFile(file, "leaf-1", "INPUT_DATA"),
      ).rejects.toThrow("File must not be empty");
    });
  });
});
