package store

// schemaVersion is bumped whenever the DDL below changes. Migrations are
// applied inside a single transaction and recorded through PRAGMA user_version.
const schemaVersion = 3

// schemaDDL is the milestone 1 data model. Tables capacity_instances,
// capacity_events, partner_snapshots and operations follow development
// document §12; audit_log and the extra columns are documented extensions
// needed to satisfy the audit, drain-deadline and stale-event rules.
const schemaDDL = `
CREATE TABLE IF NOT EXISTS capacity_instances (
    id                   TEXT PRIMARY KEY,
    partner_id           TEXT NOT NULL,
    endpoint             TEXT NOT NULL,
    service_endpoint     TEXT,
    lease_id             TEXT,
    spec_json            TEXT NOT NULL DEFAULT '{}',
    instance_state       TEXT NOT NULL,
    service_state        TEXT NOT NULL,
    role                 TEXT,
    role_assigned_at     TEXT,
    router_worker_id     TEXT,
    readiness_generation INTEGER NOT NULL DEFAULT 0,
    prepare_attempts     INTEGER NOT NULL DEFAULT 0,
    start_attempts       INTEGER NOT NULL DEFAULT 0,
    drain_deadline_at    TEXT,
    pending_release      INTEGER NOT NULL DEFAULT 0,
    pending_update_json  TEXT,
    lease_updated_at     TEXT,
    last_error           TEXT,
    last_seen_at         TEXT,
    created_at           TEXT NOT NULL,
    updated_at           TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS capacity_instances_by_state
    ON capacity_instances(instance_state, service_state);
CREATE INDEX IF NOT EXISTS capacity_instances_by_partner
    ON capacity_instances(partner_id);

CREATE TABLE IF NOT EXISTS capacity_events (
    event_id     TEXT PRIMARY KEY,
    partner_id   TEXT NOT NULL,
    event_type   TEXT NOT NULL,
    payload_json TEXT NOT NULL,
    source       TEXT NOT NULL,
    occurred_at  TEXT,
    received_at  TEXT NOT NULL,
    processed_at TEXT,
    result       TEXT NOT NULL,
    reason       TEXT
);

CREATE INDEX IF NOT EXISTS capacity_events_by_partner
    ON capacity_events(partner_id, received_at);
CREATE INDEX IF NOT EXISTS capacity_events_by_result
    ON capacity_events(result, received_at);
CREATE INDEX IF NOT EXISTS capacity_events_pending_order
    ON capacity_events(result, occurred_at, event_id);

CREATE TABLE IF NOT EXISTS partner_snapshots (
    partner_id     TEXT PRIMARY KEY,
    snapshot_version TEXT NOT NULL,
    payload_json   TEXT NOT NULL,
    observed_at    TEXT NOT NULL,
    status         TEXT NOT NULL,
    instance_count INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS operations (
    operation_id TEXT PRIMARY KEY,
    instance_id  TEXT NOT NULL,
    operation_type TEXT NOT NULL,
    status       TEXT NOT NULL,
    attempt      INTEGER NOT NULL,
    next_retry_at TEXT,
    last_error   TEXT,
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS operations_open
    ON operations(status, next_retry_at);
CREATE INDEX IF NOT EXISTS operations_by_instance
    ON operations(instance_id, created_at);

CREATE TABLE IF NOT EXISTS audit_log (
    audit_id     INTEGER PRIMARY KEY AUTOINCREMENT,
    occurred_at  TEXT NOT NULL,
    actor        TEXT NOT NULL,
    action       TEXT NOT NULL,
    instance_id  TEXT,
    partner_id   TEXT,
    event_id     TEXT,
    details_json TEXT NOT NULL DEFAULT '{}'
);

CREATE INDEX IF NOT EXISTS audit_log_by_instance
    ON audit_log(instance_id, audit_id);
CREATE INDEX IF NOT EXISTS audit_log_by_action
    ON audit_log(action, audit_id);
`
