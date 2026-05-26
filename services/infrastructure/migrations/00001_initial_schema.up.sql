-- 00001_initial_schema.up.sql
-- Initial database schema for Lettuce.
--
--




--
-- Name: btree_gist; Type: EXTENSION; Schema: -; Owner: -
--

CREATE EXTENSION IF NOT EXISTS btree_gist WITH SCHEMA public;


--
-- Name: EXTENSION btree_gist; Type: COMMENT; Schema: -; Owner: -
--

COMMENT ON EXTENSION btree_gist IS 'support for indexing common datatypes in GiST';


--
-- Name: pgcrypto; Type: EXTENSION; Schema: -; Owner: -
--

CREATE EXTENSION IF NOT EXISTS pgcrypto WITH SCHEMA public;


--
-- Name: EXTENSION pgcrypto; Type: COMMENT; Schema: -; Owner: -
--

COMMENT ON EXTENSION pgcrypto IS 'cryptographic functions';


--
-- Name: assignment_outcome; Type: TYPE; Schema: public; Owner: -
--

CREATE TYPE public.assignment_outcome AS ENUM (
    'COMPLETED',
    'EXPIRED',
    'ABANDONED',
    'REJECTED'
);


--
-- Name: comparison_mode; Type: TYPE; Schema: public; Owner: -
--

CREATE TYPE public.comparison_mode AS ENUM (
    'EXACT',
    'NUMERIC_TOLERANCE',
    'CUSTOM'
);


--
-- Name: leaf_state; Type: TYPE; Schema: public; Owner: -
--

CREATE TYPE public.leaf_state AS ENUM (
    'DRAFT',
    'CONFIGURING',
    'ACTIVE',
    'PAUSED',
    'COMPLETED',
    'ARCHIVED'
);


--
-- Name: leaf_visibility; Type: TYPE; Schema: public; Owner: -
--

CREATE TYPE public.leaf_visibility AS ENUM (
    'PUBLIC',
    'UNLISTED',
    'PRIVATE'
);


--
-- Name: runtime_type; Type: TYPE; Schema: public; Owner: -
--

CREATE TYPE public.runtime_type AS ENUM (
    'NATIVE',
    'CONTAINER',
    'WASM',
    'SCRIPT'
);


--
-- Name: task_pattern; Type: TYPE; Schema: public; Owner: -
--

CREATE TYPE public.task_pattern AS ENUM (
    'PARAMETER_SWEEP',
    'MAP_REDUCE',
    'MONTE_CARLO',
    'CUSTOM'
);


--
-- Name: validation_status; Type: TYPE; Schema: public; Owner: -
--

CREATE TYPE public.validation_status AS ENUM (
    'PENDING',
    'AGREED',
    'DISAGREED'
);


--
-- Name: work_unit_priority; Type: TYPE; Schema: public; Owner: -
--

CREATE TYPE public.work_unit_priority AS ENUM (
    'NORMAL',
    'HIGH',
    'CRITICAL'
);


--
-- Name: work_unit_state; Type: TYPE; Schema: public; Owner: -
--

CREATE TYPE public.work_unit_state AS ENUM (
    'CREATED',
    'QUEUED',
    'ASSIGNED',
    'RUNNING',
    'COMPLETED',
    'VALIDATED',
    'REJECTED',
    'EXPIRED',
    'FAILED'
);


