"use client";

import { useState, useTransition } from "react";
import {
  activateLeafVersion,
  deleteLeafVersion,
  listLeafVersions,
  publishLeafVersion,
} from "@/lib/actions/versions";
import type { ArtifactVersion } from "@/types/infrastructure";

interface VersionManagerProps {
  leafId: string;
  slug: string;
  initialVersions: ArtifactVersion[];
  currentVersionId: string | null;
}

/**
 * VersionManager renders the per-leaf artifact version registry (TODO #38): publish an
 * immutable version of the leaf's current artifact, view history, and activate (roll
 * back to) any prior version. Running volunteers pick up the active version on their
 * next work request — no restart.
 */
export function VersionManager({
  leafId,
  slug,
  initialVersions,
  currentVersionId,
}: VersionManagerProps) {
  const [versions, setVersions] = useState<ArtifactVersion[]>(initialVersions);
  const [currentId, setCurrentId] = useState<string | null>(currentVersionId);
  const [label, setLabel] = useState("");
  const [notes, setNotes] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [pending, startTransition] = useTransition();

  const refresh = async () => {
    const res = await listLeafVersions(leafId);
    if ("data" in res) setVersions(res.data);
  };

  const onPublish = () => {
    setError(null);
    if (!label.trim()) {
      setError("A version label is required.");
      return;
    }
    startTransition(async () => {
      const res = await publishLeafVersion(leafId, slug, {
        version_label: label.trim(),
        notes: notes.trim() || undefined,
      });
      if ("error" in res) {
        setError(res.error.message);
        return;
      }
      setLabel("");
      setNotes("");
      setCurrentId(res.data.id);
      await refresh();
    });
  };

  const onActivate = (versionId: string) => {
    setError(null);
    startTransition(async () => {
      const res = await activateLeafVersion(leafId, slug, versionId);
      if ("error" in res) {
        setError(res.error.message);
        return;
      }
      setCurrentId(versionId);
      await refresh();
    });
  };

  const onDelete = (versionId: string) => {
    setError(null);
    startTransition(async () => {
      const res = await deleteLeafVersion(leafId, slug, versionId);
      if ("error" in res) {
        setError(res.error.message);
        return;
      }
      await refresh();
    });
  };

  return (
    <section className="mt-8 rounded-lg border border-gray-200 bg-white p-6 dark:border-gray-800 dark:bg-gray-900">
      <h2 className="text-lg font-semibold text-gray-900 dark:text-gray-100">
        Artifact versions
      </h2>
      <p className="mt-1 text-sm text-gray-500 dark:text-gray-400">
        Publish an immutable version of the leaf&apos;s current artifact. Running
        volunteers pick up the active version automatically — no restart. Roll back by
        activating any prior version.
      </p>

      {error && (
        <div className="mt-4 rounded-md border border-red-200 bg-red-50 px-3 py-2 text-sm text-red-700 dark:border-red-900 dark:bg-red-950 dark:text-red-300">
          {error}
        </div>
      )}

      <div className="mt-4 flex flex-col gap-2 sm:flex-row sm:items-end">
        <div className="flex-1">
          <label className="block text-xs font-medium text-gray-600 dark:text-gray-400">
            Version label
          </label>
          <input
            type="text"
            value={label}
            onChange={(e) => setLabel(e.target.value)}
            placeholder="e.g. native-go-2.0"
            className="mt-1 w-full rounded-md border border-gray-300 px-3 py-1.5 text-sm dark:border-gray-700 dark:bg-gray-800"
          />
        </div>
        <div className="flex-1">
          <label className="block text-xs font-medium text-gray-600 dark:text-gray-400">
            Notes (optional)
          </label>
          <input
            type="text"
            value={notes}
            onChange={(e) => setNotes(e.target.value)}
            placeholder="what changed"
            className="mt-1 w-full rounded-md border border-gray-300 px-3 py-1.5 text-sm dark:border-gray-700 dark:bg-gray-800"
          />
        </div>
        <button
          type="button"
          onClick={onPublish}
          disabled={pending}
          className="rounded-md bg-green-600 px-4 py-1.5 text-sm font-medium text-white hover:bg-green-700 disabled:opacity-50"
        >
          {pending ? "Working…" : "Publish & activate"}
        </button>
      </div>

      <div className="mt-6 overflow-x-auto">
        {versions.length === 0 ? (
          <p className="text-sm text-gray-500 dark:text-gray-400">
            No versions published yet. The leaf dispatches its working configuration
            until you publish one.
          </p>
        ) : (
          <table className="min-w-full text-sm">
            <thead>
              <tr className="border-b border-gray-200 text-left text-xs uppercase text-gray-500 dark:border-gray-800 dark:text-gray-400">
                <th className="py-2 pr-4">Version</th>
                <th className="py-2 pr-4">Runtime</th>
                <th className="py-2 pr-4">Digest</th>
                <th className="py-2 pr-4">Published</th>
                <th className="py-2 pr-4" />
              </tr>
            </thead>
            <tbody>
              {versions.map((v) => {
                const isCurrent = v.id === currentId;
                return (
                  <tr
                    key={v.id}
                    className="border-b border-gray-100 dark:border-gray-800"
                  >
                    <td className="py-2 pr-4 font-medium text-gray-900 dark:text-gray-100">
                      {v.version_label}
                      {isCurrent && (
                        <span className="ml-2 rounded bg-green-100 px-1.5 py-0.5 text-xs font-medium text-green-800 dark:bg-green-900 dark:text-green-200">
                          current
                        </span>
                      )}
                    </td>
                    <td className="py-2 pr-4 text-gray-600 dark:text-gray-400">
                      {v.runtime_type}
                    </td>
                    <td className="py-2 pr-4 font-mono text-xs text-gray-500 dark:text-gray-400">
                      {v.image_digest ? `${v.image_digest.slice(0, 19)}…` : "—"}
                    </td>
                    <td className="py-2 pr-4 text-gray-500 dark:text-gray-400">
                      {new Date(v.published_at).toLocaleString()}
                    </td>
                    <td className="py-2 pr-4">
                      {!isCurrent && (
                        <div className="flex gap-2">
                          <button
                            type="button"
                            onClick={() => onActivate(v.id)}
                            disabled={pending}
                            className="rounded border border-gray-300 px-2 py-1 text-xs hover:bg-gray-50 disabled:opacity-50 dark:border-gray-700 dark:hover:bg-gray-800"
                          >
                            Activate
                          </button>
                          <button
                            type="button"
                            onClick={() => onDelete(v.id)}
                            disabled={pending}
                            className="rounded border border-red-300 px-2 py-1 text-xs text-red-700 hover:bg-red-50 disabled:opacity-50 dark:border-red-900 dark:text-red-300 dark:hover:bg-red-950"
                          >
                            Delete
                          </button>
                        </div>
                      )}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        )}
      </div>
    </section>
  );
}
