-- 00004_leaf_artifact_versions.up.sql
-- Immutable, content-addressed artifact VERSION REGISTRY per leaf (+ a current-
-- version pointer on the leaf).
--
-- BACKGROUND (TODO #38): a leaf's runnable artifact (native binaries + per-platform
-- SHA-256, or a container image) lived ONLY as mutable fields inside
-- leafs.execution_config. Updating it overwrote the single current value in place,
-- so there was no version history, no rollback, no content-addressed identity, and —
-- combined with the head's in-process leaf snapshot never refreshing — running
-- volunteers kept executing the OLD artifact even on freshly generated work units.
--
-- This registry makes every published artifact an IMMUTABLE ROW:
--   * version_label      operator-chosen identity, UNIQUE + immutable per leaf (the
--                        head owner picks the scheme: semver, gitsha, native-go-2.0…).
--   * execution_config   the frozen snapshot the head builds assignments from. Once
--                        published it is never mutated (enforced in the repo layer);
--                        a new artifact is a NEW row, never an in-place edit.
--   * image_digest       container content address (sha256:<hex>) for pull-by-digest
--                        and post-pull integrity verification on the volunteer. The
--                        native content address is the per-platform SHA-256 already
--                        inside execution_config (binary_checksums); no extra column.
--
-- leafs.current_artifact_version_id points at the row the leaf currently dispatches.
-- "Publish a new version" = insert a row + repoint. "Rollback" = repoint to any
-- prior row (artifacts are retained per the operator's retention policy, see #38).
-- Old in-flight work finishes on its pinned version (00005); new work takes current.
-- A NULL pointer preserves the legacy path (build assignments from
-- leafs.execution_config directly), so this migration is non-breaking for existing
-- leaves until they publish their first version.

CREATE TABLE public.leaf_artifact_versions (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    leaf_id uuid NOT NULL,
    version_label character varying(200) NOT NULL,
    runtime_type public.runtime_type NOT NULL,
    execution_config jsonb DEFAULT '{}'::jsonb NOT NULL,
    image_digest character varying(71),
    notes text,
    published_by uuid,
    published_at timestamp with time zone DEFAULT now() NOT NULL,
    superseded_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT leaf_artifact_versions_version_label_check CHECK ((char_length((version_label)::text) >= 1)),
    CONSTRAINT leaf_artifact_versions_image_digest_check CHECK (((image_digest IS NULL) OR (image_digest ~ '^sha256:[0-9a-f]{64}$'::text)))
);

ALTER TABLE ONLY public.leaf_artifact_versions
    ADD CONSTRAINT leaf_artifact_versions_pkey PRIMARY KEY (id);

-- version_label is the operator-facing identity: unique and (by repo contract)
-- immutable per leaf. Re-publishing a used label is rejected, not overwritten.
ALTER TABLE ONLY public.leaf_artifact_versions
    ADD CONSTRAINT leaf_artifact_versions_leaf_label_key UNIQUE (leaf_id, version_label);

ALTER TABLE ONLY public.leaf_artifact_versions
    ADD CONSTRAINT leaf_artifact_versions_leaf_id_fkey FOREIGN KEY (leaf_id) REFERENCES public.leafs(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.leaf_artifact_versions
    ADD CONSTRAINT leaf_artifact_versions_published_by_fkey FOREIGN KEY (published_by) REFERENCES public.users(id) ON DELETE SET NULL;

-- History/listing in publish order, and the retention-GC sweep, both scan per leaf
-- newest-first.
CREATE INDEX idx_leaf_artifact_versions_leaf_time
    ON public.leaf_artifact_versions USING btree (leaf_id, published_at DESC);

CREATE TRIGGER trg_leaf_artifact_versions_updated_at BEFORE UPDATE ON public.leaf_artifact_versions FOR EACH ROW EXECUTE FUNCTION public.update_updated_at();

-- Current-version pointer on the leaf. NULL = legacy path (dispatch from
-- leafs.execution_config). Set on first publish; moved on publish/rollback.
ALTER TABLE public.leafs
    ADD COLUMN current_artifact_version_id uuid;

ALTER TABLE ONLY public.leafs
    ADD CONSTRAINT leafs_current_artifact_version_id_fkey FOREIGN KEY (current_artifact_version_id) REFERENCES public.leaf_artifact_versions(id) ON DELETE SET NULL;