--
-- Name: update_updated_at(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.update_updated_at() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
  NEW.updated_at = NOW();
  RETURN NEW;
END;
$$;




--
-- Name: accounts; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.accounts (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    user_id uuid NOT NULL,
    type character varying(50) NOT NULL,
    provider character varying(50) NOT NULL,
    provider_account_id character varying(255) NOT NULL,
    access_token text,
    refresh_token text,
    expires_at integer,
    token_type character varying(50),
    scope text,
    id_token text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: api_keys; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.api_keys (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    user_id uuid NOT NULL,
    name character varying(100) NOT NULL,
    key_prefix character varying(12) NOT NULL,
    key_hash bytea NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    last_used_at timestamp with time zone,
    revoked_at timestamp with time zone
);


--
-- Name: batches; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.batches (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    leaf_id uuid NOT NULL,
    sequence_number integer NOT NULL,
    total_work_units integer NOT NULL,
    completed_work_units integer DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT batches_completed_work_units_check CHECK ((completed_work_units >= 0)),
    CONSTRAINT batches_sequence_number_check CHECK ((sequence_number > 0)),
    CONSTRAINT batches_total_work_units_check CHECK ((total_work_units > 0))
);


--
-- Name: credit_attestations; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.credit_attestations (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    leaf_id uuid NOT NULL,
    volunteer_public_key bytea NOT NULL,
    work_unit_id uuid NOT NULL,
    raw_metrics jsonb NOT NULL,
    validation_outcome character varying(20) NOT NULL,
    credit_amount numeric(18,6) NOT NULL,
    attestation_timestamp timestamp with time zone NOT NULL,
    signature bytea NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT credit_attestations_credit_amount_check CHECK ((credit_amount >= (0)::numeric)),
    CONSTRAINT credit_attestations_validation_outcome_check CHECK (((validation_outcome)::text = ANY ((ARRAY['AGREED'::character varying, 'DISAGREED'::character varying, 'EXPIRED'::character varying])::text[])))
);


--
-- Name: credit_ledger; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.credit_ledger (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    volunteer_id uuid NOT NULL,
    leaf_id uuid NOT NULL,
    work_unit_id uuid NOT NULL,
    result_id uuid NOT NULL,
    credit_amount numeric(18,6) NOT NULL,
    granted_at timestamp with time zone DEFAULT now() NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT credit_ledger_credit_amount_check CHECK ((credit_amount > (0)::numeric))
);


--
-- Name: file_uploads; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.file_uploads (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    leaf_id uuid NOT NULL,
    file_type character varying(30) NOT NULL,
    filename character varying(255) NOT NULL,
    storage_key text NOT NULL,
    size_bytes bigint NOT NULL,
    content_type character varying(100),
    checksum_sha256 character varying(64) NOT NULL,
    uploaded_by uuid,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    work_unit_id uuid,
    checkpoint_sequence integer,
    volunteer_id uuid,
    CONSTRAINT file_uploads_file_type_check CHECK (((file_type)::text = ANY ((ARRAY['INPUT_DATA'::character varying, 'CODE_ARTIFACT'::character varying, 'RESULT_DATA'::character varying, 'CHECKPOINT'::character varying])::text[]))),
    CONSTRAINT file_uploads_size_bytes_check CHECK ((size_bytes > 0))
);


--
-- Name: health_metrics_history; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.health_metrics_history (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    leaf_id uuid NOT NULL,
    metric_name character varying(50) NOT NULL,
    metric_value double precision NOT NULL,
    recorded_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: identity_challenges; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.identity_challenges (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    public_key bytea NOT NULL,
    challenge bytea NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    verified boolean DEFAULT false NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: leaf_stats_snapshots; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.leaf_stats_snapshots (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    leaf_id uuid NOT NULL,
    snapshot_at timestamp with time zone DEFAULT now() NOT NULL,
    total_work_units integer DEFAULT 0 NOT NULL,
    work_units_queued integer DEFAULT 0 NOT NULL,
    work_units_assigned integer DEFAULT 0 NOT NULL,
    work_units_running integer DEFAULT 0 NOT NULL,
    work_units_completed integer DEFAULT 0 NOT NULL,
    work_units_validated integer DEFAULT 0 NOT NULL,
    work_units_failed integer DEFAULT 0 NOT NULL,
    active_volunteers integer DEFAULT 0 NOT NULL,
    total_credit_granted numeric(18,6) DEFAULT 0 NOT NULL,
    avg_completion_seconds numeric(12,2),
    agreement_rate numeric(5,4),
    throughput_per_hour numeric(12,4),
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    spot_checks_total integer DEFAULT 0 NOT NULL,
    spot_checks_passed integer DEFAULT 0 NOT NULL,
    spot_checks_failed integer DEFAULT 0 NOT NULL,
    CONSTRAINT leaf_stats_snapshots_active_volunteers_check CHECK ((active_volunteers >= 0)),
    CONSTRAINT leaf_stats_snapshots_agreement_rate_check CHECK (((agreement_rate >= (0)::numeric) AND (agreement_rate <= (1)::numeric))),
    CONSTRAINT leaf_stats_snapshots_throughput_per_hour_check CHECK ((throughput_per_hour >= (0)::numeric)),
    CONSTRAINT leaf_stats_snapshots_total_credit_granted_check CHECK ((total_credit_granted >= (0)::numeric)),
    CONSTRAINT leaf_stats_snapshots_total_work_units_check CHECK ((total_work_units >= 0)),
    CONSTRAINT leaf_stats_snapshots_work_units_assigned_check CHECK ((work_units_assigned >= 0)),
    CONSTRAINT leaf_stats_snapshots_work_units_completed_check CHECK ((work_units_completed >= 0)),
    CONSTRAINT leaf_stats_snapshots_work_units_failed_check CHECK ((work_units_failed >= 0)),
    CONSTRAINT leaf_stats_snapshots_work_units_queued_check CHECK ((work_units_queued >= 0)),
    CONSTRAINT leaf_stats_snapshots_work_units_running_check CHECK ((work_units_running >= 0)),
    CONSTRAINT leaf_stats_snapshots_work_units_validated_check CHECK ((work_units_validated >= 0))
);


--
-- Name: leafs; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.leafs (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    name character varying(100) NOT NULL,
    slug character varying(100) NOT NULL,
    description text NOT NULL,
    research_area text[],
    creator_id uuid,
    creator_public_key bytea,
    state public.leaf_state DEFAULT 'DRAFT'::public.leaf_state NOT NULL,
    task_pattern public.task_pattern NOT NULL,
    execution_config jsonb DEFAULT '{}'::jsonb NOT NULL,
    validation_config jsonb DEFAULT '{}'::jsonb NOT NULL,
    fault_tolerance_config jsonb DEFAULT '{}'::jsonb NOT NULL,
    data_config jsonb DEFAULT '{}'::jsonb NOT NULL,
    credit_config jsonb DEFAULT '{}'::jsonb NOT NULL,
    resource_requirements jsonb DEFAULT '{}'::jsonb NOT NULL,
    is_ongoing boolean DEFAULT false NOT NULL,
    visibility public.leaf_visibility DEFAULT 'PUBLIC'::public.leaf_visibility NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    stats_cache_seconds integer DEFAULT 60 NOT NULL,
    rsc_fpops_est double precision,
    CONSTRAINT chk_leafs_creator CHECK (((creator_id IS NOT NULL) OR (creator_public_key IS NOT NULL))),
    CONSTRAINT leafs_rsc_fpops_est_check CHECK (((rsc_fpops_est IS NULL) OR (rsc_fpops_est > (0)::double precision))),
    CONSTRAINT leafs_description_check CHECK (((char_length(description) >= 10) AND (char_length(description) <= 10000))),
    CONSTRAINT leafs_name_check CHECK ((char_length((name)::text) >= 3))
);


--
-- Name: research_areas; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.research_areas (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    slug character varying(50) NOT NULL,
    name character varying(100) NOT NULL,
    description text,
    display_order integer DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: results; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.results (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    work_unit_id uuid NOT NULL,
    volunteer_id uuid,
    output_data jsonb,
    output_data_ref text,
    output_checksum character varying(64) NOT NULL,
    execution_metadata jsonb NOT NULL,
    validation_status public.validation_status DEFAULT 'PENDING'::public.validation_status NOT NULL,
    submitted_at timestamp with time zone DEFAULT now() NOT NULL,
    validated_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT chk_results_output CHECK (((output_data IS NOT NULL) OR (output_data_ref IS NOT NULL)))
);


--
-- Name: sessions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.sessions (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    user_id uuid NOT NULL,
    session_token text NOT NULL,
    expires timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: users; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.users (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    email character varying(255) NOT NULL,
    email_verified boolean DEFAULT false NOT NULL,
    email_verified_at timestamp with time zone,
    password_hash text,
    username character varying(50) NOT NULL,
    display_name character varying(100),
    avatar_url text,
    role character varying(20) DEFAULT 'USER'::character varying NOT NULL,
    github_id character varying(50),
    google_id character varying(50),
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    deactivated_at timestamp with time zone,
    CONSTRAINT chk_users_auth_method CHECK (((password_hash IS NOT NULL) OR (github_id IS NOT NULL) OR (google_id IS NOT NULL))),
    CONSTRAINT chk_users_username_format CHECK ((((username)::text ~ '^[a-z][a-z0-9-]*$'::text) AND (char_length((username)::text) >= 3))),
    CONSTRAINT users_role_check CHECK (((role)::text = ANY ((ARRAY['USER'::character varying, 'ADMIN'::character varying])::text[])))
);


--
-- Name: verification_tokens; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.verification_tokens (
    identifier character varying(255) NOT NULL,
    token text NOT NULL,
    expires timestamp with time zone NOT NULL
);


--
-- Name: volunteer_leaf_preferences; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.volunteer_leaf_preferences (
    volunteer_id uuid NOT NULL,
    leaf_id uuid NOT NULL,
    preference character varying(20) DEFAULT 'ATTACHED'::character varying NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT volunteer_leaf_preferences_preference_check CHECK (((preference)::text = ANY ((ARRAY['ATTACHED'::character varying, 'BLOCKED'::character varying])::text[])))
);


--
-- Name: volunteer_rac; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.volunteer_rac (
    volunteer_id uuid NOT NULL,
    leaf_id uuid NOT NULL,
    rac numeric(18,6) DEFAULT 0 NOT NULL,
    total_credit numeric(18,6) DEFAULT 0 NOT NULL,
    last_credit_at timestamp with time zone,
    last_updated_at timestamp with time zone DEFAULT now() NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT volunteer_rac_rac_check CHECK ((rac >= (0)::numeric)),
    CONSTRAINT volunteer_rac_total_credit_check CHECK ((total_credit >= (0)::numeric))
);


--
-- Name: volunteers; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.volunteers (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    public_key bytea NOT NULL,
    user_id uuid,
    display_name character varying(100),
    hardware_capabilities jsonb DEFAULT '{}'::jsonb NOT NULL,
    available_runtimes text[] DEFAULT '{NATIVE}'::text[] NOT NULL,
    scheduling_mode character varying(20) DEFAULT 'ALWAYS'::character varying NOT NULL,
    schedule_config jsonb,
    is_active boolean DEFAULT false NOT NULL,
    last_seen_at timestamp with time zone,
    total_work_units_completed integer DEFAULT 0 NOT NULL,
    total_work_units_rejected integer DEFAULT 0 NOT NULL,
    registered_at timestamp with time zone DEFAULT now() NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    numeric_id integer NOT NULL,
    CONSTRAINT volunteers_scheduling_mode_check CHECK (((scheduling_mode)::text = ANY ((ARRAY['ALWAYS'::character varying, 'WHEN_IDLE'::character varying, 'SCHEDULED'::character varying])::text[]))),
    CONSTRAINT volunteers_total_work_units_completed_check CHECK ((total_work_units_completed >= 0)),
    CONSTRAINT volunteers_total_work_units_rejected_check CHECK ((total_work_units_rejected >= 0))
);


--
-- Name: volunteers_numeric_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.volunteers_numeric_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: volunteers_numeric_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.volunteers_numeric_id_seq OWNED BY public.volunteers.numeric_id;


--
-- Name: work_unit_assignment_history; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.work_unit_assignment_history (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    work_unit_id uuid NOT NULL,
    volunteer_id uuid,
    assigned_at timestamp with time zone NOT NULL,
    outcome public.assignment_outcome,
    outcome_at timestamp with time zone,
    result_id uuid,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: work_units; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.work_units (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    leaf_id uuid NOT NULL,
    batch_id uuid,
    state public.work_unit_state DEFAULT 'CREATED'::public.work_unit_state NOT NULL,
    priority public.work_unit_priority DEFAULT 'NORMAL'::public.work_unit_priority NOT NULL,
    input_data jsonb,
    input_data_ref text,
    code_artifact_ref text NOT NULL,
    parameters jsonb,
    estimated_duration_seconds integer,
    deadline_seconds integer NOT NULL,
    output_spec jsonb,
    assigned_volunteer_id uuid,
    assigned_at timestamp with time zone,
    started_at timestamp with time zone,
    completed_at timestamp with time zone,
    validated_at timestamp with time zone,
    reassignment_count integer DEFAULT 0 NOT NULL,
    max_reassignments integer DEFAULT 3 NOT NULL,
    last_heartbeat_at timestamp with time zone,
    flagged_for_review boolean DEFAULT false NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    spot_check boolean DEFAULT false NOT NULL,
    last_checkpoint_at timestamp with time zone,
    last_checkpoint_sequence integer DEFAULT 0,
    CONSTRAINT work_units_deadline_seconds_check CHECK ((deadline_seconds >= 0)),
    CONSTRAINT work_units_estimated_duration_seconds_check CHECK ((estimated_duration_seconds > 0)),
    CONSTRAINT work_units_max_reassignments_check CHECK ((max_reassignments >= 1)),
    CONSTRAINT work_units_reassignment_count_check CHECK ((reassignment_count >= 0))
);


--
-- Name: volunteers numeric_id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.volunteers ALTER COLUMN numeric_id SET DEFAULT nextval('public.volunteers_numeric_id_seq'::regclass);


--
-- Name: accounts accounts_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.accounts
    ADD CONSTRAINT accounts_pkey PRIMARY KEY (id);


--
-- Name: api_keys api_keys_key_hash_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.api_keys
    ADD CONSTRAINT api_keys_key_hash_key UNIQUE (key_hash);


--
-- Name: api_keys api_keys_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.api_keys
    ADD CONSTRAINT api_keys_pkey PRIMARY KEY (id);


--
-- Name: batches batches_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.batches
    ADD CONSTRAINT batches_pkey PRIMARY KEY (id);


--
-- Name: credit_attestations credit_attestations_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.credit_attestations
    ADD CONSTRAINT credit_attestations_pkey PRIMARY KEY (id);


--
-- Name: credit_ledger credit_ledger_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.credit_ledger
    ADD CONSTRAINT credit_ledger_pkey PRIMARY KEY (id);


--
-- Name: file_uploads file_uploads_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.file_uploads
    ADD CONSTRAINT file_uploads_pkey PRIMARY KEY (id);


--
-- Name: health_metrics_history health_metrics_history_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.health_metrics_history
    ADD CONSTRAINT health_metrics_history_pkey PRIMARY KEY (id);


--
-- Name: identity_challenges identity_challenges_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.identity_challenges
    ADD CONSTRAINT identity_challenges_pkey PRIMARY KEY (id);


--
-- Name: leaf_stats_snapshots leaf_stats_snapshots_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.leaf_stats_snapshots
    ADD CONSTRAINT leaf_stats_snapshots_pkey PRIMARY KEY (id);


--
-- Name: leafs leafs_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.leafs
    ADD CONSTRAINT leafs_pkey PRIMARY KEY (id);


--
-- Name: research_areas research_areas_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.research_areas
    ADD CONSTRAINT research_areas_pkey PRIMARY KEY (id);


--
-- Name: results results_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.results
    ADD CONSTRAINT results_pkey PRIMARY KEY (id);


--
-- Name: sessions sessions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.sessions
    ADD CONSTRAINT sessions_pkey PRIMARY KEY (id);


--
-- Name: accounts uq_accounts_provider; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.accounts
    ADD CONSTRAINT uq_accounts_provider UNIQUE (provider, provider_account_id);


--
-- Name: batches uq_batches_leaf_sequence; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.batches
    ADD CONSTRAINT uq_batches_leaf_sequence UNIQUE (leaf_id, sequence_number);


--
-- Name: credit_ledger uq_credit_ledger_result; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.credit_ledger
    ADD CONSTRAINT uq_credit_ledger_result UNIQUE (result_id);


--
-- Name: file_uploads uq_file_uploads_storage_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.file_uploads
    ADD CONSTRAINT uq_file_uploads_storage_key UNIQUE (storage_key);


--
-- Name: leafs uq_leafs_slug_creator; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.leafs
    ADD CONSTRAINT uq_leafs_slug_creator UNIQUE (slug, creator_id);


--
-- Name: research_areas uq_research_areas_name; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.research_areas
    ADD CONSTRAINT uq_research_areas_name UNIQUE (name);


--
-- Name: research_areas uq_research_areas_slug; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.research_areas
    ADD CONSTRAINT uq_research_areas_slug UNIQUE (slug);


--
-- Name: results uq_results_work_unit_volunteer; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.results
    ADD CONSTRAINT uq_results_work_unit_volunteer UNIQUE (work_unit_id, volunteer_id);


--
-- Name: sessions uq_sessions_token; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.sessions
    ADD CONSTRAINT uq_sessions_token UNIQUE (session_token);


--
-- Name: users uq_users_email; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.users
    ADD CONSTRAINT uq_users_email UNIQUE (email);


--
-- Name: users uq_users_username; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.users
    ADD CONSTRAINT uq_users_username UNIQUE (username);


--
-- Name: verification_tokens uq_verification_tokens_token; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.verification_tokens
    ADD CONSTRAINT uq_verification_tokens_token UNIQUE (token);


--
-- Name: volunteers uq_volunteers_public_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.volunteers
    ADD CONSTRAINT uq_volunteers_public_key UNIQUE (public_key);


--
-- Name: users users_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.users
    ADD CONSTRAINT users_pkey PRIMARY KEY (id);


--
-- Name: verification_tokens verification_tokens_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.verification_tokens
    ADD CONSTRAINT verification_tokens_pkey PRIMARY KEY (identifier, token);


--
-- Name: volunteer_leaf_preferences volunteer_leaf_preferences_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.volunteer_leaf_preferences
    ADD CONSTRAINT volunteer_leaf_preferences_pkey PRIMARY KEY (volunteer_id, leaf_id);


--
-- Name: volunteer_rac volunteer_rac_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.volunteer_rac
    ADD CONSTRAINT volunteer_rac_pkey PRIMARY KEY (volunteer_id, leaf_id);


--
-- Name: volunteers volunteers_numeric_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.volunteers
    ADD CONSTRAINT volunteers_numeric_id_key UNIQUE (numeric_id);


--
-- Name: volunteers volunteers_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.volunteers
    ADD CONSTRAINT volunteers_pkey PRIMARY KEY (id);


--
-- Name: work_unit_assignment_history work_unit_assignment_history_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.work_unit_assignment_history
    ADD CONSTRAINT work_unit_assignment_history_pkey PRIMARY KEY (id);


--
-- Name: work_units work_units_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.work_units
    ADD CONSTRAINT work_units_pkey PRIMARY KEY (id);


--
-- Name: idx_api_keys_user_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_api_keys_user_id ON public.api_keys USING btree (user_id);


--
-- Name: idx_assignment_history_volunteer; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_assignment_history_volunteer ON public.work_unit_assignment_history USING btree (volunteer_id, assigned_at DESC);


--
-- Name: idx_assignment_history_work_unit; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_assignment_history_work_unit ON public.work_unit_assignment_history USING btree (work_unit_id, assigned_at DESC);


--
-- Name: idx_attestations_leaf_time; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_attestations_leaf_time ON public.credit_attestations USING btree (leaf_id, attestation_timestamp);


--
-- Name: idx_attestations_volunteer; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_attestations_volunteer ON public.credit_attestations USING btree (volunteer_public_key, leaf_id);


--
-- Name: idx_credit_ledger_leaf_time; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_credit_ledger_leaf_time ON public.credit_ledger USING btree (leaf_id, granted_at);


--
-- Name: idx_credit_ledger_volunteer_leaf; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_credit_ledger_volunteer_leaf ON public.credit_ledger USING btree (volunteer_id, leaf_id);


--
-- Name: idx_credit_ledger_volunteer_time; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_credit_ledger_volunteer_time ON public.credit_ledger USING btree (volunteer_id, granted_at);


--
-- Name: idx_file_uploads_checkpoint_lookup; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_file_uploads_checkpoint_lookup ON public.file_uploads USING btree (work_unit_id, file_type, checkpoint_sequence DESC) WHERE ((file_type)::text = 'CHECKPOINT'::text);


--
-- Name: idx_file_uploads_leaf_type; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_file_uploads_leaf_type ON public.file_uploads USING btree (leaf_id, file_type);


--
-- Name: idx_health_metrics_leaf_time; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_health_metrics_leaf_time ON public.health_metrics_history USING btree (leaf_id, metric_name, recorded_at DESC);


--
-- Name: idx_identity_challenges_expiry; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_identity_challenges_expiry ON public.identity_challenges USING btree (expires_at) WHERE (verified = false);


--
-- Name: idx_leafs_creator_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_leafs_creator_id ON public.leafs USING btree (creator_id);


--
-- Name: idx_leafs_state; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_leafs_state ON public.leafs USING btree (state);


--
-- Name: idx_leafs_visibility_state; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_leafs_visibility_state ON public.leafs USING btree (visibility, state);


--
-- Name: idx_results_validation; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_results_validation ON public.results USING btree (work_unit_id, validation_status);


--
-- Name: idx_results_volunteer; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_results_volunteer ON public.results USING btree (volunteer_id, submitted_at DESC);


--
-- Name: idx_results_work_unit; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_results_work_unit ON public.results USING btree (work_unit_id);


--
-- Name: idx_stats_snapshots_leaf_time; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_stats_snapshots_leaf_time ON public.leaf_stats_snapshots USING btree (leaf_id, snapshot_at DESC);


--
-- Name: idx_stats_snapshots_time; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_stats_snapshots_time ON public.leaf_stats_snapshots USING btree (snapshot_at);


--
-- Name: idx_volunteer_rac_leaf; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_volunteer_rac_leaf ON public.volunteer_rac USING btree (leaf_id, rac DESC);


--
-- Name: idx_volunteer_rac_volunteer; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_volunteer_rac_volunteer ON public.volunteer_rac USING btree (volunteer_id);


--
-- Name: idx_volunteers_active; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_volunteers_active ON public.volunteers USING btree (is_active, last_seen_at);


--
-- Name: idx_volunteers_user_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_volunteers_user_id ON public.volunteers USING btree (user_id);


--
-- Name: idx_work_units_assignment; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_work_units_assignment ON public.work_units USING btree (assigned_volunteer_id, state);


--
-- Name: idx_work_units_batch; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_work_units_batch ON public.work_units USING btree (batch_id, state);


--
-- Name: idx_work_units_heartbeat; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_work_units_heartbeat ON public.work_units USING btree (state, last_heartbeat_at) WHERE (state = 'RUNNING'::public.work_unit_state);


--
-- Name: idx_work_units_leaf_state; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_work_units_leaf_state ON public.work_units USING btree (leaf_id, state);


--
-- Name: idx_work_units_queue; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_work_units_queue ON public.work_units USING btree (leaf_id, priority DESC, created_at) WHERE (state = 'QUEUED'::public.work_unit_state);


--
-- Name: uq_users_github_id; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX uq_users_github_id ON public.users USING btree (github_id) WHERE (github_id IS NOT NULL);


--
-- Name: uq_users_google_id; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX uq_users_google_id ON public.users USING btree (google_id) WHERE (google_id IS NOT NULL);


--
-- Name: accounts trg_accounts_updated_at; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_accounts_updated_at BEFORE UPDATE ON public.accounts FOR EACH ROW EXECUTE FUNCTION public.update_updated_at();


--
-- Name: batches trg_batches_updated_at; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_batches_updated_at BEFORE UPDATE ON public.batches FOR EACH ROW EXECUTE FUNCTION public.update_updated_at();


--
-- Name: leafs trg_leafs_updated_at; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_leafs_updated_at BEFORE UPDATE ON public.leafs FOR EACH ROW EXECUTE FUNCTION public.update_updated_at();


--
-- Name: research_areas trg_research_areas_updated_at; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_research_areas_updated_at BEFORE UPDATE ON public.research_areas FOR EACH ROW EXECUTE FUNCTION public.update_updated_at();


--
-- Name: results trg_results_updated_at; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_results_updated_at BEFORE UPDATE ON public.results FOR EACH ROW EXECUTE FUNCTION public.update_updated_at();


--
-- Name: users trg_users_updated_at; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_users_updated_at BEFORE UPDATE ON public.users FOR EACH ROW EXECUTE FUNCTION public.update_updated_at();


--
-- Name: volunteers trg_volunteers_updated_at; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_volunteers_updated_at BEFORE UPDATE ON public.volunteers FOR EACH ROW EXECUTE FUNCTION public.update_updated_at();


--
-- Name: work_units trg_work_units_updated_at; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER trg_work_units_updated_at BEFORE UPDATE ON public.work_units FOR EACH ROW EXECUTE FUNCTION public.update_updated_at();


--
-- Name: accounts accounts_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.accounts
    ADD CONSTRAINT accounts_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- Name: api_keys api_keys_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.api_keys
    ADD CONSTRAINT api_keys_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- Name: batches batches_leaf_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.batches
    ADD CONSTRAINT batches_leaf_id_fkey FOREIGN KEY (leaf_id) REFERENCES public.leafs(id) ON DELETE CASCADE;


--
-- Name: credit_attestations credit_attestations_leaf_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.credit_attestations
    ADD CONSTRAINT credit_attestations_leaf_id_fkey FOREIGN KEY (leaf_id) REFERENCES public.leafs(id) ON DELETE RESTRICT;


--
-- Name: credit_attestations credit_attestations_work_unit_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.credit_attestations
    ADD CONSTRAINT credit_attestations_work_unit_id_fkey FOREIGN KEY (work_unit_id) REFERENCES public.work_units(id) ON DELETE RESTRICT;


--
-- Name: credit_ledger credit_ledger_leaf_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.credit_ledger
    ADD CONSTRAINT credit_ledger_leaf_id_fkey FOREIGN KEY (leaf_id) REFERENCES public.leafs(id) ON DELETE RESTRICT;


--
-- Name: credit_ledger credit_ledger_result_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.credit_ledger
    ADD CONSTRAINT credit_ledger_result_id_fkey FOREIGN KEY (result_id) REFERENCES public.results(id) ON DELETE RESTRICT;


--
-- Name: credit_ledger credit_ledger_volunteer_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.credit_ledger
    ADD CONSTRAINT credit_ledger_volunteer_id_fkey FOREIGN KEY (volunteer_id) REFERENCES public.volunteers(id) ON DELETE RESTRICT;


--
-- Name: credit_ledger credit_ledger_work_unit_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.credit_ledger
    ADD CONSTRAINT credit_ledger_work_unit_id_fkey FOREIGN KEY (work_unit_id) REFERENCES public.work_units(id) ON DELETE RESTRICT;


--
-- Name: file_uploads file_uploads_leaf_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.file_uploads
    ADD CONSTRAINT file_uploads_leaf_id_fkey FOREIGN KEY (leaf_id) REFERENCES public.leafs(id) ON DELETE CASCADE;


--
-- Name: file_uploads file_uploads_uploaded_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.file_uploads
    ADD CONSTRAINT file_uploads_uploaded_by_fkey FOREIGN KEY (uploaded_by) REFERENCES public.users(id) ON DELETE SET NULL;


--
-- Name: file_uploads file_uploads_volunteer_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.file_uploads
    ADD CONSTRAINT file_uploads_volunteer_id_fkey FOREIGN KEY (volunteer_id) REFERENCES public.volunteers(id) ON DELETE SET NULL;


--
-- Name: file_uploads file_uploads_work_unit_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.file_uploads
    ADD CONSTRAINT file_uploads_work_unit_id_fkey FOREIGN KEY (work_unit_id) REFERENCES public.work_units(id) ON DELETE CASCADE;


--
-- Name: health_metrics_history health_metrics_history_leaf_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.health_metrics_history
    ADD CONSTRAINT health_metrics_history_leaf_id_fkey FOREIGN KEY (leaf_id) REFERENCES public.leafs(id) ON DELETE CASCADE;


--
-- Name: leaf_stats_snapshots leaf_stats_snapshots_leaf_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.leaf_stats_snapshots
    ADD CONSTRAINT leaf_stats_snapshots_leaf_id_fkey FOREIGN KEY (leaf_id) REFERENCES public.leafs(id) ON DELETE CASCADE;


--
-- Name: leafs leafs_creator_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.leafs
    ADD CONSTRAINT leafs_creator_id_fkey FOREIGN KEY (creator_id) REFERENCES public.users(id) ON DELETE SET NULL;


--
-- Name: results results_volunteer_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.results
    ADD CONSTRAINT results_volunteer_id_fkey FOREIGN KEY (volunteer_id) REFERENCES public.volunteers(id) ON DELETE SET NULL;


--
-- Name: results results_work_unit_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.results
    ADD CONSTRAINT results_work_unit_id_fkey FOREIGN KEY (work_unit_id) REFERENCES public.work_units(id) ON DELETE CASCADE;


--
-- Name: sessions sessions_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.sessions
    ADD CONSTRAINT sessions_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- Name: volunteer_leaf_preferences volunteer_leaf_preferences_leaf_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.volunteer_leaf_preferences
    ADD CONSTRAINT volunteer_leaf_preferences_leaf_id_fkey FOREIGN KEY (leaf_id) REFERENCES public.leafs(id) ON DELETE CASCADE;


--
-- Name: volunteer_leaf_preferences volunteer_leaf_preferences_volunteer_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.volunteer_leaf_preferences
    ADD CONSTRAINT volunteer_leaf_preferences_volunteer_id_fkey FOREIGN KEY (volunteer_id) REFERENCES public.volunteers(id) ON DELETE CASCADE;


--
-- Name: volunteer_rac volunteer_rac_leaf_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.volunteer_rac
    ADD CONSTRAINT volunteer_rac_leaf_id_fkey FOREIGN KEY (leaf_id) REFERENCES public.leafs(id) ON DELETE CASCADE;


--
-- Name: volunteer_rac volunteer_rac_volunteer_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.volunteer_rac
    ADD CONSTRAINT volunteer_rac_volunteer_id_fkey FOREIGN KEY (volunteer_id) REFERENCES public.volunteers(id) ON DELETE CASCADE;


--
-- Name: volunteers volunteers_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.volunteers
    ADD CONSTRAINT volunteers_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE SET NULL;


--
-- Name: work_unit_assignment_history work_unit_assignment_history_result_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.work_unit_assignment_history
    ADD CONSTRAINT work_unit_assignment_history_result_id_fkey FOREIGN KEY (result_id) REFERENCES public.results(id) ON DELETE SET NULL;


--
-- Name: work_unit_assignment_history work_unit_assignment_history_volunteer_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.work_unit_assignment_history
    ADD CONSTRAINT work_unit_assignment_history_volunteer_id_fkey FOREIGN KEY (volunteer_id) REFERENCES public.volunteers(id) ON DELETE SET NULL;


--
-- Name: work_unit_assignment_history work_unit_assignment_history_work_unit_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.work_unit_assignment_history
    ADD CONSTRAINT work_unit_assignment_history_work_unit_id_fkey FOREIGN KEY (work_unit_id) REFERENCES public.work_units(id) ON DELETE CASCADE;


--
-- Name: work_units work_units_assigned_volunteer_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.work_units
    ADD CONSTRAINT work_units_assigned_volunteer_id_fkey FOREIGN KEY (assigned_volunteer_id) REFERENCES public.volunteers(id) ON DELETE SET NULL;


--
-- Name: work_units work_units_batch_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.work_units
    ADD CONSTRAINT work_units_batch_id_fkey FOREIGN KEY (batch_id) REFERENCES public.batches(id) ON DELETE SET NULL;


--
-- Name: work_units work_units_leaf_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.work_units
    ADD CONSTRAINT work_units_leaf_id_fkey FOREIGN KEY (leaf_id) REFERENCES public.leafs(id) ON DELETE CASCADE;


--
--

-- Seed data
INSERT INTO research_areas (slug, name, description, display_order) VALUES
  ('physics',          'Physics',               'Physics and astrophysics',                1),
  ('biology',          'Biology',               'Biology and life sciences',               2),
  ('chemistry',        'Chemistry',             'Chemistry and materials science',          3),
  ('climate',          'Climate Science',        'Climate modeling and earth science',       4),
  ('mathematics',      'Mathematics',           'Pure and applied mathematics',             5),
  ('computer-science', 'Computer Science',       'Algorithms, systems, and theory',          6),
  ('economics',        'Economics',             'Economics and econometrics',               7),
  ('social-science',   'Social Science',         'Psychology, sociology, political science', 8),
  ('medicine',         'Medicine & Health',      'Medical research and public health',       9),
  ('engineering',      'Engineering',           'Mechanical, electrical, civil engineering',10),
  ('ml-ai',            'Machine Learning & AI', 'Machine learning and artificial intelligence', 11),
  ('other',            'Other',                 'Other research areas',                    99);
