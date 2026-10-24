package store

// This additive observation-cache migration is developed as one unshipped
// change. Private cache rows are not part of pulled transcript manifests.
const migrationV68 = `
CREATE TABLE collective_observation_scopes (
    village_url TEXT NOT NULL,
    user_id TEXT NOT NULL,
    incarnation TEXT NOT NULL UNIQUE,
    invocation_seq INTEGER NOT NULL DEFAULT 0 CHECK (invocation_seq >= 0),
    attempt_generation INTEGER NOT NULL DEFAULT 0 CHECK (attempt_generation >= 0),
    receipt_generation INTEGER NOT NULL DEFAULT 0 CHECK (receipt_generation >= 0),
    diagnostic_generation INTEGER NOT NULL DEFAULT 0 CHECK (diagnostic_generation >= 0),
    attempt_coverage TEXT NOT NULL DEFAULT 'unknown' CHECK (attempt_coverage IN ('available','unknown')),
    receipt_coverage TEXT NOT NULL DEFAULT 'available' CHECK (receipt_coverage IN ('available','unknown')),
    diagnostic_coverage TEXT NOT NULL DEFAULT 'available' CHECK (diagnostic_coverage IN ('available','unknown')),
    cache_revision INTEGER NOT NULL DEFAULT 0 CHECK (cache_revision >= 0),
    PRIMARY KEY (village_url, user_id)
) WITHOUT ROWID;
CREATE TABLE collective_attempt_sources (
    village_url TEXT NOT NULL,
    user_id TEXT NOT NULL,
    collective_id TEXT NOT NULL,
    transcript_id TEXT NOT NULL,
    installed_seq INTEGER NOT NULL CHECK (installed_seq > 0),
    observed_at TEXT NOT NULL,
    PRIMARY KEY (village_url, user_id, collective_id, transcript_id),
    FOREIGN KEY (village_url, user_id) REFERENCES collective_observation_scopes(village_url, user_id)
) WITHOUT ROWID;
CREATE TABLE remote_contribution_attempts (
    village_url TEXT NOT NULL,
    user_id TEXT NOT NULL,
    collective_id TEXT NOT NULL,
    transcript_id TEXT NOT NULL,
    event_num INTEGER NOT NULL CHECK (event_num BETWEEN 1 AND 2147483647),
    status TEXT NOT NULL CHECK (status IN ('pending','approved','rejected','retracted','revoked')),
    recorded_at TEXT NOT NULL,
    decided_at TEXT,
    decided_by_actor TEXT NOT NULL CHECK (decided_by_actor IN ('','owner','collective','moderator')),
    first_seen_at TEXT NOT NULL,
    last_validated_at TEXT NOT NULL,
    PRIMARY KEY (village_url, user_id, collective_id, transcript_id, event_num),
    FOREIGN KEY (village_url, user_id) REFERENCES collective_observation_scopes(village_url, user_id)
) WITHOUT ROWID;
CREATE TABLE local_contribution_receipts (
    village_url TEXT NOT NULL, user_id TEXT NOT NULL,
    receipt_id TEXT NOT NULL CHECK(length(receipt_id) BETWEEN 1 AND 256),
    operation TEXT NOT NULL CHECK(operation IN ('publish','share')),
    transcript_id TEXT NOT NULL, collective_id TEXT,
    confirmed_at TEXT NOT NULL,
    outcome TEXT NOT NULL CHECK(outcome IN ('published','submitted')),
    source_receipt_id TEXT,
    PRIMARY KEY(village_url,user_id,receipt_id),
    FOREIGN KEY(village_url,user_id) REFERENCES collective_observation_scopes(village_url,user_id)
) WITHOUT ROWID;
CREATE INDEX idx_local_contribution_receipts_order ON local_contribution_receipts(village_url,user_id,confirmed_at,receipt_id);
CREATE INDEX idx_local_contribution_receipts_collective ON local_contribution_receipts(village_url,user_id,collective_id,confirmed_at,receipt_id);
CREATE TABLE local_operation_diagnostics (
    village_url TEXT NOT NULL, user_id TEXT NOT NULL, operation_id TEXT NOT NULL,
    operation TEXT NOT NULL CHECK(operation IN ('publish','share','fetch','pull')),
    transcript_id TEXT, collective_id TEXT, attempted_at TEXT NOT NULL,
    outcome TEXT NOT NULL CHECK(outcome IN ('failed','uncertain')),
    issue_code TEXT NOT NULL CHECK(issue_code IN ('auth','remote','contract','deadline','persistence','superseded')),
    issue_message TEXT NOT NULL, issue_retry TEXT NOT NULL,
    PRIMARY KEY(village_url,user_id,operation_id),
    FOREIGN KEY(village_url,user_id) REFERENCES collective_observation_scopes(village_url,user_id)
) WITHOUT ROWID;
CREATE INDEX idx_local_operation_diagnostics_order ON local_operation_diagnostics(village_url,user_id,attempted_at,operation_id);
CREATE INDEX idx_local_operation_diagnostics_collective ON local_operation_diagnostics(village_url,user_id,collective_id,attempted_at,operation_id);
CREATE TABLE transcript_collective_observations (
    village_url TEXT NOT NULL, user_id TEXT NOT NULL, transcript_id TEXT NOT NULL,
    installed_seq INTEGER NOT NULL DEFAULT 0, attempt_seq INTEGER NOT NULL DEFAULT 0,
    observed_at TEXT, state TEXT CHECK(state IN ('observed','invalidated')),
    last_attempt_at TEXT NOT NULL,
    issue_code TEXT CHECK(issue_code IN ('auth','remote','contract','deadline','persistence','superseded')),
    issue_message TEXT, issue_retry TEXT,
    PRIMARY KEY(village_url,user_id,transcript_id),
    FOREIGN KEY(village_url,user_id) REFERENCES collective_observation_scopes(village_url,user_id)
) WITHOUT ROWID;
CREATE TABLE transcript_collective_evidence (
    village_url TEXT NOT NULL, user_id TEXT NOT NULL, transcript_id TEXT NOT NULL,
    collective_id TEXT NOT NULL, version INTEGER NOT NULL CHECK(version>0),
    name TEXT NOT NULL, description TEXT, linked_github_org TEXT, shared_at TEXT NOT NULL,
    first_observed_at TEXT NOT NULL, last_observed_at TEXT NOT NULL,
    current INTEGER NOT NULL CHECK(current IN (0,1)),
    PRIMARY KEY(village_url,user_id,transcript_id,collective_id,version),
    FOREIGN KEY(village_url,user_id,transcript_id) REFERENCES transcript_collective_observations(village_url,user_id,transcript_id)
) WITHOUT ROWID;
CREATE UNIQUE INDEX idx_transcript_collective_evidence_current ON transcript_collective_evidence(village_url,user_id,transcript_id,collective_id) WHERE current=1;
CREATE TABLE collective_observation_versions (
    village_url TEXT NOT NULL,user_id TEXT NOT NULL,collective_id TEXT NOT NULL,
    version INTEGER NOT NULL CHECK(version>0),name TEXT NOT NULL,description TEXT,
    acceptance_mode TEXT CHECK(acceptance_mode IN ('open','verified_only','curated')),
    data_access TEXT CHECK(data_access IN ('members_only','contributors','public')),
    state TEXT NOT NULL CHECK(state IN ('observed','unavailable','invalidated')),
    first_observed_at TEXT NOT NULL,last_observed_at TEXT NOT NULL,current INTEGER NOT NULL CHECK(current IN (0,1)),
    PRIMARY KEY(village_url,user_id,collective_id,version),
    FOREIGN KEY(village_url,user_id) REFERENCES collective_observation_scopes(village_url,user_id)
) WITHOUT ROWID;
CREATE UNIQUE INDEX idx_collective_observation_versions_current ON collective_observation_versions(village_url,user_id,collective_id) WHERE current=1;
CREATE TABLE collective_relevance_observations (
    village_url TEXT NOT NULL,user_id TEXT NOT NULL,collective_id TEXT NOT NULL,
    reason TEXT NOT NULL CHECK(reason IN ('membership','contribution','organization','explicit')),
    observed_at TEXT NOT NULL,current INTEGER NOT NULL CHECK(current IN (0,1)),
    PRIMARY KEY(village_url,user_id,collective_id,reason),
    FOREIGN KEY(village_url,user_id) REFERENCES collective_observation_scopes(village_url,user_id)
) WITHOUT ROWID;
CREATE TABLE collective_tracking_preferences (
    village_url TEXT NOT NULL,user_id TEXT NOT NULL,collective_id TEXT NOT NULL,selected_at TEXT NOT NULL,
    PRIMARY KEY(village_url,user_id,collective_id),
    FOREIGN KEY(village_url,user_id) REFERENCES collective_observation_scopes(village_url,user_id)
) WITHOUT ROWID;
CREATE TABLE collective_source_installs (
    village_url TEXT NOT NULL,user_id TEXT NOT NULL,
    source TEXT NOT NULL CHECK(source IN ('membership','contribution-discovery','organization','explicit','submissions','attempts','associations')),
    source_id TEXT NOT NULL,installed_seq INTEGER NOT NULL,
    issued_seq INTEGER NOT NULL DEFAULT 0 CHECK(issued_seq>=0),
    PRIMARY KEY(village_url,user_id,source,source_id),
    FOREIGN KEY(village_url,user_id) REFERENCES collective_observation_scopes(village_url,user_id)
) WITHOUT ROWID;
CREATE TABLE collective_domain_observations (
    village_url TEXT NOT NULL,user_id TEXT NOT NULL,
    domain TEXT NOT NULL CHECK(domain IN ('catalog','history','associations')),
    installed_seq INTEGER NOT NULL DEFAULT 0,attempt_seq INTEGER NOT NULL DEFAULT 0,
    summary_json TEXT NOT NULL CHECK(json_valid(summary_json)),
    PRIMARY KEY(village_url,user_id,domain),
    FOREIGN KEY(village_url,user_id) REFERENCES collective_observation_scopes(village_url,user_id)
) WITHOUT ROWID;
CREATE TABLE collective_fetch_reports (
    village_url TEXT NOT NULL,user_id TEXT NOT NULL,invocation_seq INTEGER NOT NULL,
    last_attempt_at TEXT NOT NULL,last_completed_at TEXT,
    report_json TEXT NOT NULL CHECK(json_valid(report_json)),
    PRIMARY KEY(village_url,user_id),
    FOREIGN KEY(village_url,user_id) REFERENCES collective_observation_scopes(village_url,user_id)
) WITHOUT ROWID;
`
